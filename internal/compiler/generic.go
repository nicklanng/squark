package compiler

import (
	"fmt"
	"reflect"
	"strings"
)

// anytype parameters: a function with one is a template. Each call fixes the types of its anytype
// arguments, and the compiler makes (or reuses) an instance: a copy of the function with those
// types, checked, proven and compiled like any other function. Nothing happens at run time.

// AnyType is the type of an anytype parameter in a generic function's signature.
type AnyType struct{}

func (t *AnyType) Underlying() Type { return t }
func (t *AnyType) String() string   { return "anytype" }

var tAnyType = &AnyType{}

func hasAnyParam(sig *Signature) bool {
	for _, p := range sig.Params {
		if p.Type == tAnyType {
			return true
		}
	}
	return false
}

// instantiate returns the instance of e's generic callee for the types of its anytype arguments,
// and which arguments it has already checked.
func (c *Checker) instantiate(e *CallExpr) (*Func, []bool) {
	gen := e.Callee
	params := gen.Sig.Params
	nfixed := len(params)
	if gen.Sig.Variadic {
		nfixed-- // the variadic parameter has a concrete type; args checks the rest
	}
	if len(e.Args) < nfixed || !gen.Sig.Variadic && len(e.Args) != len(params) {
		c.errorf(e.P, "wrong argument count in call to %s: have %d, want %d", exprString(e.Fun), len(e.Args), len(params))
		return nil, nil
	}
	pre := make([]bool, len(e.Args))
	var targs []Type
	for i, p := range params[:nfixed] {
		if p.Type != tAnyType {
			continue
		}
		a := e.Args[i]
		pre[i] = true
		if !c.valueExpr(a, nil) {
			return nil, nil
		}
		c.defaultUntyped(a)
		if viewOf(a.info().T) != nil {
			c.toView(a)
		}
		t := a.info().T
		if t == tUntypedNil || t == tInvalid {
			c.errorf(a.Pos(), "cannot use %s as an anytype argument: its type is unknown", exprString(a))
			return nil, nil
		}
		targs = append(targs, t)
	}
	for _, in := range gen.Instances {
		same := true
		for k, t := range in.TypeArgs {
			if !Identical(t, targs[k]) {
				same = false
				break
			}
		}
		if same {
			return in, pre
		}
	}

	// A new instance requested while checking an instance of the same template (directly or
	// through others) is recursion whose argument types grow (f(&x) inside f): it would make
	// instances without end, so report it here rather than in the call graph.
	for in := c.inst; in != nil; in = in.InstFrom {
		if in.GenericOf == gen {
			c.errorf(e.P, "recursion is not allowed: %s calls itself (with %s arguments)\n\tsquark sizes every task stack at compile time, which needs an acyclic call graph; rewrite with a loop and an explicit fixed-size stack", gen.FullName(), typeList(targs))
			return nil, nil
		}
	}
	inst := *gen
	inst.InstFrom = c.inst
	inst.Generic = false
	inst.Instances = nil
	inst.GenericOf = gen
	inst.TypeArgs = targs
	inst.InstPos = e.P
	inst.Decl = cloneFuncDecl(gen.Decl)
	sig := *gen.Sig
	sig.Params, sig.Results = nil, nil
	k := 0
	for _, p := range gen.Sig.Params {
		v := *p
		v.Obj, v.CName = nil, ""
		if p.Type == tAnyType {
			v.Type = targs[k]
			k++
		}
		sig.Params = append(sig.Params, &v)
	}
	for _, r := range gen.Sig.Results {
		v := *r
		v.Obj, v.CName = nil, ""
		sig.Results = append(sig.Results, &v)
	}
	inst.Sig = &sig
	gen.Instances = append(gen.Instances, &inst)
	gen.Pkg.Funcs = append(gen.Pkg.Funcs, &inst)
	c.prog.Funcs = append(c.prog.Funcs, &inst)
	c.pending = append(c.pending, &inst)
	return &inst, pre
}

func typeList(ts []Type) string {
	var parts []string
	for _, t := range ts {
		parts = append(parts, t.String())
	}
	return strings.Join(parts, ", ")
}

// instanceNote describes the instance being checked, for error messages.
func (c *Checker) instanceNote() string {
	in := c.inst
	var parts []string
	k := 0
	for _, p := range in.GenericOf.Sig.Params {
		if p.Type == tAnyType {
			parts = append(parts, p.Name+" "+in.TypeArgs[k].String())
			k++
		}
	}
	return fmt.Sprintf(" (in %s with %s, called at %s:%d)", in.GenericOf.FullName(), strings.Join(parts, ", "), shortFile(in.InstPos.File), in.InstPos.Line)
}

// cloneFuncDecl copies a generic function's syntax tree for an instance: the checker annotates
// trees in place, and the template's body is never checked. Checker objects the tree may point to
// (types, objects, variables, functions, packages) are shared, not copied.
func cloneFuncDecl(d *FuncDecl) *FuncDecl {
	out := cloneValue(reflect.ValueOf(d), map[uintptr]reflect.Value{}).Interface().(*FuncDecl)
	out.Name = &Ident{Name: d.Name.Name, P: d.Name.P} // not the template's object
	return out
}

var (
	astPkg    = reflect.TypeOf(FuncDecl{}).PkgPath()
	typeIface = reflect.TypeOf((*Type)(nil)).Elem()
	shared    = map[reflect.Type]bool{
		reflect.TypeOf(Object{}): true, reflect.TypeOf(Var{}): true, reflect.TypeOf(Func{}): true,
		reflect.TypeOf(Package{}): true, reflect.TypeOf(Scope{}): true, reflect.TypeOf(File{}): true,
	}
)

func cloneValue(v reflect.Value, memo map[uintptr]reflect.Value) reflect.Value {
	switch v.Kind() {
	case reflect.Ptr:
		if v.IsNil() {
			return v
		}
		et := v.Type().Elem()
		if et.PkgPath() != astPkg || shared[et] || v.Type().Implements(typeIface) {
			return v
		}
		if m, ok := memo[v.Pointer()]; ok {
			return m
		}
		n := reflect.New(et)
		memo[v.Pointer()] = n
		n.Elem().Set(cloneValue(v.Elem(), memo))
		return n
	case reflect.Interface:
		if v.IsNil() {
			return v
		}
		inner := cloneValue(v.Elem(), memo)
		out := reflect.New(v.Type()).Elem()
		out.Set(inner)
		return out
	case reflect.Slice:
		if v.IsNil() {
			return v
		}
		out := reflect.MakeSlice(v.Type(), v.Len(), v.Len())
		for i := 0; i < v.Len(); i++ {
			out.Index(i).Set(cloneValue(v.Index(i), memo))
		}
		return out
	case reflect.Struct:
		out := reflect.New(v.Type()).Elem()
		out.Set(v) // unexported fields stay shared
		for i := 0; i < v.NumField(); i++ {
			if f := out.Field(i); f.CanSet() {
				f.Set(cloneValue(v.Field(i), memo))
			}
		}
		return out
	}
	return v
}
