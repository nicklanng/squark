package compiler

import (
	"fmt"
	"strings"
)

// Loop versioning: an innermost loop whose indexes are the counter (times a positive constant)
// plus an amount that doesn't change in the loop, into slices, strings and arrays whose length doesn't change in it,
// gets a second copy without bounds checks. Before the loop, one test (sq_inrange per index) shows
// whether every index the counter can reach is in range; if so the unchecked copy runs, otherwise
// the checked one, so a loop that would panic still panics at the same iteration. Without the
// checks clang can vectorize the loop. The slices it indexes are read into locals first: a slice
// parameter is passed in memory (24 bytes), and clang can't tell that a store through its
// pointer doesn't change its length.
//
// A slice in a struct field (p.data, s.data) or a package variable can change through any store,
// so its header is copied only when the loop can't change it: no calls, and every store is to a
// local variable or of a value without references (squark is type-safe, so storing a number can't
// change a slice header). A nil p skips the unchecked copy, and the checked loop panics as before.

// loopPlan is what versioning a loop needs: the counter and the indexes the unchecked copy
// leaves unchecked.
type loopPlan struct {
	iv      *Var
	indexes []loopIndex
	copies  []*Var        // slice and string variables to read into locals before the loop
	headers []*loopHeader // slices in fields and package variables, read before the loop
}

// loopIndex is x[scale*iv + off] (off nil: x[scale*iv]), with x read from hdr if it is a field or
// package variable.
type loopIndex struct {
	e     *IndexExpr
	scale int64
	off   Expr
	hdr   *loopHeader
}

// loopHeader is a field or package slice (p.data, s.data, pkgSlice) copied before the loop.
type loopHeader struct {
	x   Expr
	key hdrKey
	tmp string // its C local, set by versioned
}

// hdrKey names a header: a package variable (field -1), or a field of a local.
type hdrKey struct {
	v     *Var
	field int
}

// headerKey returns x's key if x names a package variable or a field of a local.
func headerKey(x Expr) (hdrKey, bool) {
	switch e := unparen(x).(type) {
	case *Ident:
		if e.Obj != nil && e.Obj.Kind == ObjVar && e.Obj.Var != nil && e.Obj.Var.Global {
			return hdrKey{e.Obj.Var, -1}, true
		}
	case *SelectorExpr:
		if v := localVar(e.X); v != nil && e.Kind == SelField {
			return hdrKey{v, e.Path[0]}, true
		}
	}
	return hdrKey{}, false
}

// maxVersionedStmts bounds the body of a loop that gets a second copy.
const maxVersionedStmts = 40

// walkStmt calls f for s and every statement and expression in it, in source order; f returns
// false to skip a node's children.
func walkStmt(s Stmt, f func(Node) bool) {
	if s == nil || !f(s) {
		return
	}
	ws := func(s Stmt) { walkStmt(s, f) }
	we := func(x Expr) { walkExpr(x, f) }
	switch s := s.(type) {
	case *ExprStmt:
		we(s.X)
	case *SendStmt:
		we(s.Chan)
		we(s.Value)
	case *IncDecStmt:
		we(s.X)
	case *AssignStmt:
		for _, x := range s.Lhs {
			we(x)
		}
		for _, x := range s.Rhs {
			we(x)
		}
	case *GoStmt:
		we(s.Call)
	case *DeferStmt:
		we(s.Call)
	case *ReturnStmt:
		for _, x := range s.Results {
			we(x)
		}
	case *BlockStmt:
		for _, x := range s.List {
			ws(x)
		}
	case *IfStmt:
		ws(s.Init)
		we(s.Cond)
		ws(s.Body)
		ws(s.Else)
	case *CaseClause:
		for _, x := range s.List {
			we(x)
		}
		for _, x := range s.Body {
			ws(x)
		}
	case *SwitchStmt:
		ws(s.Init)
		we(s.Tag)
		for _, c := range s.Cases {
			ws(c)
		}
	case *ForStmt:
		ws(s.Init)
		we(s.Cond)
		ws(s.Post)
		ws(s.Body)
	case *RangeStmt:
		we(s.Key)
		we(s.Value)
		we(s.Lo)
		we(s.X)
		ws(s.Body)
	case *SelectStmt:
		for _, c := range s.Cases {
			ws(c.Comm)
			we(c.Timeout)
			for _, x := range c.Body {
				ws(x)
			}
		}
	case *DeclStmt:
		for _, v := range s.Decl.Values {
			for _, x := range v.Values {
				we(x)
			}
		}
	case *LabeledStmt:
		ws(s.Stmt)
	case *ArenaStmt:
		we(s.Size)
		ws(s.Body)
	}
}

