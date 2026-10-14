package compiler

import (
	"fmt"
	"strings"
)

// hoist declares storage in the current C block, before the statement being generated.
func (g *gen) hoist(decl string) {
	g.hoisted = append(g.hoisted, decl)
}

func isSimpleC(x Expr) bool {
	switch e := unparen(x).(type) {
	case *Ident:
		return true
	case *BasicLit:
		return true
	case *SelectorExpr:
		return e.Kind == SelQualified || (e.Kind == SelField && isSimpleC(e.X))
	}
	return x.info().Const != nil
}

// coerce emits x converted to type t (handles nil and struct-type identity differences).
func (g *gen) coerce(x Expr, t Type) string {
	xi := x.info()
	if isNilLit(x) || isNilType(xi.T) {
		return g.zero(t)
	}
	if xi.Const != nil {
		ct := t
		if _, ok := t.Underlying().(*Basic); !ok {
			ct = xi.T
		}
		return g.constant(xi.Const, ct)
	}
	s := g.expr(x)
	if xi.T == nil || t == nil {
		return s
	}
	from, to := g.ctype(xi.T), g.ctype(t)
	if from == to {
		return s
	}
	if _, ok := t.Underlying().(*Basic); ok {
		return fmt.Sprintf("((%s)(%s))", to, s)
	}
	switch t.Underlying().(type) {
	case *Pointer, *Signature:
		return fmt.Sprintf("((%s)(%s))", to, s)
	}
	v := g.tmp()
	return fmt.Sprintf("({ %s %s = %s; *(%s*)&%s; })", from, v, s, to, v)
}

func (g *gen) expr(x Expr) string {
	xi := x.info()
	if xi.Const != nil {
		return g.constant(xi.Const, xi.T)
	}
	switch e := x.(type) {
	case *Ident:
		return g.ident(e)
	case *ParenExpr:
		return "(" + g.expr(e.X) + ")"
	case *SelectorExpr:
		return g.selector(e)
	case *IndexExpr:
		return g.indexExpr(e)
	case *SliceExpr:
		return g.sliceExpr(e)
	case *StarExpr:
		return "(*" + g.expr(e.X) + ")"
	case *UnaryExpr:
		return g.unaryExpr(e)
	case *BinaryExpr:
		return g.binaryExpr(e)
	case *CompositeLit:
		return g.compositeLit(e)
	case *CallExpr:
		return g.callExpr(e)
	}
	return fmt.Sprintf("/* unsupported %T */0", x)
}

func (g *gen) ident(e *Ident) string {
	o := e.Obj
	if o == nil {
		return "0"
	}
	switch o.Kind {
	case ObjVar:
		return o.Var.CName
	case ObjFunc:
		return o.Func.CName
	case ObjNil:
		return g.zero(e.T)
	}
	return "0"
}

func (g *gen) selector(e *SelectorExpr) string {
	switch e.Kind {
	case SelQualified:
		o := e.Sel.Obj
		switch o.Kind {
		case ObjVar:
			return o.Var.CName
		case ObjFunc:
			return o.Func.CName
		}
		return "0"
	case SelField:
		st := e.X.info().T.Underlying()
		if p, ok := st.(*Pointer); ok && e.Implicit == "*" {
			st = p.Elem.Underlying()
		}
		f := st.(*Struct).Fields[e.Path[0]]
		fn := fieldName(f.Name, e.Path[0])
		if e.Implicit == "*" {
			return "(" + g.expr(e.X) + ")->" + fn
		}
		return "(" + g.expr(e.X) + ")." + fn
	}
	return "0"
}

