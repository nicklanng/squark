package compiler

import (
	"fmt"
	"go/constant"
	"sort"
	"strconv"
	"strings"
)

// DocLine renders a function signature with its provenance (declared or inferred), e.g.
//
//	func Cut(s, sep string) (string, string, bool) from s, sep
func DocLine(fn *Func) string {
	d := fn.Decl
	var b strings.Builder
	switch d.Kind {
	case KindTask:
		b.WriteString("task ")
	case KindExtern:
		b.WriteString("extern func ")
	default:
		b.WriteString("func ")
	}
	slots := slotVars(fn)
	into := map[int][]string{}
	static := map[int]bool{}
	var from []string
	if s := fn.Summary; s != nil {
		seenFrom := map[int]bool{}
		for _, e := range s.Edges {
			switch {
			case !e.From.Result && e.To.Result && e.To.Level == 0:
				if !seenFrom[e.From.Index] && e.From.Index < len(slots) {
					seenFrom[e.From.Index] = true
					from = append(from, slots[e.From.Index].Name)
				}
			case !e.From.Result && !e.To.Result && e.To.Level == e.From.Level+1 && e.From.Index != e.To.Index && e.To.Index < len(slots):
				dst := slots[e.To.Index].Name
				dup := false
				for _, x := range into[e.From.Index] {
					dup = dup || x == dst
				}
				if !dup {
					into[e.From.Index] = append(into[e.From.Index], dst)
				}
			}
		}
		for _, p := range s.Static {
			if p.Level == 0 && !p.Result {
				static[p.Index] = true
			}
		}
	}
	base := 0
	if fn.Sig.Recv != nil {
		fmt.Fprintf(&b, "(%s %s) ", fn.Sig.Recv.Name, fn.Sig.Recv.Type)
		base = 1
	}
	b.WriteString(fn.Name + "(")
	for i, p := range fn.Sig.Params {
		if i > 0 {
			b.WriteString(", ")
		}
		slot := base + i
		b.WriteString(p.Name + " ")
		if static[slot] {
			b.WriteString("static ")
		}
		t := p.Type.String()
		if fn.Sig.Variadic && i == len(fn.Sig.Params)-1 {
			t = "..." + p.Type.(*Slice).Elem.String()
		}
		b.WriteString(t)
		if dst := into[slot]; len(dst) > 0 {
			sort.Strings(dst)
			b.WriteString(" into " + strings.Join(dst, ", "))
		}
	}
	b.WriteString(")")
	switch len(fn.Sig.Results) {
	case 0:
	case 1:
		b.WriteString(" " + fn.Sig.Results[0].Type.String())
	default:
		var rs []string
		for _, r := range fn.Sig.Results {
			rs = append(rs, r.Type.String())
		}
		b.WriteString(" (" + strings.Join(rs, ", ") + ")")
	}
	hasRefResult := false
	for _, r := range fn.Sig.Results {
		if HasRefs(r.Type) {
			hasRefResult = true
		}
	}
	if len(from) > 0 {
		b.WriteString(" from " + strings.Join(from, ", "))
	} else if hasRefResult {
		b.WriteString(" static")
	}
	if fn.Pool > 0 {
		fmt.Fprintf(&b, " pool %d", fn.Pool)
	}
	if fn.StackHint > 0 {
		fmt.Fprintf(&b, " stack %d", fn.StackHint)
	}
	if fn.Blocking {
		b.WriteString(" blocking")
	}
	if fn.Preemptible {
		b.WriteString(" preemptible")
	}
	return b.String()
}

// DocPackage renders a package's API: constants, variables, types and functions with provenance.
// Unexported names are included only when all is set.
func DocPackage(p *Package, all bool) string {
	show := func(name string) bool { return all || isExported(name) }
	type entry struct {
		pos  Pos
		text string
	}
	var consts, vars, types []entry
	for name, o := range p.Scope.names {
		if !show(name) {
			continue
		}
		switch o.Kind {
		case ObjConst:
			v := ""
			if cv, ok := o.Const.(constant.Value); ok && cv.Kind() == constant.Float {
				// String abbreviates (Pi = 3.14159); show the float64 value in full
				f, _ := constant.Float64Val(cv)
				v = strconv.FormatFloat(f, 'g', -1, 64)
			} else if cv, ok := o.Const.(interface{ String() string }); ok {
				v = cv.String()
			}
			t := ""
			if o.Type != nil && !isUntyped(o.Type) {
				t = " " + o.Type.String()
			}
			consts = append(consts, entry{o.Pos, fmt.Sprintf("const %s%s = %s", name, t, v)})
		case ObjVar:
			v := o.Var
			switch {
			case v.ArenaSize > 0:
				vars = append(vars, entry{o.Pos, fmt.Sprintf("arena %s %d", name, v.ArenaSize)})
			case v.ConstData:
				vars = append(vars, entry{o.Pos, fmt.Sprintf("const %s %s", name, v.Type)})
			case v.ReadOnly:
				vars = append(vars, entry{o.Pos, fmt.Sprintf("const %s = %s", name, errorInit(p, v))})
			default:
				vars = append(vars, entry{o.Pos, fmt.Sprintf("var %s %s", name, v.Type)})
			}
		case ObjType:
			n, ok := o.Type.(*Named)
			if !ok {
				continue
			}
			var b strings.Builder
			if st, ok := n.Underlying().(*Struct); ok {
				fmt.Fprintf(&b, "type %s struct {", name)
				hidden := false
				for _, f := range st.Fields {
					if !show(f.Name) {
						hidden = true
						continue
					}
					fmt.Fprintf(&b, "\n\t%s %s", f.Name, f.Type)
				}
				if hidden {
					b.WriteString("\n\t// unexported fields")
				}
				b.WriteString("\n}")
			} else {
				fmt.Fprintf(&b, "type %s %s", name, n.Underlying())
			}
			for _, m := range n.Methods {
				if show(m.Name) {
					b.WriteString("\n" + DocLine(m))
				}
			}
			types = append(types, entry{o.Pos, b.String()})
		}
	}
	var funcs []entry
	for _, f := range p.Funcs {
		if f.RecvType != nil || !show(f.Name) || f.Sig == nil {
			continue
		}
		funcs = append(funcs, entry{f.Decl.P, DocLine(f)})
	}
	byPos := func(es []entry) {
		sort.Slice(es, func(i, j int) bool {
			if es[i].pos.File != es[j].pos.File {
				return es[i].pos.File < es[j].pos.File
			}
			return es[i].pos.Line < es[j].pos.Line
		})
	}
	var b strings.Builder
	fmt.Fprintf(&b, "package %s\n", p.Name)
	for _, group := range [][]entry{consts, vars, types, funcs} {
		if len(group) == 0 {
			continue
		}
		byPos(group)
		b.WriteString("\n")
		for _, e := range group {
			b.WriteString(e.text + "\n")
		}
	}
	return b.String()
}

func errorInit(p *Package, v *Var) string {
	for _, vs := range p.GlobalInits {
		for i, n := range vs.Names {
			if n.Obj != nil && n.Obj.Var == v && i < len(vs.Values) {
				return exprString(vs.Values[i])
			}
		}
	}
	return "error(...)"
}
