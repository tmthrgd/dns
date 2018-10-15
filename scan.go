package dns

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const maxTok = 2048 // Largest token we can return.

// Tokinize a RFC 1035 zone file. The tokenizer will normalize it:
// * Add ownernames if they are left blank;
// * Suppress sequences of spaces;
// * Make each RR fit on one line (_NEWLINE is send as last)
// * Handle comments: ;
// * Handle braces - anywhere.
const (
	// Zonefile
	zEOF = iota
	zString
	zBlank
	zQuote
	zNewline
	zRrtpe
	zOwner
	zClass
	zDirOrigin   // $ORIGIN
	zDirTTL      // $TTL
	zDirInclude  // $INCLUDE
	zDirGenerate // $GENERATE

	// Privatekey file
	zValue
	zKey

	zExpectOwnerDir      // Ownername
	zExpectOwnerBl       // Whitespace after the ownername
	zExpectAny           // Expect rrtype, ttl or class
	zExpectAnyNoClass    // Expect rrtype or ttl
	zExpectAnyNoClassBl  // The whitespace after _EXPECT_ANY_NOCLASS
	zExpectAnyNoTTL      // Expect rrtype or class
	zExpectAnyNoTTLBl    // Whitespace after _EXPECT_ANY_NOTTL
	zExpectRrtype        // Expect rrtype
	zExpectRrtypeBl      // Whitespace BEFORE rrtype
	zExpectRdata         // The first element of the rdata
	zExpectDirTTLBl      // Space after directive $TTL
	zExpectDirTTL        // Directive $TTL
	zExpectDirOriginBl   // Space after directive $ORIGIN
	zExpectDirOrigin     // Directive $ORIGIN
	zExpectDirIncludeBl  // Space after directive $INCLUDE
	zExpectDirInclude    // Directive $INCLUDE
	zExpectDirGenerate   // Directive $GENERATE
	zExpectDirGenerateBl // Space after directive $GENERATE
)

// ParseError is a parsing error. It contains the parse error and the location in the io.Reader
// where the error occurred.
type ParseError struct {
	file string
	err  string
	lex  lex
}

func (e *ParseError) Error() (s string) {
	if e.file != "" {
		s = e.file + ": "
	}
	s += "dns: " + e.err + ": " + strconv.QuoteToASCII(e.lex.token) + " at line: " +
		strconv.Itoa(e.lex.line) + ":" + strconv.Itoa(e.lex.column)
	return
}

type lex struct {
	token   string // text of the token
	err     bool   // when true, token text has lexer error
	value   uint8  // value: zString, _BLANK, etc.
	torc    uint16 // type or class as parsed in the lexer, we only need to look this up in the grammar
	line    int    // line in the file
	column  int    // column in the file
	comment string // any comment text seen
}

// Token holds the token that are returned when a zone file is parsed.
type Token struct {
	// The scanned resource record when error is not nil.
	RR
	// When an error occurred, this has the error specifics.
	Error *ParseError
	// A potential comment positioned after the RR and on the same line.
	Comment string
}

// ttlState describes the state necessary to fill in an omitted RR TTL
type ttlState struct {
	ttl           uint32 // ttl is the current default TTL
	isByDirective bool   // isByDirective indicates whether ttl was set by a $TTL directive
}

// NewRR reads the RR contained in the string s. Only the first RR is
// returned. If s contains no RR, return nil with no error. The class
// defaults to IN and TTL defaults to 3600. The full zone file syntax
// like $TTL, $ORIGIN, etc. is supported. All fields of the returned
// RR are set, except RR.Header().Rdlength which is set to 0.
func NewRR(s string) (RR, error) {
	if len(s) > 0 && s[len(s)-1] != '\n' { // We need a closing newline
		return ReadRR(strings.NewReader(s+"\n"), "")
	}
	return ReadRR(strings.NewReader(s), "")
}