func (g *gen) indexExpr(e *IndexExpr) string {
	xt := e.X.info().T.Underlying()
	idx := g.coerce(e.Index, tInt)
	checked := func(n string) string {
		if v, ok := constInt(e.Index); ok {
			_ = v
			if _, isArr := xt.(*Array); isArr {
				return idx // bounds verified at compile time
			}
		}
		return fmt.Sprintf("SQ_IDX(%s, %s)", idx, n)
	}
	switch u := xt.(type) {
	case *Array:
		if e.X.info().Mode == ModeVar {
			return fmt.Sprintf("(%s).a[%s]", g.expr(e.X), checked(fmt.Sprint(u.Len)))
		}
		t := g.tmp()
		return fmt.Sprintf("({ %s %s = %s; %s.a[%s]; })", g.ctype(e.X.info().T), t, g.expr(e.X), t, checked(fmt.Sprint(u.Len)))
	case *Pointer:
		a := u.Elem.Underlying().(*Array)
		return fmt.Sprintf("(%s)->a[%s]", g.expr(e.X), checked(fmt.Sprint(a.Len)))
	case *Slice:
		if isSimpleC(e.X) {
			s := g.expr(e.X)
			return fmt.Sprintf("(%s).p[%s]", s, checked("("+s+").len"))
		}
		t := g.tmp()
		return fmt.Sprintf("(*({ %s %s = %s; &%s.p[%s]; }))", g.ctype(e.X.info().T), t, g.expr(e.X), t, checked(t+".len"))
	case *Basic:
		if isSimpleC(e.X) {
			s := g.expr(e.X)
			return fmt.Sprintf("(%s).p[%s]", s, checked("("+s+").len"))
		}
		t := g.tmp()
		return fmt.Sprintf("({ sq_string %s = %s; %s.p[%s]; })", t, g.expr(e.X), t, checked(t+".len"))
	case *Map:
		kt, vt := g.ctype(u.Key), g.ctype(u.Value)
		k, p := g.tmp(), g.tmp()
		return fmt.Sprintf("({ %s %s = %s; %s *%s = sq_map_get(%s, &%s); %s ? *%s : %s; })",
			kt, k, g.coerce(e.Index, u.Key), vt, p, g.expr(e.X), k, p, p, g.zero(u.Value))
	}
	return "0"
}

// mapSlot returns an expression for a pointer to the value slot of m[k], inserting if absent.
func (g *gen) mapSlot(e *IndexExpr) string {
	u := e.X.info().T.Underlying().(*Map)
	kt, vt := g.ctype(u.Key), g.ctype(u.Value)
	k := g.tmp()
	return fmt.Sprintf("((%s*)({ %s %s = %s; sq_map_put(%s, &%s, __FILE__, __LINE__); }))", vt, kt, k, g.coerce(e.Index, u.Key), g.expr(e.X), k)
}

func (g *gen) sliceExpr(e *SliceExpr) string {
	xt := e.X.info().T
	resT := g.ctype(e.T)
	t := g.tmp()
	lo, hi, max := g.tmp(), g.tmp(), g.tmp()
	bound := func(x Expr, def string) string {
		if x == nil {
			return def
		}
		return g.coerce(x, tInt)
	}
	var b strings.Builder
	switch u := xt.Underlying().(type) {
	case *Array:
		fmt.Fprintf(&b, "({ %s *%s = (%s).a; ", g.ctype(u.Elem), t, g.expr(e.X))
		n := fmt.Sprint(u.Len)
		fmt.Fprintf(&b, "int64_t %s = %s, %s = %s, %s = %s; ", lo, bound(e.Lo, "0"), hi, bound(e.Hi, n), max, bound(e.Max, n))
		fmt.Fprintf(&b, "SQ_SLICE_CHECK(%s, %s, %s, %s); (%s){%s + %s, %s - %s, %s - %s}; })", lo, hi, max, n, resT, t, lo, hi, lo, max, lo)
	case *Pointer:
		a := u.Elem.Underlying().(*Array)
		fmt.Fprintf(&b, "({ %s *%s = (%s)->a; ", g.ctype(a.Elem), t, g.expr(e.X))
		n := fmt.Sprint(a.Len)
		fmt.Fprintf(&b, "int64_t %s = %s, %s = %s, %s = %s; ", lo, bound(e.Lo, "0"), hi, bound(e.Hi, n), max, bound(e.Max, n))
		fmt.Fprintf(&b, "SQ_SLICE_CHECK(%s, %s, %s, %s); (%s){%s + %s, %s - %s, %s - %s}; })", lo, hi, max, n, resT, t, lo, hi, lo, max, lo)
	case *Slice:
		fmt.Fprintf(&b, "({ %s %s = %s; ", g.ctype(xt), t, g.expr(e.X))
		fmt.Fprintf(&b, "int64_t %s = %s, %s = %s, %s = %s; ", lo, bound(e.Lo, "0"), hi, bound(e.Hi, t+".len"), max, bound(e.Max, t+".cap"))
		fmt.Fprintf(&b, "SQ_SLICE_CHECK(%s, %s, %s, %s.cap); (%s){%s.p + %s, %s - %s, %s - %s}; })", lo, hi, max, t, resT, t, lo, hi, lo, max, lo)
	case *Basic:
		fmt.Fprintf(&b, "({ sq_string %s = %s; ", t, g.coerce(e.X, tString))
		fmt.Fprintf(&b, "int64_t %s = %s, %s = %s; ", lo, bound(e.Lo, "0"), hi, bound(e.Hi, t+".len"))
		fmt.Fprintf(&b, "SQ_SLICE_CHECK(%s, %s, %s, %s.len); (sq_string){%s.p + %s, %s - %s}; })", lo, hi, hi, t, t, lo, hi, lo)
	}
	return b.String()
}

