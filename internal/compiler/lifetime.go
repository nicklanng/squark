package compiler

import (
	"fmt"
	"math/bits"
)

// Lifetime analysis: squark's replacement for Go's escape analysis.
//
// Where Go would move a value to the heap because a reference to it outlives its stack frame,
// squark reports a compile error. Every reference-typed value (pointer, slice, string, map, chan,
// or a struct/array containing one) carries two regions:
//
//   lo: a lower bound on how long the memory it references lives
//   hi: an upper bound on how long that memory lives (used when storing *through* the reference)
//
// Region depths: 0 = static, 1 = owned by the caller (derived from parameters), >=2 = a local block
// at that lexical depth. A deeper block dies sooner. Storing a reference requires that it outlive the
// storage it is written to; returning one requires depth <= 1.
//
// Recursion is banned, so the call graph is a DAG and every function is analysed after its callees.
// Each function gets a Summary of how references flow between its parameters and results, which call
// sites check against their actual arguments. No lifetime annotations are needed.

type Region struct {
	D      int
	M      uint64 // parameter slots the reference derives from (when D == 1)
	Src    string // description of the referenced memory, for diagnostics
	SrcPos Pos
}

var static = Region{}

// join returns the shorter-lived of two regions (used for lower bounds).
func join(a, b Region) Region {
	r := a
	if b.D > a.D || (a.D == 0 && b.D == 0 && a.Src == "") {
		r = b
	}
	r.M = a.M | b.M
	return r
}

// meet returns the longer-lived of two regions (used for upper bounds).
func meet(a, b Region) Region {
	r := a
	if b.D < a.D {
		r = b
	}
	r.M = a.M | b.M
	return r
}

type Flow struct{ Src, Dst int }

// Summary describes how a function's reference-typed parameters may flow.
type Summary struct {
	RetMask   uint64 // slots whose references may be returned
	StaticReq uint64 // slots whose references must point to static memory
	Flows     []Flow // references from slot Src may be stored through slot Dst
}

func (s *Summary) addFlow(src, dst int) {
	if src == dst {
		return
	}
	for _, f := range s.Flows {
		if f.Src == src && f.Dst == dst {
			return
		}
	}
	s.Flows = append(s.Flows, Flow{src, dst})
}

type lifetime struct {
	prog   *Program
	errs   ErrorList
	seen   map[string]bool
	fn     *Func
	depth  int
	report bool
	sum    *Summary
	inInit bool
	change bool
}

// AnalyzeLifetimes runs the lifetime checker over all functions (callees first).
func AnalyzeLifetimes(prog *Program, order []*Func) error {
	l := &lifetime{prog: prog, seen: map[string]bool{}}
	for _, fn := range order {
		if fn.Decl == nil || fn.Decl.Body == nil {
			fn.Summary = &Summary{}
			continue
		}
		l.function(fn)
	}
	l.globals()
	if len(l.errs) > 0 {
		return l.errs
	}
	return nil
}

func (l *lifetime) errorf(pos Pos, format string, args ...any) {
	if !l.report {
		return
	}
	msg := fmt.Sprintf(format, args...)
	key := pos.String() + msg
	if l.seen[key] {
		return
	}
	l.seen[key] = true
	l.errs = append(l.errs, &Error{pos, msg})
}

func slotOf(fn *Func, v *Var) int {
	if fn.Sig.Recv != nil {
		if v == fn.Sig.Recv {
			return 0
		}
		return v.Index + 1
	}
	return v.Index
}

func numSlots(fn *Func) int {
	n := len(fn.Sig.Params)
	if fn.Sig.Recv != nil {
		n++
	}
	return n
}

func lifetimeOf(v *Var) int {
	if v.Global {
		return 0
	}
	if v.Param || v.Depth < 2 {
		return 2
	}
	return v.Depth
}

func (l *lifetime) function(fn *Func) {
	l.fn = fn
	if numSlots(fn) > 63 {
		l.report = true
		l.errorf(fn.Decl.P, "too many parameters")
		return
	}
	params := fn.Sig.Params
	if fn.Sig.Recv != nil {
		params = append([]*Var{fn.Sig.Recv}, params...)
	}
	for _, p := range params {
		s := slotOf(fn, p)
		r := Region{D: 1, M: 1 << s, Src: "parameter " + p.Name, SrcPos: p.Pos}
		p.Content = r
		p.ContentHi = r
		p.hiSet = true
	}
	// iterate to a fixed point, then report
	for i := 0; i < 20; i++ {
		l.change = false
		l.report = false
		l.sum = &Summary{}
		l.body(fn)
		if !l.change {
			break
		}
	}
	l.report = true
	l.sum = &Summary{}
	l.body(fn)
	fn.Summary = l.sum
}