// ReadRR reads the RR contained in q.
// See NewRR for more documentation.
func ReadRR(q io.Reader, filename string) (RR, error) {
	defttl := &ttlState{defaultTtl, false}
	r := <-parseZoneHelper(q, ".", filename, defttl, 1)
	if r == nil {
		return nil, nil
	}

	if r.Error != nil {
		return nil, r.Error
	}
	return r.RR, nil
}

// ParseZone reads a RFC 1035 style zonefile from r. It returns *Tokens on the
// returned channel, each consisting of either a parsed RR and optional comment
// or a nil RR and an error. The string file is only used
// in error reporting. The string origin is used as the initial origin, as
// if the file would start with an $ORIGIN directive.
// The directives $INCLUDE, $ORIGIN, $TTL and $GENERATE are supported.
// The channel t is closed by ParseZone when the end of r is reached.
//
// Basic usage pattern when reading from a string (z) containing the
// zone data:
//
//	for x := range dns.ParseZone(strings.NewReader(z), "", "") {
//		if x.Error != nil {
//                  // log.Println(x.Error)
//              } else {
//                  // Do something with x.RR
//              }
//	}
//
// Comments specified after an RR (and on the same line!) are returned too:
//
//	foo. IN A 10.0.0.1 ; this is a comment
//
// The text "; this is comment" is returned in Token.Comment. Comments inside the
// RR are returned concatenated along with the RR. Comments on a line by themselves
// are discarded.
func ParseZone(r io.Reader, origin, file string) chan *Token {
	return parseZoneHelper(r, origin, file, nil, 10000)
}

func parseZoneHelper(r io.Reader, origin, file string, defttl *ttlState, chansize int) chan *Token {
	t := make(chan *Token, chansize)
	go parseZone(r, origin, file, defttl, t)
	return t
}

func parseZone(r io.Reader, origin, file string, defttl *ttlState, t chan *Token) {
	defer close(t)

	zp := NewZoneParser(r, origin, file)
	zp.defttl = defttl

	for rr, ok := zp.Next(); ok; rr, ok = zp.Next() {
		t <- &Token{RR: rr, Comment: zp.Comment()}
	}

	if err := zp.Err(); err != nil {
		pe, ok := err.(*ParseError)
		if !ok {
			pe = &ParseError{file: file, err: err.Error()}
		}

		t <- &Token{Error: pe}
	}
}

type ZoneParser struct {
	c *zlexer

	parseErr *ParseError

	origin string
	file   string

	defttl *ttlState

	h        RR_Header
	prevName string

	include int

	sub *ZoneParser
	gen []RR

	osFile *os.File

	com string
}

func NewZoneParser(r io.Reader, origin, file string) *ZoneParser {
	var pe *ParseError
	if origin != "" {
		origin = Fqdn(origin)
		if _, ok := IsDomainName(origin); !ok {
			pe = &ParseError{file, "bad initial origin name", lex{}}
		}
	}

	return &ZoneParser{
		c: newZLexer(r),

		parseErr: pe,

		origin: origin,
		file:   file,
	}
}

func (zp *ZoneParser) Err() error {
	if zp.parseErr != nil {
		return zp.parseErr
	}

	if zp.sub != nil {
		if err := zp.sub.Err(); err != nil {
			return err
		}
	}

	return zp.c.Err()
}

func (zp *ZoneParser) Comment() string {
	return zp.com
}

func (zp *ZoneParser) subNext() (RR, bool) {
	rr, ok := zp.sub.Next()
	zp.com = zp.sub.com

	if !ok && zp.sub.Err() == nil {
		// zp.sub has ended
		zp.sub.osFile.Close()
		zp.sub = nil
		return zp.Next()
	}

	return rr, ok
}

