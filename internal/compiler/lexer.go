package compiler

import (
	"bytes"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

type lexer struct {
	file string
	src  []byte
	off  int
	line int
	col  int

	insertSemi bool
	prevTok    Tok // the token before, for a member spelled like a keyword (p.for, .case)
	errs       ErrorList

	// pragmas collects "//squark:..." comment lines seen since the last token.
	pragmas []string
	// doc collects the comment lines directly above the next token (no blank line between, and
	// not trailing code on their line): a declaration's doc comment.
	doc         []string
	docEnd      int // line of doc's last comment
	lastTokLine int
}

type lexToken struct {
	tok     Tok
	lit     string
	pos     Pos
	end     Pos // just after the token
	pragmas []string
	doc     []string
}

func newLexer(file string, src []byte) *lexer {
	l := &lexer{file: file, src: src, line: 1, col: 1}
	if bytes.HasPrefix(src, []byte("\xEF\xBB\xBF")) {
		l.off = 3 // a leading byte order mark is ignored, as in Go
	}
	if !utf8.Valid(src) {
		// as in Go, source must be UTF-8: report the first bad byte
		line, col := 1, 1
		for i := 0; i < len(src); {
			r, sz := utf8.DecodeRune(src[i:])
			if r == utf8.RuneError && sz == 1 {
				l.errorf(Pos{file, line, col}, "invalid UTF-8 encoding")
				break
			}
			if r == '\n' {
				line, col = line+1, 1
			} else {
				col++
			}
			i += sz
		}
	}
	return l
}

func (l *lexer) pos() Pos { return Pos{l.file, l.line, l.col} }

// text is the source from from up to to without its comments, with each run of white space (a line
// break and the indentation after it) as one space.
func (l *lexer) text(from, to Pos) string {
	off := func(p Pos) int {
		line, col, i := 1, 1, 0
		for i < len(l.src) && (line < p.Line || line == p.Line && col < p.Col) {
			r, sz := utf8.DecodeRune(l.src[i:])
			i += sz
			if r == '\n' {
				line, col = line+1, 1
			} else {
				col++
			}
		}
		return i
	}
	i, j := off(from), off(to)
	if j < i {
		return ""
	}
	src := l.src[i:j]
	var b strings.Builder
	var quote byte // inside a literal that ends with quote
	space := false // white space to write before the next character
	for k := 0; k < len(src); k++ {
		c := src[k]
		switch {
		case quote != 0:
			if c == '\\' && quote != '`' && k+1 < len(src) {
				b.WriteByte(c)
				k++
				c = src[k]
			} else if c == quote {
				quote = 0
			}
		case c == '"' || c == '\'' || c == '`':
			quote = c
		case c == '/' && k+1 < len(src) && src[k+1] == '/':
			for k < len(src) && src[k] != '\n' {
				k++
			}
			c = ' '
		case c == '/' && k+1 < len(src) && src[k+1] == '*':
			end := bytes.Index(src[k+2:], []byte("*/"))
			if end < 0 {
				end = len(src) - k - 2
			}
			k += end + 3
			c = ' '
		}
		if quote == 0 && (c == ' ' || c == '\t' || c == '\n' || c == '\r') {
			space = true
			continue
		}
		if space && b.Len() > 0 {
			b.WriteByte(' ')
		}
		space = false
		b.WriteByte(c)
	}
	return b.String()
}

func (l *lexer) errorf(p Pos, msg string) {
	l.errs = append(l.errs, &Error{p, msg})
}

func (l *lexer) peekc() rune {
	if l.off >= len(l.src) {
		return -1
	}
	r, _ := utf8.DecodeRune(l.src[l.off:])
	return r
}

func (l *lexer) peekAt(n int) byte {
	if l.off+n >= len(l.src) {
		return 0
	}
	return l.src[l.off+n]
}

func (l *lexer) next() rune {
	if l.off >= len(l.src) {
		return -1
	}
	r, sz := utf8.DecodeRune(l.src[l.off:])
	l.off += sz
	if r == '\n' {
		l.line++
		l.col = 1
	} else {
		l.col++
	}
	return r
}

func isLetter(r rune) bool {
	return r == '_' || unicode.IsLetter(r)
}

func isDigit(r rune) bool { return r >= '0' && r <= '9' }

// Next returns the next token, inserting semicolons Go-style.
func (l *lexer) Next() lexToken {
again:
	// skip whitespace, but newline may become a semicolon
	for {
		c := l.peekc()
		if c == '\n' {
			if l.insertSemi {
				p := l.pos()
				l.next()
				l.insertSemi = false
				return lexToken{tok: SEMICOLON, lit: "\n", pos: p}
			}
			l.next()
			continue
		}
		if c == ' ' || c == '\t' || c == '\r' {
			l.next()
			continue
		}
		break
	}
	p := l.pos()
	c := l.peekc()
	if c == -1 {
		if l.insertSemi {
			l.insertSemi = false
			return lexToken{tok: SEMICOLON, lit: "\n", pos: p}
		}
		return lexToken{tok: EOF, pos: p}
	}
	// comments
	if c == '/' && l.peekAt(1) == '/' {
		start := l.off
		for l.peekc() != '\n' && l.peekc() != -1 {
			l.next()
		}
		text := string(l.src[start:l.off])
		if strings.HasPrefix(text, "//squark:") {
			l.pragmas = append(l.pragmas, strings.TrimSpace(text[len("//squark:"):]))
		} else if p.Line != l.lastTokLine { // a comment line of its own (not trailing code)
			if len(l.doc) == 0 || l.docEnd != p.Line-1 {
				l.doc = nil
			}
			l.doc = append(l.doc, strings.TrimPrefix(text[2:], " "))
			l.docEnd = p.Line
		}
		goto again
	}
	if c == '/' && l.peekAt(1) == '*' {
		l.next()
		l.next()
		hadNewline := false
		for {
			r := l.next()
			if r == -1 {
				l.errorf(p, "unterminated comment")
				break
			}
			if r == '\n' {
				hadNewline = true
			}
			if r == '*' && l.peekc() == '/' {
				l.next()
				break
			}
		}
		if hadNewline && l.insertSemi {
			l.insertSemi = false
			return lexToken{tok: SEMICOLON, lit: "\n", pos: p}
		}
		goto again
	}

	t := l.scan(p)
	t.end = l.pos()
	afterDot := l.prevTok == PERIOD
	l.prevTok = t.tok
	switch t.tok {
	case IDENT, INT, FLOAT, CHAR, STRING, BREAK, CONTINUE, RETURN, INC, DEC, RPAREN, RBRACK, RBRACE, ARENA, BANG, CHAN:
		l.insertSemi = true
	default:
		// a member may be spelled like a keyword (k := K.case): it ends a line like a name
		_, kw := keywords[t.lit]
		l.insertSemi = afterDot && kw
	}
	if len(l.pragmas) > 0 && t.tok != SEMICOLON {
		t.pragmas = l.pragmas
		l.pragmas = nil
	}
	if t.tok != SEMICOLON {
		if len(l.doc) > 0 && l.docEnd == t.pos.Line-1 {
			t.doc = l.doc
		}
		l.doc = nil
		l.lastTokLine = t.pos.Line
	}
	return t
}

func (l *lexer) scan(p Pos) lexToken {
	c := l.peekc()
	if isLetter(c) {
		start := l.off
		for isLetter(l.peekc()) || isDigit(l.peekc()) {
			l.next()
		}
		lit := string(l.src[start:l.off])
		if kw, ok := keywords[lit]; ok {
			return lexToken{tok: kw, lit: lit, pos: p}
		}
		return lexToken{tok: IDENT, lit: lit, pos: p}
	}
	if isDigit(c) || (c == '.' && isDigit(rune(l.peekAt(1))) && (l.off == 0 || l.src[l.off-1] != '.')) {
		return l.number(p)
	}
	switch c {
	case '"':
		return l.str(p)
	case '`':
		l.next()
		start := l.off
		for l.peekc() != '`' {
			if l.peekc() == -1 {
				l.errorf(p, "unterminated raw string")
				return lexToken{tok: STRING, lit: "", pos: p}
			}
			l.next()
		}
		raw := strings.ReplaceAll(string(l.src[start:l.off]), "\r", "")
		l.next()
		return lexToken{tok: STRING, lit: raw, pos: p}
	case '\'':
		return l.char(p)
	}
	l.next()
	two := func(second rune, yes, no Tok) lexToken {
		if l.peekc() == second {
			l.next()
			return lexToken{tok: yes, pos: p}
		}
		return lexToken{tok: no, pos: p}
	}
	switch c {
	case '+':
		if l.peekc() == '+' {
			l.next()
			return lexToken{tok: INC, pos: p}
		}
		return two('=', ADD_ASSIGN, ADD)
	case '-':
		if l.peekc() == '-' {
			l.next()
			return lexToken{tok: DEC, pos: p}
		}
		if l.peekc() == '>' {
			l.next()
			return lexToken{tok: ARROWR, pos: p}
		}
		return two('=', SUB_ASSIGN, SUB)
	case '*':
		return two('=', MUL_ASSIGN, MUL)
	case '/':
		return two('=', QUO_ASSIGN, QUO)
	case '%':
		return two('=', REM_ASSIGN, REM)
	case '^':
		return two('=', XOR_ASSIGN, XOR)
	case '&':
		if l.peekc() == '&' {
			l.next()
			return lexToken{tok: AMPAMP, pos: p}
		}
		if l.peekc() == '^' {
			l.next()
			return two('=', AND_NOT_ASSIGN, AND_NOT)
		}
		return two('=', AND_ASSIGN, AND)
	case '|':
		if l.peekc() == '|' {
			l.next()
			return lexToken{tok: BARBAR, pos: p}
		}
		return two('=', OR_ASSIGN, OR)
	case '<':
		if l.peekc() == '-' {
			l.next()
			return lexToken{tok: ARROW, pos: p}
		}
		if l.peekc() == '<' {
			l.next()
			return two('=', SHL_ASSIGN, SHL)
		}
		return two('=', LEQ, LSS)
	case '>':
		if l.peekc() == '>' {
			l.next()
			return two('=', SHR_ASSIGN, SHR)
		}
		return two('=', GEQ, GTR)
	case '=':
		return two('=', EQL, ASSIGN)
	case '!':
		return two('=', NEQ, BANG)
	case ':':
		return two('=', DEFINE, COLON)
	case '.':
		if l.peekc() == '.' && l.peekAt(1) == '.' {
			l.next()
			l.next()
			return lexToken{tok: ELLIPSIS, pos: p}
		}
		if l.peekc() == '.' {
			l.next()
			return lexToken{tok: DOTDOT, pos: p}
		}
		return lexToken{tok: PERIOD, pos: p}
	case '(':
		return lexToken{tok: LPAREN, pos: p}
	case ')':
		return lexToken{tok: RPAREN, pos: p}
	case '[':
		return lexToken{tok: LBRACK, pos: p}
	case ']':
		return lexToken{tok: RBRACK, pos: p}
	case '{':
		return lexToken{tok: LBRACE, pos: p}
	case '}':
		return lexToken{tok: RBRACE, pos: p}
	case ',':
		return lexToken{tok: COMMA, pos: p}
	case ';':
		return lexToken{tok: SEMICOLON, lit: ";", pos: p}
	}
	l.errorf(p, "unexpected character "+string(c))
	return l.Next()
}

func (l *lexer) number(p Pos) lexToken {
	start := l.off
	isFloat := false
	if l.peekc() == '0' && (l.peekAt(1) == 'x' || l.peekAt(1) == 'X' || l.peekAt(1) == 'b' || l.peekAt(1) == 'B' || l.peekAt(1) == 'o' || l.peekAt(1) == 'O') {
		l.next()
		l.next()
		hex := l.src[start+1] == 'x' || l.src[start+1] == 'X'
		hexDigits := func() {
			for c := l.peekc(); isDigit(c) || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F') || c == '_'; c = l.peekc() {
				l.next()
			}
		}
		hexDigits()
		if hex && ((l.peekc() == '.' && l.peekAt(1) != '.') || l.peekc() == 'p' || l.peekc() == 'P') { // 0x0..0x3 is a range
			// hexadecimal float: 0x1.8p3
			if l.peekc() == '.' {
				l.next()
				hexDigits()
			}
			if l.peekc() != 'p' && l.peekc() != 'P' {
				l.errorf(p, "hexadecimal mantissa requires a 'p' exponent")
			} else {
				l.exponent(p)
			}
			return l.numberToken(FLOAT, start, p)
		}
		return l.numberToken(INT, start, p)
	}
	for isDigit(l.peekc()) || l.peekc() == '_' {
		l.next()
	}
	if l.peekc() == '.' && l.peekAt(1) != '.' { // 0..n is a range, not 0. and .n
		isFloat = true
		l.next()
		for isDigit(l.peekc()) || l.peekc() == '_' {
			l.next()
		}
	}
	if l.peekc() == 'e' || l.peekc() == 'E' {
		isFloat = true
		l.exponent(p)
	}
	// size suffixes: 4K, 16M, 1G (binary multiples)
	if !isFloat {
		if c := l.peekc(); c == 'K' || c == 'M' || c == 'G' {
			n := rune(l.peekAt(1))
			if !isLetter(n) && !isDigit(n) {
				l.next()
			}
		}
	}
	if isFloat {
		return l.numberToken(FLOAT, start, p)
	}
	return l.numberToken(INT, start, p)
}