func (l *lifetime) body(fn *Func) {
	l.depth = 2
	l.stmts(fn.Decl.Body.List)
}

func (l *lifetime) globals() {
	l.fn = nil
	l.report = true
	l.inInit = true
	l.depth = 0
	l.sum = &Summary{}
	for _, pkg := range l.prog.Packages {
		for _, vs := range pkg.GlobalInits {
			if len(vs.Values) != len(vs.Names) {
				continue
			}
			for i, x := range vs.Values {
				lo, _ := l.regs(x)
				if HasRefs(x.info().T) {
					l.checkFlow(lo, static, x.Pos(), "package variable "+vs.Names[i].Name)
				}
			}
		}
	}
	l.inInit = false
}

// ---- regions of expressions ----

func (l *lifetime) cur() Region {
	if l.fn == nil {
		return static
	}
	return Region{D: l.depth, Src: "a temporary in this block"}
}

func (l *lifetime) lo(e Expr) Region {
	lo, _ := l.regs(e)
	return lo
}

func (l *lifetime) regs(e Expr) (lo, hi Region) {
	lo, hi = l.regs1(e)
	t := e.info().T
	if t == nil || e.info().Const != nil || !HasRefs(t) {
		return static, static
	}
	return lo, hi
}

func (l *lifetime) varRegs(v *Var, pos Pos) (Region, Region) {
	if v.Global {
		return static, static
	}
	if v.AddrTaken {
		r := Region{D: lifetimeOf(v), Src: "variable " + v.Name, SrcPos: v.Pos}
		if v.Param && !HasRefs(v.Type) {
			return r, static
		}
		// stores through pointers may have put anything that outlives v into it
		return join(r, v.Content), static
	}
	hi := v.ContentHi
	if !v.hiSet {
		hi = static
	}
	return v.Content, hi
}

func (l *lifetime) regs1(e Expr) (Region, Region) {
	switch e := e.(type) {
	case *Ident:
		if e.Obj != nil && e.Obj.Kind == ObjVar {
			return l.varRegs(e.Obj.Var, e.P)
		}
		return static, static
	case *BasicLit:
		return static, static
	case *ParenExpr:
		return l.regs(e.X)
	case *SelectorExpr:
		switch e.Kind {
		case SelQualified:
			return static, static
		case SelField:
			if e.Implicit == "*" {
				return l.lo(e.X), static
			}
			return l.regs(e.X)
		}
		l.regs(e.X)
		return static, static
	case *IndexExpr:
		xt := e.X.info().T.Underlying()
		l.regs(e.Index)
		switch u := xt.(type) {
		case *Array:
			return l.regs(e.X)
		case *Pointer, *Slice, *Map:
			_ = u
			return l.lo(e.X), static
		}
		l.regs(e.X)
		return static, static
	case *SliceExpr:
		for _, ix := range []Expr{e.Lo, e.Hi, e.Max} {
			if ix != nil {
				l.regs(ix)
			}
		}
		switch e.X.info().T.Underlying().(type) {
		case *Array:
			a := l.addr(e.X)
			return a, a
		}
		return l.regs(e.X)
	case *StarExpr:
		return l.lo(e.X), static
	case *UnaryExpr:
		switch e.Op {
		case AND:
			if cl, ok := unparen(e.X).(*CompositeLit); ok {
				c := l.cur()
				lo, _ := l.regs(cl)
				l.checkFlow(lo, c, cl.P, "composite literal")
				return c, c
			}
			a := l.addr(e.X)
			return a, a
		case ARROW:
			l.regs(e.X)
			return static, static
		}
		l.regs(e.X)
		return static, static
	case *BinaryExpr:
		l.regs(e.X)
		l.regs(e.Y)
		return static, static
	case *CompositeLit:
		return l.compositeLit(e)
	case *KeyValueExpr:
		return l.regs(e.Value)
	case *CallExpr:
		return l.call(e)
	}
	return static, static
}