func walkExpr(x Expr, f func(Node) bool) {
	if x == nil || !f(x) {
		return
	}
	we := func(x Expr) { walkExpr(x, f) }
	switch e := x.(type) {
	case *CompositeLit:
		for _, el := range e.Elts {
			we(el)
		}
	case *KeyValueExpr:
		we(e.Key)
		we(e.Value)
	case *ParenExpr:
		we(e.X)
	case *SelectorExpr:
		we(e.X)
	case *IndexExpr:
		we(e.X)
		we(e.Index)
	case *SliceExpr:
		we(e.X)
		we(e.Lo)
		we(e.Hi)
		we(e.Max)
	case *CallExpr:
		we(e.Fun)
		for _, a := range e.Args {
			we(a)
		}
	case *StarExpr:
		we(e.X)
	case *UnaryExpr:
		we(e.X)
	case *BinaryExpr:
		we(e.X)
		we(e.Y)
	case *TryExpr:
		we(e.X)
	case *InExpr:
		we(e.X)
		we(e.Arena)
	}
}

// localVar returns the local variable x names, if it does.
func localVar(x Expr) *Var {
	id, ok := unparen(x).(*Ident)
	if !ok || id.Obj == nil || id.Obj.Kind != ObjVar || id.Obj.Var == nil || id.Obj.Var.Global {
		return nil
	}
	return id.Obj.Var
}

// rootVar returns the variable an addressable expression is part of (v in v.f[i].g).
func rootVar(x Expr) *Var {
	for {
		switch e := unparen(x).(type) {
		case *Ident:
			return localVar(e)
		case *SelectorExpr:
			if e.Kind != SelField || e.Implicit == "*" {
				return nil
			}
			x = e.X
		case *IndexExpr:
			if _, ok := e.X.info().T.Underlying().(*Array); !ok {
				return nil
			}
			x = e.X
		default:
			return nil
		}
	}
}

// addressedVars returns the locals of the function being generated whose address is taken
// (&v, &v.f, a pointer method called on v, v[:] of an array): a store through the pointer can
// change them anywhere.
func (g *gen) addressedVars() map[*Var]bool {
	if g.addressed != nil {
		return g.addressed
	}
	g.addressed = map[*Var]bool{}
	walkStmt(g.fn.Decl.Body, func(n Node) bool {
		var x Expr
		switch e := n.(type) {
		case *UnaryExpr:
			if e.Op == AND {
				x = e.X
			}
		case *SelectorExpr:
			if e.Implicit == "&" {
				x = e.X
			}
		case *SliceExpr:
			if _, ok := e.X.info().T.Underlying().(*Array); ok {
				x = e.X
			}
		}
		if v := rootVar(x); v != nil {
			g.addressed[v] = true
		}
		return true
	})
	return g.addressed
}

func isIntType(t Type) bool {
	b, ok := t.Underlying().(*Basic)
	return ok && b.Kind == Int
}