func (zp *ZoneParser) Next() (RR, bool) {
	zp.com = ""

	if zp.parseErr != nil {
		return nil, false
	}
	if zp.sub != nil {
		return zp.subNext()
	}
	if len(zp.gen) > 0 {
		return zp.generateNext()
	}

	// 6 possible beginnings of a line, _ is a space
	// 0. zRRTYPE                              -> all omitted until the rrtype
	// 1. zOwner _ zRrtype                     -> class/ttl omitted
	// 2. zOwner _ zString _ zRrtype           -> class omitted
	// 3. zOwner _ zString _ zClass  _ zRrtype -> ttl/class
	// 4. zOwner _ zClass  _ zRrtype           -> ttl omitted
	// 5. zOwner _ zClass  _ zString _ zRrtype -> class/ttl (reversed)
	// After detecting these, we know the zRrtype so we can jump to functions
	// handling the rdata for each of these types.

	st := zExpectOwnerDir // initial state
	h := &zp.h
	for l, ok := zp.c.Next(); ok; l, ok = zp.c.Next() {
		// Lexer spotted an error already
		if l.err {
			zp.parseErr = &ParseError{zp.file, l.token, l}
			return nil, false
		}
		switch st {
		case zExpectOwnerDir:
			// We can also expect a directive, like $TTL or $ORIGIN
			if zp.defttl != nil {
				h.Ttl = zp.defttl.ttl
			}
			h.Class = ClassINET
			switch l.value {
			case zNewline:
				st = zExpectOwnerDir
			case zOwner:
				h.Name = l.token
				name, ok := toAbsoluteName(l.token, zp.origin)
				if !ok {
					zp.parseErr = &ParseError{zp.file, "bad owner name", l}
					return nil, false
				}
				h.Name = name
				zp.prevName = h.Name
				st = zExpectOwnerBl
			case zDirTTL:
				st = zExpectDirTTLBl
			case zDirOrigin:
				st = zExpectDirOriginBl
			case zDirInclude:
				st = zExpectDirIncludeBl
			case zDirGenerate:
				st = zExpectDirGenerateBl
			case zRrtpe:
				h.Name = zp.prevName
				h.Rrtype = l.torc
				st = zExpectRdata
			case zClass:
				h.Name = zp.prevName
				h.Class = l.torc
				st = zExpectAnyNoClassBl
			case zBlank:
				// Discard, can happen when there is nothing on the
				// line except the RR type
			case zString:
				ttl, ok := stringToTTL(l.token)
				if !ok {
					zp.parseErr = &ParseError{zp.file, "not a TTL", l}
					return nil, false
				}
				h.Ttl = ttl
				if zp.defttl == nil || !zp.defttl.isByDirective {
					zp.defttl = &ttlState{ttl, false}
				}
				st = zExpectAnyNoTTLBl

			default:
				zp.parseErr = &ParseError{zp.file, "syntax error at beginning", l}
				return nil, false
			}
		case zExpectDirIncludeBl:
			if l.value != zBlank {
				zp.parseErr = &ParseError{zp.file, "no blank after $INCLUDE-directive", l}
				return nil, false
			}
			st = zExpectDirInclude
		case zExpectDirInclude:
			if l.value != zString {
				zp.parseErr = &ParseError{zp.file, "expecting $INCLUDE value, not this...", l}
				return nil, false
			}
			neworigin := zp.origin // There may be optionally a new origin set after the filename, if not use current one
			switch l, _ := zp.c.Next(); l.value {
			case zBlank:
				l, _ := zp.c.Next()
				if l.value == zString {
					name, ok := toAbsoluteName(l.token, zp.origin)
					if !ok {
						zp.parseErr = &ParseError{zp.file, "bad origin name", l}
						return nil, false
					}
					neworigin = name
				}
			case zNewline, zEOF:
				// Ok
			default:
				zp.parseErr = &ParseError{zp.file, "garbage after $INCLUDE", l}
				return nil, false
			}
			if zp.include >= 7 {
				zp.parseErr = &ParseError{zp.file, "too deeply nested $INCLUDE", l}
				return nil, false
			}
			// Start with the new file
			includePath := l.token
			if !filepath.IsAbs(includePath) {
				includePath = filepath.Join(filepath.Dir(zp.file), includePath)
			}
			r1, e1 := os.Open(includePath)
			if e1 != nil {
				msg := fmt.Sprintf("failed to open `%s'", l.token)
				if !filepath.IsAbs(l.token) {
					msg += fmt.Sprintf(" as `%s'", includePath)
				}
				zp.parseErr = &ParseError{zp.file, msg, l}
				return nil, false
			}

			zp.sub = NewZoneParser(r1, neworigin, includePath)
			zp.sub.defttl, zp.sub.include, zp.sub.osFile = zp.defttl, zp.include+1, r1
			return zp.subNext()
		case zExpectDirTTLBl:
			if l.value != zBlank {
				zp.parseErr = &ParseError{zp.file, "no blank after $TTL-directive", l}
				return nil, false
			}
			st = zExpectDirTTL
		case zExpectDirTTL:
			if l.value != zString {
				zp.parseErr = &ParseError{zp.file, "expecting $TTL value, not this...", l}
				return nil, false
			}
			if e, _ := slurpRemainder(zp.c, zp.file); e != nil {
				zp.parseErr = e
				return nil, false
			}
			ttl, ok := stringToTTL(l.token)
			if !ok {
				zp.parseErr = &ParseError{zp.file, "expecting $TTL value, not this...", l}
				return nil, false
			}
			zp.defttl = &ttlState{ttl, true}
			st = zExpectOwnerDir
		case zExpectDirOriginBl:
			if l.value != zBlank {
				zp.parseErr = &ParseError{zp.file, "no blank after $ORIGIN-directive", l}
				return nil, false
			}
			st = zExpectDirOrigin
		case zExpectDirOrigin:
			if l.value != zString {
				zp.parseErr = &ParseError{zp.file, "expecting $ORIGIN value, not this...", l}
				return nil, false
			}
			if e, _ := slurpRemainder(zp.c, zp.file); e != nil {
				zp.parseErr = e
				return nil, false
			}
			name, ok := toAbsoluteName(l.token, zp.origin)
			if !ok {
				zp.parseErr = &ParseError{zp.file, "bad origin name", l}
				return nil, false
			}
			zp.origin = name
			st = zExpectOwnerDir
		case zExpectDirGenerateBl:
			if l.value != zBlank {
				zp.parseErr = &ParseError{zp.file, "no blank after $GENERATE-directive", l}
				return nil, false
			}
			st = zExpectDirGenerate
		case zExpectDirGenerate:
			if l.value != zString {
				zp.parseErr = &ParseError{zp.file, "expecting $GENERATE value, not this...", l}
				return nil, false
			}
			if errMsg := zp.generate(l); errMsg != "" {
				zp.parseErr = &ParseError{zp.file, errMsg, l}
				return nil, false
			}
			if len(zp.gen) > 0 {
				return zp.generateNext()
			}
			st = zExpectOwnerDir
		case zExpectOwnerBl:
			if l.value != zBlank {
				zp.parseErr = &ParseError{zp.file, "no blank after owner", l}
				return nil, false
			}
			st = zExpectAny
		case zExpectAny:
			switch l.value {
			case zRrtpe:
				if zp.defttl == nil {
					zp.parseErr = &ParseError{zp.file, "missing TTL with no previous value", l}
					return nil, false
				}
				h.Rrtype = l.torc
				st = zExpectRdata
			case zClass:
				h.Class = l.torc
				st = zExpectAnyNoClassBl
			case zString:
				ttl, ok := stringToTTL(l.token)
				if !ok {
					zp.parseErr = &ParseError{zp.file, "not a TTL", l}
					return nil, false
				}
				h.Ttl = ttl
				if zp.defttl == nil || !zp.defttl.isByDirective {
					zp.defttl = &ttlState{ttl, false}
				}
				st = zExpectAnyNoTTLBl
			default:
				zp.parseErr = &ParseError{zp.file, "expecting RR type, TTL or class, not this...", l}
				return nil, false
			}
		case zExpectAnyNoClassBl:
			if l.value != zBlank {
				zp.parseErr = &ParseError{zp.file, "no blank before class", l}
				return nil, false
			}
			st = zExpectAnyNoClass
		case zExpectAnyNoTTLBl:
			if l.value != zBlank {
				zp.parseErr = &ParseError{zp.file, "no blank before TTL", l}
				return nil, false
			}
			st = zExpectAnyNoTTL
		case zExpectAnyNoTTL:
			switch l.value {
			case zClass:
				h.Class = l.torc
				st = zExpectRrtypeBl
			case zRrtpe:
				h.Rrtype = l.torc
				st = zExpectRdata
			default:
				zp.parseErr = &ParseError{zp.file, "expecting RR type or class, not this...", l}
				return nil, false
			}
		case zExpectAnyNoClass:
			switch l.value {
			case zString:
				ttl, ok := stringToTTL(l.token)
				if !ok {
					zp.parseErr = &ParseError{zp.file, "not a TTL", l}
					return nil, false
				}
				h.Ttl = ttl
				if zp.defttl == nil || !zp.defttl.isByDirective {
					zp.defttl = &ttlState{ttl, false}
				}
				st = zExpectRrtypeBl
			case zRrtpe:
				h.Rrtype = l.torc
				st = zExpectRdata
			default:
				zp.parseErr = &ParseError{zp.file, "expecting RR type or TTL, not this...", l}
				return nil, false
			}
		case zExpectRrtypeBl:
			if l.value != zBlank {
				zp.parseErr = &ParseError{zp.file, "no blank before RR type", l}
				return nil, false
			}
			st = zExpectRrtype
		case zExpectRrtype:
			if l.value != zRrtpe {
				zp.parseErr = &ParseError{zp.file, "unknown RR type", l}
				return nil, false
			}
			h.Rrtype = l.torc
			st = zExpectRdata
		case zExpectRdata:
			r, e, c1 := setRR(*h, zp.c, zp.origin, zp.file)
			if e != nil {
				// If e.lex is nil than we have encounter a unknown RR type
				// in that case we substitute our current lex token
				if e.lex.token == "" && e.lex.value == 0 {
					e.lex = l // Uh, dirty
				}
				zp.parseErr = e
				return nil, false
			}
			zp.com = c1
			return r, true
		}
	}
	// If we get here, we and the h.Rrtype is still zero, we haven't parsed anything, this
	// is not an error, because an empty zone file is still a zone file.

	return nil, false
}

