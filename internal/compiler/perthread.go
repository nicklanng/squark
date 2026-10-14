package compiler

import "sort"

// PerThreadVar is a package variable that, with threads N, every scheduler thread has its own copy
// of, and that the program changes after start-up: its value can differ from thread to thread (a
// counter counts one thread's requests, a channel is one thread's queue). Listing them keeps that
// from going unnoticed.
type PerThreadVar struct {
	Var    *Var
	Reason string // e.g. "written in handle", "a channel: each thread has its own queue"
}

// PerThreadVars lists those variables in the program's own packages, except those declared with
// thread var (per thread on purpose). The standard library isn't
// included: it is written for threads (its per-thread state, like the HTTP server's handler table,
// is per thread on purpose).
func PerThreadVars(prog *Program) []PerThreadVar {
	found := map[*Var]string{}
	for _, p := range prog.Packages {
		if p.Std {
			continue
		}
		for _, fn := range p.Funcs {
			if fn.Decl == nil || fn.Decl.Body == nil {
				continue
			}
			w := &ptWalker{fn: fn, found: found}
			w.stmt(fn.Decl.Body)
		}
	}
	var out []PerThreadVar
	for v, why := range found {
		if v.Pkg != nil && !v.Pkg.Std {
			out = append(out, PerThreadVar{Var: v, Reason: why})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i].Var.Pos, out[j].Var.Pos
		if a.File != b.File {
			return a.File < b.File
		}
		return a.Line < b.Line
	})
	return out
}

type ptWalker struct {
	fn    *Func
	found map[*Var]string
}

// perThread reports whether v is a package variable with one copy per scheduler thread that
// wasn't declared so on purpose (thread var).
func perThread(v *Var) bool {
	return v != nil && v.Global && !v.Shared && !v.Thread && !v.ReadOnly
}

func (w *ptWalker) note(v *Var, why string) {
	if perThread(v) {
		if _, ok := w.found[v]; !ok {
			w.found[v] = why + " in " + w.fn.Name + storage(v.Type)
		}
	}
}

// referenced notes a package map, vector or channel that x names being passed on: whoever receives
// it can change it.
func (w *ptWalker) referenced(x Expr, how string) {
	if v := globalOf(x); v != nil && v.Type != nil && storage(v.Type) != "" {
		w.note(v, how)
	}
}

// globalOf returns the package variable x names, or nil.
func globalOf(x Expr) *Var {
	switch e := unparen(x).(type) {
	case *Ident:
		if e.Obj != nil && e.Obj.Var != nil && e.Obj.Var.Global {
			return e.Obj.Var
		}
	case *SelectorExpr:
		if e.Kind == SelQualified && e.Sel.Obj != nil && e.Sel.Obj.Var != nil && e.Sel.Obj.Var.Global {
			return e.Sel.Obj.Var
		}
	}
	return nil
}

// written records that x (a variable, or an element or field of one) is changed.
func (w *ptWalker) written(x Expr, how string) {
	for {
		if v := globalOf(x); v != nil {
			w.note(v, how)
			return
		}
		switch e := unparen(x).(type) {
		case *IndexExpr:
			x = e.X
		case *SelectorExpr:
			if e.Kind != SelField {
				return
			}
			x = e.X
		default:
			return
		}
	}
}

// storage explains what a per-thread copy of a map, vector or channel means.
func storage(t Type) string {
	if t == nil {
		return ""
	}
	switch t.Underlying().(type) {
	case *MapStore:
		return " (a map: each thread has its own entries)"
	case *ChanStore:
		return " (a channel: each thread has its own queue; a shared var channel connects threads)"
	case *VecType:
		return " (a vector: each thread has its own elements)"
	}
	return ""
}

func (w *ptWalker) stmts(list []Stmt) {
	for _, s := range list {
		w.stmt(s)
	}
}

