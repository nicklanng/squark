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
	View  Type // non-nil: the expression is storage (this type) used as a view (T)
	Temp  bool // the view is of a temporary copy (a vector that isn't in a variable; read-only uses)
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
	P    Pos
	Kind Tok // INT, FLOAT, CHAR, STRING
	Lit  string
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
	X       Expr
	Index   Expr
	P       Pos
	CommaOk bool // v, ok := m[k]
}

type SliceExpr struct {
	ExprInfo
	X      Expr
	Lo, Hi Expr
	Max    Expr
	P      Pos
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
	InArena    Expr   // new/make/append/copying conversions allocate here
}

type StarExpr struct {
	ExprInfo
	P Pos
	X Expr
}

type UnaryExpr struct {
	ExprInfo
	P       Pos
	Op      Tok
	X       Expr
	CommaOk bool // v, ok := <-ch
}

type BinaryExpr struct {
	ExprInfo
	X  Expr
	Op Tok
	Y  Expr
	P  Pos

	InArena Expr // string concatenation allocates here
}

// ImplicitMember is .name: a member of the enum the context expects (Who.alice where a Who is
// expected).
type ImplicitMember struct {
	ExprInfo
	P    Pos
	Name *Ident
}

func (x *ImplicitMember) Pos() Pos { return x.P }

// ---- type expressions ----

type ArrayType struct {
	ExprInfo
	P    Pos
	Len  Expr // nil for slices
	Elem Expr
	// MaybeMap: Elem[Len] with Len a name, which is an array length or a map's key type
	// (int[n], int[str]); the checker decides
	MaybeMap bool
}

type MapType struct {
	ExprInfo
	P     Pos
	Key   Expr
	Value Expr
	Cap   Expr // map[K]V cap N: storage
}

// VecTypeExpr is [..N]T: a bounded vector with inline storage.
type VecTypeExpr struct {
	ExprInfo
	P    Pos
	Len  Expr
	Elem Expr
}

// ArenaType is the type `arena` (a reference to arena storage).
type ArenaTypeExpr struct {
	ExprInfo
	P Pos
}

// TryExpr is `try call`.
type TryExpr struct {
	ExprInfo
	P Pos
	X Expr
}

// InExpr is `x in arena`: allocations made by x come from the arena.
type InExpr struct {
	ExprInfo
	X       Expr
	Arena   Expr
	P       Pos
	CommaOk bool // v, ok := x in a: ok is false, instead of a panic, when a is full
}

type ChanType struct {
	ExprInfo
	P    Pos
	Elem Expr
	Cap  Expr // chan T cap N: storage
}

type Field struct {
	Doc      []string // a struct field's doc comment
	Names    []*Ident
	Type     Expr
	Variadic bool
	Pub      bool // a struct field visible outside its package

	// provenance (parameters): `name static T`, `name T into other`
	Static bool
	Into   *Ident
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

	// provenance (results): `... T from a, b` or `... T static`
	From         []*Ident
	ResultStatic bool
}

func (x *Ident) Pos() Pos         { return x.P }
func (x *BasicLit) Pos() Pos      { return x.P }
func (x *CompositeLit) Pos() Pos  { return x.P }
func (x *KeyValueExpr) Pos() Pos  { return x.Key.Pos() }
func (x *ParenExpr) Pos() Pos     { return x.P }
func (x *SelectorExpr) Pos() Pos  { return x.Sel.P }
func (x *IndexExpr) Pos() Pos     { return x.X.Pos() } // as go/ast: where the expression starts
func (x *SliceExpr) Pos() Pos     { return x.X.Pos() }
func (x *CallExpr) Pos() Pos      { return x.Fun.Pos() }
func (x *StarExpr) Pos() Pos      { return x.P }
func (x *UnaryExpr) Pos() Pos     { return x.P }
func (x *BinaryExpr) Pos() Pos    { return x.P }
func (x *ArrayType) Pos() Pos     { return x.P }
func (x *MapType) Pos() Pos       { return x.P }
func (x *ChanType) Pos() Pos      { return x.P }
func (x *StructType) Pos() Pos    { return x.P }
func (x *ArenaTypeExpr) Pos() Pos { return x.P }
func (x *VecTypeExpr) Pos() Pos   { return x.P }
func (x *TryExpr) Pos() Pos       { return x.P }
func (x *InExpr) Pos() Pos        { return x.P }
func (x *FuncType) Pos() Pos      { return x.P }

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
	Else *BlockStmt // start f() else { ... }: run instead of waiting when the pool is full
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
	Lo         Expr // for i in Lo..X: the first value (nil: 0)
	Single     bool // for x in X: one variable, the element (the checker moves it to Value)
	Body       *BlockStmt
	P          Pos
	Label      string
	ID         int
}

// SelectStmt is select { case ...: ... }: it waits until one of its channel operations can
// proceed and runs that case.
type SelectStmt struct {
	P     Pos
	Label string
	Cases []*CommClause
}

// CommClause is one case of a select.
type CommClause struct {
	P    Pos
	Comm Stmt // *SendStmt, *ExprStmt (<-c) or *AssignStmt (v[, ok] = or := <-c); nil: default
	Body []Stmt
	// set by the checker
	Recv    *UnaryExpr // the receive, in a receive case
	Timeout Expr       // case <-time.After(d): d
}

type DeclStmt struct {
	Decl *GenDecl
}

type LabeledStmt struct {
	Label *Ident
	Stmt  Stmt
	P     Pos
}

