package compiler

import (
	"fmt"
	"strings"
)

// Lifetime analysis: squark's replacement for Go's escape analysis.
//
// Where Go would move a value to the heap because a reference outlives its storage, squark
// reports a compile error. The analysis is region inference in the style of Rust's borrow
// checker (without ownership): every value that holds references gets a *region vector*, one
// region variable per pointer level (level 0 = the storage it points to directly, level 1 = the
// storage that storage points to, ...; struct fields share levels, deeper levels collapse into
// the last). Regions are related by "outlives" constraints:
//
//   - copying a value is covariant at level 0 (the copy may claim a shorter region) and
//     invariant below (anything stored through either copy is visible through the other);
//   - creating storage (a variable, an arena allocation, a literal's backing array) requires
//     whatever is stored in it to outlive it;
//   - a function's parameters and results are regions owned by the caller, which outlive every
//     local of the function.
//
// A program is rejected when the constraints force a shorter-lived region (a local variable,
// a temporary, an arena) to outlive a longer-lived one (a caller's region, static memory, an
// outer variable). Recursion is banned, so functions are analysed callees-first and each gets a
// Summary: the constraints its body implies between its parameters' and results' regions. Call
// sites instantiate the summary for their arguments. No lifetime annotations are needed.

// levels is the depth of region vectors; deeper pointer levels share the last region.
const levels = 4

type rkind uint8

const (
	rVar    rkind = iota // inference variable
	rConst               // a concrete lifetime: static, the caller, or a local object
	rParam               // a parameter level of the function being analysed
	rResult              // a result level of the function being analysed
)

type why struct {
	pos  Pos
	text string
	kind wkind
}

// wkind classifies a constraint for choosing where to report an error.
type wkind uint8

const (
	wStore wkind = iota // stored somewhere that may outlive it: a field, a global, a result, a channel
	wCopy               // copied into a local variable
	wPass               // passed as an argument
	wDecl               // a declaration's "contents outlive the storage" constraint
)

type redge struct {
	to  int
	why *why
}

type rnode struct {
	kind  rkind
	rank  int // constants: 0 static, 1 caller, >= 2 local block depth (deeper dies sooner)
	desc  string
	pos   Pos
	slot  int // rParam/rResult: index
	level int
	out   []redge
}

type vec []int

// IfacePos names a level of a parameter slot or result in a summary.
type IfacePos struct {
	Result bool
	Index  int
	Level  int
}

// SumEdge says: region From outlives region To (From ≥ To).
type SumEdge struct {
	From, To IfacePos
}

// Summary describes how a function relates its parameters' and results' regions.
type Summary struct {
	Edges     []SumEdge
	Static    []IfacePos // regions that must be static
	Constant  []IfacePos // regions that must be constant data (error messages, shared vars)
	ResStatic []IfacePos // result regions that may be mutable static memory (a package variable)
}

func (p IfacePos) String() string {
	if p.Result {
		return fmt.Sprintf("result %d level %d", p.Index, p.Level)
	}
	return fmt.Sprintf("arg %d level %d", p.Index, p.Level)
}

type lifetime struct {
	prog *Program
	errs ErrorList
	seen map[string]bool

	fn      *Func
	nodes   []rnode
	static  int // static memory: package variables, which can change
	rodata  int // constant data: literals, constants, error messages, shared vars, C externs
	caller  int
	body    int // the function body's lifetime (deferred calls run at its end)
	vars    map[*Var]vec
	lives   map[*Var]int
	depth   int
	params  []vec
	results []vec
}

// AnalyzeLifetimes runs region inference over all functions, callees first.
func AnalyzeLifetimes(prog *Program, order []*Func) error {
	l := &lifetime{prog: prog, seen: map[string]bool{}}
	for _, fn := range order {
		if fn == prog.InitFunc {
			continue
		}
		if fn.Decl == nil || fn.Decl.Body == nil {
			fn.Summary = contractSummary(fn.Sig, len(fn.Sig.Results))
			if fn.Summary == nil {
				fn.Summary = &Summary{}
			}
			continue
		}
		l.function(fn)
		if fn.Sig.HasContract {
			// declared provenance names which parameters and results are related; the body
			// supplies the exact pointer levels, so callers keep using the inferred summary
			l.checkContract(fn, fn.Summary)
		}
	}
	l.globals()
	if len(l.errs) > 0 {
		return l.errs
	}
	return nil
}

func (l *lifetime) errorf(pos Pos, format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	key := pos.String() + msg
	if l.seen[key] {
		return
	}
	l.seen[key] = true
	l.errs = append(l.errs, &Error{pos, msg})
}

// contractSummary turns declared provenance into a summary (nil when nothing is declared).
func contractSummary(sig *Signature, nresults int) *Summary {
	if sig == nil || !sig.HasContract {
		return nil
	}
	s := &Summary{}
	slot := func(i, k int) IfacePos { return IfacePos{Index: i, Level: k} }
	res := func(i, k int) IfacePos { return IfacePos{Result: true, Index: i, Level: k} }
	for _, p := range sig.From {
		for r := 0; r < nresults; r++ {
			for k := 0; k < levels; k++ {
				s.Edges = append(s.Edges, SumEdge{slot(p, k), res(r, k)})
				if k > 0 {
					s.Edges = append(s.Edges, SumEdge{res(r, k), slot(p, k)})
				}
			}
		}
	}
	for _, in := range sig.Into {
		src, dst := in[0], in[1]
		for k := 0; k+1 < levels; k++ {
			s.Edges = append(s.Edges, SumEdge{slot(src, k), slot(dst, k+1)})
			if k > 0 {
				s.Edges = append(s.Edges, SumEdge{slot(dst, k+1), slot(src, k)})
			}
		}
		// deeper levels collapse into the last one
		s.Edges = append(s.Edges, SumEdge{slot(src, levels-1), slot(dst, levels-1)}, SumEdge{slot(dst, levels-1), slot(src, levels-1)})
	}
	for _, p := range sig.StaticSlots {
		for k := 0; k < levels; k++ {
			s.Static = append(s.Static, slot(p, k))
		}
	}
	return s
}

