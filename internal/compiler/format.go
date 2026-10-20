package compiler

import (
	"bytes"
	"fmt"
	"strings"
	"unicode/utf8"
)

// Format returns src in squark's standard layout. It works on the tokens and keeps the line
// breaks and comments as written, normalizing the rest:
//
//   - indentation is one tab per open block or bracket (and one more for a statement continued on
//     the next line); case and default line up with their switch;
//   - at most one blank line in a row, none at the start or end of a block or the file;
//   - one space or none between tokens (where either could be meant, as in `T{1}` against
//     `if x {`, the source's choice is kept);
//   - the fields of a struct, the = of a group of constants or variables (and of enum members),
//     and trailing comments are aligned across consecutive lines;
//   - trailing white space and semicolons at the ends of lines are dropped.
//
// src must parse. Formatting its result again changes nothing.
func Format(path string, src []byte) ([]byte, error) {
	if _, err := ParseFile(path, src); err != nil {
		return nil, err
	}
	toks := fmtTokens(path, src)
	out := formatLines(splitLines(toks))
	// the result must hold the same tokens: a mistake here must never change a program
	if a, b := tokenText(fmtTokens(path, src)), tokenText(fmtTokens(path, out)); a != b {
		return nil, fmt.Errorf("%s: squark fmt would change the program's tokens (a squark fmt bug)", path)
	}
	return out, nil
}

type ftok struct {
	tok      Tok
	text     string // as in the source
	line     int    // first and last line
	endLine  int
	spaced   bool // white space before it on its line, in the source
	col      int  // its column in the source
	ends     bool // a statement ends after it (a semicolon was inserted at the end of its line)
	unary    bool // an operator applied to what follows (-x, *p, &v, <-c)
	spaceOut bool // (an operator) a space after it
}

// fmtTokens lexes src keeping comments; inserted semicolons become ends on the token before.
func fmtTokens(path string, src []byte) []*ftok {
	lx := newLexer(path, src)
	lx.comments = true
	var toks []*ftok
	var last *ftok // the last token that isn't a comment
	for {
		t := lx.Next()
		if t.tok == EOF {
			break
		}
		if t.tok == SEMICOLON && t.lit == "\n" {
			if last != nil {
				last.ends = true
			}
			continue
		}
		ft := &ftok{tok: t.tok, text: string(src[t.off:t.endOff]), line: t.pos.Line, endLine: t.end.Line, col: t.pos.Col}
		if t.off > 0 {
			c := src[t.off-1]
			ft.spaced = c == ' ' || c == '\t'
		}
		if t.tok == COMMENT && strings.HasPrefix(ft.text, "//") {
			ft.text = strings.TrimRight(ft.text, " \t\r")
		}
		if t.tok == STRING && strings.HasPrefix(ft.text, "`") {
			ft.text = strings.ReplaceAll(ft.text, "\r", "")
		}
		toks = append(toks, ft)
		if t.tok != COMMENT {
			last = ft
		}
	}
	return toks
}

// tokenText is the tokens without comments' trailing space and without the semicolons Format
// may drop, for checking that formatting kept them.
func tokenText(toks []*ftok) string {
	var b strings.Builder
	for i, t := range toks {
		if t.tok == SEMICOLON && droppable(toks, i) {
			continue
		}
		b.WriteString(t.text)
		b.WriteByte(0)
	}
	return b.String()
}

// droppable reports whether the semicolon toks[i] ends its line (only a comment after it) after
// a token that ends a statement anyway.
func droppable(toks []*ftok, i int) bool {
	if i == 0 || !semiAfter(toks[i-1].tok) || toks[i-1].endLine != toks[i].line {
		return false
	}
	for _, n := range toks[i+1:] {
		if n.line != toks[i].line {
			return true
		}
		if n.tok != COMMENT {
			return false
		}
	}
	return true
}

// semiAfter reports whether a line ending in t ends a statement (the lexer's rule).
func semiAfter(t Tok) bool {
	switch t {
	case IDENT, INT, FLOAT, CHAR, STRING, BREAK, CONTINUE, RETURN, INC, DEC, RPAREN, RBRACK, RBRACE, ARENA, BANG, CHAN:
		return true
	}
	return false
}

type fline struct {
	toks    []*ftok
	blank   bool // a blank line before it in the source
	endLine int
}