func (l *lifetime) compositeLit(e *CompositeLit) (Region, Region) {
	lo, hi := static, static
	first := true
	for _, el := range e.Elts {
		v := el
		if kv, ok := el.(*KeyValueExpr); ok {
			v = kv.Value
		}
		elo, ehi := l.regs(v)
		if !HasRefs(v.info().T) {
			continue
		}
		lo = join(lo, elo)
		if first {
			hi = ehi
			first = false
		} else {
			hi = meet(hi, ehi)
		}
	}
	if _, ok := e.T.Underlying().(*Slice); ok {
		// the backing array lives in the enclosing block
		c := l.cur()
		l.checkFlow(lo, c, e.P, "slice literal")
		return c, c
	}
	return lo, hi
}

// addr returns the lifetime of the storage denoted by an addressable expression.
func (l *lifetime) addr(e Expr) Region {
	switch e := e.(type) {
	case *Ident:
		if e.Obj == nil || e.Obj.Var == nil {
			return static
		}
		v := e.Obj.Var
		if v.Global {
			return static
		}
		if !v.AddrTaken {
			v.AddrTaken = true
			l.change = true
		}
		return Region{D: lifetimeOf(v), Src: "variable " + v.Name, SrcPos: v.Pos}
	case *ParenExpr:
		return l.addr(e.X)
	case *SelectorExpr:
		if e.Kind == SelQualified {
			return static
		}
		if e.Implicit == "*" {
			return l.lo(e.X)
		}
		return l.addr(e.X)
	case *IndexExpr:
		l.regs(e.Index)
		if _, ok := e.X.info().T.Underlying().(*Array); ok {
			return l.addr(e.X)
		}
		return l.lo(e.X)
	case *StarExpr:
		return l.lo(e.X)
	case *CompositeLit:
		c := l.cur()
		lo, _ := l.regs(e)
		l.checkFlow(lo, c, e.P, "composite literal")
		return c
	}
	l.regs(e)
	return l.cur()
}

// ---- flow checks ----

// checkFlow checks that references with lifetime lo may be stored into storage living until bound.
func (l *lifetime) checkFlow(lo, bound Region, pos Pos, into string) {
	if lo.D == 0 {
		return
	}
	switch {
	case bound.D == 0:
		if lo.D == 1 {
			l.sum.StaticReq |= lo.M
			return
		}
		l.errorf(pos, "%s cannot escape into %s: %s", describe(lo), into, escapeHint(into))
	case bound.D == 1:
		if lo.D == 1 {
			for s := lo.M; s != 0; s &= s - 1 {
				src := bits.TrailingZeros64(s)
				for d := bound.M; d != 0; d &= d - 1 {
					l.sum.addFlow(src, bits.TrailingZeros64(d))
				}
			}
			return
		}
		l.errorf(pos, "%s cannot be stored in %s, which outlives this function (in Go it would escape to the heap)", describe(lo), into)
	default:
		if lo.D > bound.D {
			l.errorf(pos, "%s does not live long enough to be stored in %s", describe(lo), into)
		}
	}
}

func escapeHint(into string) string {
	return "it would outlive its stack frame (Go would move it to the heap); only references to package-level variables or constants can be stored there"
}

func describe(r Region) string {
	if r.Src == "" {
		return "a reference to local memory"
	}
	if r.SrcPos.File != "" {
		return fmt.Sprintf("a reference to %s (declared at %s:%d)", r.Src, shortFile(r.SrcPos.File), r.SrcPos.Line)
	}
	return "a reference to " + r.Src
}

func shortFile(f string) string {
	for i := len(f) - 1; i >= 0; i-- {
		if f[i] == '/' {
			return f[i+1:]
		}
	}
	return f
}

func targetName(e Expr) string {
	return exprString(e)
}

// store records an assignment of a value with regions (lo, hi) to the target expression.
func (l *lifetime) store(target Expr, lo, hi Region, pos Pos) {
	if isBlank(target) {
		return
	}
	if !HasRefs(target.info().T) {
		l.walkTarget(target)
		return
	}
	switch t := target.(type) {
	case *Ident:
		if t.Obj == nil || t.Obj.Var == nil {
			return
		}
		l.storeVar(t.Obj.Var, lo, hi, pos)
		return
	case *ParenExpr:
		l.store(t.X, lo, hi, pos)
		return
	case *SelectorExpr:
		if t.Kind == SelQualified {
			l.checkFlow(lo, static, pos, "package variable "+exprString(t))
			return
		}
		if t.Implicit == "*" {
			_, phi := l.regs(t.X)
			l.checkFlow(lo, phi, pos, exprString(t))
			return
		}
		l.store(t.X, lo, hi, pos)
		return
	case *IndexExpr:
		l.regs(t.Index)
		switch t.X.info().T.Underlying().(type) {
		case *Array:
			l.store(t.X, lo, hi, pos)
		case *Map:
			_, mhi := l.regs(t.X)
			l.checkFlow(lo, mhi, pos, exprString(t.X))
		default:
			_, phi := l.regs(t.X)
			l.checkFlow(lo, phi, pos, exprString(t))
		}
		return
	case *StarExpr:
		_, phi := l.regs(t.X)
		l.checkFlow(lo, phi, pos, exprString(t))
		return
	}
}