type zlexer struct {
	br io.ByteReader

	readErr error

	line   int
	column int

	com string

	l lex

	brace  int
	quote  bool
	space  bool
	commt  bool
	rrtype bool
	owner  bool

	nextL bool

	eol bool // end-of-line
}

func newZLexer(r io.Reader) *zlexer {
	br, ok := r.(io.ByteReader)
	if !ok {
		br = bufio.NewReaderSize(r, 1024)
	}

	return &zlexer{
		br: br,

		line: 1,

		owner: true,
	}
}

func (zl *zlexer) Err() error {
	if zl.readErr == io.EOF {
		return nil
	}

	return zl.readErr
}

// readByte returns the next byte from the input
func (zl *zlexer) readByte() (byte, bool) {
	if zl.readErr != nil {
		return 0, false
	}

	c, err := zl.br.ReadByte()
	if err != nil {
		zl.readErr = err
		return 0, false
	}

	// delay the newline handling until the next token is delivered,
	// fixes off-by-one errors when reporting a parse error.
	if zl.eol {
		zl.line++
		zl.column = 0
		zl.eol = false
	}

	if c == '\n' {
		zl.eol = true
	} else {
		zl.column++
	}

	return c, true
}

func (zl *zlexer) Next() (lex, bool) {
	l := &zl.l
	if zl.nextL {
		zl.nextL = false
		return *l, true
	}
	if l.err {
		// Parsing errors should be sticky.
		return lex{value: zEOF}, false
	}

	var (
		str [maxTok]byte // Hold string text
		com [maxTok]byte // Hold comment text

		stri int // Offset in str (0 means empty)
		comi int // Offset in com (0 means empty)

		escape bool
	)

	if zl.com != "" {
		comi = copy(com[:], zl.com)
		zl.com = ""
	}

	for x, ok := zl.readByte(); ok; x, ok = zl.readByte() {
		l.line, l.column = zl.line, zl.column
		l.comment = ""

		if stri >= len(str) {
			l.token = "token length insufficient for parsing"
			l.err = true
			return *l, true
		}
		if comi >= len(com) {
			l.token = "comment length insufficient for parsing"
			l.err = true
			return *l, true
		}

		switch x {
		case ' ', '\t':
			if escape || zl.quote {
				// Inside quotes or escaped this is legal.
				str[stri] = x
				stri++

				escape = false
				break
			}

			if zl.commt {
				com[comi] = x
				comi++
				break
			}

			var retL lex
			if stri == 0 {
				// Space directly in the beginning, handled in the grammar
			} else if zl.owner {
				// If we have a string and its the first, make it an owner
				l.value = zOwner
				l.token = string(str[:stri])

				// escape $... start with a \ not a $, so this will work
				switch strings.ToUpper(l.token) {
				case "$TTL":
					l.value = zDirTTL
				case "$ORIGIN":
					l.value = zDirOrigin
				case "$INCLUDE":
					l.value = zDirInclude
				case "$GENERATE":
					l.value = zDirGenerate
				}

				retL = *l
			} else {
				l.value = zString
				l.token = string(str[:stri])

				if !zl.rrtype {
					tokenUpper := strings.ToUpper(l.token)
					if t, ok := StringToType[tokenUpper]; ok {
						l.value = zRrtpe
						l.torc = t

						zl.rrtype = true
					} else if strings.HasPrefix(tokenUpper, "TYPE") {
						t, ok := typeToInt(l.token)
						if !ok {
							l.token = "unknown RR type"
							l.err = true
							return *l, true
						}

						l.value = zRrtpe
						l.torc = t

						zl.rrtype = true
					}

					if t, ok := StringToClass[tokenUpper]; ok {
						l.value = zClass
						l.torc = t
					} else if strings.HasPrefix(tokenUpper, "CLASS") {
						t, ok := classToInt(l.token)
						if !ok {
							l.token = "unknown class"
							l.err = true
							return *l, true
						}

						l.value = zClass
						l.torc = t
					}
				}

				retL = *l
			}

			zl.owner = false

			if !zl.space {
				zl.space = true

				l.value = zBlank
				l.token = " "

				if retL == (lex{}) {
					return *l, true
				}

				zl.nextL = true
			}

			if retL != (lex{}) {
				return retL, true
			}
		case ';':
			if escape || zl.quote {
				// Inside quotes or escaped this is legal.
				str[stri] = x
				stri++

				escape = false
				break
			}

			zl.commt = true
			zl.com = ""

			if comi > 1 {
				// A newline was previously seen inside a comment that
				// was inside braces and we delayed adding it until now.
				com[comi] = ' ' // convert newline to space
				comi++
			}

			com[comi] = ';'
			comi++

			if stri > 0 {
				zl.com = string(com[:comi])

				l.value = zString
				l.token = string(str[:stri])
				return *l, true
			}
		case '\r':
			escape = false

			if zl.quote {
				str[stri] = x
				stri++
			}

			// discard if outside of quotes
		case '\n':
			escape = false

			// Escaped newline
			if zl.quote {
				str[stri] = x
				stri++
				break
			}

			if zl.commt {
				// Reset a comment
				zl.commt = false
				zl.rrtype = false

				// If not in a brace this ends the comment AND the RR
				if zl.brace == 0 {
					zl.owner = true

					l.value = zNewline
					l.token = "\n"
					l.comment = string(com[:comi])
					return *l, true
				}

				zl.com = string(com[:comi])
				break
			}

			if zl.brace == 0 {
				// If there is previous text, we should output it here
				var retL lex
				if stri != 0 {
					l.value = zString
					l.token = string(str[:stri])

					if !zl.rrtype {
						tokenUpper := strings.ToUpper(l.token)
						if t, ok := StringToType[tokenUpper]; ok {
							zl.rrtype = true

							l.value = zRrtpe
							l.torc = t
						}
					}

					retL = *l
				}

				l.value = zNewline
				l.token = "\n"
				l.comment = zl.com

				zl.com = ""
				zl.rrtype = false
				zl.owner = true

				if retL != (lex{}) {
					zl.nextL = true
					return retL, true
				}

				return *l, true
			}
		case '\\':
			// comments do not get escaped chars, everything is copied
			if zl.commt {
				com[comi] = x
				comi++
				break
			}

			// something already escaped must be in string
			if escape {
				str[stri] = x
				stri++

				escape = false
				break
			}

			// something escaped outside of string gets added to string
			str[stri] = x
			stri++

			escape = true
		case '"':
			if zl.commt {
				com[comi] = x
				comi++
				break
			}

			if escape {
				str[stri] = x
				stri++

				escape = false
				break
			}

			zl.space = false

			// send previous gathered text and the quote
			var retL lex
			if stri != 0 {
				l.value = zString
				l.token = string(str[:stri])

				retL = *l
			}

			// send quote itself as separate token
			l.value = zQuote
			l.token = "\""

			zl.quote = !zl.quote

			if retL != (lex{}) {
				zl.nextL = true
				return retL, true
			}

			return *l, true
		case '(', ')':
			if zl.commt {
				com[comi] = x
				comi++
				break
			}

			if escape || zl.quote {
				// Inside quotes or escaped this is legal.
				str[stri] = x
				stri++

				escape = false
				break
			}

			switch x {
			case ')':
				zl.brace--

				if zl.brace < 0 {
					l.token = "extra closing brace"
					l.err = true
					return *l, true
				}
			case '(':
				zl.brace++
			}
		default:
			escape = false

			if zl.commt {
				com[comi] = x
				comi++
				break
			}

			str[stri] = x
			stri++

			zl.space = false
		}
	}

	var retL lex
	if stri > 0 {
		// Send remainder of str
		l.value = zString
		l.token = string(str[:stri])
		retL = *l

		if comi <= 0 {
			return retL, true
		}
	}

	if comi > 0 {
		// Send remainder of com
		l.value = zNewline
		l.token = "\n"
		l.comment = string(com[:comi])

		if retL != (lex{}) {
			zl.nextL = true
			return retL, true
		}

		return *l, true
	}

	if zl.brace != 0 {
		l.comment = "" // in case there was left over string and comment
		l.token = "unbalanced brace"
		l.err = true
		return *l, true
	}

	return lex{value: zEOF}, false
}

