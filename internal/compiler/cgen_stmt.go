package compiler

import (
	"fmt"
	"strings"
)

func (g *gen) stmts(list []Stmt) {
	for _, s := range list {
		g.stmt(s)
	}
}

// stmt emits one statement, placing storage hoisted by its expressions just before it.
func (g *gen) stmt(s Stmt) {
	prevH := g.hoisted
	prevOut := g.out
	g.hoisted = nil
	var buf strings.Builder
	g.out = &buf
	g.stmt1(s)
	g.out = prevOut
	for _, h := range g.hoisted {
		g.line("%s", h)
	}
	g.out.WriteString(buf.String())
	g.hoisted = prevH
}

func (g *gen) label(s Stmt) int {
	if id, ok := g.labelIDs[s]; ok {
		return id
	}
	g.nextLbl++
	g.labelIDs[s] = g.nextLbl
	return g.nextLbl
}

func (g *gen) open() {
	g.line("{")
	g.ind++
}

func (g *gen) close() {
	g.ind--
	g.line("}")
}

func (g *gen) stmt1(s Stmt) {
	switch s := s.(type) {
	case *EmptyStmt:
	case *ExprStmt:
		g.lineDirective(s.Pos())
		g.line("(void)%s;", g.expr(s.X))
	case *SendStmt:
		g.lineDirective(s.P)
		et := s.Chan.info().T.Underlying().(*Chan).Elem
		v := g.tmp()
		g.line("{ %s %s = %s; sq_chan_send(%s, &%s, __FILE__, __LINE__); }", g.ctype(et), v, g.coerce(s.Value, et), g.expr(s.Chan), v)
	case *IncDecStmt:
		g.lineDirective(s.P)
		op := ADD
		if s.Tok == DEC {
			op = SUB
		}
		g.opAssign(s.X, op, nil, "1")
	case *AssignStmt:
		g.lineDirective(s.P)
		g.assign(s)
	case *GoStmt:
		g.lineDirective(s.P)
		g.goStmt(s)
	case *DeferStmt:
		g.lineDirective(s.P)
		g.deferStmt(s)
	case *ReturnStmt:
		g.lineDirective(s.P)
		g.returnStmt(s)
	case *BranchStmt:
		id := g.label(s.Target)
		if s.Tok == BREAK {
			g.line("goto sq_brk%d;", id)
		} else {
			g.line("goto sq_cont%d;", id)
		}
	case *BlockStmt:
		g.open()
		g.stmts(s.List)
		g.close()
	case *IfStmt:
		g.ifStmt(s)
	case *SwitchStmt:
		g.switchStmt(s)
	case *SelectStmt:
		g.selectStmt(s)
	case *ForStmt:
		g.forStmt(s)
	case *RangeStmt:
		g.rangeStmt(s)
	case *DeclStmt:
		g.declStmt(s.Decl)
	case *LabeledStmt:
		g.stmt1(s.Stmt)
	case *ArenaStmt:
		v := s.Name.Obj.Var
		g.lineDirective(s.P)
		g.open()
		// storage is part of this frame, so the stack proof counts it; pages are touched only as used
		g.line("uint8_t %s_buf[%d] __attribute__((aligned(16)));", v.CName, v.ArenaSize)
		g.line("sq_arena %s_arena = {%s_buf, 0, %d, %s};", v.CName, v.CName, v.ArenaSize, cString(v.Name))
		g.line("sq_arena *%s = &%s_arena;", v.CName, v.CName)
		g.stmts(s.Body.List)
		g.close()
	}
}

func (g *gen) declStmt(d *GenDecl) {
	if d.Tok != VAR {
		return
	}
	for _, vs := range d.Values {
		g.lineDirective(vs.P)
		if len(vs.Values) == 0 {
			for _, n := range vs.Names {
				v := n.Obj.Var
				g.line("%s = %s;", g.localDecl(v.Type, v.CName), g.zero(v.Type))
			}
			continue
		}
		if len(vs.Values) == len(vs.Names) {
			vals := make([]string, len(vs.Values))
			for i, x := range vs.Values {
				vals[i] = g.coerce(x, vs.Names[i].Obj.Var.Type)
			}
			for i, n := range vs.Names {
				v := n.Obj.Var
				g.line("%s = %s;", g.localDecl(v.Type, v.CName), vals[i])
			}
			continue
		}
		var lhs []Expr
		for _, n := range vs.Names {
			v := n.Obj.Var
			g.line("%s = %s;", g.localDecl(v.Type, v.CName), g.zero(v.Type))
			lhs = append(lhs, n)
		}
		g.multiAssign(lhs, vs.Values[0])
	}
}

