package compiler

import (
	"fmt"
	"strings"
)

// Loop versioning: an innermost loop whose indexes are the loop counter plus an amount that
// doesn't change in the loop, into slices, strings and arrays whose length doesn't change in it,
// gets a second copy without bounds checks. Before the loop, one test (sq_inrange per index) shows
// whether every index the counter can reach is in range; if so the unchecked copy runs, otherwise
// the checked one, so a loop that would panic still panics at the same iteration. Without the
// checks clang can vectorize the loop. The slices it indexes are read into locals first: a slice
// parameter is passed in memory (24 bytes), and clang can't tell that a store through its
// pointer doesn't change its length.

// loopPlan is what versioning a loop needs: the counter and the indexes the unchecked copy
// leaves unchecked.
type loopPlan struct {
	iv      *Var
	indexes []loopIndex
	copies  []*Var // slice and string variables to read into locals before the loop
}

// loopIndex is x[iv + off] (off nil: x[iv]).
type loopIndex struct {
	e   *IndexExpr
	off Expr
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
	// variables the body assigns or declares change in the loop
	changes := map[*Var]bool{}
	ok, stmts := true, 0
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
					if v := localVar(x); v != nil {
						changes[v] = true
					}
				}
			case *IncDecStmt:
				if v := localVar(s.X); v != nil {
					changes[v] = true
				}
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
			switch s.Builtin {
			case "make", "new", "append":
				ok = false
			}
		}
		return true
	})
	if !ok || stmts > maxVersionedStmts || changes[iv] {
		return nil
	}
	fixed := func(v *Var) bool { return v != nil && v != iv && !changes[v] && !g.addressedVars()[v] }
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
		if !invariant(b) {
			return nil
		}
	}
	isIV := func(x Expr) bool {
		id, ok := unparen(x).(*Ident)
		return ok && id.Obj != nil && id.Obj.Var == iv
	}
	p := &loopPlan{iv: iv}
	copied := map[*Var]bool{}
	walkStmt(body, func(n Node) bool {
		e, ok := n.(*IndexExpr)
		if !ok || e.X.info().View != nil || !isIntType(e.Index.info().T) {
			return true
		}
		switch e.X.info().T.Underlying().(type) {
		case *Slice, *Basic:
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
		switch ix := unparen(e.Index).(type) {
		case *Ident:
			if !isIV(ix) {
				return true
			}
		case *BinaryExpr:
			switch {
			case ix.Op == ADD && isIV(ix.X) && invariant(ix.Y):
				off = ix.Y
			case ix.Op == ADD && isIV(ix.Y) && invariant(ix.X):
				off = ix.X
			case ix.Op == SUB && isIV(ix.X) && invariant(ix.Y):
				off = ix // iv - y: the offset is -y, emitted by offsetC
			default:
				return true
			}
		default:
			return true
		}
		p.indexes = append(p.indexes, loopIndex{e, off})
		return true
	})
	if len(p.indexes) == 0 {
		return nil
	}
	return p
}

// loopBound says which values the counter takes: lo, then up to hi (excluded, or included when
// incl), counting up by one. They are C expressions, valid before the loop.
type loopBound struct {
	lo, hi string
	incl   bool
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
	bound := b()
	conds := map[string]bool{}
	var list []string
	for _, ix := range p.indexes {
		var n string
		switch u := ix.e.X.info().T.Underlying().(type) {
		case *Array:
			n = fmt.Sprint(u.Len)
		case *Pointer:
			n = fmt.Sprint(u.Elem.Underlying().(*Array).Len)
		default:
			n = "(" + g.expr(ix.e.X) + ").len"
		}
		fn := "sq_inrange"
		if bound.incl {
			fn = "sq_inrange_incl"
		}
		c := fmt.Sprintf("%s(%s, %s, %s, %s)", fn, bound.lo, bound.hi, g.offsetC(ix.off), n)
		if !conds[c] {
			conds[c] = true
			list = append(list, c)
		}
	}
	g.line("if (%s) {", strings.Join(list, " && "))
	g.ind++
	orig := g.label(s)
	g.nextLbl++
	g.labelIDs[s] = g.nextLbl
	g.unchecked = map[*IndexExpr]bool{}
	for _, ix := range p.indexes {
		g.unchecked[ix.e] = true
	}
	loop()
	g.unchecked = nil
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

// forLoopCounter returns the counter of `for i := lo; i < hi; i++` (or i <= hi, hi > i,
// i += 1) and the bound expression, or nil.
func forLoopCounter(s *ForStmt) (iv *Var, hi Expr, incl bool) {
	init, ok := s.Init.(*AssignStmt)
	if !ok || init.Tok != DEFINE || len(init.Lhs) != 1 || len(init.NewVars) != 1 || !init.NewVars[0] {
		return nil, nil, false
	}
	iv = localVar(init.Lhs[0])
	if iv == nil {
		return nil, nil, false
	}
	isIV := func(x Expr) bool { return localVar(x) == iv }
	switch post := s.Post.(type) {
	case *IncDecStmt:
		if post.Tok != INC || !isIV(post.X) {
			return nil, nil, false
		}
	case *AssignStmt:
		if v, ok := constInt(post.Rhs[0]); post.Tok != ADD_ASSIGN || len(post.Lhs) != 1 || !isIV(post.Lhs[0]) || !ok || v != 1 {
			return nil, nil, false
		}
	default:
		return nil, nil, false
	}
	c, ok := unparen(s.Cond).(*BinaryExpr)
	if !ok {
		return nil, nil, false
	}
	switch {
	case (c.Op == LSS || c.Op == LEQ) && isIV(c.X):
		return iv, c.Y, c.Op == LEQ
	case (c.Op == GTR || c.Op == GEQ) && isIV(c.Y):
		return iv, c.X, c.Op == GEQ
	}
	return nil, nil, false
}