// Extract the class number from CLASSxx
func classToInt(token string) (uint16, bool) {
	offset := 5
	if len(token) < offset+1 {
		return 0, false
	}
	class, err := strconv.ParseUint(token[offset:], 10, 16)
	if err != nil {
		return 0, false
	}
	return uint16(class), true
}

// Extract the rr number from TYPExxx
func typeToInt(token string) (uint16, bool) {
	offset := 4
	if len(token) < offset+1 {
		return 0, false
	}
	typ, err := strconv.ParseUint(token[offset:], 10, 16)
	if err != nil {
		return 0, false
	}
	return uint16(typ), true
}

// stringToTTL parses things like 2w, 2m, etc, and returns the time in seconds.
func stringToTTL(token string) (uint32, bool) {
	s := uint32(0)
	i := uint32(0)
	for _, c := range token {
		switch c {
		case 's', 'S':
			s += i
			i = 0
		case 'm', 'M':
			s += i * 60
			i = 0
		case 'h', 'H':
			s += i * 60 * 60
			i = 0
		case 'd', 'D':
			s += i * 60 * 60 * 24
			i = 0
		case 'w', 'W':
			s += i * 60 * 60 * 24 * 7
			i = 0
		case '0', '1', '2', '3', '4', '5', '6', '7', '8', '9':
			i *= 10
			i += uint32(c) - '0'
		default:
			return 0, false
		}
	}
	return s + i, true
}

