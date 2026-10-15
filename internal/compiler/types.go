package compiler

import (
	"fmt"
	"strings"
)

type Type interface {
	Underlying() Type
	String() string
}

type BasicKind int

const (
	Invalid BasicKind = iota
	Bool
	Int
	Int8
	Int16
	Int32
	Int64
	Uint
	Uint8
	Uint16
	Uint32
	Uint64
	Uintptr
	Float32
	Float64
	String
	Rawptr
	UntypedBool
	UntypedInt
	UntypedRune
	UntypedFloat
	UntypedString
	UntypedNil
)

type Basic struct {
	Kind BasicKind
	Name string
}

func (b *Basic) Underlying() Type { return b }
func (b *Basic) String() string   { return b.Name }

var (
	tInvalid       = &Basic{Invalid, "invalid type"}
	tBool          = &Basic{Bool, "bool"}
	tInt           = &Basic{Int, "int"}
	tInt8          = &Basic{Int8, "int8"}
	tInt16         = &Basic{Int16, "int16"}
	tInt32         = &Basic{Int32, "int32"}
	tInt64         = &Basic{Int64, "int64"}
	tUint          = &Basic{Uint, "uint"}
	tUint8         = &Basic{Uint8, "byte"}
	tUint16        = &Basic{Uint16, "uint16"}
	tUint32        = &Basic{Uint32, "uint32"}
	tUint64        = &Basic{Uint64, "uint64"}
	tUintptr       = &Basic{Uintptr, "uintptr"}
	tFloat32       = &Basic{Float32, "float32"}
	tFloat64       = &Basic{Float64, "float64"}
	tString        = &Basic{String, "string"}
	tRawptr        = &Basic{Rawptr, "rawptr"}
	tUntypedBool   = &Basic{UntypedBool, "untyped bool"}
	tUntypedInt    = &Basic{UntypedInt, "untyped int"}
	tUntypedRune   = &Basic{UntypedRune, "untyped rune"}
	tUntypedFloat  = &Basic{UntypedFloat, "untyped float"}
	tUntypedString = &Basic{UntypedString, "untyped string"}
	tUntypedNil    = &Basic{UntypedNil, "untyped nil"}
)

type Array struct {
	Len  int64
	Elem Type
}

type Slice struct{ Elem Type }

type Pointer struct{ Elem Type }

type Chan struct{ Elem Type }

type Map struct{ Key, Value Type }

type StructField struct {
	Name string
	Type Type
	Pos  Pos
}

type Struct struct {
	Fields []*StructField
}

type Signature struct {
	Recv     *Var // nil for functions
	Params   []*Var
	Results  []*Var
	Variadic bool

	// declared provenance (slots count the receiver first)
	HasContract  bool
	From         []int // results may point into these slots
	ResultStatic bool  // results point only to static memory
	StaticSlots  []int // these slots must point to static memory
	Into         [][2]int
}

// Tuple is the type of a multi-value call.
type Tuple struct{ Types []Type }

type Named struct {
	Obj        *Object
	underlying Type
	Methods    []*Func
	Pkg        *Package
}

// ErrorType is the predeclared error type: a static message plus an integer code.
type ErrorType struct{}

func (t *Array) Underlying() Type     { return t }
func (t *Slice) Underlying() Type     { return t }
func (t *Pointer) Underlying() Type   { return t }
func (t *Chan) Underlying() Type      { return t }
func (t *Map) Underlying() Type       { return t }
func (t *Struct) Underlying() Type    { return t }
func (t *Signature) Underlying() Type { return t }
func (t *Tuple) Underlying() Type     { return t }
func (t *ErrorType) Underlying() Type { return t }
func (t *Named) Underlying() Type {
	if t.underlying == nil {
		return tInvalid
	}
	return t.underlying
}

var tError = &ErrorType{}

// VecType is [..N]T: a length plus inline storage for N elements.
type VecType struct {
	Len  int64
	Elem Type
}

// MapStore is map[K]V cap N: inline storage for a fixed-capacity map.
type MapStore struct {
	Key, Value Type
	Cap        int64
}

// ChanStore is chan T cap N: inline storage for a channel and its buffer.
type ChanStore struct {
	Elem Type
	Cap  int64
}

func (t *VecType) Underlying() Type   { return t }
func (t *MapStore) Underlying() Type  { return t }
func (t *ChanStore) Underlying() Type { return t }
func (t *VecType) String() string     { return fmt.Sprintf("[..%d]%s", t.Len, t.Elem) }
func (t *MapStore) String() string {
	return fmt.Sprintf("map[%s]%s cap %d", t.Key, t.Value, t.Cap)
}
func (t *ChanStore) String() string { return fmt.Sprintf("chan %s cap %d", t.Elem, t.Cap) }

