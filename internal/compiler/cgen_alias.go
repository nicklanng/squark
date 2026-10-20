package compiler

// What a call can write, for loop versioning: a loop that reads a slice from a field (p.data) or
// a package variable keeps its unchecked copy across a call when nothing the call can run
// changes that slice. A function's summary is the stores it makes outside its own locals, and
// those of everything it calls: the package variables it assigns, the struct fields it stores
// to (by struct type and field), and the types it stores whole (a store of T, or of a value
// holding a T inline, can change any T's fields). A call it can't see into (a func value, an
// extern other than std/math's) or one that can park (a channel operation, select, start, or
// any runtime call), letting another task run, makes the summary unknown.

type fieldKey struct {
	t     string // the struct type's typeKey
	field int
}

type writeSummary struct {
	unknown bool
	globals map[*Var]bool
	fields  map[fieldKey]bool
	types   map[string]bool
}

func newSummary() *writeSummary {
	return &writeSummary{globals: map[*Var]bool{}, fields: map[fieldKey]bool{}, types: map[string]bool{}}
}

func (s *writeSummary) merge(o *writeSummary) {
	s.unknown = s.unknown || o.unknown
	for k := range o.globals {
		s.globals[k] = true
	}
	for k := range o.fields {
		s.fields[k] = true
	}
	for k := range o.types {
		s.types[k] = true
	}
}

// writes returns f's summary (computed once per function).
func (g *gen) writes(f *Func) *writeSummary {
	if g.writeMemo == nil {
		g.writeMemo = map[*Func]*writeSummary{}
	}
	if s, ok := g.writeMemo[f]; ok {
		return s
	}
	s := newSummary()
	g.writeMemo[f] = s // no recursion; this also ends a cycle the checker missed
	switch {
	case f.Extern != "":
		s.unknown = f.Pkg == nil || f.Pkg.Path != "math"
		return s
	case f.Decl == nil || f.Decl.Body == nil || f.GenericOf != nil:
		s.unknown = true // an instance's body is shared with its template's other instances
		return s
	}
	g.bodyWrites(f.Decl.Body, s)
	return s
}

// bodyWrites adds the stores and calls in body to s.
func (g *gen) bodyWrites(body Stmt, s *writeSummary) {
	walkStmt(body, func(n Node) bool {
		if s.unknown {
			return false
		}
		switch n := n.(type) {
		case *AssignStmt:
			for _, x := range n.Lhs {
				g.storeWrites(x, s)
			}
		case *IncDecStmt:
			g.storeWrites(n.X, s)
		case *RangeStmt:
			if !n.Define {
				g.storeWrites(n.Key, s)
				g.storeWrites(n.Value, s)
			}
		case *SendStmt, *SelectStmt, *GoStmt:
			s.unknown = true
		case *UnaryExpr:
			if n.Op == ARROW {
				s.unknown = true
			}
		case *CallExpr:
			g.callWrites(n, s)
		}
		return true
	})
}

func (g *gen) callWrites(c *CallExpr, s *writeSummary) {
	if c.Conversion {
		return
	}
	switch c.Builtin {
	case "":
	case "copy", "append", "simd.store":
		if len(c.Args) > 0 {
			if sl, ok := c.Args[0].info().T.Underlying().(*Slice); ok {
				g.typeWrites(sl.Elem, s) // elements of some slice, wherever it points
			} else {
				s.unknown = true
			}
		}
		return
	case "vec.push", "vec.try_push", "vec.append", "vec.try_append", "vec.pop", "vec.clear":
		if se, ok := unparen(c.Fun).(*SelectorExpr); ok {
			g.storeWrites(se.X, s)
		}
		return
	case "delete", "close", "len", "cap", "min", "max", "panic", "assert", "print", "println", "make", "new":
		return
	default:
		if len(c.Builtin) > 5 && c.Builtin[:5] == "simd." || len(c.Builtin) > 6 && c.Builtin[:6] == "error." || len(c.Builtin) > 6 && c.Builtin[:6] == "arena." {
			return
		}
		s.unknown = true
		return
	}
	if c.Callee == nil {
		s.unknown = true // a func value: anything
		return
	}
	s.merge(g.writes(c.Callee))
}