// ArenaStmt is `arena NAME SIZE { ... }`.
type ArenaStmt struct {
	Name *Ident
	Size Expr
	Body *BlockStmt
	P    Pos
}

// DeadlineStmt is `with deadline D { ... }`: blocking I/O in the body (and in what it calls)
// returns a timeout once D has passed.
type DeadlineStmt struct {
	D    Expr
	Body *BlockStmt
	P    Pos
}

type EmptyStmt struct{ P Pos }

func (s *ExprStmt) Pos() Pos    { return s.X.Pos() }
func (s *SendStmt) Pos() Pos    { return s.P }
func (s *IncDecStmt) Pos() Pos  { return s.P }
func (s *AssignStmt) Pos() Pos  { return s.Lhs[0].Pos() }
func (s *GoStmt) Pos() Pos      { return s.P }
func (s *DeferStmt) Pos() Pos   { return s.P }
func (s *ReturnStmt) Pos() Pos  { return s.P }
func (s *BranchStmt) Pos() Pos  { return s.P }
func (s *BlockStmt) Pos() Pos   { return s.P }
func (s *IfStmt) Pos() Pos      { return s.P }
func (s *CaseClause) Pos() Pos  { return s.P }
func (s *SwitchStmt) Pos() Pos  { return s.P }
func (s *SelectStmt) Pos() Pos  { return s.P }
func (s *ForStmt) Pos() Pos     { return s.P }
func (s *RangeStmt) Pos() Pos   { return s.P }
func (s *DeclStmt) Pos() Pos    { return s.Decl.P }
func (s *LabeledStmt) Pos() Pos { return s.P }
func (s *EmptyStmt) Pos() Pos   { return s.P }
func (s *ArenaStmt) Pos() Pos   { return s.P }
func (s *DeadlineStmt) Pos() Pos { return s.P }
func (*ExprStmt) stmt()         {}
func (*SelectStmt) stmt()       {}
func (*SendStmt) stmt()         {}
func (*IncDecStmt) stmt()       {}
func (*AssignStmt) stmt()       {}
func (*GoStmt) stmt()           {}
func (*DeferStmt) stmt()        {}
func (*ReturnStmt) stmt()       {}
func (*BranchStmt) stmt()       {}
func (*BlockStmt) stmt()        {}
func (*IfStmt) stmt()           {}
func (*CaseClause) stmt()       {}
func (*SwitchStmt) stmt()       {}
func (*ForStmt) stmt()          {}
func (*RangeStmt) stmt()        {}
func (*DeclStmt) stmt()         {}
func (*LabeledStmt) stmt()      {}
func (*EmptyStmt) stmt()        {}

// ---- declarations ----

type ImportSpec struct {
	Name *Ident // optional
	Path string
	P    Pos
	Pkg  *Package // resolved by the loader
}

type ValueSpec struct {
	Pub    bool
	Doc    []string // the comment lines above it (or above its declaration)
	Names  []*Ident
	Type   Expr
	Values []Expr
	P      Pos
}

type TypeSpec struct {
	Pub  bool
	Doc  []string
	Name *Ident
	Type Expr
	P    Pos
	Enum []*EnumMember // enum Name T { ... }: the members, in order (Type is T, int by default)
	// Form is how it was declared: "struct" (struct Name { ... }), "enum" or "named" (Name: T).
	Form string
}

// EnumMember is one name of an enum, with its value if written (otherwise the previous one + 1,
// or 0 for the first).
type EnumMember struct {
	Name  *Ident
	Value Expr
}

type GenDecl struct {
	Tok     Tok  // CONST, VAR, TYPE
	Shared  bool // shared var: one copy for every scheduler thread, read-only after initialization
	Thread  bool // thread var: one copy per scheduler thread (as var), on purpose
	Values  []*ValueSpec
	Types   []*TypeSpec
	P       Pos
	Pragmas []string
}

type FuncKind int

const (
	KindFunc FuncKind = iota
	KindTask
	KindExtern
)

type FuncDecl struct {
	Pub     bool
	Doc     []string
	Recv    *Field
	Name    *Ident
	Type    *FuncType
	Body    *BlockStmt // nil for extern
	Pragmas []string
	P       Pos

	Kind           FuncKind
	ExternSym      string // extern "sym" (empty: same as Name)
	Pool           Expr   // task ... pool N
	Stack          Expr   // ... stack N
	Blocking       bool   // extern ... blocking: the call runs on a helper thread
	BlockingPos    Pos
	Preemptible    bool // task ... preemptible: its loops may yield after a time slice
	PreemptiblePos Pos
	Isolate        bool // task ... isolate: a panic in it ends only that task
	IsolatePos     Pos
}

// Directive is a top-level `budget N`, `link "lib"`, `cflags "..."` or `ldflags "..."`.
type Directive struct {
	Name   string
	Target string // pool directive: "pkg.task"
	Value  Expr
	Unit   string // shutdown directive: "ms", "s" or "m" after the number
	P      Pos
}

// ArenaDecl is a package-level `arena NAME SIZE`.
type ArenaDecl struct {
	Pub  bool
	Name *Ident
	Size Expr
	P    Pos
}

type File struct {
	Doc        []string // the package doc comment, above the package clause
	Path       string
	Package    string
	Imports    []*ImportSpec
	Decls      []any // *GenDecl, *FuncDecl or *ArenaDecl
	Pragmas    []string
	Directives []*Directive
}

func (*ArenaStmt) stmt() {}
func (*DeadlineStmt) stmt() {}
