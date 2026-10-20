package compiler

import "fmt"

type Tok int

const (
	EOF Tok = iota
	IDENT
	INT
	FLOAT
	CHAR
	STRING

	// operators
	ADD     // +
	SUB     // -
	MUL     // *
	QUO     // /
	REM     // %
	AND     // &
	OR      // |
	XOR     // ^
	SHL     // <<
	SHR     // >>
	AND_NOT // &^

	ADD_ASSIGN     // +=
	SUB_ASSIGN     // -=
	MUL_ASSIGN     // *=
	QUO_ASSIGN     // /=
	REM_ASSIGN     // %=
	AND_ASSIGN     // &=
	OR_ASSIGN      // |=
	XOR_ASSIGN     // ^=
	SHL_ASSIGN     // <<=
	SHR_ASSIGN     // >>=
	AND_NOT_ASSIGN // &^=

	LAND  // and
	LOR   // or
	ARROW // <-
	INC   // ++
	DEC   // --

	EQL    // ==
	LSS    // <
	GTR    // >
	ASSIGN // =
	NOT    // not
	BANG   // ! (postfix: propagate an error)
	AMPAMP // && (not squark: and)
	BARBAR // || (not squark: or)
	ARROWR // ->

	NEQ      // !=
	LEQ      // <=
	GEQ      // >=
	DEFINE   // :=
	ELLIPSIS // ...
	DOTDOT   // ..

	LPAREN // (
	LBRACK // [
	LBRACE // {
	COMMA  // ,
	PERIOD // .

	RPAREN    // )
	RBRACK    // ]
	RBRACE    // }
	SEMICOLON // ;
	COLON     // :

	// keywords
	BREAK
	CASE
	CHAN
	CONST
	CONTINUE
	DEFAULT
	DEFER
	ELSE
	ENUM
	FAULT
	FN
	FOR
	IF
	IMPL
	PUB
	RETURN
	STRUCT
	SWITCH
	TYPE
	VAR
	WHILE
	ARENA
	EXTERN
	IN
	TASK

	lastKeyword = TASK
)

var tokStrings = [...]string{
	EOF: "EOF", IDENT: "identifier", INT: "int literal", FLOAT: "float literal", CHAR: "char literal", STRING: "string literal",
	ADD: "+", SUB: "-", MUL: "*", QUO: "/", REM: "%", AND: "&", OR: "|", XOR: "^", SHL: "<<", SHR: ">>", AND_NOT: "&^",
	ADD_ASSIGN: "+=", SUB_ASSIGN: "-=", MUL_ASSIGN: "*=", QUO_ASSIGN: "/=", REM_ASSIGN: "%=", AND_ASSIGN: "&=", OR_ASSIGN: "|=",
	XOR_ASSIGN: "^=", SHL_ASSIGN: "<<=", SHR_ASSIGN: ">>=", AND_NOT_ASSIGN: "&^=",
	LAND: "and", LOR: "or", ARROW: "<-", INC: "++", DEC: "--",
	BANG: "!", AMPAMP: "&&", BARBAR: "||", ARROWR: "->",
	EQL: "==", LSS: "<", GTR: ">", ASSIGN: "=", NOT: "not", NEQ: "!=", LEQ: "<=", GEQ: ">=", DEFINE: ":=", ELLIPSIS: "...", DOTDOT: "..",
	LPAREN: "(", LBRACK: "[", LBRACE: "{", COMMA: ",", PERIOD: ".", RPAREN: ")", RBRACK: "]", RBRACE: "}", SEMICOLON: ";", COLON: ":",
	BREAK: "break", CASE: "case", CHAN: "chan", CONST: "const", CONTINUE: "continue", DEFAULT: "default", DEFER: "defer",
	ELSE: "else", ENUM: "enum", FAULT: "fault", FN: "fn", FOR: "for", IF: "if", IMPL: "impl", PUB: "pub",
	RETURN: "return", STRUCT: "struct", SWITCH: "switch", TYPE: "type", VAR: "var", WHILE: "while",
	ARENA: "arena", EXTERN: "extern", IN: "in", TASK: "task",
}

func (t Tok) String() string {
	if int(t) < len(tokStrings) && tokStrings[t] != "" {
		return tokStrings[t]
	}
	return fmt.Sprintf("tok(%d)", int(t))
}

var keywords = map[string]Tok{}

func init() {
	for t := BREAK; t <= lastKeyword; t++ {
		if t != TYPE { // TYPE names type declarations (Name: T, struct, enum); "type" is an ordinary word
			keywords[tokStrings[t]] = t
		}
	}
	keywords["not"] = NOT
	keywords["and"] = LAND
	keywords["or"] = LOR
}

// Precedence returns the binary operator precedence, or 0 if t is not a binary operator.
func (t Tok) Precedence() int {
	switch t {
	case LOR:
		return 1
	case LAND:
		return 2
	case EQL, NEQ, LSS, LEQ, GTR, GEQ:
		return 3
	case ADD, SUB, OR, XOR:
		return 4
	case MUL, QUO, REM, SHL, SHR, AND, AND_NOT:
		return 5
	}
	return 0
}

// Pos is a source position.
type Pos struct {
	File string
	Line int
	Col  int
}

func (p Pos) String() string {
	if p.File == "" {
		return "?"
	}
	if p.Line == 0 { // a whole file or directory (no main in the program)
		return p.File
	}
	return fmt.Sprintf("%s:%d:%d", p.File, p.Line, p.Col)
}

// Error is a compile error at a position.
type Error struct {
	Pos Pos
	Msg string
}

func (e *Error) Error() string { return e.Pos.String() + ": " + e.Msg }

type ErrorList []*Error

func (l ErrorList) Error() string {
	s := ""
	for i, e := range l {
		if i > 0 {
			s += "\n"
		}
		s += e.Error()
		if i >= 30 {
			s += fmt.Sprintf("\n... and %d more errors", len(l)-i-1)
			break
		}
	}
	return s
}
