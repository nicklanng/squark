package compiler

import (
	"fmt"
	"go/constant"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

// docText renders a declaration's doc comment, one "// " line each, ending in a newline.
func docText(doc []string) string {
	var b strings.Builder
	for _, l := range doc {
		if l == "" {
			b.WriteString("//\n")
		} else {
			b.WriteString("// " + l + "\n")
		}
	}
	return b.String()
}

// DocLine renders a function signature with its provenance (declared or inferred), e.g.
//
//	fn cut(s str, sep str) -> (str, str, bool) in s, sep
func DocLine(fn *Func) string {
	d := fn.Decl
	var b strings.Builder
	switch d.Kind {
	case KindTask:
		b.WriteString("task ")
	case KindExtern:
		b.WriteString("extern fn ")
	default:
		b.WriteString("fn ")
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
	params := fn.Sig.Params
	if fn.Sig.Recv != nil {
		params = append([]*Var{fn.Sig.Recv}, params...)
		base = 1
	}
	b.WriteString(fn.Name + "(")
	for i, p := range params {
		if i > 0 {
			b.WriteString(", ")
		}
		slot := i
		if i == 0 && fn.Sig.Recv != nil {
			if _, ptr := p.Type.(*Pointer); ptr {
				b.WriteString("&self")
			} else {
				b.WriteString("self")
			}
			if static[slot] {
				b.WriteString(" in static")
			}
			continue
		}
		b.WriteString(p.Name + " ")
		t := p.Type.String()
		if fn.Sig.Variadic && i == len(params)-1 {
			t = "..." + p.Type.(*Slice).Elem.String()
		}
		b.WriteString(t)
		if static[slot] {
			b.WriteString(" in static")
		}
		if dst := into[slot]; len(dst) > 0 {
			sort.Strings(dst)
			b.WriteString(" in " + strings.Join(dst, ", "))
		}
	}
	_ = base
	b.WriteString(")")
	b.WriteString(resultsText(fn.Sig.Results))
	if fn.Summary == nil && fn.Sig.HasContract && len(from) == 0 {
		// a generic function's template is never analyzed: show its declared contract
		for _, i := range fn.Sig.From {
			if i < len(slots) {
				from = append(from, slots[i].Name)
			}
		}
	}
	hasRefResult := false
	for _, r := range fn.Sig.Results {
		if HasRefs(r.Type) {
			hasRefResult = true
		}
	}
	// a template without a contract has no provenance of its own: each instance's is inferred
	perInstance := fn.Generic && fn.Summary == nil && !fn.Sig.HasContract
	if len(from) > 0 {
		b.WriteString(" in " + strings.Join(from, ", "))
	} else if hasRefResult && !perInstance {
		b.WriteString(" in static")
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
	if fn.Isolate {
		b.WriteString(" isolate")
	}
	if fn.Extern != "" && fn.Extern != fn.Name {
		fmt.Fprintf(&b, " c%q", fn.Extern)
	}
	if perInstance && hasRefResult {
		b.WriteString(" // provenance inferred for each instance")
	}
	return b.String()
}

// resultsText prints a signature's results after ->, with ! for a last error result.
func resultsText(results []*Var) string {
	rs := results
	bang := len(rs) > 0 && rs[len(rs)-1].Type == tError
	if bang {
		rs = rs[:len(rs)-1]
	}
	var ts []string
	for _, r := range rs {
		ts = append(ts, r.Type.String())
	}
	s := ""
	switch len(ts) {
	case 0:
		if bang {
			return " -> !"
		}
		return ""
	case 1:
		s = " -> " + ts[0]
	default:
		s = " -> (" + strings.Join(ts, ", ") + ")"
	}
	if bang {
		s += " !"
	}
	return s
}

// DocPackage renders a package's API: constants, variables, types and functions with provenance.
// Names that aren't pub are included only when all is set.
func DocPackage(p *Package, all bool) string {
	show := func(pub bool) bool { return all || pub || p.Name == "main" }
	type entry struct {
		pos  Pos
		text string
	}
	var consts, vars, types []entry
	for name, o := range p.Scope.names {
		if !show(o.Pub) {
			continue
		}
		switch o.Kind {
		case ObjConst:
			var doc []string
			var src Expr // the constant's own expression, if it has one
			if cd, ok := o.decl.(constDecl); ok {
				doc = cd.spec.Doc
				if cd.index < len(cd.spec.Values) {
					src = cd.spec.Values[cd.index]
				}
			}
			v := ""
			cv, isValue := o.Const.(constant.Value)
			lit, _ := src.(*BasicLit)
			switch {
			case isValue && cv.Kind() == constant.Float:
				// String abbreviates (Pi = 3.14159); show the value in full, at its type's precision
				bits := 64
				if o.Type != nil && basicKind(o.Type) == Float32 {
					bits = 32
				}
				f, _ := constant.Float64Val(cv)
				v = strconv.FormatFloat(f, 'g', -1, bits)
			case isValue && cv.Kind() == constant.String:
				v = cv.ExactString() // String abbreviates long strings
			case isValue && cv.Kind() == constant.Int && (o.Type == tUntypedRune || lit != nil && lit.Kind == CHAR):
				if r, ok := constant.Int64Val(cv); ok && r >= 0 && r <= utf8.MaxRune {
					v = strconv.QuoteRune(rune(r))
				} else {
					v = cv.String()
				}
			default:
				if s, ok := o.Const.(interface{ String() string }); ok {
					v = s.String()
				}
			}
			t := ""
			if o.Type != nil && !isUntyped(o.Type) {
				t = " " + o.Type.String()
			}
			consts = append(consts, entry{o.Pos, docText(doc) + fmt.Sprintf("const %s%s = %s", name, t, v)})
		case ObjVar:
			v := o.Var
			n := len(vars)
			switch {
			case v.ArenaSize > 0:
				vars = append(vars, entry{o.Pos, fmt.Sprintf("arena %s %d", name, v.ArenaSize)})
			case v.Shared:
				vars = append(vars, entry{o.Pos, fmt.Sprintf("shared %s %s", name, v.Type)})
			case v.ConstData:
				vars = append(vars, entry{o.Pos, fmt.Sprintf("const %s %s", name, v.Type)})
			case v.ReadOnly:
				vars = append(vars, entry{o.Pos, fmt.Sprintf("fault %s %s", name, faultInit(p, v))})
			default:
				vars = append(vars, entry{o.Pos, fmt.Sprintf("thread %s %s", name, v.Type)})
			}
			if v.Spec != nil && len(vars) > n {
				vars[n].text = docText(v.Spec.Doc) + vars[n].text
			}
		case ObjType:
			n, ok := o.Type.(*Named)
			if !ok {
				continue
			}
			var b strings.Builder
			if o.TypeDecl != nil {
				b.WriteString(docText(o.TypeDecl.Doc))
			}
			// the methods, inside the type's block after a blank line
			// the methods, for an impl block after the type: a blank line before each documented one
			methods := func(after bool) string {
				var mb strings.Builder
				first := true
				for _, m := range n.Methods {
					if !show(m.Pub) {
						continue
					}
					if first && after || !first && len(m.Decl.Doc) > 0 {
						mb.WriteString("\n")
					}
					first = false
					text := docText(m.Decl.Doc) + DocLine(m)
					mb.WriteString("\n\t" + strings.ReplaceAll(text, "\n", "\n\t"))
				}
				return mb.String()
			}
			if st, ok := n.Underlying().(*Struct); ok {
				fieldDoc := map[string][]string{} // a field's doc comment, on the first name it declares
				if o.TypeDecl != nil {
					if ast, ok := o.TypeDecl.Type.(*StructType); ok {
						for _, f := range ast.Fields {
							if len(f.Doc) > 0 && len(f.Names) > 0 {
								fieldDoc[f.Names[0].Name] = f.Doc
							}
						}
					}
				}
				fmt.Fprintf(&b, "struct %s {", name)
				hidden, shown := false, 0
				for _, f := range st.Fields {
					if !show(f.Pub) {
						hidden = true
						continue
					}
					if doc := fieldDoc[f.Name]; len(doc) > 0 {
						if shown > 0 {
							b.WriteString("\n") // a blank line before a documented field
						}
						b.WriteString("\n\t" + strings.ReplaceAll(strings.TrimSuffix(docText(doc), "\n"), "\n", "\n\t"))
					}
					fmt.Fprintf(&b, "\n\t%s %s", f.Name, f.Type)
					shown++
				}
				if hidden {
					b.WriteString("\n\t// fields that aren't pub")
				}
				b.WriteString("\n}")
			} else if len(n.Enum) > 0 {
				t := ""
				if n.Underlying() != tInt {
					t = " " + n.Underlying().String()
				}
				fmt.Fprintf(&b, "enum %s%s {", name, t)
				for _, m := range n.Enum {
					fmt.Fprintf(&b, "\n\t%s = %s", m.Name, m.Const)
				}
				b.WriteString("\n}")
			} else if o.TypeDecl != nil && o.TypeDecl.Type != nil {
				fmt.Fprintf(&b, "%s: %s", name, exprString(o.TypeDecl.Type)) // as declared: Temp: Celsius
			} else {
				fmt.Fprintf(&b, "%s: %s", name, n.Underlying())

			}
			if ms := methods(false); ms != "" {
				fmt.Fprintf(&b, "\n\nimpl %s {%s\n}", name, ms)
			}
			types = append(types, entry{o.Pos, b.String()})
		}
	}
	var funcs []entry
	for _, f := range append(p.Funcs, p.Generics...) {
		if f.RecvType != nil || !show(f.Pub) || f.Sig == nil || f.GenericOf != nil {
			continue // instances are documented by their template
		}
		funcs = append(funcs, entry{f.Decl.P, docText(f.Decl.Doc) + DocLine(f)})
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
	// the package doc comment: the first that starts "Package name", else the first file's
	var pkgDoc []string
	for _, f := range p.Files {
		if len(f.Doc) > 0 && strings.HasPrefix(f.Doc[0], "Package "+p.Name+" ") {
			pkgDoc = f.Doc
			break
		}
		if pkgDoc == nil {
			pkgDoc = f.Doc
		}
	}
	b.WriteString(docText(pkgDoc))
	if p.Name == "main" {
		b.WriteString("program\n")
	} else if p.Std && p.Path != p.Name {
		fmt.Fprintf(&b, "module %s // use %s\n", p.Name, p.Path) // a nested path: math/rand
	} else {
		fmt.Fprintf(&b, "module %s\n", p.Name)
	}
	for gi, group := range [][]entry{consts, vars, types, funcs} {
		if len(group) == 0 {
			continue
		}
		byPos(group)
		b.WriteString("\n")
		for i, e := range group {
			if gi == 2 && i > 0 && (strings.Contains(e.text, "\n") || strings.Contains(group[i-1].text, "\n")) {
				b.WriteString("\n") // a blank line around a type that takes more than a line
			}
			b.WriteString(e.text + "\n")
		}
	}
	return b.String()
}

// faultInit prints an error constant's code and message: 404 "not found".
func faultInit(p *Package, v *Var) string {
	for _, vs := range p.GlobalInits {
		for i, n := range vs.Names {
			if n.Obj != nil && n.Obj.Var == v && i < len(vs.Values) {
				if call, ok := vs.Values[i].(*CallExpr); ok && isErrorValue(call) {
					var args []string
					for _, a := range call.Args {
						args = append(args, exprString(a))
					}
					return strings.Join(args, " ")
				}
				return "= " + exprString(vs.Values[i])
			}
		}
	}
	return "..."
}