// store emits lhs = value (C expression already coerced to lhs type).
func (g *gen) store(lhs Expr, val string) {
	if isBlank(lhs) {
		g.line("(void)(%s);", val)
		return
	}
	if ix, ok := unparen(lhs).(*IndexExpr); ok {
		if m, ok := ix.X.info().T.Underlying().(*Map); ok {
			v := g.tmp()
			g.line("{ %s %s = %s; *%s = %s; }", g.ctype(m.Value), v, val, g.mapSlot(ix), v)
			return
		}
	}
	g.line("%s = %s;", g.expr(lhs), val)
}

func (g *gen) assign(s *AssignStmt) {
	switch s.Tok {
	case DEFINE, ASSIGN:
	default:
		op := map[Tok]Tok{ADD_ASSIGN: ADD, SUB_ASSIGN: SUB, MUL_ASSIGN: MUL, QUO_ASSIGN: QUO, REM_ASSIGN: REM,
			AND_ASSIGN: AND, OR_ASSIGN: OR, XOR_ASSIGN: XOR, SHL_ASSIGN: SHL, SHR_ASSIGN: SHR, AND_NOT_ASSIGN: AND_NOT}[s.Tok]
		g.opAssign(s.Lhs[0], op, s.Rhs[0], "")
		return
	}
	isNew := func(i int) bool { return s.Tok == DEFINE && s.NewVars != nil && s.NewVars[i] }
	if len(s.Lhs) == len(s.Rhs) {
		if len(s.Lhs) == 1 {
			l := s.Lhs[0]
			if isNew(0) {
				v := l.(*Ident).Obj.Var
				g.line("%s = %s;", g.localDecl(v.Type, v.CName), g.coerce(s.Rhs[0], v.Type))
				return
			}
			if isBlank(l) {
				g.line("(void)(%s);", g.expr(s.Rhs[0]))
				return
			}
			g.store(l, g.coerce(s.Rhs[0], l.info().T))
			return
		}
		// parallel assignment: evaluate everything first
		targets := g.lhsTargets(s.Lhs, isNew)
		tmps := make([]string, len(s.Rhs))
		for i, x := range s.Rhs {
			t := x.info().T
			if !isBlank(s.Lhs[i]) {
				t = s.Lhs[i].info().T
			}
			if isUntyped(t) {
				t = defaultType(t)
			}
			tmps[i] = g.tmp()
			g.line("%s %s = %s;", g.ctype(t), tmps[i], g.coerce(x, t))
		}
		for i, l := range s.Lhs {
			if isNew(i) {
				v := l.(*Ident).Obj.Var
				g.line("%s = %s;", g.localDecl(v.Type, v.CName), tmps[i])
				continue
			}
			if isBlank(l) {
				continue
			}
			g.storeTarget(l, targets[i], tmps[i])
		}
		return
	}
	for i, l := range s.Lhs {
		if isNew(i) {
			v := l.(*Ident).Obj.Var
			g.line("%s = %s;", g.localDecl(v.Type, v.CName), g.zero(v.Type))
		}
	}
	g.multiAssign(s.Lhs, s.Rhs[0])
}

// lhsTargets is the first phase of a multi-target assignment, as in Go: the index and pointer
// operands of the targets are evaluated before anything is stored, so i, x[i] = 2, 7 stores
// into x[old i]. For each target it returns a C lvalue, or "" for a variable or blank (and where
// skip says so), which g.store handles directly.
func (g *gen) lhsTargets(lhs []Expr, skip func(int) bool) []string {
	targets := make([]string, len(lhs))
	for i, l := range lhs {
		if (skip != nil && skip(i)) || isBlank(l) {
			continue
		}
		switch x := unparen(l).(type) {
		case *Ident:
			continue
		case *IndexExpr:
			if m, ok := x.X.info().T.Underlying().(*Map); ok {
				mp, k := g.tmp(), g.tmp()
				g.line("sq_map *%s = %s; %s %s = %s;", mp, g.expr(x.X), g.ctype(m.Key), k, g.coerce(x.Index, m.Key))
				targets[i] = fmt.Sprintf("(*(%s*)sq_map_put(%s, &%s, __FILE__, __LINE__))", g.ctype(m.Value), mp, k)
				continue
			}
		}
		p := g.tmp()
		g.line("%s *%s = &(%s);", g.ctype(l.info().T), p, g.expr(l))
		targets[i] = "(*" + p + ")"
	}
	return targets
}