// exponent reads e/E/p/P, an optional sign and the exponent's digits.
func (l *lexer) exponent(p Pos) {
	l.next()
	if l.peekc() == '+' || l.peekc() == '-' {
		l.next()
	}
	if !isDigit(l.peekc()) {
		l.errorf(p, "exponent has no digits")
	}
	for isDigit(l.peekc()) || l.peekc() == '_' {
		l.next()
	}
}

func (l *lexer) numberToken(tok Tok, start int, p Pos) lexToken {
	lit := string(l.src[start:l.off])
	if i := invalidSep(lit); i >= 0 {
		l.errorf(p, "'_' must separate successive digits")
	}
	return lexToken{tok: tok, lit: lit, pos: p}
}

// invalidSep returns the index of the first '_' in a number literal that does not separate two
// digits (a base prefix counts as a digit), or -1. It is go/scanner's rule.
func invalidSep(x string) int {
	x1 := ' ' // prefix char, we only care if it's 'x'
	d := '.'  // previous char class: '_', '0' (a digit), or '.' (anything else)
	i := 0
	if len(x) >= 2 && x[0] == '0' {
		x1 = rune(x[1]) | 0x20
		if x1 == 'x' || x1 == 'o' || x1 == 'b' {
			d = '0'
			i = 2
		}
	}
	for ; i < len(x); i++ {
		p := d
		d = rune(x[i])
		switch {
		case d == '_':
			if p != '0' {
				return i
			}
		case isDigit(d) || x1 == 'x' && (d|0x20 >= 'a' && d|0x20 <= 'f'):
			d = '0'
		default:
			if p == '_' {
				return i - 1
			}
			d = '.'
		}
	}
	if d == '_' {
		return len(x) - 1
	}
	return -1
}