// Parse LOC records' <digits>[.<digits>][mM] into a
// mantissa exponent format. Token should contain the entire
// string (i.e. no spaces allowed)
func stringToCm(token string) (e, m uint8, ok bool) {
	if token[len(token)-1] == 'M' || token[len(token)-1] == 'm' {
		token = token[0 : len(token)-1]
	}
	s := strings.SplitN(token, ".", 2)
	var meters, cmeters, val int
	var err error
	switch len(s) {
	case 2:
		if cmeters, err = strconv.Atoi(s[1]); err != nil {
			return
		}
		fallthrough
	case 1:
		if meters, err = strconv.Atoi(s[0]); err != nil {
			return
		}
	case 0:
		// huh?
		return 0, 0, false
	}
	ok = true
	if meters > 0 {
		e = 2
		val = meters
	} else {
		e = 0
		val = cmeters
	}
	for val > 10 {
		e++
		val /= 10
	}
	if e > 9 {
		ok = false
	}
	m = uint8(val)
	return
}

func toAbsoluteName(name, origin string) (absolute string, ok bool) {
	// check for an explicit origin reference
	if name == "@" {
		// require a nonempty origin
		if origin == "" {
			return "", false
		}
		return origin, true
	}

	// require a valid domain name
	_, ok = IsDomainName(name)
	if !ok || name == "" {
		return "", false
	}

	// check if name is already absolute
	if name[len(name)-1] == '.' {
		return name, true
	}

	// require a nonempty origin
	if origin == "" {
		return "", false
	}
	return appendOrigin(name, origin), true
}