func (l *lifetime) walkTarget(target Expr) {
	switch t := target.(type) {
	case *IndexExpr:
		l.regs(t.X)
		l.regs(t.Index)
		if m, ok := t.X.info().T.Underlying().(*Map); ok && HasRefs(m.Key) {
			_, mhi := l.regs(t.X)
			l.checkFlow(l.lo(t.Index), mhi, t.Index.Pos(), "map "+exprString(t.X))
		}
	case *SelectorExpr:
		l.walkTarget(t.X)
	case *StarExpr:
		l.regs(t.X)
	case *ParenExpr:
		l.walkTarget(t.X)
	}
}

func (l *lifetime) storeVar(v *Var, lo, hi Region, pos Pos) {
	if v.Global {
		l.checkFlow(lo, static, pos, "package variable "+v.Name)
		return
	}
	nc := join(v.Content, lo)
	if nc.D != v.Content.D || nc.M != v.Content.M {
		v.Content = nc
		l.change = true
	}
	if !v.hiSet {
		v.ContentHi = hi
		v.hiSet = true
		l.change = true
	} else {
		nh := meet(v.ContentHi, hi)
		if nh.D != v.ContentHi.D || nh.M != v.ContentHi.M {
			v.ContentHi = nh
			l.change = true
		}
	}
	lt := lifetimeOf(v)
	if lo.D > lt {
		l.errorf(pos, "%s does not live long enough to be stored in %s (declared at %s:%d)", describe(lo), v.Name, shortFile(v.Pos.File), v.Pos.Line)
	}
}

// ---- calls ----

type slotVal struct {
	lo, hi Region
	pos    Pos
}

func (l *lifetime) callSlots(e *CallExpr, sig *Signature) []slotVal {
	var slots []slotVal
	if se, ok := unparen(e.Fun).(*SelectorExpr); ok && se.Kind == SelMethod {
		var lo, hi Region
		switch se.Implicit {
		case "&":
			a := l.addr(se.X)
			lo, hi = a, a
		case "*":
			lo, hi = l.lo(se.X), static
		default:
			lo, hi = l.regs(se.X)
		}
		if !HasRefs(se.Method.Sig.Recv.Type) {
			lo, hi = static, static
		}
		slots = append(slots, slotVal{lo, hi, se.X.Pos()})
	} else {
		l.regs(e.Fun)
	}
	if e.Variadic >= 0 && !e.Ellipsis {
		for i := 0; i < e.Variadic && i < len(e.Args); i++ {
			lo, hi := l.regs(e.Args[i])
			slots = append(slots, slotVal{lo, hi, e.Args[i].Pos()})
		}
		c := l.cur()
		for _, a := range e.Args[min(e.Variadic, len(e.Args)):] {
			lo, _ := l.regs(a)
			l.checkFlow(lo, c, a.Pos(), "variadic argument list")
		}
		slots = append(slots, slotVal{c, c, e.P})
		return slots
	}
	for _, a := range e.Args {
		lo, hi := l.regs(a)
		slots = append(slots, slotVal{lo, hi, a.Pos()})
	}
	return slots
}

func (l *lifetime) summaries(e *CallExpr, sig *Signature) []*Summary {
	if e.Callee != nil {
		if e.Callee.Summary == nil {
			return []*Summary{{}}
		}
		return []*Summary{e.Callee.Summary}
	}
	var out []*Summary
	for _, f := range l.prog.Funcs {
		if f.AddrTaken && f.Summary != nil && f.Sig.Recv == nil && Identical(f.Sig, sig) {
			out = append(out, f.Summary)
		}
	}
	return out
}

func (l *lifetime) applySummary(sums []*Summary, slots []slotVal, what string) (Region, Region) {
	res := static
	for _, s := range sums {
		for i := range slots {
			if s.StaticReq&(1<<i) != 0 {
				l.checkFlow(slots[i].lo, static, slots[i].pos, "static storage via "+what)
			}
			if s.RetMask&(1<<i) != 0 {
				res = join(res, slots[i].lo)
			}
		}
		for _, f := range s.Flows {
			if f.Src < len(slots) && f.Dst < len(slots) {
				l.checkFlow(slots[f.Src].lo, slots[f.Dst].hi, slots[f.Src].pos, fmt.Sprintf("storage reachable from argument %d of %s", f.Dst+1, what))
			}
		}
	}
	return res, static
}