func (g *gen) unaryExpr(e *UnaryExpr) string {
	switch e.Op {
	case AND:
		if cl, ok := unparen(e.X).(*CompositeLit); ok {
			s := g.tmp()
			ct := g.ctype(cl.T)
			if g.fn == nil {
				g.hoist(fmt.Sprintf("%s %s;", ct, s))
			} else {
				g.hoist(fmt.Sprintf("%s %s;", ct, s))
			}
			return fmt.Sprintf("(%s = %s, &%s)", s, g.compositeLit(cl), s)
		}
		return "(&" + g.expr(e.X) + ")"
	case ARROW:
		v := g.tmp()
		return fmt.Sprintf("({ %s %s; sq_chan_recv(%s, &%s); %s; })", g.ctype(e.T), v, g.expr(e.X), v, v)
	case SUB:
		return g.wrapSmall(e.T, "-"+"("+g.expr(e.X)+")")
	case ADD:
		return g.expr(e.X)
	case XOR:
		return g.wrapSmall(e.T, "~("+g.expr(e.X)+")")
	case NOT:
		return "!(" + g.expr(e.X) + ")"
	}
	return "0"
}

// wrapSmall truncates C integer promotion results back to sub-int types.
func (g *gen) wrapSmall(t Type, s string) string {
	switch basicKind(t) {
	case Int8, Int16, Int32, Uint8, Uint16, Uint32, Float32:
		return fmt.Sprintf("((%s)(%s))", g.ctype(t), s)
	}
	return "(" + s + ")"
}

func divName(t Type) string {
	switch basicKind(t) {
	case Int8:
		return "i8"
	case Int16:
		return "i16"
	case Int32:
		return "i32"
	case Uint8:
		return "u8"
	case Uint16:
		return "u16"
	case Uint32:
		return "u32"
	case Uint, Uint64, Uintptr:
		return "u64"
	}
	return "i64"
}

func intWidth(t Type) int {
	return int(sizeofBasic(basicKind(t)) * 8)
}

func (g *gen) binaryExpr(e *BinaryExpr) string {
	if isComparison(e.Op) {
		return g.comparison(e)
	}
	t := e.T
	switch e.Op {
	case LAND:
		return "(" + g.expr(e.X) + " && " + g.expr(e.Y) + ")"
	case LOR:
		return "(" + g.expr(e.X) + " || " + g.expr(e.Y) + ")"
	case SHL, SHR:
		return g.shift(e.Op, g.expr(e.X), e.Y, t)
	}
	return g.arith(e.Op, g.coerce(e.X, t), g.coerce(e.Y, t), e.Y, t)
}