func (w *ptWalker) stmt(s Stmt) {
	switch s := s.(type) {
	case nil:
	case *BlockStmt:
		if s != nil {
			w.stmts(s.List)
		}
	case *ExprStmt:
		w.expr(s.X)
	case *SendStmt:
		w.written(s.Chan, "sent on")
		w.expr(s.Chan)
		w.expr(s.Value)
	case *IncDecStmt:
		w.written(s.X, "changed")
		w.expr(s.X)
	case *AssignStmt:
		for _, l := range s.Lhs {
			w.written(l, "written")
			w.expr(l)
		}
		for _, r := range s.Rhs {
			w.referenced(r, "aliased")
			w.expr(r)
		}
	case *GoStmt:
		w.expr(s.Call)
	case *DeferStmt:
		w.expr(s.Call)
	case *ReturnStmt:
		for _, r := range s.Results {
			w.expr(r)
		}
	case *IfStmt:
		w.stmt(s.Init)
		w.expr(s.Cond)
		w.stmt(s.Body)
		w.stmt(s.Else)
	case *SelectStmt:
		for _, cc := range s.Cases {
			w.stmt(cc.Comm)
			w.stmts(cc.Body)
		}
	case *SwitchStmt:
		w.stmt(s.Init)
		w.expr(s.Tag)
		for _, cc := range s.Cases {
			for _, x := range cc.List {
				w.expr(x)
			}
			w.stmts(cc.Body)
		}
	case *ForStmt:
		w.stmt(s.Init)
		w.expr(s.Cond)
		w.stmt(s.Post)
		w.stmt(s.Body)
	case *RangeStmt:
		if !s.Define {
			w.written(s.Key, "written")
			w.written(s.Value, "written")
		}
		if t := s.X.info().T; t != nil {
			if _, ok := t.Underlying().(*ChanStore); ok {
				w.written(s.X, "received from")
			}
		}
		w.expr(s.X)
		w.stmt(s.Body)
	case *DeclStmt:
		for _, vs := range s.Decl.Values {
			for _, x := range vs.Values {
				w.expr(x)
			}
		}
	case *LabeledStmt:
		w.stmt(s.Stmt)
	case *ArenaStmt:
		w.stmt(s.Body)
	}
}

func (w *ptWalker) expr(x Expr) {
	switch e := x.(type) {
	case nil:
	case *SelectorExpr:
		if globalOf(e) != nil {
			return
		}
		if e.Kind == SelMethod && e.Implicit == "&" {
			w.written(e.X, "changed by method "+e.Sel.Name)
		}
		w.expr(e.X)
	case *ParenExpr:
		w.expr(e.X)
	case *IndexExpr:
		w.expr(e.X)
		w.expr(e.Index)
	case *SliceExpr:
		if _, ok := e.X.info().T.Underlying().(*Array); ok {
			w.written(e.X, "sliced (a writable view)")
		}
		w.expr(e.X)
		w.expr(e.Lo)
		w.expr(e.Hi)
		w.expr(e.Max)
	case *CallExpr:
		switch e.Builtin {
		case "delete", "close":
			if len(e.Args) > 0 {
				w.written(e.Args[0], "changed by "+e.Builtin)
			}
		case "copy":
			if len(e.Args) > 0 {
				w.written(e.Args[0], "copied into")
			}
		case "vec.Push", "vec.TryPush", "vec.Pop", "vec.Clear":
			if se, ok := unparen(e.Fun).(*SelectorExpr); ok {
				w.written(se.X, "changed by "+se.Sel.Name)
			}
		}
		w.expr(e.Fun)
		for _, a := range e.Args {
			if e.Builtin == "" {
				w.referenced(a, "passed to a function")
			}
			w.expr(a)
		}
	case *StarExpr:
		w.expr(e.X)
	case *UnaryExpr:
		switch e.Op {
		case AND:
			w.written(e.X, "address taken")
		case ARROW:
			w.written(e.X, "received from")
		}
		w.expr(e.X)
	case *BinaryExpr:
		w.expr(e.X)
		w.expr(e.Y)
		w.expr(e.InArena)
	case *CompositeLit:
		for _, el := range e.Elts {
			w.referenced(el, "stored in a value")
			w.expr(el)
		}
	case *KeyValueExpr:
		w.expr(e.Value)
	case *TryExpr:
		w.expr(e.X)
	case *InExpr:
		w.expr(e.X)
		w.expr(e.Arena)
	}
}