func (l *lexer) escape(quote rune, sb *strings.Builder) bool {
	p := l.pos()
	c := l.next()
	ok := true
	switch c {
	case 'n':
		sb.WriteByte('\n')
	case 't':
		sb.WriteByte('\t')
	case 'r':
		sb.WriteByte('\r')
	case '0', '1', '2', '3', '4', '5', '6', '7':
		var v int
		v, ok = l.escDigits(2, 8, "octal")
		v += int(c-'0') << 6
		if ok && v > 255 {
			l.errorf(p, fmt.Sprintf("octal escape value %d > 255", v))
			ok = false
		}
		sb.WriteByte(byte(v))
	case 'a':
		sb.WriteByte(7)
	case 'b':
		sb.WriteByte(8)
	case 'f':
		sb.WriteByte(12)
	case 'v':
		sb.WriteByte(11)
	case '\\':
		sb.WriteByte('\\')
	case 'x':
		var v int
		v, ok = l.escDigits(2, 16, "hexadecimal")
		sb.WriteByte(byte(v))
	case 'u', 'U':
		n := 4
		if c == 'U' {
			n = 8
		}
		var v int
		v, ok = l.escDigits(n, 16, "Unicode")
		if ok && (v > utf8.MaxRune || v >= 0xD800 && v < 0xE000) {
			l.errorf(p, "escape is invalid Unicode code point")
			ok = false
		}
		sb.WriteRune(rune(v))
	default:
		if c == quote {
			sb.WriteRune(c)
			return true
		}
		l.errorf(p, "unknown escape sequence")
		ok = false
	}
	return ok
}