func splitLines(toks []*ftok) []*fline {
	var lines []*fline
	var cur *fline
	for i, t := range toks {
		if t.tok == SEMICOLON && droppable(toks, i) {
			toks[i-1].ends = true
			continue
		}
		if cur == nil || t.line > cur.endLine {
			cur = &fline{blank: cur != nil && t.line > cur.endLine+1}
			lines = append(lines, cur)
		}
		cur.toks = append(cur.toks, t)
		cur.endLine = t.endLine
	}
	return lines
}

// a bracket left open at the end of a line
type open struct {
	tok    Tok
	level  int  // the indentation of the lines inside
	indent int  // the indentation of the line that opened it
	base   int  // the indentation of the statement that line belongs to
	block  bool // a block (or a struct or enum body) rather than a literal, call or list
	cases  bool // a switch's or select's block: case and default line up with the switch
	group  bool // const ( ... ), use ( ... ) and the like: each line is a declaration
	kind   byte // 's': a struct's fields, '=': a group of specs or an enum's members (names aligned), 'u': imports
	parent *open
}

// statements reports whether the lines inside o (or at the top level, o nil) are statements or
// declarations, which a line can continue, rather than the items of a list.
func (o *open) statements() bool { return o == nil || o.block || o.group }

func formatLines(lines []*fline) []byte {
	var out bytes.Buffer
	var stack []*open
	top := func() *open {
		if len(stack) == 0 {
			return nil
		}
		return stack[len(stack)-1]
	}
	type laid struct {
		line   *fline
		indent int
		in     *open
		cells  []string
		blank  bool
	}
	var laidOut []laid
	cont := false // the line before continues on this one
	base := 0     // the indentation of the statement the line before belongs to
	for li, l := range lines {
		first := l.toks[0]
		closers := 0
		for _, t := range l.toks {
			if t.tok != RBRACE && t.tok != RPAREN && t.tok != RBRACK {
				break
			}
			closers++
		}
		var indent int
		in := top()
		switch {
		case first.tok == COMMENT && len(l.toks) == 1:
			// a comment line: as the code line after it would be, inside the block
			if in != nil {
				indent = in.level
			}
			for _, n := range lines[li+1:] {
				if n.toks[0].tok == COMMENT && len(n.toks) == 1 {
					continue
				}
				// before a case or a closer, the comment ends the block above unless it was
				// written at the case's or closer's own level
				next := n.toks[0]
				outdented := first.col <= next.col
				switch {
				case (next.tok == CASE || next.tok == DEFAULT) && in != nil && in.cases && outdented:
					indent--
				case (next.tok == RBRACE || next.tok == RPAREN || next.tok == RBRACK) && outdented:
					indent--
				}
				break
			}
		case closers > 0 && in != nil:
			// a closer lines up with the line that opened its bracket, or for a block with
			// the statement (fn f(a,\n\tb) {\n}); what follows it belongs to that statement
			indent = in.indent
			if in.block {
				indent = in.base
			}
			base = in.base
			in = in.parent
		default:
			if in != nil {
				indent = in.level
			}
			switch {
			case (first.tok == CASE || first.tok == DEFAULT) && in != nil && in.cases:
				indent--
				base = indent
			case isLabel(l.toks):
				base = indent
				indent--
			case cont:
				indent++ // base stays the statement's
			case in.statements():
				base = indent
			default:
				base = in.base // an item of a list the statement opened
			}
		}
		indent = max(indent, 0)
		// the brackets this line leaves open
		code := l.toks
		for i, t := range code {
			switch t.tok {
			case LBRACE, LPAREN, LBRACK:
				o := &open{tok: t.tok, level: indent + 1, indent: indent, base: base, parent: top()}
				if t.tok == LBRACE && (i == 0 && top().statements() || i > 0 && (t.spaced || code[i-1].tok == RPAREN || code[i-1].tok >= BREAK && code[i-1].tok <= lastKeyword)) {
					// a block (if x {, fn f() {, struct {): its lines are one level in from the
					// statement's, wherever the statement's line breaks fall
					o.block = true
					o.level = base + 1
					for _, p := range code[:i] {
						switch p.tok {
						case SWITCH:
							o.cases = true
						case IDENT:
							if p.text == "select" && p == code[0] {
								o.cases = true
							}
						case STRUCT:
							o.kind = 's'
						case ENUM:
							o.kind = '='
						}
					}
				} else if t.tok == LPAREN && (i == 1 || i == 2 && code[0].tok == PUB) {
					// a group of declarations: its lines are statements
					switch p := code[i-1]; {
					case p.tok == CONST || p.tok == VAR || p.tok == IDENT && (p.text == "thread" || p.text == "shared"):
						o.kind, o.group = '=', true
					case p.tok == FAULT:
						o.group = true
					case p.tok == IDENT && p.text == "use":
						o.group, o.kind = true, 'u'
					}
				}
				stack = append(stack, o)
			case RBRACE, RPAREN, RBRACK:
				if len(stack) > 0 {
					stack = stack[:len(stack)-1]
				}
			}
		}
		// does the statement go on? Not after a semicolon, an opening bracket, a comma or a
		// colon, and not inside a list, whose bracket's level is enough
		if last := lastCode(l.toks); last != nil {
			cont = false
			if !last.ends {
				switch last.tok {
				case LBRACE, LPAREN, LBRACK, COMMA, COLON, SEMICOLON:
				default:
					cont = top().statements()
				}
			}
		}
		laidOut = append(laidOut, laid{line: l, indent: indent, in: in, cells: cells(l.toks, in), blank: l.blank})
	}

	// blank lines: none at the start, none right inside an opening bracket or before a closer
	for i := range laidOut {
		if i == 0 {
			laidOut[i].blank = false
			continue
		}
		prev := lastCode(laidOut[i-1].line.toks)
		if prev != nil && (prev.tok == LBRACE || prev.tok == LPAREN || prev.tok == LBRACK) && lastTok(laidOut[i-1].line.toks) == prev {
			laidOut[i].blank = false
		}
		switch laidOut[i].line.toks[0].tok {
		case RBRACE, RPAREN, RBRACK:
			laidOut[i].blank = false
		}
	}

	// alignment: each column is padded across a run of consecutive lines (same block and
	// indentation, no blank or comment line between) that have a cell after it
	widths := make([][]int, len(laidOut))
	for i := range laidOut {
		widths[i] = make([]int, len(laidOut[i].cells))
	}
	commentLine := func(l laid) bool { return len(l.line.toks) == 1 && l.line.toks[0].tok == COMMENT }
	for i := 0; i < len(laidOut); {
		j := i + 1
		for j < len(laidOut) && !laidOut[j].blank && !commentLine(laidOut[j]) && laidOut[j].in == laidOut[i].in && laidOut[j].indent == laidOut[i].indent {
			j++
		}
		var run []int
		for k := i; k < j; k++ {
			if !commentLine(laidOut[k]) {
				run = append(run, k)
			}
		}
		for col := 0; ; col++ {
			any := false
			for k := 0; k < len(run); {
				if len(laidOut[run[k]].cells) <= col+1 {
					k++
					continue
				}
				any = true
				e := k
				w := 0
				for e < len(run) && len(laidOut[run[e]].cells) > col+1 {
					w = max(w, utf8.RuneCountInString(laidOut[run[e]].cells[col]))
					e++
				}
				for ; k < e; k++ {
					widths[run[k]][col] = w
				}
			}
			if !any {
				break
			}
		}
		i = j
	}

	for i, l := range laidOut {
		if l.blank {
			out.WriteByte('\n')
		}
		var line strings.Builder
		line.WriteString(strings.Repeat("\t", l.indent))
		for c, cell := range l.cells {
			line.WriteString(cell)
			if c < len(l.cells)-1 && (cell != "" || widths[i][c] > 0) { // an empty column takes no room
				line.WriteString(strings.Repeat(" ", widths[i][c]-utf8.RuneCountInString(cell)+1))
			}
		}
		out.WriteString(strings.TrimRight(line.String(), " "))
		out.WriteByte('\n')
	}
	return out.Bytes()
}