func (g *gen) arith(op Tok, a, b string, y Expr, t Type) string {
	switch op {
	case QUO, REM:
		if isInteger(t) {
			if v, ok := constInt(y); ok && v != 0 && v != -1 {
				return g.wrapSmall(t, fmt.Sprintf("%s %s %s", a, op, b))
			}
			m := "SQ_DIV"
			if op == REM {
				m = "SQ_REM"
			}
			return fmt.Sprintf("%s(%s, %s, %s)", m, divName(t), a, b)
		}
	case AND_NOT:
		return g.wrapSmall(t, fmt.Sprintf("%s & ~%s", a, b))
	}
	return g.wrapSmall(t, fmt.Sprintf("%s %s %s", a, op, b))
}

func (g *gen) shift(op Tok, a string, y Expr, t Type) string {
	w := intWidth(t)
	n := g.expr(y)
	if v, ok := constInt(y); ok && v >= 0 && v < int64(w) {
		return g.wrapSmall(t, fmt.Sprintf("%s %s %d", a, op, v))
	}
	ct := g.ctype(t)
	if op == SHL {
		return fmt.Sprintf("((%s)sq_shl((uint64_t)(%s), (uint64_t)(%s), %d))", ct, a, n, w)
	}
	if isUnsigned(t) {
		return fmt.Sprintf("((%s)sq_shru((uint64_t)(%s), (uint64_t)(%s), %d))", ct, a, n, w)
	}
	return fmt.Sprintf("((%s)sq_shrs((int64_t)(%s), (uint64_t)(%s), %d))", ct, a, n, w)
}

func (g *gen) comparison(e *BinaryExpr) string {
	x, y := e.X, e.Y
	t := x.info().T
	if isNilLit(x) || isNilType(t) {
		x, y = y, x
		t = x.info().T
	}
	if isNilLit(y) {
		a := g.expr(x)
		var test string
		switch t.Underlying().(type) {
		case *Slice:
			test = "(" + a + ").p == NULL"
		case *ErrorType:
			test = "(" + a + ").msg.p == NULL"
		default:
			test = "(" + a + ") == NULL"
		}
		if e.Op == NEQ {
			return "(!(" + test + "))"
		}
		return "(" + test + ")"
	}
	if isUntyped(t) {
		t = y.info().T
	}
	a, b := g.coerce(x, t), g.coerce(y, t)
	if isString(t) {
		switch e.Op {
		case EQL:
			return fmt.Sprintf("sq_str_eq(%s, %s)", a, b)
		case NEQ:
			return fmt.Sprintf("(!sq_str_eq(%s, %s))", a, b)
		}
		return fmt.Sprintf("(sq_str_cmp(%s, %s) %s 0)", a, b, e.Op)
	}
	switch t.Underlying().(type) {
	case *Struct, *Array, *ErrorType:
		eq := g.eqExpr(t, a, b)
		if e.Op == NEQ {
			return "(!" + eq + ")"
		}
		return eq
	}
	return fmt.Sprintf("(%s %s %s)", a, e.Op, b)
}

func (g *gen) compositeLit(e *CompositeLit) string {
	t := e.T
	ct := g.ctype(t)
	switch u := t.Underlying().(type) {
	case *Struct:
		if len(e.Elts) == 0 {
			return "((" + ct + "){0})"
		}
		var parts []string
		for i, el := range e.Elts {
			if kv, ok := el.(*KeyValueExpr); ok {
				name := kv.Key.(*Ident).Name
				for fi, f := range u.Fields {
					if f.Name == name {
						parts = append(parts, fmt.Sprintf(".%s = %s", fieldName(f.Name, fi), g.coerce(kv.Value, f.Type)))
					}
				}
				continue
			}
			f := u.Fields[i]
			parts = append(parts, fmt.Sprintf(".%s = %s", fieldName(f.Name, i), g.coerce(el, f.Type)))
		}
		return "((" + ct + "){" + strings.Join(parts, ", ") + "})"
	case *Array:
		if len(e.Elts) == 0 {
			return "((" + ct + "){0})"
		}
		return "((" + ct + "){ .a = {" + strings.Join(g.indexedInits(e.Elts, u.Elem), ", ") + "} })"
	case *Slice:
		if len(e.Elts) == 0 {
			return fmt.Sprintf("((%s){(%s*)sq_zerobase, 0, 0})", ct, g.ctype(u.Elem))
		}
		n := literalLen(e.Elts)
		arr := g.ctype(&Array{Len: n, Elem: u.Elem})
		s := g.tmp()
		g.hoist(fmt.Sprintf("%s %s;", arr, s))
		return fmt.Sprintf("(%s = (%s){ .a = {%s} }, (%s){%s.a, %d, %d})", s, arr, strings.Join(g.indexedInits(e.Elts, u.Elem), ", "), ct, s, n, n)
	}
	return "0"
}