// storeTarget stores val into l, through target if lhsTargets captured one.
func (g *gen) storeTarget(l Expr, target, val string) {
	if target == "" {
		g.store(l, val)
		return
	}
	g.line("%s = %s;", target, val)
}

// multiAssign handles a, b = f(), v, ok = m[k] and v, ok = <-ch (lhs already declared).
func (g *gen) multiAssign(lhs []Expr, rhs Expr) {
	targets := g.lhsTargets(lhs, nil)
	switch r := unparen(rhs).(type) {
	case *IndexExpr:
		if r.CommaOk {
			m := r.X.info().T.Underlying().(*Map)
			k, p := g.tmp(), g.tmp()
			g.line("{")
			g.ind++
			g.line("%s %s = %s;", g.ctype(m.Key), k, g.coerce(r.Index, m.Key))
			g.line("%s *%s = sq_map_get(%s, &%s);", g.ctype(m.Value), p, g.expr(r.X), k)
			g.storeTarget(lhs[0], targets[0], fmt.Sprintf("(%s ? *%s : %s)", p, p, g.zero(m.Value)))
			g.storeTarget(lhs[1], targets[1], fmt.Sprintf("(%s != NULL)", p))
			g.ind--
			g.line("}")
			return
		}
	case *UnaryExpr:
		if sr, ok := g.selRecv[r]; ok && r.CommaOk {
			g.storeTarget(lhs[0], targets[0], sr[0])
			g.storeTarget(lhs[1], targets[1], sr[1])
			return
		}
		if r.CommaOk {
			et := r.X.info().T.Underlying().(*Chan).Elem
			v, ok := g.tmp(), g.tmp()
			g.line("{")
			g.ind++
			g.line("%s %s; bool %s = sq_chan_recv(%s, &%s);", g.ctype(et), v, ok, g.expr(r.X), v)
			g.storeTarget(lhs[0], targets[0], v)
			g.storeTarget(lhs[1], targets[1], ok)
			g.ind--
			g.line("}")
			return
		}
	}
	tu := rhs.info().T.(*Tuple)
	t := g.tmp()
	g.line("{")
	g.ind++
	g.line("%s %s = %s;", g.ctype(tu), t, g.expr(rhs))
	for i, l := range lhs {
		if isBlank(l) {
			continue
		}
		val := fmt.Sprintf("%s.r%d", t, i)
		if g.ctype(l.info().T) != g.ctype(tu.Types[i]) {
			val = fmt.Sprintf("({ %s _v = %s; *(%s*)&_v; })", g.ctype(tu.Types[i]), val, g.ctype(l.info().T))
		}
		g.storeTarget(l, targets[i], val)
	}
	g.ind--
	g.line("}")
}