// isLabel reports whether a line starts with a label (outer: for ...), which goes one level out.
func isLabel(toks []*ftok) bool {
	if len(toks) < 2 || toks[0].tok != IDENT || toks[1].tok != COLON {
		return false
	}
	if len(toks) == 2 {
		return true
	}
	switch n := toks[2]; n.tok {
	case FOR, WHILE, SWITCH, COMMENT:
		return true
	case IDENT:
		return n.text == "loop" || n.text == "select"
	}
	return false
}

func lastCode(toks []*ftok) *ftok {
	for i := len(toks) - 1; i >= 0; i-- {
		if toks[i].tok != COMMENT {
			return toks[i]
		}
	}
	return nil
}

func lastTok(toks []*ftok) *ftok { return toks[len(toks)-1] }

// cells splits a line into the parts that line up with the lines around it: the names of a
// struct field or a spec (or an enum member) and the rest, a literal's key and value, a one-line
// function's signature and body, and a trailing comment.
func cells(toks []*ftok, in *open) []string {
	if toks[0].tok == IDENT && toks[0].text == "use" || in != nil && in.kind == 'u' {
		// an unquoted import path ends at white space (bytes/. as b): keep the spacing
		var b strings.Builder
		for i, t := range toks {
			if i > 0 && (t.spaced || t.tok == COMMENT) {
				b.WriteByte(' ')
			}
			b.WriteString(t.text)
		}
		return []string{b.String()}
	}
	for _, t := range toks {
		if t.endLine != t.line { // a raw string or block comment over several lines: no columns
			return []string{joinTokens(toks)}
		}
	}
	code := toks
	var comment *ftok
	if n := len(toks); n > 1 && toks[n-1].tok == COMMENT && strings.HasPrefix(toks[n-1].text, "//") {
		code, comment = toks[:n-1], toks[n-1]
	}
	var out []string
	split := -1
	if in != nil && !in.block && !in.group && len(code) > 2 && code[0].tok == IDENT && code[1].tok == COLON && oneElement(code) {
		split = 2 // a keyed element of a literal on its own line: key: value
	}
	if in != nil && (in.kind == 's' || in.kind == '=') {
		// [pub] a, b T
		i := 0
		if i < len(code) && code[i].tok == PUB {
			i++
		}
		for i < len(code) && code[i].tok == IDENT {
			i++
			if i < len(code) && code[i].tok == COMMA {
				i++
				continue
			}
			break
		}
		if i > 0 && code[i-1].tok == IDENT && (i < len(code) || in.kind == '=') {
			// in a group or an enum, a line of just names (an enum member) still has its
			// names column, so the comments after the names line up
			split = i
		}
	}
	if split < 0 && oneLineFunc(code) {
		// fn f() -> int { return x }: consecutive one-line functions line up their bodies
		depth := 0
		for i, t := range code {
			switch t.tok {
			case LPAREN, LBRACK:
				depth++
			case RPAREN, RBRACK:
				depth--
			case LBRACE:
				if depth == 0 && split < 0 {
					split = i
				}
			}
		}
	}
	if split > 0 {
		out = append(out, joinTokens(code[:split]), joinTokens(code[split:])) // the rest may be empty
	} else if len(code) > 0 {
		out = append(out, joinTokens(code))
	}
	if comment != nil {
		out = append(out, comment.text)
	}
	return out
}