func (l *lifetime) call(e *CallExpr) (Region, Region) {
	if e.Conversion {
		lo, hi := l.regs(e.Args[0])
		if basicKind(e.T) == Rawptr || basicKind(e.T) == Uintptr {
			return static, static
		}
		return lo, hi
	}
	switch e.Builtin {
	case "":
	case "append":
		slo, shi := l.regs(e.Args[0])
		var et Type
		if st, ok := e.Args[0].info().T.Underlying().(*Slice); ok {
			et = st.Elem
		}
		for _, a := range e.Args[1:] {
			alo, _ := l.regs(a)
			if et != nil && HasRefs(et) {
				l.checkFlow(alo, shi, a.Pos(), exprString(e.Args[0]))
			}
		}
		return slo, shi
	case "copy":
		_, dhi := l.regs(e.Args[0])
		slo, _ := l.regs(e.Args[1])
		if st, ok := e.Args[0].info().T.Underlying().(*Slice); ok && HasRefs(st.Elem) {
			l.checkFlow(slo, dhi, e.Args[1].Pos(), exprString(e.Args[0]))
		}
		return static, static
	case "make":
		for _, a := range e.Args[1:] {
			l.regs(a)
		}
		if l.fn == nil {
			return static, static
		}
		c := Region{D: l.depth, Src: "make() storage", SrcPos: e.P}
		return c, c
	case "__error":
		l.regs(e.Args[0])
		lo, _ := l.regs(e.Args[1])
		l.checkFlow(lo, static, e.Args[1].Pos(), "an error value (error messages must be static strings)")
		return static, static
	case "min", "max":
		r := static
		for _, a := range e.Args {
			r = join(r, l.lo(a))
		}
		return r, static
	case "error.Error", "error.Code":
		if se, ok := unparen(e.Fun).(*SelectorExpr); ok {
			l.regs(se.X)
		}
		return static, static
	default:
		for _, a := range e.Args {
			if e.Builtin == "delete" {
				l.regs(a)
				continue
			}
			l.regs(a)
		}
		return static, static
	}
	sig, _ := e.Fun.info().T.Underlying().(*Signature)
	if sig == nil {
		return static, static
	}
	slots := l.callSlots(e, sig)
	return l.applySummary(l.summaries(e, sig), slots, exprString(e.Fun))
}

// ---- statements ----

func (l *lifetime) stmts(list []Stmt) {
	for _, s := range list {
		l.stmt(s)
	}
}

func (l *lifetime) block(list []Stmt) {
	l.depth++
	l.stmts(list)
	l.depth--
}

func (l *lifetime) stmt(s Stmt) {
	switch s := s.(type) {
	case *ExprStmt:
		l.regs(s.X)
	case *SendStmt:
		l.regs(s.Chan)
		lo, _ := l.regs(s.Value)
		l.checkFlow(lo, static, s.Value.Pos(), "a channel (values sent on channels are shared between goroutines)")
	case *IncDecStmt:
		l.walkTarget(s.X)
	case *AssignStmt:
		l.assign(s)
	case *GoStmt:
		l.goStmt(s)
	case *DeferStmt:
		l.deferStmt(s)
	case *ReturnStmt:
		l.returnStmt(s)
	case *BlockStmt:
		l.block(s.List)
	case *IfStmt:
		l.depth++
		if s.Init != nil {
			l.stmt(s.Init)
		}
		l.regs(s.Cond)
		l.stmt(s.Body)
		if s.Else != nil {
			l.stmt(s.Else)
		}
		l.depth--
	case *SwitchStmt:
		l.depth++
		if s.Init != nil {
			l.stmt(s.Init)
		}
		if s.Tag != nil {
			l.regs(s.Tag)
		}
		for _, cc := range s.Cases {
			for _, x := range cc.List {
				l.regs(x)
			}
			l.block(cc.Body)
		}
		l.depth--
	case *ForStmt:
		l.depth++
		if s.Init != nil {
			l.stmt(s.Init)
		}
		if s.Cond != nil {
			l.regs(s.Cond)
		}
		if s.Post != nil {
			l.stmt(s.Post)
		}
		l.stmt(s.Body)
		l.depth--
	case *RangeStmt:
		l.depth++
		var klo, khi, vlo, vhi Region
		switch u := s.X.info().T.Underlying().(type) {
		case *Array:
			vlo, vhi = l.regs(s.X)
		case *Pointer, *Slice:
			vlo = l.lo(s.X)
			vhi = static
		case *Map:
			_ = u
			klo = l.lo(s.X)
			vlo = klo
		default:
			l.regs(s.X)
		}
		if s.Key != nil {
			l.store(s.Key, klo, khi, s.P)
		}
		if s.Value != nil {
			l.store(s.Value, vlo, vhi, s.P)
		}
		l.stmt(s.Body)
		l.depth--
	case *DeclStmt:
		if s.Decl.Tok != VAR {
			return
		}
		for _, vs := range s.Decl.Values {
			if len(vs.Values) == len(vs.Names) {
				for i, x := range vs.Values {
					lo, hi := l.regs(x)
					l.store(vs.Names[i], lo, hi, x.Pos())
				}
			} else if len(vs.Values) == 1 {
				lo, hi := l.regs(vs.Values[0])
				for _, n := range vs.Names {
					l.store(n, lo, hi, vs.Values[0].Pos())
				}
			}
		}
	case *LabeledStmt:
		l.stmt(s.Stmt)
	}
}