// opAssign emits lhs op= rhs, evaluating lhs once.
func (g *gen) opAssign(lhs Expr, op Tok, rhs Expr, rhsC string) {
	t := lhs.info().T
	val := func(cur string) string {
		if op == SHL || op == SHR {
			return g.shift(op, cur, rhs, t)
		}
		r := rhsC
		if rhs != nil {
			r = g.coerce(rhs, t)
		} else {
			r = fmt.Sprintf("((%s)%s)", g.ctype(t), rhsC)
		}
		return g.arith(op, cur, r, rhs, t)
	}
	if rhs == nil && (op == SHL || op == SHR) {
		panic("unreachable")
	}
	if ix, ok := unparen(lhs).(*IndexExpr); ok {
		if _, ok := ix.X.info().T.Underlying().(*Map); ok {
			p := g.tmp()
			if rhs != nil && rhs.info().Const == nil {
				// as in Go, the right side is evaluated before the entry is created:
				// m[k] += len(m) sees the map without k
				r, cur := g.tmp(), "(*"+p+")"
				var pre, v string
				if op == SHL || op == SHR {
					ct := rhs.info().T
					if isUntyped(ct) {
						ct = defaultType(ct)
					}
					pre = fmt.Sprintf("%s %s = %s;", g.ctype(ct), r, g.expr(rhs))
					v = g.shiftC(op, cur, r, !isUnsigned(ct), t)
				} else {
					pre = fmt.Sprintf("%s %s = %s;", g.ctype(t), r, g.coerce(rhs, t))
					v = g.arith(op, cur, r, rhs, t)
				}
				g.line("{ %s %s *%s = %s; *%s = %s; }", pre, g.ctype(t), p, g.mapSlot(ix), p, v)
				return
			}
			g.line("{ %s *%s = %s; *%s = %s; }", g.ctype(t), p, g.mapSlot(ix), p, val("(*"+p+")"))
			return
		}
	}
	if id, ok := unparen(lhs).(*Ident); ok {
		n := g.expr(id)
		g.line("%s = %s;", n, val(n))
		return
	}
	p := g.tmp()
	g.line("{ %s *%s = &%s; *%s = %s; }", g.ctype(t), p, g.expr(lhs), p, val("(*"+p+")"))
}

func (g *gen) goStmt(s *GoStmt) {
	call := s.Call
	f := call.Callee
	pool := g.poolFor(f)
	sig := f.Sig
	tramp := fmt.Sprintf("sq_gotramp%d", s.ID)
	pool.Trampolines = append(pool.Trampolines, tramp)
	// argument struct
	var fields, unpack []string
	var ptypes []Type
	if sig.Recv != nil {
		ptypes = append(ptypes, sig.Recv.Type)
	}
	for _, p := range sig.Params {
		ptypes = append(ptypes, p.Type)
	}
	for i, t := range ptypes {
		fields = append(fields, fmt.Sprintf("%s a%d;", g.ctype(t), i))
		unpack = append(unpack, fmt.Sprintf("a->a%d", i))
	}
	if len(fields) == 0 {
		fields = append(fields, "char none;")
	}
	fmt.Fprintf(&g.goSites, "struct sq_ga%d { %s };\n", s.ID, strings.Join(fields, " "))
	fmt.Fprintf(&g.goSites, "static %s;\n", g.proto(f))
	fmt.Fprintf(&g.goSites, "static void %s(void *p) { struct sq_ga%d *a = p; (void)a; %s(%s); }\n", tramp, s.ID, f.CName, strings.Join(unpack, ", "))
	args := g.callArgs(call, sig)
	ga := g.tmp()
	if len(args) == 0 {
		g.line("{ struct sq_ga%d %s = {0}; sq_go(&%s, %s, &%s, sizeof %s); }", s.ID, ga, pool.CName, tramp, ga, ga)
		return
	}
	g.line("{ struct sq_ga%d %s = { %s }; sq_go(&%s, %s, &%s, sizeof %s); }", s.ID, ga, strings.Join(args, ", "), pool.CName, tramp, ga, ga)
}

func (g *gen) deferStmt(s *DeferStmt) {
	call := s.Call
	// evaluate arguments now, call at function exit
	var args []string
	var types []Type
	if call.Builtin != "" {
		for _, a := range call.Args {
			t := a.info().T
			if isUntyped(t) {
				t = defaultType(t)
			}
			types = append(types, t)
			args = append(args, g.coerce(a, t))
		}
	} else {
		sig := call.Fun.info().T.Underlying().(*Signature)
		args = g.callArgs(call, sig)
		if call.Callee == nil {
			types = append(types, call.Fun.info().T)
			args = append([]string{g.expr(call.Fun)}, args...)
		} else if call.Callee.Sig.Recv != nil {
			types = append(types, call.Callee.Sig.Recv.Type)
		}
		for _, p := range sig.Params {
			types = append(types, p.Type)
		}
	}
	for i, a := range args {
		name := fmt.Sprintf("_d%d_a%d", s.ID, i)
		g.topDecls = append(g.topDecls, fmt.Sprintf("%s = %s;", g.decl(types[i], name), g.zero(types[i])))
		g.line("%s = %s;", name, a)
	}
	g.line("_d%d_on = true;", s.ID)
}

