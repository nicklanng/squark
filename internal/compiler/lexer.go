package compiler

import (
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
	errs       ErrorList

	// pragmas collects "//squark:..." comment lines seen since the last token.
	pragmas []string
}

type lexToken struct {
	tok     Tok
	lit     string
	pos     Pos
	pragmas []string
}

func newLexer(file string, src []byte) *lexer {
	return &lexer{file: file, src: src, line: 1, col: 1}
}

func (l *lexer) pos() Pos { return Pos{l.file, l.line, l.col} }

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
	switch t.tok {
	case IDENT, INT, FLOAT, CHAR, STRING, BREAK, CONTINUE, RETURN, INC, DEC, RPAREN, RBRACK, RBRACE:
		l.insertSemi = true
	default:
		l.insertSemi = false
	}
	if len(l.pragmas) > 0 && t.tok != SEMICOLON {
		t.pragmas = l.pragmas
		l.pragmas = nil
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
	if isDigit(c) || (c == '.' && isDigit(rune(l.peekAt(1)))) {
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
			return lexToken{tok: LAND, pos: p}
		}
		if l.peekc() == '^' {
			l.next()
			return two('=', AND_NOT_ASSIGN, AND_NOT)
		}
		return two('=', AND_ASSIGN, AND)
	case '|':
		if l.peekc() == '|' {
			l.next()
			return lexToken{tok: LOR, pos: p}
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
		return two('=', NEQ, NOT)
	case ':':
		return two('=', DEFINE, COLON)
	case '.':
		if l.peekc() == '.' && l.peekAt(1) == '.' {
			l.next()
			l.next()
			return lexToken{tok: ELLIPSIS, pos: p}
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
		for {
			c := l.peekc()
			if isDigit(c) || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F') || c == '_' {
				l.next()
				continue
			}
			break
		}
		return lexToken{tok: INT, lit: string(l.src[start:l.off]), pos: p}
	}
	for isDigit(l.peekc()) || l.peekc() == '_' {
		l.next()
	}
	if l.peekc() == '.' {
		isFloat = true
		l.next()
		for isDigit(l.peekc()) || l.peekc() == '_' {
			l.next()
		}
	}
	if l.peekc() == 'e' || l.peekc() == 'E' {
		isFloat = true
		l.next()
		if l.peekc() == '+' || l.peekc() == '-' {
			l.next()
		}
		for isDigit(l.peekc()) {
			l.next()
		}
	}
	lit := string(l.src[start:l.off])
	if isFloat {
		return lexToken{tok: FLOAT, lit: lit, pos: p}
	}
	return lexToken{tok: INT, lit: lit, pos: p}
}

func (l *lexer) escape(quote rune, sb *strings.Builder) {
	p := l.pos()
	c := l.next()
	switch c {
	case 'n':
		sb.WriteByte('\n')
	case 't':
		sb.WriteByte('\t')
	case 'r':
		sb.WriteByte('\r')
	case '0', '1', '2', '3', '4', '5', '6', '7':
		v := int(c - '0')
		for i := 0; i < 2; i++ {
			d := l.next()
			v = v*8 + int(d-'0')
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
		v := 0
		for i := 0; i < 2; i++ {
			v = v*16 + hexval(l.next())
		}
		sb.WriteByte(byte(v))
	case 'u', 'U':
		n := 4
		if c == 'U' {
			n = 8
		}
		v := 0
		for i := 0; i < n; i++ {
			v = v*16 + hexval(l.next())
		}
		sb.WriteRune(rune(v))
	default:
		if c == quote {
			sb.WriteRune(c)
			return
		}
		l.errorf(p, "unknown escape sequence")
	}
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
	return 0
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
	c := l.next()
	if c == '\\' {
		l.escape('\'', &sb)
	} else {
		sb.WriteRune(c)
	}
	if l.peekc() != '\'' {
		l.errorf(p, "invalid char literal")
	} else {
		l.next()
	}
	s := sb.String()
	r, _ := utf8.DecodeRuneInString(s)
	if len(s) == 1 {
		r = rune(s[0])
	}
	return lexToken{tok: CHAR, lit: string(rune(r)), pos: p}
}