// escDigits reads the n base-b digits of an escape. A non-digit is reported and left unread, so a
// closing quote still ends the literal.
func (l *lexer) escDigits(n, base int, what string) (int, bool) {
	v := 0
	for i := 0; i < n; i++ {
		c := l.peekc()
		d := hexval(c)
		if d < 0 || d >= base {
			if c == -1 || c == '\n' {
				l.errorf(l.pos(), "escape sequence not terminated")
			} else {
				l.errorf(l.pos(), fmt.Sprintf("invalid character %q in %s escape", c, what))
			}
			return v, false
		}
		l.next()
		v = v*base + d
	}
	return v, true
}

func hexval(r rune) int {
	switch {
	case r >= '0' && r <= '9':
		return int(r - '0')
	case r >= 'a' && r <= 'f':
		return int(r-'a') + 10
	case r >= 'A' && r <= 'F':
		return int(r-'A') + 10
	}
	return -1
}

func (l *lexer) str(p Pos) lexToken {
	l.next()
	var sb strings.Builder
	for {
		c := l.peekc()
		if c == '"' {
			l.next()
			break
		}
		if c == '\n' || c == -1 {
			l.errorf(p, "unterminated string literal")
			break
		}
		l.next()
		if c == '\\' {
			l.escape('"', &sb)
			continue
		}
		sb.WriteRune(c)
	}
	return lexToken{tok: STRING, lit: sb.String(), pos: p}
}

func (l *lexer) char(p Pos) lexToken {
	l.next()
	var sb strings.Builder
	switch c := l.peekc(); c {
	case '\'':
		l.next()
		l.errorf(p, "empty rune literal or unescaped ' in rune literal")
		return lexToken{tok: CHAR, lit: "\x00", pos: p}
	case '\n', -1:
		l.errorf(p, "rune literal not terminated")
		return lexToken{tok: CHAR, lit: "\x00", pos: p}
	}
	c := l.next()
	ok := true
	if c == '\\' {
		ok = l.escape('\'', &sb)
	} else {
		sb.WriteRune(c)
	}
	if l.peekc() != '\'' {
		if ok { // after a bad escape, just find the end
			l.errorf(p, "more than one character in rune literal")
		}
		for c := l.peekc(); c != '\'' && c != '\n' && c != -1; c = l.peekc() {
			l.next()
		}
	}
	if l.peekc() == '\'' {
		l.next()
	}
	s := sb.String()
	r, _ := utf8.DecodeRuneInString(s)
	if len(s) == 1 {
		r = rune(s[0])
	}
	return lexToken{tok: CHAR, lit: string(rune(r)), pos: p}
}