func (g *gen) deferCall(d *DeferStmt) string {
	call := d.Call
	n := 0
	if call.Builtin != "" {
		n = len(call.Args)
	} else {
		sig := call.Fun.info().T.Underlying().(*Signature)
		n = len(sig.Params)
		if call.Callee == nil || call.Callee.Sig.Recv != nil {
			n++
		}
	}
	var names []string
	for i := 0; i < n; i++ {
		names = append(names, fmt.Sprintf("_d%d_a%d", d.ID, i))
	}
	switch call.Builtin {
	case "close":
		return fmt.Sprintf("sq_chan_close(%s, __FILE__, __LINE__)", names[0])
	case "panic":
		t := call.Args[0].info().T
		if isUntyped(t) {
			t = defaultType(t)
		}
		return panicC(names[0], t)
	case "print", "println":
		var parts []string
		for i, a := range call.Args {
			if i > 0 && call.Builtin == "println" {
				parts = append(parts, "sq_print_byte(' ')")
			}
			t := a.info().T
			if isUntyped(t) {
				t = defaultType(t)
			}
			parts = append(parts, g.printArgC(names[i], t))
		}
		if call.Builtin == "println" {
			parts = append(parts, "sq_print_byte('\\n')")
		}
		return strings.Join(parts, "; ")
	}
	if call.Callee == nil {
		return fmt.Sprintf("(%s)(%s)", names[0], strings.Join(names[1:], ", "))
	}
	return fmt.Sprintf("%s(%s)", call.Callee.CName, strings.Join(names, ", "))
}

func (g *gen) printArgC(s string, t Type) string {
	switch u := t.Underlying().(type) {
	case *Basic:
		switch {
		case u.Kind == String:
			return "sq_print_str(" + s + ")"
		case u.Kind == Bool:
			return "sq_print_bool(" + s + ")"
		case u.Kind == Float32 || u.Kind == Float64:
			return "sq_print_float((double)(" + s + "))"
		case u.Kind == Rawptr:
			return "sq_print_ptr(" + s + ")"
		case isUnsigned(u):
			return "sq_print_uint((uint64_t)(" + s + "))"
		default:
			return "sq_print_int((int64_t)(" + s + "))"
		}
	case *ErrorType:
		return "sq_print_err(" + s + ")"
	case *Pointer:
		return "sq_print_ptr(" + s + ")"
	case *Slice:
		return fmt.Sprintf("sq_print_str((sq_string){(%s).p, (%s).len})", s, s)
	}
	return "sq_print_ptr((const void*)&" + s + ")"
}

func (g *gen) returnStmt(s *ReturnStmt) {
	f := g.fn
	sig := f.Sig
	hasDefers := len(f.Defers) > 0
	var val string
	switch {
	case len(sig.Results) == 0:
		g.runDefers()
		g.line("return;")
		return
	case len(s.Results) == 0:
		// named results
		if len(sig.Results) == 1 {
			val = sig.Results[0].CName
		} else {
			var parts []string
			for _, r := range sig.Results {
				parts = append(parts, r.CName)
			}
			val = fmt.Sprintf("(%s){%s}", g.resultC(sig), strings.Join(parts, ", "))
		}
	case len(s.Results) == 1 && len(sig.Results) == 1:
		val = g.coerce(s.Results[0], sig.Results[0].Type)
	case len(s.Results) == 1:
		// return f() with a tuple result
		val = g.expr(s.Results[0])
	default:
		var parts []string
		for i, x := range s.Results {
			parts = append(parts, g.coerce(x, sig.Results[i].Type))
		}
		val = fmt.Sprintf("((%s){%s})", g.resultC(sig), strings.Join(parts, ", "))
	}
	if !hasDefers {
		g.line("return %s;", val)
		return
	}
	if namedResults(sig) {
		// as in Go: the values go to the named results first, so deferred calls can change them
		g.line("{")
		g.ind++
		if len(s.Results) > 0 {
			r := g.tmp()
			g.line("%s %s = %s;", g.resultC(sig), r, val)
			g.line("%s", g.assignNamedResults(sig, r))
		}
		g.runDefers()
		g.line("return %s;", g.namedResultsValue(sig))
		g.ind--
		g.line("}")
		return
	}
	r := g.tmp()
	g.line("{")
	g.ind++
	g.line("%s %s = %s;", g.resultC(sig), r, val)
	g.runDefers()
	g.line("return %s;", r)
	g.ind--
	g.line("}")
}