// viewOf returns the view type of a storage type, or nil.
func viewOf(t Type) Type {
	switch u := t.Underlying().(type) {
	case *VecType:
		return &Slice{Elem: u.Elem}
	case *MapStore:
		return &Map{Key: u.Key, Value: u.Value}
	case *ChanStore:
		return &Chan{Elem: u.Elem}
	}
	return nil
}

// ArenaType is the type `arena`: a reference to a block of arena storage.
type ArenaType struct{}

func (t *ArenaType) Underlying() Type { return t }
func (t *ArenaType) String() string   { return "arena" }

var tArena = &ArenaType{}

func (t *Array) String() string   { return fmt.Sprintf("[%d]%s", t.Len, t.Elem) }
func (t *Slice) String() string   { return "[]" + t.Elem.String() }
func (t *Pointer) String() string { return "*" + t.Elem.String() }
func (t *Chan) String() string    { return "chan " + t.Elem.String() }
func (t *Map) String() string     { return "map[" + t.Key.String() + "]" + t.Value.String() }
func (t *ErrorType) String() string {
	return "error"
}
func (t *Struct) String() string {
	var parts []string
	for _, f := range t.Fields {
		parts = append(parts, f.Name+" "+f.Type.String())
	}
	return "struct{" + strings.Join(parts, "; ") + "}"
}
func (t *Signature) String() string {
	var ps []string
	for i, p := range t.Params {
		s := p.Type.String()
		if t.Variadic && i == len(t.Params)-1 {
			s = "..." + p.Type.(*Slice).Elem.String()
		}
		ps = append(ps, s)
	}
	s := "func(" + strings.Join(ps, ", ") + ")"
	switch len(t.Results) {
	case 0:
	case 1:
		s += " " + t.Results[0].Type.String()
	default:
		var rs []string
		for _, r := range t.Results {
			rs = append(rs, r.Type.String())
		}
		s += " (" + strings.Join(rs, ", ") + ")"
	}
	return s
}
func (t *Tuple) String() string {
	var rs []string
	for _, r := range t.Types {
		rs = append(rs, r.String())
	}
	return "(" + strings.Join(rs, ", ") + ")"
}
func (t *Named) String() string {
	if t.Pkg != nil && t.Pkg.Name != "main" {
		return t.Pkg.Name + "." + t.Obj.Name
	}
	return t.Obj.Name
}

// ---- objects ----

type ObjKind int

const (
	ObjVar ObjKind = iota + 1
	ObjConst
	ObjType
	ObjFunc
	ObjPkg
	ObjBuiltin
	ObjNil
)

type Object struct {
	Kind ObjKind
	Name string
	Type Type
	Pos  Pos
	Pkg  *Package

	Var     *Var
	Func    *Func
	Const   any // constant.Value
	PkgRef  *Package
	TypeDecl *TypeSpec

	// for ObjConst/global ObjVar/ObjType resolution
	decl    any
	state   int // 0 unresolved, 1 resolving, 2 done
}

type Var struct {
	Name   string
	Type   Type
	Pos    Pos
	Global bool
	Param  bool
	Index  int // param index
	Pkg    *Package
	Obj    *Object

	// lifetime analysis
	Depth     int // lexical block depth of declaration (0 = global)
	AddrTaken bool
	CName    string // unique C name (locals)
	Used     bool
	ReadOnly bool // error constants, constant composites, arenas
	ConstData bool // const X = T{...}: must be built from constants only
	Shared    bool // shared var: one copy for every scheduler thread, read-only after initialization
	Thread    bool // thread var: one copy per scheduler thread, like var, declared so on purpose

	ArenaSize     int64 // for arenas
	ArenaSizeExpr Expr
	Spec          *ValueSpec // a package variable's declaration
}

type Func struct {
	Name     string
	Sig      *Signature
	Decl     *FuncDecl
	Pkg      *Package
	RecvType *Named // for methods
	Extern   string // C symbol for body-less functions
	Pragmas  []string

	// analysis results
	Summary   *Summary
	Calls     []*Func      // static callees
	Indirect  []*Signature // signatures of indirect calls through func values
	GoSites   []*GoStmt
	Defers    []*DeferStmt
	AddrTaken bool
	GoTarget  bool
	Pool      int   // goroutine pool size if a go target
	StackHint int64 // //squark:stack N (for externs)
	Blocking  bool  // extern ... blocking: called on a helper thread while the goroutine waits
	Preemptible bool // task ... preemptible: loops in it and its callees may yield after a time slice
	CName     string
	Intrinsic string
	IsTask    bool
	StackLimit int64 // asserted upper bound (stack N on a task or function)

	// anytype parameters (see generic.go)
	Generic   bool    // a template: has anytype parameters; never compiled itself
	Instances []*Func // its instances so far
	GenericOf *Func   // for an instance: its template
	TypeArgs  []Type  // for an instance: the types of its anytype parameters
	InstPos   Pos     // for an instance: the call that made it
	InstFrom  *Func   // for an instance: the instance whose body made it (nil from ordinary code)
}