// planLoop decides whether the loop with counter iv (an int variable that only the loop
// changes, by one per iteration) and this body gets an unchecked copy, and of which indexes.
// The loop's bounds must not change in it either.
func (g *gen) planLoop(iv *Var, body *BlockStmt, bounds ...Expr) *loopPlan {
	if iv == nil || !isIntType(iv.Type) || g.addressedVars()[iv] {
		return nil
	}
	// variables the body assigns or declares change in the loop; memSafe: no calls, and no store
	// that could change a slice header outside a local variable
	changes := map[*Var]bool{}
	ok, stmts, memSafe := true, 0, !g.fnPreempt
	store := func(x Expr) {
		if v := localVar(x); v != nil {
			changes[v] = true
		} else if HasRefs(x.info().T) {
			memSafe = false
		}
	}
	walkStmt(body, func(n Node) bool {
		if !ok {
			return false
		}
		switch s := n.(type) {
		case Stmt:
			stmts++
			switch s := s.(type) {
			case *ForStmt, *RangeStmt, *SwitchStmt, *SelectStmt, *LabeledStmt, *GoStmt, *DeferStmt, *ArenaStmt, *SendStmt:
				// an inner loop or a statement with labels or registrations is not copied
				ok = false
			case *AssignStmt:
				for _, x := range s.Lhs {
					store(x)
				}
			case *IncDecStmt:
				store(s.X)
			case *DeclStmt:
				for _, vs := range s.Decl.Values {
					for _, id := range vs.Names {
						if v := localVar(id); v != nil {
							changes[v] = true
						}
					}
				}
			}
		case *InExpr, *TryExpr:
			ok = false
		case *UnaryExpr:
			if s.Op == ARROW {
				ok = false
			}
		case *CallExpr:
			switch {
			case s.Builtin == "make" || s.Builtin == "new" || s.Builtin == "append":
				ok = false
			case s.Conversion, s.Builtin == "len", s.Builtin == "cap", s.Builtin == "min", s.Builtin == "max",
				s.Builtin == "print", s.Builtin == "println", s.Builtin == "panic", strings.HasPrefix(s.Builtin, "simd."):
			default:
				memSafe = false // a call can store anywhere
			}
		}
		return true
	})
	if !ok || stmts > maxVersionedStmts || changes[iv] {
		return nil
	}
	fixed := func(v *Var) bool { return v != nil && v != iv && !changes[v] && !g.addressedVars()[v] }
	// header: a slice or string in a field of a fixed local (p.data, s.data) or in a package
	// variable, which the loop can't change
	header := func(x Expr) bool {
		if !memSafe || x.info().View != nil {
			return false
		}
		k, ok := headerKey(x)
		return ok && (k.field < 0 || fixed(k.v))
	}
	// invariant: an int expression whose value is the same in every iteration and that can be
	// evaluated before the loop (no calls, so no panics)
	var invariant func(x Expr) bool
	invariant = func(x Expr) bool {
		if !isIntType(x.info().T) && x.info().Const == nil {
			return false
		}
		if x.info().Const != nil {
			_, ok := constInt(x)
			return ok
		}
		switch e := unparen(x).(type) {
		case *Ident:
			return fixed(localVar(e))
		case *BinaryExpr:
			return (e.Op == ADD || e.Op == SUB || e.Op == MUL) && invariant(e.X) && invariant(e.Y)
		case *CallExpr:
			if e.Builtin != "len" || len(e.Args) != 1 {
				return false
			}
			switch e.Args[0].info().T.Underlying().(type) {
			case *Array:
				return true
			case *Slice, *Basic:
				return e.Args[0].info().View == nil && fixed(localVar(e.Args[0]))
			}
		}
		return false
	}
	for _, b := range bounds {
		// a bound is evaluated before the first iteration anyway, so it may read a header
		if c, ok := unparen(b).(*CallExpr); ok && c.Builtin == "len" && len(c.Args) == 1 && header(c.Args[0]) {
			continue
		}
		if !invariant(b) {
			return nil
		}
	}
	isIV := func(x Expr) bool {
		id, ok := unparen(x).(*Ident)
		return ok && id.Obj != nil && id.Obj.Var == iv
	}
	// scaled: x is iv or k*iv (iv*k) for a constant k > 0, and its k
	scaled := func(x Expr) int64 {
		if isIV(x) {
			return 1
		}
		if b, ok := unparen(x).(*BinaryExpr); ok && b.Op == MUL {
			k, ok := constInt(b.X)
			if !isIV(b.Y) {
				k, ok = constInt(b.Y)
				if !isIV(b.X) {
					return 0
				}
			}
			if ok && k > 0 {
				return k
			}
		}
		return 0
	}
	p := &loopPlan{iv: iv}
	copied := map[*Var]bool{}
	headers := map[hdrKey]*loopHeader{} // p.data in two indexes is one header
	walkStmt(body, func(n Node) bool {
		e, ok := n.(*IndexExpr)
		if !ok || e.X.info().View != nil || !isIntType(e.Index.info().T) {
			return true
		}
		var hdr *loopHeader
		switch e.X.info().T.Underlying().(type) {
		case *Slice, *Basic:
			if header(e.X) {
				key, _ := headerKey(e.X)
				if hdr = headers[key]; hdr == nil {
					hdr = &loopHeader{x: e.X, key: key}
					headers[key] = hdr
					p.headers = append(p.headers, hdr)
				}
				break
			}
			v := localVar(e.X)
			if !fixed(v) {
				return true
			}
			if !copied[v] {
				copied[v] = true
				p.copies = append(p.copies, v)
			}
		case *Array, *Pointer:
			if !isSimpleC(e.X) {
				return true
			}
		default:
			return true // maps
		}
		var off Expr
		k := scaled(e.Index)
		if ix, ok := unparen(e.Index).(*BinaryExpr); ok && k == 0 {
			switch {
			case ix.Op == ADD && scaled(ix.X) > 0 && invariant(ix.Y):
				k, off = scaled(ix.X), ix.Y
			case ix.Op == ADD && scaled(ix.Y) > 0 && invariant(ix.X):
				k, off = scaled(ix.Y), ix.X
			case ix.Op == SUB && scaled(ix.X) > 0 && invariant(ix.Y):
				k, off = scaled(ix.X), ix // k*iv - y: the offset is -y, emitted by offsetC
			}
		}
		if k == 0 {
			return true
		}
		p.indexes = append(p.indexes, loopIndex{e, k, off, hdr})
		return true
	})
	if len(p.indexes) == 0 {
		return nil
	}
	return p
}

