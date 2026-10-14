package compiler

import (
	"fmt"
	"strings"
)

// Callees returns the functions fn may call directly or through function values.
func (p *Program) Callees(fn *Func) []*Func {
	out := append([]*Func(nil), fn.Calls...)
	for _, sig := range fn.Indirect {
		for _, f := range p.Funcs {
			if f.AddrTaken && f.Sig != nil && f.Sig.Recv == nil && Identical(f.Sig, sig) {
				out = append(out, f)
			}
		}
	}
	return out
}

// CallGraphOrder rejects recursion and returns functions ordered callees-first.
func CallGraphOrder(prog *Program) ([]*Func, error) {
	const (
		white = iota
		grey
		black
	)
	color := map[*Func]int{}
	var order []*Func
	var errs ErrorList
	var stack []*Func
	reported := map[*Func]bool{}
	var visit func(f *Func)
	visit = func(f *Func) {
		switch color[f] {
		case black:
			return
		case grey:
			// cycle: find it on the stack
			i := len(stack) - 1
			for i >= 0 && stack[i] != f {
				i--
			}
			cycle := append([]*Func(nil), stack[i:]...)
			if !reported[f] {
				for _, g := range cycle {
					reported[g] = true
				}
				var names []string
				for _, g := range cycle {
					names = append(names, g.FullName())
				}
				names = append(names, f.FullName())
				pos := Pos{}
				if f.Decl != nil {
					pos = f.Decl.P
				}
				msg := "recursion is not allowed: " + strings.Join(names, " → ")
				msg += "\n\tsquark sizes every goroutine stack at compile time, which needs an acyclic call graph; rewrite with a loop and an explicit fixed-size stack"
				errs = append(errs, &Error{pos, msg})
			}
			return
		}
		color[f] = grey
		stack = append(stack, f)
		for _, g := range prog.Callees(f) {
			visit(g)
		}
		stack = stack[:len(stack)-1]
		color[f] = black
		order = append(order, f)
	}
	for _, f := range prog.Funcs {
		visit(f)
	}
	visit(prog.InitFunc)
	if len(errs) > 0 {
		return nil, errs
	}
	return order, nil
}

// Compile runs all front-end passes.
func Compile(prog *Program) ([]*Func, error) {
	if err := Check(prog); err != nil {
		return nil, err
	}
	order, err := CallGraphOrder(prog)
	if err != nil {
		return nil, err
	}
	if err := AnalyzeLifetimes(prog, order); err != nil {
		return nil, err
	}
	return order, nil
}

var _ = fmt.Sprint