func (g *gen) ifStmt(s *IfStmt) {
	wrap := s.Init != nil
	if wrap {
		g.open()
		g.stmt(s.Init)
	}
	g.lineDirective(s.P)
	cond := g.expr(s.Cond)
	g.line("if (%s) {", cond)
	g.ind++
	g.stmts(s.Body.List)
	g.ind--
	switch e := s.Else.(type) {
	case nil:
		g.line("}")
	case *BlockStmt:
		g.line("} else {")
		g.ind++
		g.stmts(e.List)
		g.ind--
		g.line("}")
	case *IfStmt:
		g.line("} else {")
		g.ind++
		g.stmt(e)
		g.ind--
		g.line("}")
	}
	if wrap {
		g.close()
	}
}

// selectStmt evaluates the channels and sent values once, in source order, lets sq_select pick a
// case, and runs it; a receive case's assignment reads the value sq_select delivered (selRecv).
func (g *gen) selectStmt(s *SelectStmt) {
	id := g.label(s)
	g.open()
	g.lineDirective(s.P)
	n := 0
	for _, cc := range s.Cases {
		if cc.Comm != nil && cc.Timeout == nil {
			n++
		}
	}
	cs, ws := g.tmp(), g.tmp()
	if n > 0 {
		g.line("sq_scase %s[%d]; sq_waiter %s[%d];", cs, n, ws, n)
	} else {
		g.line("sq_scase *%s = NULL; sq_waiter *%s = NULL;", cs, ws)
	}
	if g.selRecv == nil {
		g.selRecv = map[*UnaryExpr][2]string{}
	}
	timeout, block := "-1", true
	index := map[*CommClause]int{}
	i := 0
	for _, cc := range s.Cases {
		switch {
		case cc.Comm == nil:
			block = false
		case cc.Timeout != nil:
			timeout = g.tmp()
			g.line("int64_t %s = %s;", timeout, g.coerce(cc.Timeout, tInt))
			g.line("if (%s < 0) %s = 0;", timeout, timeout)
		case cc.Recv != nil:
			et := cc.Recv.T
			v := g.tmp()
			g.line("%s %s;", g.ctype(et), v)
			g.line("%s[%d] = (sq_scase){.c = %s, .elem = &%s};", cs, i, g.expr(cc.Recv.X), v)
			g.selRecv[cc.Recv] = [2]string{v, fmt.Sprintf("%s[%d].ok", cs, i)}
			index[cc] = i
			i++
		default:
			send := cc.Comm.(*SendStmt)
			et := send.Chan.info().T.Underlying().(*Chan).Elem
			ch, v := g.tmp(), g.tmp()
			g.line("sq_chan *%s = %s;", ch, g.expr(send.Chan))
			g.line("%s %s = %s;", g.ctype(et), v, g.coerce(send.Value, et))
			g.line("%s[%d] = (sq_scase){.c = %s, .elem = &%s, .send = true};", cs, i, ch, v)
			index[cc] = i
			i++
		}
	}
	sel := g.tmp()
	g.line("int64_t %s = sq_select(%s, %s, %d, %t, %s, __FILE__, __LINE__);", sel, cs, ws, n, block, timeout)
	first := true
	for _, cc := range s.Cases {
		var cond string
		switch {
		case cc.Comm == nil:
			cond = sel + " == -1"
		case cc.Timeout != nil:
			cond = fmt.Sprintf("%s == %d", sel, n)
		default:
			cond = fmt.Sprintf("%s == %d", sel, index[cc])
		}
		kw := "} else if"
		if first {
			kw, first = "if", false
		}
		g.line("%s (%s) {", kw, cond)
		g.ind++
		g.open()
		if cc.Recv != nil && cc.Timeout == nil {
			g.stmt(cc.Comm)
		}
		g.stmts(cc.Body)
		g.close()
		g.ind--
	}
	if !first {
		g.line("}")
	}
	g.line("sq_brk%d:;", id)
	g.close()
}