// checkContract verifies that every flow the body implies is between parameters (and results)
// the declared provenance relates.
func (l *lifetime) checkContract(fn *Func, inferred *Summary) {
	sig := fn.Sig
	fromOK := map[int]bool{}
	for _, p := range sig.From {
		fromOK[p] = true
	}
	intoOK := map[[2]int]bool{}
	for _, in := range sig.Into {
		intoOK[in] = true
	}
	staticOK := map[int]bool{}
	for _, p := range sig.StaticSlots {
		staticOK[p] = true
	}
	slots := slotVars(fn)
	name := func(i int) string {
		if i < len(slots) {
			return slots[i].Name
		}
		return fmt.Sprint(i)
	}
	reported := map[string]bool{}
	complain := func(msg string) {
		if !reported[msg] {
			reported[msg] = true
			l.errorf(fn.Decl.P, "%s", msg)
		}
	}
	// declared relations, closed transitively: `s into out` and `from out` imply results may
	// reach s
	const result = -1
	rel := map[int][]int{}
	for p := range fromOK {
		rel[p] = append(rel[p], result)
	}
	for in := range intoOK {
		rel[in[0]] = append(rel[in[0]], in[1])
	}
	reaches := func(a, b int) bool {
		seen := map[int]bool{a: true}
		queue := []int{a}
		for len(queue) > 0 {
			x := queue[0]
			queue = queue[1:]
			if x == b {
				return true
			}
			for _, y := range rel[x] {
				if !seen[y] {
					seen[y] = true
					queue = append(queue, y)
				}
			}
		}
		return false
	}
	// Stores between parameters first, then results. Each complaint assumes its fix, so flows that
	// the suggested `into`/`from` would already cover are not reported again (a.next = b; return a
	// is one missing `b into a`, not three problems).
	for _, e := range inferred.Edges {
		if e.From.Result || e.To.Result {
			continue
		}
		src, dst := e.From.Index, e.To.Index
		if src == dst || reaches(src, dst) || reaches(dst, src) || staticOK[src] {
			continue
		}
		// a flow between two parameters: the one stored is the one at the shallower level
		if e.From.Level > e.To.Level {
			src, dst = dst, src
		}
		complain(fmt.Sprintf("%s stores %s inside %s, but its signature does not say so: declare the parameter as `%s T in %s`", fn.FullName(), name(src), name(dst), name(src), name(dst)))
		rel[src] = append(rel[src], dst)
	}
	for _, e := range inferred.Edges {
		if e.From.Result == e.To.Result {
			continue
		}
		p := e.From.Index
		if e.From.Result {
			p = e.To.Index
		}
		if !reaches(p, result) && !staticOK[p] {
			complain(fmt.Sprintf("%s returns a reference into %s, but its signature does not say so: add `in %s` after the results", fn.FullName(), name(p), name(p)))
			rel[p] = append(rel[p], result)
		}
	}
	for _, p := range inferred.Static {
		if !staticOK[p.Index] && !p.Result {
			complain(fmt.Sprintf("%s keeps %s in static memory, but its signature does not say so: declare the parameter as `%s T in static`", fn.FullName(), name(p.Index), name(p.Index)))
		}
	}
	if sig.ResultStatic {
		for _, e := range inferred.Edges {
			if e.To.Result && !e.From.Result {
				complain(fmt.Sprintf("%s is declared to return static memory, but it returns a reference into %s", fn.FullName(), name(e.From.Index)))
			}
		}
	}
}

// ---- graph construction ----

func (l *lifetime) reset(fn *Func) {
	l.fn = fn
	l.nodes = l.nodes[:0]
	l.vars = map[*Var]vec{}
	l.lives = map[*Var]int{}
	l.params, l.results = nil, nil
	l.static = l.constant(0, "package-variable memory", Pos{})
	l.rodata = l.constant(-1, "constant data", Pos{})
	l.caller = l.constant(1, "the caller", Pos{})
	l.body = l.constant(2, "the function body", Pos{})
}

func (l *lifetime) node(k rkind) int {
	l.nodes = append(l.nodes, rnode{kind: k})
	return len(l.nodes) - 1
}

func (l *lifetime) constant(rank int, desc string, pos Pos) int {
	n := l.node(rConst)
	l.nodes[n].rank = rank
	l.nodes[n].desc = desc
	l.nodes[n].pos = pos
	return n
}

func (l *lifetime) fresh() vec {
	v := make(vec, levels)
	for i := range v {
		v[i] = l.node(rVar)
	}
	return v
}

func (l *lifetime) staticVec() vec {
	v := make(vec, levels)
	for i := range v {
		v[i] = l.static
	}
	return v
}

// rodataVec is a reference to constant data. Only level 0 is constant: constant data holds
// references nowhere deeper (strings, errors and arrays or structs of them), and deeper levels are
// invariant, so making them constant would wrongly forbid storing it in a package variable.
func (l *lifetime) rodataVec() vec {
	v := l.staticVec()
	v[0] = l.rodata
	return v
}

// requireConstant makes v's references constant data (error messages, shared vars).
func (l *lifetime) requireConstant(v vec, w *why) {
	l.outlives(v[0], l.rodata, w)
	for _, n := range v[1:] {
		l.outlives(n, l.static, w)
	}
}

// globalVec is the region vector of a package variable's value: constant data for variables that
// can't change after initialization, static memory otherwise.
func (l *lifetime) globalVec(v *Var) vec {
	if v.Shared || v.ConstData || v.ReadOnly && !isArena(v.Type) {
		return l.rodataVec()
	}
	return l.staticVec()
}

func isArena(t Type) bool {
	if t == nil {
		return false
	}
	_, ok := t.Underlying().(*ArenaType)
	return ok
}

// outlives records a ≥ b.
func (l *lifetime) outlives(a, b int, w *why) {
	if a == b {
		return
	}
	l.nodes[a].out = append(l.nodes[a].out, redge{b, w})
}

func (l *lifetime) equal(a, b int, w *why) {
	l.outlives(a, b, w)
	l.outlives(b, a, w)
}

// assign records that src is copied into storage typed by dst.
func (l *lifetime) assign(dst, src vec, w *why) {
	if len(dst) == 0 || len(src) == 0 {
		return
	}
	l.outlives(src[0], dst[0], w)
	for k := 1; k < len(dst) && k < len(src); k++ {
		l.equal(src[k], dst[k], w)
	}
}

// requireAll makes every level of v outlive region r.
func (l *lifetime) requireAll(v vec, r int, w *why) {
	for _, n := range v {
		l.outlives(n, r, w)
	}
}