func (f *Func) FullName() string {
	if f.RecvType != nil {
		return f.Pkg.Name + "." + f.RecvType.Obj.Name + "." + f.Name
	}
	if f.Pkg == nil {
		return f.Name
	}
	return f.Pkg.Name + "." + f.Name
}

// ---- type helpers ----

func isUntyped(t Type) bool {
	if b, ok := t.(*Basic); ok {
		return b.Kind >= UntypedBool
	}
	return false
}

func basicKind(t Type) BasicKind {
	if b, ok := t.Underlying().(*Basic); ok {
		return b.Kind
	}
	return Invalid
}

func isInteger(t Type) bool {
	k := basicKind(t)
	return (k >= Int && k <= Uintptr) || k == UntypedInt || k == UntypedRune
}

func isUnsigned(t Type) bool {
	k := basicKind(t)
	return k >= Uint && k <= Uintptr
}

func isFloat(t Type) bool {
	k := basicKind(t)
	return k == Float32 || k == Float64 || k == UntypedFloat
}

func isNumeric(t Type) bool { return isInteger(t) || isFloat(t) }

func isString(t Type) bool {
	k := basicKind(t)
	return k == String || k == UntypedString
}

func isBoolean(t Type) bool {
	k := basicKind(t)
	return k == Bool || k == UntypedBool
}

func isByteSlice(t Type) bool {
	if s, ok := t.Underlying().(*Slice); ok {
		return basicKind(s.Elem) == Uint8
	}
	return false
}

// Identical reports type identity.
func Identical(a, b Type) bool {
	if a == b {
		return true
	}
	switch x := a.(type) {
	case *Basic:
		y, ok := b.(*Basic)
		return ok && x.Kind == y.Kind
	case *Array:
		y, ok := b.(*Array)
		return ok && x.Len == y.Len && Identical(x.Elem, y.Elem)
	case *Slice:
		y, ok := b.(*Slice)
		return ok && Identical(x.Elem, y.Elem)
	case *Pointer:
		y, ok := b.(*Pointer)
		return ok && Identical(x.Elem, y.Elem)
	case *Chan:
		y, ok := b.(*Chan)
		return ok && Identical(x.Elem, y.Elem)
	case *Map:
		y, ok := b.(*Map)
		return ok && Identical(x.Key, y.Key) && Identical(x.Value, y.Value)
	case *Struct:
		y, ok := b.(*Struct)
		if !ok || len(x.Fields) != len(y.Fields) {
			return false
		}
		for i := range x.Fields {
			if x.Fields[i].Name != y.Fields[i].Name || !Identical(x.Fields[i].Type, y.Fields[i].Type) {
				return false
			}
		}
		return true
	case *Signature:
		y, ok := b.(*Signature)
		if !ok || len(x.Params) != len(y.Params) || len(x.Results) != len(y.Results) || x.Variadic != y.Variadic {
			return false
		}
		for i := range x.Params {
			if !Identical(x.Params[i].Type, y.Params[i].Type) {
				return false
			}
		}
		for i := range x.Results {
			if !Identical(x.Results[i].Type, y.Results[i].Type) {
				return false
			}
		}
		return true
	case *Tuple:
		y, ok := b.(*Tuple)
		if !ok || len(x.Types) != len(y.Types) {
			return false
		}
		for i := range x.Types {
			if !Identical(x.Types[i], y.Types[i]) {
				return false
			}
		}
		return true
	case *ErrorType:
		_, ok := b.(*ErrorType)
		return ok
	case *ArenaType:
		_, ok := b.(*ArenaType)
		return ok
	case *VecType:
		y, ok := b.(*VecType)
		return ok && x.Len == y.Len && Identical(x.Elem, y.Elem)
	case *MapStore:
		y, ok := b.(*MapStore)
		return ok && x.Cap == y.Cap && Identical(x.Key, y.Key) && Identical(x.Value, y.Value)
	case *ChanStore:
		y, ok := b.(*ChanStore)
		return ok && x.Cap == y.Cap && Identical(x.Elem, y.Elem)
	}
	return false
}