func (g *gen) switchStmt(s *SwitchStmt) {
	id := g.label(s)
	g.open()
	if s.Init != nil {
		g.stmt(s.Init)
	}
	g.lineDirective(s.P)
	var tag string
	var tagT Type
	if s.Tag != nil {
		tagT = s.Tag.info().T
		if isUntyped(tagT) {
			tagT = defaultType(tagT)
		}
		tag = g.tmp()
		g.line("%s %s = %s;", g.ctype(tagT), tag, g.coerce(s.Tag, tagT))
	}
	var def *CaseClause
	first := true
	for _, cc := range s.Cases {
		if cc.List == nil {
			def = cc
			continue
		}
		var conds []string
		for _, x := range cc.List {
			if s.Tag == nil {
				conds = append(conds, g.expr(x))
			} else {
				conds = append(conds, g.eqExpr(tagT, tag, g.coerce(x, tagT)))
			}
		}
		kw := "} else if"
		if first {
			kw = "if"
			first = false
		}
		g.line("%s (%s) {", kw, strings.Join(conds, " || "))
		g.ind++
		g.open()
		g.stmts(cc.Body)
		g.close()
		g.ind--
	}
	if def != nil {
		if first {
			g.line("{")
		} else {
			g.line("} else {")
		}
		g.ind++
		g.open()
		g.stmts(def.Body)
		g.close()
		g.ind--
		g.line("}")
	} else if !first {
		g.line("}")
	}
	g.line("sq_brk%d:;", id)
	g.close()
}

// preemptCheck ends a loop iteration in a function a preemptible task may run: if the watchdog
// asked this thread's goroutine to yield, a preemptible one does (sq_preempt).
func (g *gen) preemptCheck() {
	if !g.fnPreempt {
		return
	}
	g.usesPreempt = true
	g.line("if (__builtin_expect(atomic_load_explicit(sq_pf, memory_order_relaxed), 0)) sq_preempt();")
}

func (g *gen) forStmt(s *ForStmt) {
	id := g.label(s)
	g.open()
	if s.Init != nil {
		g.stmt(s.Init)
	}
	// The post statement runs at the end of each iteration, but its temporaries (p = &T{...}) are
	// read by the next condition, so they are declared outside the loop, where lifetime analysis
	// places them too.
	var post strings.Builder
	if s.Post != nil {
		prevH, prevOut := g.hoisted, g.out
		g.hoisted, g.out = nil, &post
		g.ind++
		g.stmt1(s.Post)
		g.ind--
		for _, h := range g.hoisted {
			prevOut.WriteString(strings.Repeat("\t", g.ind) + h + "\n")
		}
		g.hoisted, g.out = prevH, prevOut
	}
	g.lineDirective(s.P)
	g.line("for (;;) {")
	g.ind++
	if s.Cond != nil {
		g.line("if (!(%s)) break;", g.expr(s.Cond))
	}
	g.open()
	g.stmts(s.Body.List)
	g.close()
	g.line("sq_cont%d:;", id)
	g.preemptCheck()
	g.out.WriteString(post.String())
	g.ind--
	g.line("}")
	g.line("sq_brk%d:;", id)
	g.close()
}

func (g *gen) rangeVar(x Expr, define bool, val string, t Type) {
	if x == nil || isBlank(x) {
		return
	}
	if define {
		v := x.(*Ident).Obj.Var
		g.line("%s = %s;", g.localDecl(v.Type, v.CName), val)
		return
	}
	if g.ctype(x.info().T) != g.ctype(t) {
		val = fmt.Sprintf("((%s)(%s))", g.ctype(x.info().T), val)
	}
	g.store(x, val)
}