func (l *lifetime) assign(s *AssignStmt) {
	if s.Tok != ASSIGN && s.Tok != DEFINE {
		l.walkTarget(s.Lhs[0])
		l.regs(s.Rhs[0])
		return
	}
	if len(s.Lhs) == len(s.Rhs) {
		type pair struct{ lo, hi Region }
		vals := make([]pair, len(s.Rhs))
		for i, x := range s.Rhs {
			vals[i].lo, vals[i].hi = l.regs(x)
		}
		for i, t := range s.Lhs {
			l.store(t, vals[i].lo, vals[i].hi, s.Rhs[i].Pos())
		}
		return
	}
	lo, hi := l.regs1(s.Rhs[0])
	for _, t := range s.Lhs {
		if t.info().T != nil && HasRefs(t.info().T) {
			l.store(t, lo, hi, s.Rhs[0].Pos())
		} else {
			l.store(t, static, static, s.Rhs[0].Pos())
		}
	}
}

func (l *lifetime) goStmt(s *GoStmt) {
	call := s.Call
	sig := call.Callee.Sig
	slots := l.callSlots(call, sig)
	for i, sl := range slots {
		what := fmt.Sprintf("goroutine argument %d", i+1)
		if sig.Recv != nil && i == 0 {
			what = "goroutine receiver"
		}
		if sl.lo.D != 0 {
			l.checkFlow(sl.lo, static, sl.pos, what+" (a goroutine can outlive the function that starts it)")
		}
	}
	l.applySummary(l.summaries(call, sig), slots, exprString(call.Fun))
}

func (l *lifetime) deferStmt(s *DeferStmt) {
	call := s.Call
	if call.Builtin != "" {
		for _, a := range call.Args {
			lo, _ := l.regs(a)
			if lo.D > 2 {
				l.errorf(a.Pos(), "%s does not live until the deferred call runs", describe(lo))
			}
		}
		return
	}
	sig, _ := call.Fun.info().T.Underlying().(*Signature)
	if sig == nil {
		return
	}
	slots := l.callSlots(call, sig)
	for _, sl := range slots {
		if sl.lo.D > 2 {
			l.errorf(sl.pos, "%s does not live until the deferred call runs at function exit", describe(sl.lo))
		}
	}
	l.applySummary(l.summaries(call, sig), slots, exprString(call.Fun))
}

func (l *lifetime) checkReturn(lo Region, pos Pos) {
	switch {
	case lo.D == 0:
	case lo.D == 1:
		l.sum.RetMask |= lo.M
	default:
		l.errorf(pos, "cannot return %s: it lives in this function's stack frame (Go would move it to the heap); have the caller pass in the storage instead", describe(lo)[2:])
	}
}

func (l *lifetime) returnStmt(s *ReturnStmt) {
	if l.fn == nil {
		return
	}
	if len(s.Results) == 0 {
		for _, r := range l.fn.Sig.Results {
			if r.Name != "" && HasRefs(r.Type) {
				lo, _ := l.varRegs(r, s.P)
				l.checkReturn(lo, s.P)
			}
		}
		return
	}
	for _, x := range s.Results {
		lo, _ := l.regs1(x)
		if HasRefs(x.info().T) {
			l.checkReturn(lo, x.Pos())
		}
	}
}