// HasRefs reports whether values of type t can hold references (pointers, slices, strings,
// channels, maps) whose lifetime must be tracked. Errors, rawptrs and funcs are not tracked.
func HasRefs(t Type) bool {
	return hasRefs(t, map[Type]bool{})
}

func hasRefs(t Type, seen map[Type]bool) bool {
	if seen[t] {
		return false
	}
	seen[t] = true
	switch u := t.Underlying().(type) {
	case *Basic:
		return u.Kind == String || u.Kind == UntypedString
	case *Pointer, *Slice, *Chan, *Map, *ArenaType:
		return true
	case *Array:
		return hasRefs(u.Elem, seen)
	case *VecType:
		return hasRefs(u.Elem, seen)
	case *MapStore:
		return hasRefs(u.Key, seen) || hasRefs(u.Value, seen)
	case *ChanStore:
		return hasRefs(u.Elem, seen)
	case *Struct:
		for _, f := range u.Fields {
			if hasRefs(f.Type, seen) {
				return true
			}
		}
	case *Tuple:
		for _, e := range u.Types {
			if hasRefs(e, seen) {
				return true
			}
		}
	}
	return false
}

// refDesc describes the first reference inside t for error messages: "string" if t is one,
// "Msg.Body is a string" if a field holds it; "" if t holds none.
func refDesc(t Type) string {
	path, rt := refPath(t, map[Type]bool{})
	switch {
	case rt == nil:
		return ""
	case path == "":
		return rt.String()
	}
	return path + " is a " + rt.String()
}

func refPath(t Type, seen map[Type]bool) (string, Type) {
	if seen[t] {
		return "", nil
	}
	seen[t] = true
	switch u := t.Underlying().(type) {
	case *Array:
		return refPath(u.Elem, seen)
	case *VecType:
		return refPath(u.Elem, seen)
	case *Struct:
		for _, f := range u.Fields {
			if p, rt := refPath(f.Type, seen); rt != nil {
				name := t.String() + "." + f.Name
				if p != "" {
					name += p[strings.Index(p, "."):]
				}
				return name, rt
			}
		}
		return "", nil
	}
	if hasRefs(t, map[Type]bool{}) {
		return "", t
	}
	return "", nil
}

// containsChan reports whether t is channel storage or an array or struct holding some.
func containsChan(t Type) bool {
	switch u := t.Underlying().(type) {
	case *ChanStore:
		return true
	case *Array:
		return containsChan(u.Elem)
	case *Struct:
		for _, f := range u.Fields {
			if containsChan(f.Type) {
				return true
			}
		}
	}
	return false
}

// Comparable reports whether == is defined.
func Comparable(t Type) bool {
	switch u := t.Underlying().(type) {
	case *Basic:
		return u.Kind != Invalid
	case *Pointer, *Chan, *ErrorType:
		return true
	case *Struct:
		for _, f := range u.Fields {
			if !Comparable(f.Type) {
				return false
			}
		}
		return true
	case *Array:
		return Comparable(u.Elem)
	}
	return false
}

// whyIncomparable says what makes t incomparable, naming the field ("Board.hist is a vector");
// "" if t is comparable.
func whyIncomparable(t Type) string {
	switch u := t.Underlying().(type) {
	case *Basic, *Pointer, *Chan, *ErrorType:
		return ""
	case *Struct:
		for _, f := range u.Fields {
			if w := whyIncomparable(f.Type); w != "" {
				if strings.Contains(w, " is ") {
					return t.String() + "." + f.Name + "." + w
				}
				return t.String() + "." + f.Name + " is " + w
			}
		}
		return ""
	case *Array:
		return whyIncomparable(u.Elem)
	case *VecType:
		return "a vector"
	case *Slice:
		return "a slice"
	case *Map, *MapStore:
		return "a map"
	case *ChanStore:
		return "channel storage"
	case *Signature:
		return "a function"
	}
	return "a " + t.String()
}

func defaultType(t Type) Type {
	if b, ok := t.(*Basic); ok {
		switch b.Kind {
		case UntypedBool:
			return tBool
		case UntypedInt:
			return tInt
		case UntypedRune:
			return tInt32
		case UntypedFloat:
			return tFloat64
		case UntypedString:
			return tString
		}
	}
	return t
}

func sizeofBasic(k BasicKind) int64 {
	switch k {
	case Bool, Int8, Uint8:
		return 1
	case Int16, Uint16:
		return 2
	case Int32, Uint32, Float32:
		return 4
	case String:
		return 16
	}
	return 8
}