func shift(v vec) vec {
	if v == nil {
		return nil
	}
	s := make(vec, levels)
	copy(s, v[1:])
	s[levels-1] = v[levels-1]
	return s
}

// prepend builds the vector of a reference to storage r holding a value of vector v.
func (l *lifetime) prepend(r int, v vec) vec {
	out := make(vec, levels)
	out[0] = r
	if v == nil {
		for i := 1; i < levels; i++ {
			out[i] = l.node(rVar)
		}
		return out
	}
	// the last level stands for every deeper one, so v's last two levels collapse into it
	l.equal(v[levels-2], v[levels-1], nil)
	copy(out[1:], v[:levels-1])
	return out
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

// life returns the constant for the storage of a variable.
func (l *lifetime) life(v *Var) int {
	if v.Global {
		return l.static
	}
	if n, ok := l.lives[v]; ok {
		return n
	}
	desc := "variable " + v.Name
	if v.Type != nil {
		if _, ok := v.Type.Underlying().(*ArenaType); ok {
			desc = "arena " + v.Name
		}
	}
	n := l.constant(lifetimeOf(v), desc, v.Pos)
	l.lives[v] = n
	return n
}

// varVec returns the region vector of a variable, creating it (with the constraint that what it
// points to outlives it) on first use.
func (l *lifetime) varVec(v *Var) vec {
	if v.Global {
		return l.globalVec(v)
	}
	if x, ok := l.vars[v]; ok {
		return x
	}
	x := l.fresh()
	l.vars[v] = x
	l.outlives(x[0], l.life(v), &why{v.Pos, "stored in " + l.nodes[l.life(v)].desc, wDecl})
	return x
}

// tempVec is a reference to fresh storage living in the current block (or statically at package
// level): the storage outlives the reference, and its contents must outlive the storage.
func (l *lifetime) tempVec(desc string, pos Pos) vec {
	var r int
	if l.fn == nil {
		r = l.static
	} else {
		r = l.constant(l.depth, desc, pos)
	}
	v := l.fresh()
	l.outlives(r, v[0], nil)
	l.outlives(v[1], r, &why{pos, "stored in " + desc, wDecl})
	return v
}

// ---- functions ----

func slotVars(fn *Func) []*Var {
	ps := append([]*Var(nil), fn.Sig.Params...)
	if fn.Sig.Recv != nil {
		ps = append([]*Var{fn.Sig.Recv}, ps...)
	}
	return ps
}

func (l *lifetime) function(fn *Func) {
	l.reset(fn)
	for i, p := range slotVars(fn) {
		iv := make(vec, levels)
		for k := range iv {
			n := l.node(rParam)
			l.nodes[n].slot, l.nodes[n].level = i, k
			l.nodes[n].desc = "parameter " + p.Name
			l.nodes[n].pos = p.Pos
			l.outlives(n, l.caller, nil)
			iv[k] = n
		}
		l.params = append(l.params, iv)
		if HasRefs(p.Type) {
			l.assign(l.varVec(p), iv, &why{p.Pos, "parameter " + p.Name, wDecl})
		}
	}
	for i := range fn.Sig.Results {
		rv := make(vec, levels)
		for k := range rv {
			n := l.node(rResult)
			l.nodes[n].slot, l.nodes[n].level = i, k
			l.nodes[n].desc = "the function's result"
			l.outlives(n, l.caller, nil)
			rv[k] = n
		}
		l.results = append(l.results, rv)
		// with deferred calls, every return goes through the named results (as in Go), and a
		// deferred call may change them after the return statement
		if r := fn.Sig.Results[i]; r.Name != "" && len(fn.Defers) > 0 && HasRefs(r.Type) {
			l.assign(rv, l.varVec(r), &why{r.Pos, "returned (deferred calls can change it)", wStore})
		}
	}
	l.depth = 2
	l.stmts(fn.Decl.Body.List)
	l.check()
	fn.Summary = l.summarize()
}

func (l *lifetime) globals() {
	l.reset(nil)
	l.depth = 0
	for _, pkg := range l.prog.Packages {
		for _, vs := range pkg.GlobalInits {
			if len(vs.Values) != len(vs.Names) {
				continue
			}
			for i, x := range vs.Values {
				v := l.vecOf(x)
				if v == nil {
					continue
				}
				if o := vs.Names[i].Obj; o != nil && o.Var != nil && o.Var.Shared {
					// every thread reads it: it can't view memory that a thread may change
					l.requireConstant(v, &why{x.Pos(), "stored in shared var " + vs.Names[i].Name + ", which every thread reads", wStore})
				} else {
					l.requireAll(v, l.static, &why{x.Pos(), "stored in package variable " + vs.Names[i].Name, wStore})
				}
			}
		}
	}
	l.check()
}

// ---- solving ----

type pathStep struct {
	prev int
	why  *why
}

// check reports every constraint chain that makes a shorter-lived region outlive a longer one.
// Each use of a short-lived object (each constraint leaving it) is searched separately, so every
// offending statement gets its own error.
func (l *lifetime) check() {
	for u := range l.nodes {
		n := &l.nodes[u]
		if n.kind != rConst || n.rank < 2 && n.rank != 0 {
			continue
		}
		for _, start := range l.nodes[u].out {
			prev := map[int]pathStep{u: {-1, nil}, start.to: {u, start.why}}
			if t := &l.nodes[start.to]; t.kind == rConst {
				if t.rank < n.rank {
					l.report(u, start.to, prev)
				}
				continue
			}
			queue := []int{start.to}
			found := -1
			for len(queue) > 0 && found < 0 {
				x := queue[0]
				queue = queue[1:]
				for _, e := range l.nodes[x].out {
					if _, ok := prev[e.to]; ok {
						continue
					}
					prev[e.to] = pathStep{x, e.why}
					t := &l.nodes[e.to]
					if t.kind == rConst {
						if t.rank < n.rank {
							found = e.to
							break
						}
						continue // an equal or shorter lifetime: fine, and not a way through
					}
					queue = append(queue, e.to)
				}
			}
			if found >= 0 {
				l.report(u, found, prev)
			}
		}
	}
}

func (l *lifetime) report(u, v int, prev map[int]pathStep) {
	// walk back from v to u. The interesting step is the last one that is not just "x is stored
	// in variable y" (a declaration): the return, assignment, call or send that makes it escape.
	var at, copied, passed, any *why
	viaResult, viaParam := false, -1
	for x := v; x != u; x = prev[x].prev {
		st := prev[x]
		if w := st.why; w != nil && w.pos.File != "" {
			if any == nil {
				any = w
			}
			switch w.kind {
			case wStore:
				if at == nil {
					at = w
				}
			case wCopy:
				if copied == nil {
					copied = w
				}
			case wPass:
				if passed == nil {
					passed = w
				}
			}
		}
		switch l.nodes[x].kind {
		case rResult:
			viaResult = true
		case rParam:
			viaParam = x
		}
	}
	for _, w := range []*why{copied, passed, any} {
		if at == nil {
			at = w
		}
	}
	src := &l.nodes[u]
	what := "a reference to " + src.desc
	if src.pos.File != "" {
		what += fmt.Sprintf(" (declared at %s:%d)", shortFile(src.pos.File), src.pos.Line)
	}
	pos := src.pos
	how := ""
	if at != nil {
		pos = at.pos
		how = at.text
	}
	dst := &l.nodes[v]
	var msg string
	switch {
	case dst.rank < 0:
		msg = what + " cannot be used as constant data"
		if how != "" {
			msg += " (" + how + ")"
		}
		msg += ": package variables can change; use a string constant (or data built only from constants)"
	case dst.rank == 0:
		msg = what + " cannot escape into static memory"
		if how != "" {
			msg += " (" + how + ")"
		}
		msg += ": it would outlive its storage (Go would move it to the heap)"
	case dst.rank == 1 && viaResult && viaParam < 0:
		msg = fmt.Sprintf("cannot return %s: it lives in this function's stack frame (Go would move it to the heap); have the caller pass in the storage instead", strings.TrimPrefix(what, "a "))
	case dst.rank == 1:
		param := "a parameter"
		if viaParam >= 0 {
			param = l.nodes[viaParam].desc
		}
		msg = fmt.Sprintf("%s cannot be stored in memory owned by the caller (through %s)", what, param)
		if how != "" {
			msg += " (" + how + ")"
		}
		msg += ": it lives in this function's stack frame"
	default:
		msg = fmt.Sprintf("%s does not live long enough to be stored in %s", what, dst.desc)
		if dst.pos.File != "" {
			msg += fmt.Sprintf(" (declared at %s:%d)", shortFile(dst.pos.File), dst.pos.Line)
		}
		if how != "" && !strings.HasPrefix(how, "stored in") && !strings.HasPrefix(how, "assigned to") {
			msg += " (" + how + ")"
		}
	}
	l.errorf(pos, "%s", msg)
}

// summarize projects the constraint graph onto the function's parameters and results, keeping
// only pointer levels the types actually have.
func (l *lifetime) summarize() *Summary {
	s := &Summary{}
	slots := slotVars(l.fn)
	meaningful := func(p IfacePos) bool {
		var t Type
		switch {
		case p.Result && p.Index < len(l.fn.Sig.Results):
			t = l.fn.Sig.Results[p.Index].Type
		case !p.Result && p.Index < len(slots):
			t = slots[p.Index].Type
		}
		return p.Level < refLevels(t)
	}
	posOf := func(n int) IfacePos {
		x := &l.nodes[n]
		return IfacePos{Result: x.kind == rResult, Index: x.slot, Level: x.level}
	}
	seenEdge := map[SumEdge]bool{}
	for a := range l.nodes {
		k := l.nodes[a].kind
		if k != rParam && k != rResult {
			continue
		}
		visited := map[int]bool{a: true}
		queue := []int{a}
		static, constant := false, false
		for len(queue) > 0 {
			x := queue[0]
			queue = queue[1:]
			for _, e := range l.nodes[x].out {
				if visited[e.to] {
					continue
				}
				visited[e.to] = true
				t := &l.nodes[e.to]
				switch t.kind {
				case rConst:
					if e.to == l.static {
						static = true
					}
					if e.to == l.rodata {
						static, constant = true, true
					}
				case rParam, rResult:
					se := SumEdge{posOf(a), posOf(e.to)}
					if !meaningful(se.From) || !meaningful(se.To) {
						continue
					}
					if !seenEdge[se] && se.From != se.To {
						seenEdge[se] = true
						s.Edges = append(s.Edges, se)
					}
				default:
					queue = append(queue, e.to)
				}
			}
		}
		if static && meaningful(posOf(a)) {
			s.Static = append(s.Static, posOf(a))
		}
		if constant && meaningful(posOf(a)) {
			s.Constant = append(s.Constant, posOf(a))
		}
	}
	// results that may view a package variable: a caller must not use them as constant data
	visited := map[int]bool{l.static: true}
	queue := []int{l.static}
	for len(queue) > 0 {
		x := queue[0]
		queue = queue[1:]
		for _, e := range l.nodes[x].out {
			if visited[e.to] {
				continue
			}
			visited[e.to] = true
			switch t := &l.nodes[e.to]; t.kind {
			case rResult:
				if p := posOf(e.to); meaningful(p) {
					s.ResStatic = append(s.ResStatic, p)
				}
			case rVar:
				queue = append(queue, e.to)
			}
		}
	}
	return s
}

// ---- expressions ----

// vecOf returns the region vector of an expression's value (nil if it holds no references) and
// records the constraints its evaluation implies.
func (l *lifetime) vecOf(e Expr) vec {
	if e.info().View != nil {
		// storage used as a view: a reference to the storage, whose contents keep their regions
		return l.prepend(l.storage(e), l.vecOf1(e))
	}
	v := l.vecOf1(e)
	t := e.info().T
	if t == nil || e.info().Const != nil || !HasRefs(t) {
		return nil
	}
	if v == nil {
		v = l.fresh()
	}
	return v
}

func (l *lifetime) vecOf1(e Expr) vec {
	switch e := e.(type) {
	case *Ident:
		if e.Obj != nil && e.Obj.Kind == ObjVar {
			return l.varVec(e.Obj.Var)
		}
		if e.Obj != nil && e.Obj.Kind == ObjNil {
			// nil refers to nothing, so it constrains no level: as constant data, its deeper
			// levels would be static, and storing or returning it would make the destination's
			// deeper levels static too, since they are invariant
			return nil
		}
		return l.rodataVec()
	case *BasicLit:
		return l.rodataVec()
	case *ParenExpr:
		return l.vecOf1(e.X)
	case *SelectorExpr:
		switch e.Kind {
		case SelQualified:
			if o := e.Sel.Obj; o != nil && o.Kind == ObjVar {
				return l.globalVec(o.Var)
			}
			return l.rodataVec()
		case SelField:
			x := l.vecOf(e.X)
			if e.Implicit == "*" {
				return shift(x)
			}
			return x
		}
		l.vecOf(e.X)
		return nil
	case *IndexExpr:
		l.vecOf(e.Index)
		switch e.X.info().T.Underlying().(type) {
		case *Array:
			return l.vecOf(e.X)
		case *Pointer, *Slice, *Map:
			return shift(l.vecOf(e.X))
		}
		l.vecOf(e.X)
		return nil
	case *SliceExpr:
		for _, ix := range []Expr{e.Lo, e.Hi, e.Max} {
			if ix != nil {
				l.vecOf(ix)
			}
		}
		if _, ok := e.X.info().T.Underlying().(*Array); ok {
			return l.prepend(l.storage(e.X), l.vecOf(e.X))
		}
		return l.vecOf(e.X)
	case *StarExpr:
		return shift(l.vecOf(e.X))
	case *UnaryExpr:
		switch e.Op {
		case AND:
			if cl, ok := unparen(e.X).(*CompositeLit); ok {
				t := l.tempVec("a temporary in this block", cl.P)
				l.assign(shift(t), l.vecOf(cl), &why{cl.P, "stored in a temporary", wCopy})
				return t
			}
			return l.prepend(l.storage(e.X), l.vecOf(e.X))
		case ARROW:
			return shift(l.vecOf(e.X))
		}
		l.vecOf(e.X)
		return nil
	case *BinaryExpr:
		l.vecOf(e.X)
		l.vecOf(e.Y)
		if e.InArena != nil {
			if l.packageArena(e.InArena) {
				return l.rodataVec()
			}
			return l.alloc(e.InArena, e.P)
		}
		return nil
	case *CompositeLit:
		return l.compositeLit(e)
	case *KeyValueExpr:
		return l.vecOf(e.Value)
	case *CallExpr:
		rs := l.call(e)
		if len(rs) > 0 {
			return rs[0]
		}
		return nil
	case *TryExpr:
		rs := l.callResults(e.X)
		if len(rs) > 0 {
			return rs[0]
		}
		return nil
	case *InExpr:
		l.vecOf(e.Arena)
		return l.vecOf1(e.X)
	}
	return nil
}

// storage returns the region of the storage an addressable expression denotes.
func (l *lifetime) storage(e Expr) int {
	switch e := e.(type) {
	case *Ident:
		if e.Obj == nil || e.Obj.Var == nil {
			return l.static
		}
		return l.life(e.Obj.Var)
	case *ParenExpr:
		return l.storage(e.X)
	case *SelectorExpr:
		if e.Kind == SelQualified {
			return l.static
		}
		if e.Implicit == "*" {
			return l.vecOf(e.X)[0]
		}
		return l.storage(e.X)
	case *IndexExpr:
		l.vecOf(e.Index)
		if _, ok := e.X.info().T.Underlying().(*Array); ok {
			return l.storage(e.X)
		}
		return l.vecOf(e.X)[0]
	case *StarExpr:
		return l.vecOf(e.X)[0]
	case *CompositeLit:
		t := l.tempVec("a temporary in this block", e.P)
		l.assign(shift(t), l.vecOf(e), nil)
		return t[0]
	}
	l.vecOf1(e) // not vecOf: a temporary used as a view would ask for its storage again
	return l.constant(l.depth, "a temporary in this block", e.Pos())
}

func (l *lifetime) compositeLit(e *CompositeLit) vec {
	var v vec
	if HasRefs(e.T) {
		v = l.fresh()
	}
	_, slice := e.T.Underlying().(*Slice)
	var backing vec
	if slice {
		backing = l.tempVec("slice literal storage", e.P)
	}
	for _, el := range e.Elts {
		x := el
		if kv, ok := el.(*KeyValueExpr); ok {
			x = kv.Value
		}
		ev := l.vecOf(x)
		if ev == nil {
			continue
		}
		if slice {
			l.assign(shift(backing), ev, &why{x.Pos(), "stored in slice literal storage", wCopy})
		} else {
			l.assign(v, ev, nil)
		}
	}
	if slice {
		return backing
	}
	return v
}

// alloc returns the vector of a fresh object allocated in arena expression a.
// packageArena reports whether a is a package-level arena. A string copied into one is constant
// data: the arena is never freed, and nothing can write through a string.
func (l *lifetime) packageArena(a Expr) bool {
	l.vecOf(a)
	var o *Object
	switch a := unparen(a).(type) {
	case *Ident:
		o = a.Obj
	case *SelectorExpr:
		if a.Kind == SelQualified {
			o = a.Sel.Obj
		}
	}
	return o != nil && o.Kind == ObjVar && o.Var.Global && isArena(o.Var.Type)
}

func (l *lifetime) alloc(a Expr, pos Pos) vec {
	av := l.vecOf(a)
	r := l.fresh()
	if av == nil {
		return r
	}
	l.outlives(av[0], r[0], nil)
	// everything stored in arena memory shares the arena's contents region, which outlives it
	for k := 1; k < levels; k++ {
		l.equal(r[k], av[k], &why{pos, "stored in arena memory", wDecl})
	}
	return r
}

// ---- calls ----

func (l *lifetime) callResults(x Expr) []vec {
	for {
		switch e := unparen(x).(type) {
		case *CallExpr:
			return l.call(e)
		case *TryExpr: // the results without the error
			x = e.X
			continue
		case *InExpr: // the call carries the arena itself
			l.vecOf(e.Arena)
			x = e.X
			continue
		}
		return []vec{l.vecOf(x)}
	}
}

func (l *lifetime) argSlots(e *CallExpr, sig *Signature) []vec {
	var slots []vec
	if se, ok := unparen(e.Fun).(*SelectorExpr); ok && se.Kind == SelMethod {
		var rv vec
		switch se.Implicit {
		case "&":
			rv = l.prepend(l.storage(se.X), l.vecOf(se.X))
		case "*":
			rv = shift(l.vecOf(se.X))
		default:
			rv = l.vecOf(se.X)
		}
		if !HasRefs(se.Method.Sig.Recv.Type) {
			rv = nil
		}
		slots = append(slots, rv)
	} else {
		l.vecOf(e.Fun)
	}
	if e.Variadic >= 0 && !e.Ellipsis && sig.Variadic {
		for i := 0; i < e.Variadic && i < len(e.Args); i++ {
			slots = append(slots, l.vecOf(e.Args[i]))
		}
		elem := sig.Params[len(sig.Params)-1].Type.Underlying().(*Slice).Elem
		var t vec
		if HasRefs(&Slice{Elem: elem}) {
			t = l.tempVec("the variadic argument list", e.P)
		}
		for _, a := range e.Args[min(e.Variadic, len(e.Args)):] {
			if av := l.vecOf(a); av != nil && t != nil {
				l.assign(shift(t), av, &why{a.Pos(), "stored in the variadic argument list", wCopy})
			}
		}
		return append(slots, t)
	}
	for _, a := range e.Args {
		slots = append(slots, l.vecOf(a))
	}
	return slots
}

func (l *lifetime) summaries(e *CallExpr, sig *Signature) []*Summary {
	if e.Callee != nil {
		if e.Callee.Summary == nil {
			return nil
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

// describePos describes a summary position at a call: an argument, the receiver or a result.
func describePos(p IfacePos, hasRecv bool, nresults int) string {
	if !p.Result {
		return describeArg(p.Index, hasRecv)
	}
	if nresults == 1 {
		return "its result"
	}
	return fmt.Sprintf("result %d", p.Index+1)
}

func describeArg(slot int, hasRecv bool) string {
	if hasRecv {
		if slot == 0 {
			return "its receiver"
		}
		return fmt.Sprintf("argument %d", slot)
	}
	return fmt.Sprintf("argument %d", slot+1)
}

func describeSlot(name string, slot int, hasRecv bool) string {
	if hasRecv {
		if slot == 0 {
			return "the receiver of " + name
		}
		return fmt.Sprintf("argument %d of %s", slot, name)
	}
	return fmt.Sprintf("argument %d of %s", slot+1, name)
}

// applyCall instantiates the callee's summary for this call and returns the result vectors.
func (l *lifetime) applyCall(e *CallExpr, sig *Signature, slots []vec) []vec {
	name := exprString(e.Fun)
	hasRecv := e.Callee != nil && e.Callee.Sig.Recv != nil
	if e.Callee != nil && e.Callee.Extern != "" && !e.Callee.Sig.HasContract {
		// C functions without declared provenance are trusted: results are treated as constant
		// data (C string tables such as strerror's)
		var out []vec
		for range sig.Results {
			out = append(out, l.rodataVec())
		}
		return out
	}
	params := make([]vec, len(slots))
	for i, a := range slots {
		params[i] = l.fresh()
		if a != nil {
			l.assign(params[i], a, &why{e.P, "passed as " + describeSlot(name, i, hasRecv), wPass})
		}
	}
	results := make([]vec, len(sig.Results))
	for i := range results {
		results[i] = l.fresh()
	}
	at := func(p IfacePos) int {
		if p.Result {
			return results[p.Index][p.Level]
		}
		if p.Index < len(params) {
			return params[p.Index][p.Level]
		}
		return l.node(rVar)
	}
	for _, s := range l.summaries(e, sig) {
		for _, se := range s.Edges {
			var text string
			switch {
			case se.To.Result:
				text = "returned by " + name
			case se.From.Result:
				text = "used by " + name
			default:
				text = fmt.Sprintf("%s stores %s inside %s", name, describeArg(se.From.Index, hasRecv), describeArg(se.To.Index, hasRecv))
			}
			kind := wStore
			if se.To.Result || se.From.Result {
				kind = wPass
			}
			l.outlives(at(se.From), at(se.To), &why{e.P, text, kind})
		}
		for _, p := range s.Static {
			l.outlives(at(p), l.static, &why{e.P, name + " keeps " + describePos(p, hasRecv, len(sig.Results)) + " in static memory", wStore})
		}
		for _, p := range s.Constant {
			l.outlives(at(p), l.rodata, &why{e.P, name + " uses " + describePos(p, hasRecv, len(sig.Results)) + " as constant data", wStore})
		}
		for _, p := range s.ResStatic {
			l.outlives(l.static, at(p), &why{e.P, "returned by " + name + ", which may view a package variable", wPass})
		}
	}
	return results
}

func (l *lifetime) call(e *CallExpr) []vec {
	if e.Conversion {
		x := l.vecOf(e.Args[0])
		if e.InArena != nil {
			if isString(e.T) && l.packageArena(e.InArena) {
				return []vec{l.rodataVec()}
			}
			return []vec{l.alloc(e.InArena, e.P)}
		}
		if basicKind(e.T) == Rawptr || basicKind(e.T) == Uintptr || simdOf(e.T) != nil {
			return nil // a SIMD load copies the elements: the vector holds no reference
		}
		return []vec{x}
	}
	switch e.Builtin {
	case "":
	case "append":
		s := l.vecOf(e.Args[0])
		res := s
		if e.InArena != nil && s != nil {
			// the result is s's backing array or a new one in the arena
			a := l.alloc(e.InArena, e.P)
			res = l.fresh()
			l.outlives(s[0], res[0], nil)
			l.outlives(a[0], res[0], nil)
			for k := 1; k < levels; k++ {
				l.equal(res[k], s[k], nil)
				l.equal(res[k], a[k], nil)
			}
		}
		for i, a := range e.Args[1:] {
			av := l.vecOf(a)
			if av == nil || res == nil {
				continue
			}
			w := &why{a.Pos(), "appended to " + exprString(e.Args[0]), wStore}
			if e.Ellipsis && i == 0 {
				l.assign(shift(res), shift(av), w)
			} else {
				l.assign(shift(res), av, w)
			}
		}
		return []vec{res}
	case "copy":
		d := l.vecOf(e.Args[0])
		s := l.vecOf(e.Args[1])
		if d != nil && s != nil {
			l.assign(shift(d), shift(s), &why{e.Args[1].Pos(), "copied into " + exprString(e.Args[0]), wStore})
		}
		return nil
	case "new":
		return []vec{l.alloc(e.InArena, e.P)}
	case "make":
		for _, a := range e.Args[1:] {
			l.vecOf(a)
		}
		if e.InArena != nil {
			return []vec{l.alloc(e.InArena, e.P)}
		}
		return []vec{l.tempVec("make() storage", e.P)}
	case "__error":
		for _, a := range e.Args[:len(e.Args)-1] {
			l.vecOf(a)
		}
		msg := e.Args[len(e.Args)-1]
		if mv := l.vecOf(msg); mv != nil {
			l.requireConstant(mv, &why{msg.Pos(), "used as an error message, which must be constant", wStore})
		}
		return nil
	case "min", "max":
		var res vec
		if HasRefs(e.T) {
			res = l.fresh()
		}
		for _, a := range e.Args {
			if av := l.vecOf(a); av != nil && res != nil {
				l.assign(res, av, nil)
			}
		}
		return []vec{res}
	case "vec.push", "vec.try_push":
		se := unparen(e.Fun).(*SelectorExpr)
		dst := l.vecOf1(se.X)
		if v := l.vecOf(e.Args[0]); v != nil && dst != nil {
			l.assign(dst, v, &why{e.Args[0].Pos(), "pushed onto " + exprString(se.X), wStore})
		}
		return nil
	case "vec.append", "vec.try_append":
		se := unparen(e.Fun).(*SelectorExpr)
		dst := l.vecOf1(se.X)
		if s := l.vecOf(e.Args[0]); s != nil && dst != nil {
			l.assign(dst, shift(s), &why{e.Args[0].Pos(), "appended to " + exprString(se.X), wStore})
		}
		return nil
	case "vec.pop":
		se := unparen(e.Fun).(*SelectorExpr)
		return []vec{l.vecOf1(se.X)}
	case "vec.clear":
		l.vecOf1(unparen(e.Fun).(*SelectorExpr).X)
		return nil
	case "error.message", "error.code", "arena.used", "arena.left", "arena.cap":
		if se, ok := unparen(e.Fun).(*SelectorExpr); ok {
			l.vecOf(se.X)
		}
		return []vec{l.rodataVec()} // an error message is constant data
	default:
		for _, a := range e.Args {
			l.vecOf(a)
		}
		return nil
	}
	sig, _ := e.Fun.info().T.Underlying().(*Signature)
	if sig == nil {
		return nil
	}
	return l.applyCall(e, sig, l.argSlots(e, sig))
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

// store records `target = value`. Copies into local variables are told apart from stores into
// fields, elements and globals, which is where escapes usually happen.
func (l *lifetime) store(target Expr, value vec, w *why) {
	if isBlank(target) {
		return
	}
	if w != nil && w.kind == wStore {
		if id, ok := unparen(target).(*Ident); ok && id.Obj != nil && id.Obj.Var != nil && !id.Obj.Var.Global {
			w.kind = wCopy
		}
	}
	if m, ok := l.mapStore(target); ok {
		if value != nil && m != nil {
			l.assign(shift(m), value, w)
		}
		return
	}
	tv := l.vecOf(target)
	if value == nil || tv == nil {
		return
	}
	// Only the levels the target's type has: a struct's fields share levels, so equating a
	// field's levels below those its type has with the value's would tie them to its siblings'
	// for no reason (an int[] stored from a package arena made a sibling's contents static).
	n := min(refLevels(target.info().T), len(tv))
	l.assign(tv[:n:n], value[:min(n, len(value))], w)
}

// mapStore handles a write through `m[k]` (=, op= or ++/--), which may insert k: the key must then
// outlive the map's contents. It returns m's vec, and false if target is not a map index.
func (l *lifetime) mapStore(target Expr) (vec, bool) {
	ix, ok := unparen(target).(*IndexExpr)
	if !ok {
		return nil, false
	}
	if _, ok := ix.X.info().T.Underlying().(*Map); !ok {
		return nil, false
	}
	m := l.vecOf(ix.X)
	if kv := l.vecOf(ix.Index); kv != nil && m != nil {
		l.assign(shift(m), kv, &why{ix.Index.Pos(), "used as a key in " + exprString(ix.X), wStore})
	}
	return m, true
}

func (l *lifetime) stmt(s Stmt) {
	switch s := s.(type) {
	case *ExprStmt:
		l.vecOf(s.X)
	case *SendStmt:
		l.vecOf(s.Chan)
		if v := l.vecOf(s.Value); v != nil {
			l.requireAll(v, l.static, &why{s.Value.Pos(), "sent on a channel, which shares it with other goroutines", wStore})
		}
	case *IncDecStmt:
		if _, ok := l.mapStore(s.X); !ok {
			l.vecOf(s.X)
		}
	case *AssignStmt:
		l.assignStmt(s)
	case *GoStmt:
		l.goStmt(s)
		if s.Else != nil {
			l.stmt(s.Else)
		}
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
		l.vecOf(s.Cond)
		l.stmt(s.Body)
		if s.Else != nil {
			l.stmt(s.Else)
		}
		l.depth--
	case *SelectStmt:
		l.depth++
		for _, cc := range s.Cases {
			var list []Stmt
			if cc.Comm != nil {
				list = append(list, cc.Comm)
			}
			l.block(append(list, cc.Body...))
		}
		l.depth--
	case *SwitchStmt:
		l.depth++
		if s.Init != nil {
			l.stmt(s.Init)
		}
		if s.Tag != nil {
			l.vecOf(s.Tag)
		}
		for _, cc := range s.Cases {
			for _, x := range cc.List {
				l.vecOf(x)
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
			l.vecOf(s.Cond)
		}
		if s.Post != nil {
			l.stmt(s.Post)
		}
		l.stmt(s.Body)
		l.depth--
	case *RangeStmt:
		l.depth++
		if s.Lo != nil {
			l.vecOf(s.Lo)
		}
		x := l.vecOf(s.X)
		var kv, vv vec
		switch s.X.info().T.Underlying().(type) {
		case *Array:
			vv = x
		case *Pointer, *Slice:
			vv = shift(x)
		case *Map:
			kv, vv = shift(x), shift(x)
		case *Chan:
			kv = shift(x)
		}
		w := &why{s.P, "assigned to a loop variable", wCopy}
		if s.Key != nil {
			l.store(s.Key, kv, w)
		}
		if s.Value != nil {
			l.store(s.Value, vv, w)
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
					l.store(vs.Names[i], l.vecOf(x), &why{x.Pos(), "assigned to " + vs.Names[i].Name, wStore})
				}
			} else if len(vs.Values) == 1 {
				rs := l.callResults(vs.Values[0])
				for i, n := range vs.Names {
					if i < len(rs) {
						l.store(n, rs[i], &why{vs.Values[0].Pos(), "assigned to " + n.Name, wStore})
					}
				}
			}
		}
	case *LabeledStmt:
		l.stmt(s.Stmt)
	case *DeadlineStmt:
		l.vecOf(s.D)
		l.stmt(s.Body)
	case *ArenaStmt:
		l.depth++
		v := s.Name.Obj.Var
		// the arena variable points at the arena's storage, which lives exactly as long as this block
		av := l.fresh()
		arena := l.life(v)
		l.equal(av[0], arena, nil)
		l.outlives(av[1], arena, &why{s.P, "stored in arena " + v.Name, wDecl})
		l.vars[v] = av
		l.stmt(s.Body)
		l.depth--
	}
}

func (l *lifetime) assignStmt(s *AssignStmt) {
	if s.Tok != ASSIGN && s.Tok != DEFINE {
		// op-assign values are never references (string + needs an arena), but m[k] op= v inserts k
		if _, ok := l.mapStore(s.Lhs[0]); !ok {
			l.vecOf(s.Lhs[0])
		}
		l.vecOf(s.Rhs[0])
		return
	}
	if len(s.Lhs) == len(s.Rhs) {
		vals := make([]vec, len(s.Rhs))
		for i, x := range s.Rhs {
			vals[i] = l.vecOf(x)
		}
		for i, t := range s.Lhs {
			l.store(t, vals[i], &why{s.Rhs[i].Pos(), "assigned to " + exprString(t), wStore})
		}
		return
	}
	var rs []vec
	switch r := unparen(s.Rhs[0]).(type) {
	case *IndexExpr:
		if r.CommaOk {
			rs = []vec{l.vecOf(r), nil}
		}
	case *UnaryExpr:
		if r.CommaOk {
			rs = []vec{l.vecOf(r), nil}
		}
	case *InExpr:
		if r.CommaOk {
			rs = []vec{l.vecOf(r), nil}
		}
	case *TryExpr:
		rs = l.callResults(r.X)
	}
	if rs == nil {
		rs = l.callResults(s.Rhs[0])
	}
	for i, t := range s.Lhs {
		if i < len(rs) {
			l.store(t, rs[i], &why{s.Rhs[0].Pos(), "assigned to " + exprString(t), wStore})
		}
	}
}

func (l *lifetime) goStmt(s *GoStmt) {
	call := s.Call
	sig := call.Callee.Sig
	slots := l.argSlots(call, sig)
	for i, sl := range slots {
		if sl == nil {
			continue
		}
		what := describeSlot(exprString(call.Fun), i, sig.Recv != nil)
		l.requireAll(sl, l.static, &why{call.P, "passed as " + what + " to a new goroutine, which can outlive this function", wStore})
	}
	l.applyCall(call, sig, slots)
}

func (l *lifetime) deferStmt(s *DeferStmt) {
	call := s.Call
	w := &why{s.P, "passed to a deferred call, which runs when the function returns", wStore}
	if call.Builtin != "" {
		for _, a := range call.Args {
			if v := l.vecOf(a); v != nil {
				l.outlives(v[0], l.body, w)
			}
		}
		return
	}
	sig, _ := call.Fun.info().T.Underlying().(*Signature)
	if sig == nil {
		return
	}
	slots := l.argSlots(call, sig)
	for _, sl := range slots {
		if sl != nil {
			l.outlives(sl[0], l.body, w)
		}
	}
	l.applyCall(call, sig, slots)
}

func (l *lifetime) returnStmt(s *ReturnStmt) {
	if l.fn == nil {
		return
	}
	if len(s.Results) == 0 {
		for i, r := range l.fn.Sig.Results {
			if r.Name != "" && HasRefs(r.Type) {
				l.assign(l.results[i], l.varVec(r), &why{s.P, "returned", wStore})
			}
		}
		return
	}
	if len(s.Results) == 1 && len(l.results) > 1 {
		rs := l.callResults(s.Results[0])
		for i := range l.results {
			if i < len(rs) && rs[i] != nil {
				l.assign(l.results[i], rs[i], &why{s.P, "returned", wStore})
			}
		}
		return
	}
	for i, x := range s.Results {
		if v := l.vecOf(x); v != nil && i < len(l.results) {
			l.assign(l.results[i], v, &why{x.Pos(), "returned", wStore})
		}
	}
}

func shortFile(f string) string {
	for i := len(f) - 1; i >= 0; i-- {
		if f[i] == '/' {
			return f[i+1:]
		}
	}
	return f
}

// refLevels is how many pointer levels a type actually has (at most levels); region constraints
// below that are vacuous.
func refLevels(t Type) int {
	return refLevels1(t, map[Type]bool{})
}

func refLevels1(t Type, seen map[Type]bool) int {
	if t == nil {
		return 0
	}
	if seen[t] {
		return levels
	}
	seen[t] = true
	defer delete(seen, t)
	deeper := func(x Type) int { return min(levels, 1+refLevels1(x, seen)) }
	switch u := t.Underlying().(type) {
	case *Basic:
		if u.Kind == String || u.Kind == UntypedString {
			return 1
		}
		return 0
	case *Pointer:
		return deeper(u.Elem)
	case *Slice:
		return deeper(u.Elem)
	case *Chan:
		return deeper(u.Elem)
	case *Map:
		return max(deeper(u.Key), deeper(u.Value))
	case *ArenaType:
		return levels
	case *Array:
		return refLevels1(u.Elem, seen)
	case *VecType:
		return refLevels1(u.Elem, seen)
	case *ChanStore:
		return refLevels1(u.Elem, seen)
	case *MapStore:
		return max(refLevels1(u.Key, seen), refLevels1(u.Value, seen))
	case *Struct:
		n := 0
		for _, f := range u.Fields {
			n = max(n, refLevels1(f.Type, seen))
		}
		return n
	}
	return 0
}
