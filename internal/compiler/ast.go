package compiler

import "go/constant"

type Node interface {
	Pos() Pos
}

// ExprInfo is filled in by the type checker.
type ExprInfo struct {
	T     Type
	Const constant.Value // non-nil for constant expressions
	Mode  Mode
}

func (e *ExprInfo) info() *ExprInfo { return e }

type Mode int

const (
	ModeInvalid  Mode = iota
	ModeNoValue       // call with no results
	ModeValue         // rvalue
	ModeVar           // addressable
	ModeMapIndex      // m[k]: assignable but not addressable
	ModeType
	ModePkg
	ModeBuiltin
	ModeConst
)

type Expr interface {
	Node
	info() *ExprInfo
}

type Stmt interface {
	Node
	stmt()
}

// ---- expressions ----

type Ident struct {
	ExprInfo
	P    Pos
	Name string
	Obj  *Object // resolved object
}

type BasicLit struct {
	ExprInfo
	P   Pos
	Kind Tok // INT, FLOAT, CHAR, STRING
	Lit string
}

type CompositeLit struct {
	ExprInfo
	P    Pos
	Type Expr // may be nil for elided types inside another composite literal
	Elts []Expr
}

type KeyValueExpr struct {
	ExprInfo
	Key   Expr
	Value Expr
}

type ParenExpr struct {
	ExprInfo
	P Pos
	X Expr
}

type SelectorExpr struct {
	ExprInfo
	X   Expr
	Sel *Ident

	// filled by checker
	Kind     SelKind
	Path     []int  // field index path (through embedded structs not supported: len==1)
	Method   *Func  // for SelMethod
	Implicit string // "&" or "*" receiver adjustment for method calls, "*" auto-deref for fields
}

type SelKind int

const (
	SelField SelKind = iota + 1
	SelMethod
	SelQualified // pkg.Name
	SelBuiltinMethod
)

type IndexExpr struct {
	ExprInfo
	X     Expr
	Index Expr
	P     Pos
	CommaOk bool // v, ok := m[k]
}

type SliceExpr struct {
	ExprInfo
	X        Expr
	Lo, Hi   Expr
	Max      Expr
	P        Pos
}

type CallExpr struct {
	ExprInfo
	Fun      Expr
	Args     []Expr
	Ellipsis bool // f(xs...)
	P        Pos

	// filled by checker
	Builtin    string // builtin name for builtin calls
	Conversion bool   // T(x)
	Callee     *Func  // static callee when known (function or method)
	Variadic   int    // index where variadic args start (-1 if none packed)
	StorageID  int    // for make(): storage slot index
}

type StarExpr struct {
	ExprInfo
	P Pos
	X Expr
}

type UnaryExpr struct {
	ExprInfo
	P  Pos
	Op Tok
	X  Expr
	CommaOk bool // v, ok := <-ch
}

type BinaryExpr struct {
	ExprInfo
	X  Expr
	Op Tok
	Y  Expr
	P  Pos
}

// ---- type expressions ----

type ArrayType struct {
	ExprInfo
	P    Pos
	Len  Expr // nil for slices
	Elem Expr
}

type MapType struct {
	ExprInfo
	P     Pos
	Key   Expr
	Value Expr
}

type ChanType struct {
	ExprInfo
	P    Pos
	Elem Expr
}

type Field struct {
	Names    []*Ident
	Type     Expr
	Variadic bool
}

type StructType struct {
	ExprInfo
	P      Pos
	Fields []*Field
}

type FuncType struct {
	ExprInfo
	P       Pos
	Params  []*Field
	Results []*Field
}

func (x *Ident) Pos() Pos        { return x.P }
func (x *BasicLit) Pos() Pos     { return x.P }
func (x *CompositeLit) Pos() Pos { return x.P }
func (x *KeyValueExpr) Pos() Pos { return x.Key.Pos() }
func (x *ParenExpr) Pos() Pos    { return x.P }
func (x *SelectorExpr) Pos() Pos { return x.Sel.P }
func (x *IndexExpr) Pos() Pos    { return x.P }
func (x *SliceExpr) Pos() Pos    { return x.P }
func (x *CallExpr) Pos() Pos     { return x.P }
func (x *StarExpr) Pos() Pos     { return x.P }
func (x *UnaryExpr) Pos() Pos    { return x.P }
func (x *BinaryExpr) Pos() Pos   { return x.P }
func (x *ArrayType) Pos() Pos    { return x.P }
func (x *MapType) Pos() Pos      { return x.P }
func (x *ChanType) Pos() Pos     { return x.P }
func (x *StructType) Pos() Pos   { return x.P }
func (x *FuncType) Pos() Pos     { return x.P }

// ---- statements ----

type ExprStmt struct{ X Expr }