// oneElement reports whether a line holds one element of a list: no comma outside brackets but a
// last one.
func oneElement(code []*ftok) bool {
	depth := 0
	for i, t := range code {
		switch t.tok {
		case LPAREN, LBRACK, LBRACE:
			depth++
		case RPAREN, RBRACK, RBRACE:
			depth--
		case COMMA:
			if depth == 0 && i < len(code)-1 {
				return false
			}
		}
	}
	return true
}

// oneLineFunc reports whether a line is a whole function: [pub] fn ... { ... }.
func oneLineFunc(code []*ftok) bool {
	i := 0
	if len(code) > 0 && code[0].tok == PUB {
		i++
	}
	if i >= len(code) || code[i].tok != FN || code[len(code)-1].tok != RBRACE {
		return false
	}
	depth := 0
	for _, t := range code {
		switch t.tok {
		case LPAREN, LBRACK, LBRACE:
			depth++
		case RPAREN, RBRACK, RBRACE:
			depth--
		}
	}
	return depth == 0
}

// joinTokens writes a line's tokens with the spacing between them.
func joinTokens(toks []*ftok) string {
	var b strings.Builder
	var bracket []Tok // brackets open on this line, to know a slice's : from a label's
	for i, t := range toks {
		if i > 0 {
			if spaceBetween(toks[i-1], t, toks, i, bracket) {
				b.WriteByte(' ')
			}
		}
		switch t.tok {
		case LPAREN, LBRACK, LBRACE:
			bracket = append(bracket, t.tok)
		case RPAREN, RBRACK, RBRACE:
			if len(bracket) > 0 {
				bracket = bracket[:len(bracket)-1]
			}
		}
		b.WriteString(t.text)
	}
	return b.String()
}

// operand reports whether a token ends an operand, so that an operator after it is binary.
func operand(t *ftok) bool {
	switch t.tok {
	case IDENT, INT, FLOAT, CHAR, STRING, RPAREN, RBRACK, RBRACE, BANG, INC, DEC:
		return true
	}
	return false
}