func literalLen(elts []Expr) int64 {
	idx, max := int64(0), int64(0)
	for _, el := range elts {
		if kv, ok := el.(*KeyValueExpr); ok {
			idx, _ = constInt(kv.Key)
		}
		idx++
		if idx > max {
			max = idx
		}
	}
	return max
}

func (g *gen) indexedInits(elts []Expr, elem Type) []string {
	var parts []string
	idx := int64(0)
	for _, el := range elts {
		v := el
		if kv, ok := el.(*KeyValueExpr); ok {
			idx, _ = constInt(kv.Key)
			v = kv.Value
		}
		parts = append(parts, fmt.Sprintf("[%d] = %s", idx, g.coerce(v, elem)))
		idx++
	}
	return parts
}

// ---- calls ----

func hasCall(x Expr) bool {
	found := false
	var walk func(x Expr)
	walk = func(x Expr) {
		if found || x == nil {
			return
		}
		switch e := x.(type) {
		case *CallExpr:
			if e.Builtin == "len" || e.Builtin == "cap" || e.Conversion {
				for _, a := range e.Args {
					walk(a)
				}
				return
			}
			found = true
		case *ParenExpr:
			walk(e.X)
		case *SelectorExpr:
			walk(e.X)
		case *IndexExpr:
			walk(e.X)
			walk(e.Index)
		case *SliceExpr:
			walk(e.X)
			walk(e.Lo)
			walk(e.Hi)
			walk(e.Max)
		case *StarExpr:
			walk(e.X)
		case *UnaryExpr:
			if e.Op == ARROW {
				found = true
			}
			walk(e.X)
		case *BinaryExpr:
			walk(e.X)
			walk(e.Y)
		case *CompositeLit:
			for _, el := range e.Elts {
				walk(el)
			}
		case *KeyValueExpr:
			walk(e.Value)
		}
	}
	walk(x)
	return found
}

// callArgs returns the C argument expressions for a call (receiver first).
func (g *gen) callArgs(e *CallExpr, sig *Signature) []string {
	var args []string
	if se, ok := unparen(e.Fun).(*SelectorExpr); ok && se.Kind == SelMethod {
		recv := g.expr(se.X)
		switch se.Implicit {
		case "&":
			recv = "&" + recv
		case "*":
			recv = "*" + recv
		}
		args = append(args, recv)
	}
	params := sig.Params
	if e.Variadic >= 0 && !e.Ellipsis && sig.Variadic {
		for i := 0; i < e.Variadic && i < len(e.Args); i++ {
			args = append(args, g.coerce(e.Args[i], params[i].Type))
		}
		st := params[len(params)-1].Type
		elem := st.Underlying().(*Slice).Elem
		rest := e.Args[min(e.Variadic, len(e.Args)):]
		if len(rest) == 0 {
			args = append(args, g.zero(st))
		} else {
			arr := g.ctype(&Array{Len: int64(len(rest)), Elem: elem})
			s := g.tmp()
			g.hoist(fmt.Sprintf("%s %s;", arr, s))
			var inits []string
			for _, a := range rest {
				inits = append(inits, g.coerce(a, elem))
			}
			args = append(args, fmt.Sprintf("(%s = (%s){ .a = {%s} }, (%s){%s.a, %d, %d})", s, arr, strings.Join(inits, ", "), g.ctype(st), s, len(rest), len(rest)))
		}
		return args
	}
	for i, a := range e.Args {
		if i < len(params) {
			args = append(args, g.coerce(a, params[i].Type))
		} else {
			args = append(args, g.expr(a))
		}
	}
	return args
}