type SendStmt struct {
	Chan  Expr
	Value Expr
	P     Pos
}

type IncDecStmt struct {
	X   Expr
	Tok Tok
	P   Pos
}

type AssignStmt struct {
	Lhs []Expr
	Tok Tok // ASSIGN, DEFINE, or op-assign
	Rhs []Expr
	P   Pos

	NewVars []bool // for DEFINE: which lhs idents are new
}

type GoStmt struct {
	Call *CallExpr
	P    Pos
	ID   int // go-site index
}

type DeferStmt struct {
	Call *CallExpr
	P    Pos
	ID   int
}

type ReturnStmt struct {
	Results []Expr
	P       Pos
}

type BranchStmt struct {
	Tok   Tok // BREAK or CONTINUE
	Label *Ident
	P     Pos

	Target Stmt // resolved for/switch statement
}

type BlockStmt struct {
	List []Stmt
	P    Pos
	End  Pos
}

type IfStmt struct {
	Init Stmt
	Cond Expr
	Body *BlockStmt
	Else Stmt // *IfStmt or *BlockStmt
	P    Pos
}

type CaseClause struct {
	List []Expr // nil means default
	Body []Stmt
	P    Pos
}

type SwitchStmt struct {
	Init  Stmt
	Tag   Expr // may be nil
	Cases []*CaseClause
	P     Pos
	Label string
	ID    int
}

type ForStmt struct {
	Init  Stmt
	Cond  Expr
	Post  Stmt
	Body  *BlockStmt
	P     Pos
	Label string
	ID    int
}

type RangeStmt struct {
	Key, Value Expr // may be nil
	Define     bool
	X          Expr
	Body       *BlockStmt
	P          Pos
	Label      string
	ID         int
}

type DeclStmt struct {
	Decl *GenDecl
}

type LabeledStmt struct {
	Label *Ident
	Stmt  Stmt
	P     Pos
}

type EmptyStmt struct{ P Pos }

func (s *ExprStmt) Pos() Pos     { return s.X.Pos() }
func (s *SendStmt) Pos() Pos     { return s.P }
func (s *IncDecStmt) Pos() Pos   { return s.P }
func (s *AssignStmt) Pos() Pos   { return s.P }
func (s *GoStmt) Pos() Pos       { return s.P }
func (s *DeferStmt) Pos() Pos    { return s.P }
func (s *ReturnStmt) Pos() Pos   { return s.P }
func (s *BranchStmt) Pos() Pos   { return s.P }
func (s *BlockStmt) Pos() Pos    { return s.P }
func (s *IfStmt) Pos() Pos       { return s.P }
func (s *CaseClause) Pos() Pos   { return s.P }
func (s *SwitchStmt) Pos() Pos   { return s.P }
func (s *ForStmt) Pos() Pos      { return s.P }
func (s *RangeStmt) Pos() Pos    { return s.P }
func (s *DeclStmt) Pos() Pos     { return s.Decl.P }
func (s *LabeledStmt) Pos() Pos  { return s.P }
func (s *EmptyStmt) Pos() Pos    { return s.P }
func (*ExprStmt) stmt()          {}
func (*SendStmt) stmt()          {}
func (*IncDecStmt) stmt()        {}
func (*AssignStmt) stmt()        {}
func (*GoStmt) stmt()            {}
func (*DeferStmt) stmt()         {}
func (*ReturnStmt) stmt()        {}
func (*BranchStmt) stmt()        {}
func (*BlockStmt) stmt()         {}
func (*IfStmt) stmt()            {}
func (*CaseClause) stmt()        {}
func (*SwitchStmt) stmt()        {}
func (*ForStmt) stmt()           {}
func (*RangeStmt) stmt()         {}
func (*DeclStmt) stmt()          {}
func (*LabeledStmt) stmt()       {}
func (*EmptyStmt) stmt()         {}

// ---- declarations ----

type ImportSpec struct {
	Name *Ident // optional
	Path string
	P    Pos
}

type ValueSpec struct {
	Names  []*Ident
	Type   Expr
	Values []Expr
	Iota   int
	P      Pos
}

type TypeSpec struct {
	Name *Ident
	Type Expr
	P    Pos
}

type GenDecl struct {
	Tok     Tok // CONST, VAR, TYPE
	Values  []*ValueSpec
	Types   []*TypeSpec
	P       Pos
	Pragmas []string
}

type FuncDecl struct {
	Recv    *Field
	Name    *Ident
	Type    *FuncType
	Body    *BlockStmt // nil for extern
	Pragmas []string
	P       Pos
}

type File struct {
	Path    string
	Package string
	Imports []*ImportSpec
	Decls   []any // *GenDecl or *FuncDecl
	Pragmas []string
}