// loopBound says which values the counter takes, as the runtime function that tests an index
// against them (sq_inrange: from a up to b, excluded; sq_inrange_incl: b included; sq_inrange_down:
// from b down to a, included; sq_inrange_above: from b down to a, excluded) and its first two
// arguments, C expressions valid before the loop.
type loopBound struct {
	fn, a, b string
}

// versioned emits a loop with counter bound b, as loop() writes it, twice when p plans an
// unchecked copy. s is the loop statement (its labels are renumbered in the first copy).
func (g *gen) versioned(s Stmt, p *loopPlan, b func() loopBound, loop func()) {
	if p == nil {
		loop()
		return
	}
	saved := map[*Var]string{}
	for _, v := range p.copies {
		c := g.tmp()
		g.line("%s %s = %s;", g.ctype(v.Type), c, v.CName)
		saved[v] = v.CName
		v.CName = c
	}
	conds := map[string]bool{}
	var list []string
	add := func(c string) {
		if !conds[c] {
			conds[c] = true
			list = append(list, c)
		}
	}
	for _, h := range p.headers {
		h.tmp = g.tmp()
		ct := g.ctype(h.x.info().T)
		if se, ok := unparen(h.x).(*SelectorExpr); ok && se.Implicit == "*" {
			// p.data: read only if p isn't nil (the checked copy then panics where it should)
			f := se.X.info().T.Underlying().(*Pointer).Elem.Underlying().(*Struct).Fields[se.Path[0]]
			ptr := g.expr(se.X)
			g.line("%s %s = (%s) != NULL ? (%s)->%s : (%s){0};", ct, h.tmp, ptr, ptr, fieldName(f.Name, se.Path[0]), ct)
			add(fmt.Sprintf("(%s) != NULL", ptr))
			continue
		}
		g.line("%s %s = %s;", ct, h.tmp, g.expr(h.x))
	}
	bound := b()
	for _, ix := range p.indexes {
		var n string
		switch u := ix.e.X.info().T.Underlying().(type) {
		case *Array:
			n = fmt.Sprint(u.Len)
		case *Pointer:
			n = fmt.Sprint(u.Elem.Underlying().(*Array).Len)
		default:
			if ix.hdr != nil {
				n = ix.hdr.tmp + ".len"
			} else {
				n = "(" + g.expr(ix.e.X) + ").len"
			}
		}
		add(fmt.Sprintf("%s(%s, %s, %d, %s, %s)", bound.fn, bound.a, bound.b, ix.scale, g.offsetC(ix.off), n))
	}
	g.line("if (%s) {", strings.Join(list, " && "))
	g.ind++
	orig := g.label(s)
	g.nextLbl++
	g.labelIDs[s] = g.nextLbl
	g.unchecked = map[*IndexExpr]string{}
	for _, ix := range p.indexes {
		g.unchecked[ix.e] = ""
		if ix.hdr != nil {
			g.unchecked[ix.e] = ix.hdr.tmp
		}
	}
	// in the unchecked copy every read of a header reads the copy, so clang sees that len(p.data)
	// in the condition can't change
	g.headerCopies = map[hdrKey]string{}
	for _, h := range p.headers {
		g.headerCopies[h.key] = h.tmp
	}
	loop()
	g.unchecked, g.headerCopies = nil, nil
	g.line("sq_brk%d:;", g.labelIDs[s])
	g.labelIDs[s] = orig
	g.ind--
	g.line("} else {")
	g.ind++
	loop()
	g.ind--
	g.line("}")
	for v, c := range saved {
		v.CName = c
	}
}