func (g *gen) callExpr(e *CallExpr) string {
	if e.Conversion {
		return g.conversion(e)
	}
	if e.Builtin != "" {
		return g.builtinCall(e)
	}
	sig := e.Fun.info().T.Underlying().(*Signature)
	args := g.callArgs(e, sig)
	var fn string
	if e.Callee != nil {
		fn = e.Callee.CName
	} else {
		fn = "(" + g.expr(e.Fun) + ")"
	}
	// Go evaluates arguments left to right; C does not promise that.
	calls := 0
	if se, ok := unparen(e.Fun).(*SelectorExpr); ok && se.Kind == SelMethod && hasCall(se.X) {
		calls++
	}
	for _, a := range e.Args {
		if hasCall(a) {
			calls++
		}
	}
	if calls >= 2 {
		var b strings.Builder
		b.WriteString("({ ")
		var names []string
		var ptypes []Type
		if sig.Recv != nil || e.Callee != nil && e.Callee.Sig.Recv != nil {
			ptypes = append(ptypes, e.Callee.Sig.Recv.Type)
		}
		for _, p := range sig.Params {
			ptypes = append(ptypes, p.Type)
		}
		for i, a := range args {
			if i >= len(ptypes) {
				names = append(names, a)
				continue
			}
			n := g.tmp()
			fmt.Fprintf(&b, "%s %s = %s; ", g.ctype(ptypes[i]), n, a)
			names = append(names, n)
		}
		fmt.Fprintf(&b, "%s(%s); })", fn, strings.Join(names, ", "))
		return b.String()
	}
	return fmt.Sprintf("%s(%s)", fn, strings.Join(args, ", "))
}

func (g *gen) conversion(e *CallExpr) string {
	x := e.Args[0]
	from, to := x.info().T, e.T
	if isNilType(from) || isNilLit(x) {
		return g.zero(to)
	}
	s := g.expr(x)
	if isString(to) && isByteSlice(from) {
		if isSimpleC(x) {
			return fmt.Sprintf("((sq_string){(%s).p, (%s).len})", s, s)
		}
		t := g.tmp()
		return fmt.Sprintf("({ %s %s = %s; (sq_string){%s.p, %s.len}; })", g.ctype(from), t, s, t, t)
	}
	fc, tc := g.ctype(from), g.ctype(to)
	if fc == tc {
		return s
	}
	switch to.Underlying().(type) {
	case *Basic, *Pointer, *Signature:
		return fmt.Sprintf("((%s)(%s))", tc, s)
	}
	t := g.tmp()
	return fmt.Sprintf("({ %s %s = %s; *(%s*)&%s; })", fc, t, s, tc, t)
}