func appendOrigin(name, origin string) string {
	if origin == "." {
		return name + origin
	}
	return name + "." + origin
}

// LOC record helper function
func locCheckNorth(token string, latitude uint32) (uint32, bool) {
	switch token {
	case "n", "N":
		return LOC_EQUATOR + latitude, true
	case "s", "S":
		return LOC_EQUATOR - latitude, true
	}
	return latitude, false
}

// LOC record helper function
func locCheckEast(token string, longitude uint32) (uint32, bool) {
	switch token {
	case "e", "E":
		return LOC_EQUATOR + longitude, true
	case "w", "W":
		return LOC_EQUATOR - longitude, true
	}
	return longitude, false
}

// "Eat" the rest of the "line". Return potential comments
func slurpRemainder(c *zlexer, f string) (*ParseError, string) {
	l, _ := c.Next()
	com := ""
	switch l.value {
	case zBlank:
		l, _ = c.Next()
		com = l.comment
		if l.value != zNewline && l.value != zEOF {
			return &ParseError{f, "garbage after rdata", l}, ""
		}
	case zNewline:
		com = l.comment
	case zEOF:
	default:
		return &ParseError{f, "garbage after rdata", l}, ""
	}
	return nil, com
}

// Parse a 64 bit-like ipv6 address: "0014:4fff:ff20:ee64"
// Used for NID and L64 record.
func stringToNodeID(l lex) (uint64, *ParseError) {
	if len(l.token) < 19 {
		return 0, &ParseError{l.token, "bad NID/L64 NodeID/Locator64", l}
	}
	// There must be three colons at fixes postitions, if not its a parse error
	if l.token[4] != ':' && l.token[9] != ':' && l.token[14] != ':' {
		return 0, &ParseError{l.token, "bad NID/L64 NodeID/Locator64", l}
	}
	s := l.token[0:4] + l.token[5:9] + l.token[10:14] + l.token[15:19]
	u, err := strconv.ParseUint(s, 16, 64)
	if err != nil {
		return 0, &ParseError{l.token, "bad NID/L64 NodeID/Locator64", l}
	}
	return u, nil
}