func (g *gen) rangeStmt(s *RangeStmt) {
	id := g.label(s)
	g.open()
	g.lineDirective(s.P)
	xt := s.X.info().T
	i := g.tmp()
	body := func() {
		g.open()
		g.stmts(s.Body.List)
		g.close()
		g.line("sq_cont%d:;", id)
		g.preemptCheck()
	}
	switch u := xt.Underlying().(type) {
	case *Basic:
		if isString(u) {
			str, w := g.tmp(), g.tmp()
			g.line("sq_string %s = %s;", str, g.coerce(s.X, tString))
			g.line("for (int64_t %s = 0, %s = 0; %s < %s.len; %s += %s) {", i, w, i, str, i, w)
			g.ind++
			r := g.tmp()
			g.line("int32_t %s = sq_decode_rune(%s, %s, &%s);", r, str, i, w)
			g.rangeVar(s.Key, s.Define, i, tInt)
			g.rangeVar(s.Value, s.Define, r, tInt32)
			body()
			g.ind--
			g.line("}")
		} else {
			n := g.tmp()
			ct := g.ctype(xt)
			g.line("%s %s = %s;", ct, n, g.expr(s.X))
			g.line("for (%s %s = 0; %s < %s; %s++) {", ct, i, i, n, i)
			g.ind++
			g.rangeVar(s.Key, s.Define, i, xt)
			body()
			g.ind--
			g.line("}")
		}
	case *Slice:
		sl := g.tmp()
		g.line("%s %s = %s;", g.ctype(xt), sl, g.expr(s.X))
		g.line("for (int64_t %s = 0; %s < %s.len; %s++) {", i, i, sl, i)
		g.ind++
		g.rangeVar(s.Key, s.Define, i, tInt)
		g.rangeVar(s.Value, s.Define, fmt.Sprintf("%s.p[%s]", sl, i), u.Elem)
		body()
		g.ind--
		g.line("}")
	case *Array, *Pointer:
		var a Type = u
		ptr := g.tmp()
		var src string
		if p, ok := u.(*Pointer); ok {
			a = p.Elem.Underlying()
			src = g.expr(s.X)
		} else if s.X.info().Mode == ModeVar {
			src = "&" + g.expr(s.X)
		} else {
			cp := g.tmp()
			g.line("%s %s = %s;", g.ctype(xt), cp, g.expr(s.X))
			src = "&" + cp
		}
		arr := a.(*Array)
		g.line("%s *%s = %s;", g.ctype(arr), ptr, src)
		g.line("for (int64_t %s = 0; %s < %d; %s++) {", i, i, arr.Len, i)
		g.ind++
		g.rangeVar(s.Key, s.Define, i, tInt)
		g.rangeVar(s.Value, s.Define, fmt.Sprintf("%s->a[%s]", ptr, i), arr.Elem)
		body()
		g.ind--
		g.line("}")
	case *Map:
		m := g.tmp()
		g.line("sq_map *%s = %s;", m, g.expr(s.X))
		g.line("for (int64_t %s = sq_map_next(%s, 0); %s >= 0; %s = sq_map_next(%s, %s + 1)) {", i, m, i, i, m, i)
		g.ind++
		g.rangeVar(s.Key, s.Define, fmt.Sprintf("*(%s*)sq_map_key(%s, %s)", g.ctype(u.Key), m, i), u.Key)
		g.rangeVar(s.Value, s.Define, fmt.Sprintf("*(%s*)sq_map_val(%s, %s)", g.ctype(u.Value), m, i), u.Value)
		body()
		g.ind--
		g.line("}")
	case *Chan:
		c, v := g.tmp(), g.tmp()
		g.line("sq_chan *%s = %s;", c, g.expr(s.X))
		g.line("for (;;) {")
		g.ind++
		g.line("%s %s; if (!sq_chan_recv(%s, &%s)) break;", g.ctype(u.Elem), v, c, v)
		g.rangeVar(s.Key, s.Define, v, u.Elem)
		body()
		g.ind--
		g.line("}")
	}
	g.line("sq_brk%d:;", id)
	g.close()
}

// namedResults reports whether sig's results are named, and so visible to deferred calls.
func namedResults(sig *Signature) bool {
	return len(sig.Results) > 0 && sig.Results[0].Name != ""
}

// assignNamedResults is C that copies the result value r into the named results.
func (g *gen) assignNamedResults(sig *Signature, r string) string {
	if len(sig.Results) == 1 {
		return fmt.Sprintf("%s = %s;", sig.Results[0].CName, r)
	}
	var b strings.Builder
	for i, res := range sig.Results {
		fmt.Fprintf(&b, "%s = %s.r%d; ", res.CName, r, i)
	}
	return strings.TrimSpace(b.String())
}

// namedResultsValue is the C value returning the named results as they are now.
func (g *gen) namedResultsValue(sig *Signature) string {
	if len(sig.Results) == 1 {
		return sig.Results[0].CName
	}
	var parts []string
	for _, r := range sig.Results {
		parts = append(parts, r.CName)
	}
	return fmt.Sprintf("(%s){%s}", g.resultC(sig), strings.Join(parts, ", "))
}