// offsetC is the C value of an index's offset from the counter.
func (g *gen) offsetC(off Expr) string {
	if off == nil {
		return "0"
	}
	if b, ok := off.(*BinaryExpr); ok && b.Op == SUB {
		return "-(" + g.coerce(b.Y, tInt) + ")" // wraps for INT64_MIN like the index does
	}
	return g.coerce(off, tInt)
}

// forLoopCounter returns the counter of `for i := a; i < b; i++` (or i <= b, b > i, i += 1) or
// `for i := a; i >= b; i--` (or i > b, b <= i, i -= 1), the bound b, and the runtime test for
// it (see loopBound), or nil.
func forLoopCounter(s *ForStmt) (iv *Var, bound Expr, fn string) {
	init, ok := s.Init.(*AssignStmt)
	if !ok || init.Tok != DEFINE || len(init.Lhs) != 1 || len(init.NewVars) != 1 || !init.NewVars[0] {
		return nil, nil, ""
	}
	iv = localVar(init.Lhs[0])
	if iv == nil {
		return nil, nil, ""
	}
	isIV := func(x Expr) bool { return localVar(x) == iv }
	up := false
	switch post := s.Post.(type) {
	case *IncDecStmt:
		if !isIV(post.X) {
			return nil, nil, ""
		}
		up = post.Tok == INC
	case *AssignStmt:
		if v, ok := constInt(post.Rhs[0]); len(post.Lhs) != 1 || !isIV(post.Lhs[0]) || !ok || v != 1 || (post.Tok != ADD_ASSIGN && post.Tok != SUB_ASSIGN) {
			return nil, nil, ""
		}
		up = post.Tok == ADD_ASSIGN
	default:
		return nil, nil, ""
	}
	c, ok := unparen(s.Cond).(*BinaryExpr)
	if !ok {
		return nil, nil, ""
	}
	op, b := c.Op, c.Y
	if isIV(c.Y) && !isIV(c.X) {
		// b op i: the same as i op' b
		op, b = map[Tok]Tok{LSS: GTR, LEQ: GEQ, GTR: LSS, GEQ: LEQ}[c.Op], c.X
	} else if !isIV(c.X) || isIV(c.Y) {
		return nil, nil, ""
	}
	switch {
	case up && op == LSS:
		return iv, b, "sq_inrange"
	case up && op == LEQ:
		return iv, b, "sq_inrange_incl"
	case !up && op == GEQ:
		return iv, b, "sq_inrange_down"
	case !up && op == GTR:
		return iv, b, "sq_inrange_above"
	}
	return nil, nil, ""
}