func (g *gen) builtinCall(e *CallExpr) string {
	args := e.Args
	switch e.Builtin {
	case "len", "cap":
		x := args[0]
		t := x.info().T.Underlying()
		if p, ok := t.(*Pointer); ok {
			if a, ok := p.Elem.Underlying().(*Array); ok {
				return fmt.Sprint(a.Len)
			}
		}
		switch t.(type) {
		case *Map:
			return "sq_map_len(" + g.expr(x) + ")"
		case *Chan:
			if e.Builtin == "cap" {
				return "sq_chan_cap(" + g.expr(x) + ")"
			}
			return "sq_chan_len(" + g.expr(x) + ")"
		case *Array:
			return fmt.Sprint(t.(*Array).Len)
		}
		if e.Builtin == "cap" {
			return "(" + g.expr(x) + ").cap"
		}
		return "(" + g.coerce(x, defaultType(x.info().T)) + ").len"
	case "append":
		st := e.T
		elem := st.Underlying().(*Slice).Elem
		ec := g.ctype(elem)
		s := g.tmp()
		var b strings.Builder
		fmt.Fprintf(&b, "({ %s %s = %s; ", g.ctype(st), s, g.expr(args[0]))
		if e.Ellipsis {
			y := args[1]
			yt := g.tmp()
			if isString(y.info().T) {
				fmt.Fprintf(&b, "sq_string %s = %s; ", yt, g.coerce(y, tString))
			} else {
				fmt.Fprintf(&b, "%s %s = %s; ", g.ctype(y.info().T), yt, g.expr(y))
			}
			fmt.Fprintf(&b, "if (%s.len + %s.len > %s.cap) sq_panic_append(%s.len + %s.len, %s.cap, __FILE__, __LINE__); ", s, yt, s, s, yt, s)
			fmt.Fprintf(&b, "if (%s.len > 0) memmove(%s.p + %s.len, %s.p, (size_t)%s.len * sizeof(%s)); %s.len += %s.len; %s; })", yt, s, s, yt, yt, ec, s, yt, s)
			return b.String()
		}
		n := len(args) - 1
		fmt.Fprintf(&b, "if (%s.len + %d > %s.cap) sq_panic_append(%s.len + %d, %s.cap, __FILE__, __LINE__); ", s, n, s, s, n, s)
		for _, a := range args[1:] {
			fmt.Fprintf(&b, "%s.p[%s.len++] = %s; ", s, s, g.coerce(a, elem))
		}
		fmt.Fprintf(&b, "%s; })", s)
		return b.String()
	case "copy":
		d, sv := g.tmp(), g.tmp()
		dt := args[0].info().T
		elem := dt.Underlying().(*Slice).Elem
		var srcT string
		var src string
		if isString(args[1].info().T) {
			srcT, src = "sq_string", g.coerce(args[1], tString)
		} else {
			srcT, src = g.ctype(args[1].info().T), g.expr(args[1])
		}
		return fmt.Sprintf("({ %s %s = %s; %s %s = %s; sq_copy(%s.p, %s.len, %s.p, %s.len, sizeof(%s)); })",
			g.ctype(dt), d, g.expr(args[0]), srcT, sv, src, d, d, sv, sv, g.ctype(elem))
	case "make":
		return g.makeCall(e)
	case "delete":
		m := args[0].info().T.Underlying().(*Map)
		k := g.tmp()
		return fmt.Sprintf("({ %s %s = %s; sq_map_delete(%s, &%s); })", g.ctype(m.Key), k, g.coerce(args[1], m.Key), g.expr(args[0]), k)
	case "close":
		return fmt.Sprintf("sq_chan_close(%s, __FILE__, __LINE__)", g.expr(args[0]))
	case "panic":
		x := args[0]
		t := x.info().T
		switch {
		case isString(t):
			return fmt.Sprintf("sq_panic_str(%s, __FILE__, __LINE__)", g.coerce(x, tString))
		case isInteger(t):
			return fmt.Sprintf("sq_panic_int((int64_t)(%s), __FILE__, __LINE__)", g.expr(x))
		case isBoolean(t):
			return fmt.Sprintf("sq_panic_cstr((%s) ? \"true\" : \"false\", __FILE__, __LINE__)", g.expr(x))
		}
		if _, ok := t.Underlying().(*ErrorType); ok {
			return fmt.Sprintf("sq_panic_str((%s).msg, __FILE__, __LINE__)", g.expr(x))
		}
		return "sq_panic_cstr(\"panic\", __FILE__, __LINE__)"
	case "print", "println":
		var b strings.Builder
		b.WriteString("({ ")
		for i, a := range args {
			if i > 0 && e.Builtin == "println" {
				b.WriteString("sq_print_byte(' '); ")
			}
			b.WriteString(g.printArg(a) + "; ")
		}
		if e.Builtin == "println" {
			b.WriteString("sq_print_byte('\\n'); ")
		}
		b.WriteString("})")
		return b.String()
	case "min", "max":
		t := e.T
		ct := g.ctype(t)
		var b strings.Builder
		r := g.tmp()
		fmt.Fprintf(&b, "({ %s %s = %s; ", ct, r, g.coerce(args[0], t))
		for _, a := range args[1:] {
			v := g.tmp()
			fmt.Fprintf(&b, "%s %s = %s; ", ct, v, g.coerce(a, t))
			op := "<"
			if e.Builtin == "max" {
				op = ">"
			}
			if isString(t) {
				fmt.Fprintf(&b, "if (sq_str_cmp(%s, %s) %s 0) %s = %s; ", v, r, op, r, v)
			} else {
				fmt.Fprintf(&b, "if (%s %s %s) %s = %s; ", v, op, r, r, v)
			}
		}
		fmt.Fprintf(&b, "%s; })", r)
		return b.String()
	case "__error":
		return fmt.Sprintf("((sq_error){%s, %s})", g.coerce(args[0], tInt), g.coerce(args[1], tString))
	case "error.Error":
		se := unparen(e.Fun).(*SelectorExpr)
		return "(" + g.expr(se.X) + ").msg"
	case "error.Code":
		se := unparen(e.Fun).(*SelectorExpr)
		return "(" + g.expr(se.X) + ").code"
	}
	return "0"
}