// spaceBetween decides whether a space goes between a and b (toks[i]); bracket is the brackets
// open on the line before b.
func spaceBetween(a, b *ftok, toks []*ftok, i int, bracket []Tok) bool {
	inBracket := len(bracket) > 0 && bracket[len(bracket)-1] == LBRACK
	switch {
	case b.tok == COMMENT:
		return true
	case a.tok == COMMENT:
		return b.spaced
	case b.tok == SEMICOLON && (a.tok == FOR || a.tok == SEMICOLON):
		return true // for ; cond; post, and for init; ; post
	case b.tok == COMMA || b.tok == SEMICOLON:
		return false
	case a.tok == COMMA || a.tok == SEMICOLON:
		return true
	}
	// an operator that can be binary or unary: binary after an operand
	switch b.tok {
	case ADD, SUB, MUL, AND, XOR, ARROW, OR, QUO, REM, SHL, SHR, AND_NOT:
		if !operand(a) {
			b.unary = true
			b.spaceOut = false
			break
		}
		// binary: the source's spacing, made symmetric where it was only after (a+ b); a space
		// only before is a unary operator in a type or a list (p *T)
		after := i+1 < len(toks) && toks[i+1].spaced
		b.spaceOut = after
		return b.spaced || after
	}
	switch a.tok {
	case ADD, SUB, MUL, AND, XOR, ARROW, OR, QUO, REM, SHL, SHR, AND_NOT:
		if a.unary {
			return false
		}
		return a.spaceOut
	}
	switch {
	case a.tok == LPAREN || a.tok == LBRACK:
		return false
	case b.tok == RPAREN || b.tok == RBRACK:
		return false
	case b.tok == BANG:
		return b.spaced // f()! against -> T !
	case b.tok == ELLIPSIS:
		return b.spaced // xs... against xs ...int
	case a.tok == ELLIPSIS:
		return false
	case b.tok == INC || b.tok == DEC:
		return false
	case a.tok == PERIOD:
		return false
	case b.tok == PERIOD:
		return !operand(a) // x.y, but case .north and = .west
	case b.tok == COLON:
		return inBracket && b.spaced // a slice's bounds: s[lo:hi] or s[lo+1 : hi]
	case a.tok == COLON:
		return !inBracket || b.spaced
	case a.tok == ARROWR || b.tok == ARROWR:
		return true
	}
	switch b.tok {
	case ASSIGN, DEFINE, ADD_ASSIGN, SUB_ASSIGN, MUL_ASSIGN, QUO_ASSIGN, REM_ASSIGN, AND_ASSIGN, OR_ASSIGN, XOR_ASSIGN, SHL_ASSIGN, SHR_ASSIGN, AND_NOT_ASSIGN,
		EQL, NEQ, LSS, LEQ, GTR, GEQ, LAND, LOR, IN:
		return true
	}
	switch a.tok {
	case ASSIGN, DEFINE, ADD_ASSIGN, SUB_ASSIGN, MUL_ASSIGN, QUO_ASSIGN, REM_ASSIGN, AND_ASSIGN, OR_ASSIGN, XOR_ASSIGN, SHL_ASSIGN, SHR_ASSIGN, AND_NOT_ASSIGN,
		EQL, NEQ, LSS, LEQ, GTR, GEQ, LAND, LOR, IN, NOT:
		return true
	}
	switch b.tok {
	case LPAREN:
		switch a.tok {
		case IDENT:
			// a group after thread, shared or use at the start of a line; otherwise a call
			// (f(x)) or a type in parentheses (var todo (*Node)[..8]), as written
			if (i == 1 || i == 2 && toks[0].tok == PUB) && (a.text == "use" || a.text == "thread" || a.text == "shared") {
				return true
			}
			return b.spaced
		case RPAREN, RBRACK, FN:
			return false
		}
		return true
	case LBRACK:
		switch a.tok {
		case IDENT, RPAREN, RBRACK, RBRACE, STRING, CHAN, BANG:
			return false
		}
		return true
	case LBRACE:
		switch a.tok {
		case RPAREN, STRUCT, ELSE, ENUM:
			return true
		}
		if a.tok >= BREAK && a.tok <= lastKeyword {
			return true
		}
		return b.spaced
	case RBRACE:
		if a.tok == LBRACE {
			return false
		}
		return b.spaced
	}
	if a.tok == LBRACE {
		return b.spaced
	}
	if a.tok == DOTDOT || b.tok == DOTDOT {
		return b.spaced
	}
	if a.tok == IDENT && b.tok == STRING {
		return b.spaced // c"symbol"
	}
	return b.spaced // words are apart already; 20s is a number and a word
}