// storeWrites records a store to x.
func (g *gen) storeWrites(x Expr, s *writeSummary) {
	if x == nil {
		return
	}
	x = unparen(x)
	if id, ok := x.(*Ident); ok && (id.Name == "_" || localVar(id) != nil) {
		return // the function's own variable
	}
	g.typeWrites(x.info().T, s)
	for {
		switch e := unparen(x).(type) {
		case *Ident:
			if e.Obj != nil && e.Obj.Var != nil && e.Obj.Var.Global {
				s.globals[e.Obj.Var] = true
			}
			return
		case *SelectorExpr:
			if e.Kind == SelField && len(e.Path) > 0 {
				t := e.X.info().T
				if p, ok := t.Underlying().(*Pointer); ok {
					t = p.Elem
				}
				s.fields[fieldKey{g.typeKey(t), e.Path[0]}] = true
			}
			x = e.X
		case *IndexExpr:
			x = e.X
		default:
			return // through a pointer or a slice: the type says what can change
		}
	}
}

// typeWrites records a store of a value of type t: t and every type held inline in it.
func (g *gen) typeWrites(t Type, s *writeSummary) {
	if t == nil {
		return
	}
	k := g.typeKey(t)
	if s.types[k] {
		return
	}
	s.types[k] = true
	switch u := t.Underlying().(type) {
	case *Struct:
		for _, f := range u.Fields {
			g.typeWrites(f.Type, s)
		}
	case *Array:
		g.typeWrites(u.Elem, s)
	case *VecType:
		g.typeWrites(u.Elem, s)
	}
}

// addressedAnywhere finds the package variables and the struct fields whose address the program
// takes anywhere (&g, &g.f, &p.f, a pointer method on one): a store through that pointer, which
// the summaries see only as a store of the variable's or field's type, can change them.
func (g *gen) addressedAnywhere() {
	if g.addrGlobals != nil {
		return
	}
	g.addrGlobals, g.addrFields = map[*Var]bool{}, map[fieldKey]bool{}
	mark := func(x Expr) {
		for x != nil {
			switch e := unparen(x).(type) {
			case *Ident:
				if e.Obj != nil && e.Obj.Var != nil && e.Obj.Var.Global {
					g.addrGlobals[e.Obj.Var] = true
				}
				return
			case *SelectorExpr:
				if e.Kind != SelField || len(e.Path) == 0 {
					return
				}
				t := e.X.info().T
				if p, ok := t.Underlying().(*Pointer); ok {
					t = p.Elem
				}
				g.addrFields[fieldKey{g.typeKey(t), e.Path[0]}] = true
				if e.Implicit == "*" {
					return // the rest is behind a pointer
				}
				x = e.X
			case *IndexExpr:
				if _, ok := e.X.info().T.Underlying().(*Array); !ok {
					return
				}
				x = e.X
			default:
				return
			}
		}
	}
	for _, f := range g.prog.Funcs {
		if f.Decl == nil || f.Decl.Body == nil {
			continue
		}
		walkStmt(f.Decl.Body, func(n Node) bool {
			switch e := n.(type) {
			case *UnaryExpr:
				if e.Op == AND {
					mark(e.X)
				}
			case *SelectorExpr:
				if e.Implicit == "&" {
					mark(e.X)
				}
			}
			return true
		})
	}
}

// callSafe reports whether the calls a loop makes (summarized in s) leave the header k alone.
func (g *gen) callSafe(s *writeSummary, k hdrKey) bool {
	if s.unknown {
		return false
	}
	g.addressedAnywhere()
	if k.field < 0 { // a package variable
		return !s.globals[k.v] && !g.addrGlobals[k.v]
	}
	t := k.v.Type
	p, ok := t.Underlying().(*Pointer)
	if !ok {
		return true // a local struct whose address isn't taken: nothing else can reach it
	}
	key := g.typeKey(p.Elem)
	fk := fieldKey{key, k.field}
	return !s.fields[fk] && !s.types[key] && !g.addrFields[fk]
}