func (g *gen) printArg(a Expr) string {
	t := a.info().T
	if a.info().Const != nil {
		t = defaultType(t)
	}
	s := g.coerce(a, t)
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
	case *Pointer:
		return "sq_print_ptr(" + s + ")"
	case *ErrorType:
		return "sq_print_err(" + s + ")"
	case *Slice:
		t := g.tmp()
		return fmt.Sprintf("({ %s %s = %s; sq_print_str((sq_string){%s.p, %s.len}); })", g.ctype(a.info().T), t, s, t, t)
	}
	return "(void)0"
}

func nextPow2(n int64) int64 {
	p := int64(1)
	for p < n {
		p <<= 1
	}
	return p
}

func (g *gen) makeCall(e *CallExpr) string {
	t := e.T
	sizes := []int64{}
	for _, a := range e.Args[1:] {
		v, _ := constInt(a)
		sizes = append(sizes, v)
	}
	id := e.StorageID
	switch u := t.Underlying().(type) {
	case *Slice:
		n := sizes[0]
		c := n
		if len(sizes) > 1 {
			c = sizes[1]
		}
		arr := g.ctype(&Array{Len: max(c, 1), Elem: u.Elem})
		s := fmt.Sprintf("_mk%d", id)
		g.hoist(fmt.Sprintf("%s %s;", arr, s))
		return fmt.Sprintf("(memset(&%s, 0, sizeof %s), (%s){%s.a, %d, %d})", s, s, g.ctype(t), s, n, c)
	case *Map:
		cap := sizes[0]
		slots := nextPow2(cap + cap/3 + 1)
		kt, vt := g.ctype(u.Key), g.ctype(u.Value)
		s := fmt.Sprintf("_mk%d", id)
		g.hoist(fmt.Sprintf("struct { sq_map h; %s k[%d]; %s v[%d]; uint8_t c[%d]; } %s;", kt, slots, vt, slots, slots, s))
		kind := "SQ_KEY_BYTES"
		if isString(u.Key) {
			kind = "SQ_KEY_STRING"
		}
		return fmt.Sprintf("sq_map_init(&%s.h, %s.k, %s.v, %s.c, %d, %d, sizeof(%s), sizeof(%s), %s)", s, s, s, s, slots, cap, kt, vt, kind)
	case *Chan:
		cap := int64(0)
		if len(sizes) > 0 {
			cap = sizes[0]
		}
		et := g.ctype(u.Elem)
		s := fmt.Sprintf("_mk%d", id)
		g.hoist(fmt.Sprintf("struct { sq_chan h; %s buf[%d]; } %s;", et, max(cap, 1), s))
		return fmt.Sprintf("sq_chan_init(&%s.h, %s.buf, sizeof(%s), %d)", s, s, et, cap)
	}
	return "0"
}
