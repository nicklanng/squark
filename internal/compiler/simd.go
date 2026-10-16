package compiler

import (
	"fmt"
	"go/constant"
	"strings"
)

// SIMD types (float32x4, uint8x16, ...): operators work lane by lane, and code generation maps
// them to GNU C vector types, which clang and gcc compile to SIMD instructions (NEON, SSE/AVX).
// See spec §3.

// simdMethods are the methods every SIMD vector has; the mask ones only on integer vectors.
var simdMethods = map[string]bool{"Store": true, "Sum": true, "Min": true, "Max": true, "Any": true, "All": true, "Select": true, "Shuffle": true, "Swizzle": true}

func isSIMDMaskMethod(name string) bool { return name == "Any" || name == "All" || name == "Select" }

// simdBinary checks a binary operation with a SIMD operand: both operands have the same SIMD
// type, or one is an untyped constant, which is broadcast to every lane.
func (c *Checker) simdBinary(e *BinaryExpr) {
	x, y := e.X.info(), e.Y.info()
	switch {
	case isUntyped(y.T):
		if !c.convertUntyped(e.Y, x.T, "binary operation") {
			c.invalid(e)
			return
		}
	case isUntyped(x.T):
		if !c.convertUntyped(e.X, y.T, "binary operation") {
			c.invalid(e)
			return
		}
	case !Identical(x.T, y.T):
		hint := ""
		if s := simdOf(x.T); s != nil && simdOf(y.T) == nil {
			hint = "; broadcast a scalar with " + x.T.String() + "(" + exprString(e.Y) + ")"
		} else if s := simdOf(y.T); s != nil && simdOf(x.T) == nil {
			hint = "; broadcast a scalar with " + y.T.String() + "(" + exprString(e.X) + ")"
		}
		c.errorf(e.P, "invalid operation: %s (mismatched types %s and %s%s)", exprString(e), x.T, y.T, hint)
		c.invalid(e)
		return
	}
	t := x.T
	if isUntyped(t) {
		t = y.T
	}
	s := simdOf(t)
	if isComparison(e.Op) {
		// lane by lane: a mask of signed integers, -1 where the comparison holds and 0 elsewhere
		e.T, e.Mode = maskOf(s), ModeValue
		return
	}
	ok := false
	why := ""
	switch e.Op {
	case ADD, SUB, MUL:
		ok = true
	case QUO:
		ok = isFloat(s.Elem)
		why = " (integer vectors have no division: CPUs have no SIMD instruction for it; divide the lanes one at a time)"
	case REM:
		why = " (divide the lanes one at a time)"
	case AND, OR, XOR, AND_NOT:
		ok = isInteger(s.Elem)
	}
	if !ok {
		c.errorf(e.P, "invalid operation: operator %s not defined on %s (type %s)%s", e.Op, exprString(e.X), t, why)
		c.invalid(e)
		return
	}
	e.T, e.Mode = t, ModeValue
}

// simdUnary checks -x, +x and ^x on a SIMD vector.
func (c *Checker) simdUnary(e *UnaryExpr) {
	s := simdOf(e.X.info().T)
	ok := e.Op == ADD || e.Op == SUB || (e.Op == XOR && isInteger(s.Elem))
	if !ok {
		c.errorf(e.P, "invalid operation: operator %s not defined on %s (type %s)", e.Op, exprString(e.X), e.X.info().T)
		c.invalid(e)
		return
	}
	e.T, e.Mode = e.X.info().T, ModeValue
}

// simdConversion checks T(x) for a SIMD type T: a scalar of T's element type is broadcast to
// every lane, a SIMD vector with as many lanes is converted lane by lane, and a slice or array
// of T's element type is loaded (its first lanes elements; a shorter slice panics).
func (c *Checker) simdConversion(e *CallExpr, t Type) {
	s := simdOf(t)
	e.Conversion = true
	if len(e.Args) != 1 {
		c.errorf(e.P, "conversion to %s needs exactly one argument", t)
		c.invalid(e)
		return
	}
	x := e.Args[0]
	if !c.valueExpr(x, nil) {
		c.invalid(e)
		return
	}
	e.T, e.Mode = t, ModeValue
	xi := x.info()
	if isUntyped(xi.T) {
		if xi.Const != nil && c.convertUntyped(x, s.Elem, "conversion") {
			return
		}
		c.invalid(e)
		return
	}
	switch u := xi.T.Underlying().(type) {
	case *Basic:
		if u.Kind == s.Elem.Kind {
			return
		}
	case *SIMD:
		if u.Lanes == s.Lanes {
			return
		}
		c.errorf(e.P, "cannot convert %s (type %s, %d lanes) to %s (%d lanes): lane-by-lane conversion needs the same number of lanes", exprString(x), xi.T, u.Lanes, t, s.Lanes)
		c.invalid(e)
		return
	case *Slice:
		if basicKind(u.Elem) == s.Elem.Kind {
			return
		}
	case *Array:
		if basicKind(u.Elem) == s.Elem.Kind && u.Len == s.Lanes {
			return
		}
	}
	c.errorf(e.P, "cannot convert %s (type %s) to %s: it takes a %s (broadcast), a vector with %d lanes, or a []%s or [%d]%s (load)", exprString(x), xi.T, t, s.Elem, s.Lanes, s.Elem, s.Lanes, s.Elem)
	c.invalid(e)
}

// simdMethod checks the selector x.name of a built-in SIMD method and returns its signature
// (Select's is completed at the call, from its arguments).
func (c *Checker) simdMethod(e *SelectorExpr, s *SIMD, name string) *Signature {
	if isSIMDMaskMethod(name) && !isInteger(s.Elem) {
		c.errorf(e.Sel.P, "%s.%s undefined (%s is defined on integer vectors, such as the masks comparisons give)", exprString(e.X), name, name)
		return nil
	}
	switch name {
	case "Store":
		return &Signature{Params: []*Var{{Type: &Slice{Elem: s.Elem}}}}
	case "Sum", "Min", "Max":
		return &Signature{Results: []*Var{{Type: s.Elem}}}
	case "Any", "All":
		return &Signature{Results: []*Var{{Type: tBool}}}
	case "Select", "Shuffle", "Swizzle": // checked at the call (simdSelect, simdShuffle)
		return &Signature{Variadic: true}
	}
	return nil
}

// simdShuffle checks a.Shuffle(b, i0, …) and a.Swizzle(i0, …): one constant lane index per lane
// of the result, into a (0 to lanes-1) and, for Shuffle, b (lanes to 2*lanes-1).
func (c *Checker) simdShuffle(e *CallExpr, recv Type, name string) {
	s := simdOf(recv)
	args := e.Args
	limit := s.Lanes
	if name == "Shuffle" {
		if len(args) == 0 {
			c.errorf(e.P, "not enough arguments for Shuffle (expected a %s and %d lane indexes)", recv, s.Lanes)
			c.invalid(e)
			return
		}
		c.valueExpr(args[0], recv)
		if !c.assignable(args[0], recv, "argument to Shuffle") {
			c.invalid(e)
			return
		}
		args = args[1:]
		limit = 2 * s.Lanes
	}
	if int64(len(args)) != s.Lanes {
		c.errorf(e.P, "%s takes %d lane indexes for %s (one per lane), found %d", name, s.Lanes, recv, len(args))
		c.invalid(e)
		return
	}
	for _, a := range args {
		c.expr(a)
		v, ok := constInt(a)
		if a.info().Const == nil || !isInteger(a.info().T) || !ok {
			c.errorf(a.Pos(), "lane index %s must be an integer constant", exprString(a))
			c.invalid(e)
			return
		}
		c.convertUntyped(a, tInt, name)
		if v < 0 || v >= limit {
			c.errorf(a.Pos(), "lane index %d out of range [0:%d]", v, limit)
			c.invalid(e)
			return
		}
	}
	e.T, e.Mode = recv, ModeValue
}

// simdSelect checks m.Select(a, b): lanes of a where m's lane is nonzero, of b elsewhere. a and
// b have one SIMD type, with m's lane count and lane width.
func (c *Checker) simdSelect(e *CallExpr, m *SIMD) {
	if len(e.Args) != 2 {
		c.errorf(e.P, "wrong number of arguments for Select (expected 2, found %d)", len(e.Args))
		c.invalid(e)
		return
	}
	a, b := e.Args[0], e.Args[1]
	if !c.valueExpr(a, nil) || !c.valueExpr(b, nil) {
		c.invalid(e)
		return
	}
	t := a.info().T
	if isUntyped(t) {
		t = b.info().T
	}
	s := simdOf(t)
	if s == nil || s.Lanes != m.Lanes || sizeofBasic(s.Elem.Kind) != sizeofBasic(m.Elem.Kind) {
		c.errorf(e.P, "invalid arguments for Select: the mask has %d lanes of %d bits, so a and b must be vectors with the same lanes (such as %s)", m.Lanes, sizeofBasic(m.Elem.Kind)*8, m)
		c.invalid(e)
		return
	}
	if !c.assignable(a, t, "argument to Select") || !c.assignable(b, t, "argument to Select") {
		c.invalid(e)
		return
	}
	e.T, e.Mode = t, ModeValue
}

// simdConst converts an untyped constant to a SIMD type, broadcast: it must fit the element type.
func (c *Checker) simdConst(x Expr, s *SIMD, t Type, context string) bool {
	xi := x.info()
	if k := basicKind(xi.T); k == UntypedString || k == UntypedBool {
		c.errorf(x.Pos(), "cannot use %s (untyped %s constant) as type %s in %s", exprString(x), kindName(k), t, context)
		return false
	}
	v, ok := representable(xi.Const, s.Elem)
	if !ok {
		if v.Kind() == constant.Float && isInteger(s.Elem) {
			c.errorf(x.Pos(), "constant %s truncated to integer", xi.Const)
		} else {
			c.errorf(x.Pos(), "constant %s overflows %s", xi.Const, s.Elem)
		}
		return false
	}
	xi.Const = v
	setType(x, t)
	return true
}

// ---- code generation ----

// simdC is the C type of a SIMD type (typedefs in sq_rt.h).
func simdC(s *SIMD) string { return "sq_" + s.Name }

// simdSplat is a vector with the C scalar v in every lane.
func (g *gen) simdSplat(s *SIMD, v string) string {
	t := g.tmp()
	lanes := make([]string, s.Lanes)
	for i := range lanes {
		lanes[i] = t
	}
	return fmt.Sprintf("({ %s %s = %s; (%s){%s}; })", g.basicC(s.Elem.Kind), t, v, simdC(s), strings.Join(lanes, ", "))
}

// simdConstant is an untyped constant broadcast to every lane.
func (g *gen) simdConstant(v constant.Value, s *SIMD) string {
	c := g.constant(v, s.Elem)
	lanes := make([]string, s.Lanes)
	for i := range lanes {
		lanes[i] = c
	}
	return fmt.Sprintf("((%s){%s})", simdC(s), strings.Join(lanes, ", "))
}

func (g *gen) simdCompositeLit(e *CompositeLit, s *SIMD) string {
	lanes := make([]string, s.Lanes)
	for i := range lanes {
		lanes[i] = "0"
	}
	idx := int64(0)
	for _, el := range e.Elts {
		v := el
		if kv, ok := el.(*KeyValueExpr); ok {
			idx, _ = constInt(kv.Key)
			v = kv.Value
		}
		lanes[idx] = g.coerce(v, s.Elem)
		idx++
	}
	return fmt.Sprintf("((%s){%s})", simdC(s), strings.Join(lanes, ", "))
}

// simdIndex is v[i], a lane (read-only).
func (g *gen) simdIndex(e *IndexExpr, s *SIMD) string {
	i := g.coerce(e.Index, tInt)
	if _, ok := constInt(e.Index); !ok {
		i = fmt.Sprintf("SQ_IDX(%s, %d)", i, s.Lanes)
	}
	if isSimpleC(e.X) {
		return fmt.Sprintf("(%s)[%s]", g.expr(e.X), i)
	}
	t := g.tmp()
	return fmt.Sprintf("({ %s %s = %s; %s[%s]; })", simdC(s), t, g.expr(e.X), t, i)
}

// simdShift shifts every lane by the scalar count y; as in Go, a count of at least the lane
// width gives 0 (or the sign, for >> of a signed lane), and a negative count panics.
func (g *gen) simdShift(e *BinaryExpr, s *SIMD) string {
	a := g.expr(e.X)
	w := sizeofBasic(s.Elem.Kind) * 8
	if v, ok := constInt(e.Y); ok && v < w {
		return fmt.Sprintf("((%s) %s %d)", a, e.Op, v)
	}
	n, x := g.tmp(), g.tmp()
	count := g.expr(e.Y)
	if e.Y.info().Const == nil && !isUnsigned(e.Y.info().T) {
		count = fmt.Sprintf("({ int64_t _sn = (int64_t)(%s); if (_sn < 0) sq_panic_cstr(\"negative shift amount\", __FILE__, __LINE__); _sn; })", count)
	}
	big := fmt.Sprintf("((%s){})", simdC(s))
	if e.Op == SHR && !isUnsigned(s.Elem) {
		big = fmt.Sprintf("(%s >> %d)", x, w-1)
	}
	return fmt.Sprintf("({ %s %s = %s; uint64_t %s = (uint64_t)(%s); %s >= %d ? %s : (%s %s (int)%s); })",
		simdC(s), x, a, n, count, n, w, big, x, e.Op, n)
}

// simdComparison compares lane by lane, giving a mask.
func (g *gen) simdComparison(e *BinaryExpr) string {
	t := e.X.info().T
	if isUntyped(t) {
		t = e.Y.info().T
	}
	return fmt.Sprintf("((%s)(%s %s %s))", simdC(maskOf(simdOf(t))), g.coerce(e.X, t), e.Op, g.coerce(e.Y, t))
}

// simdConversion is T(x) for a SIMD type T: a broadcast, a load or a lane-by-lane conversion.
func (g *gen) simdConversion(e *CallExpr, s *SIMD) string {
	x := e.Args[0]
	ct := simdC(s)
	switch u := x.info().T.Underlying().(type) {
	case *SIMD:
		if u.Elem.Kind == s.Elem.Kind {
			return g.expr(x)
		}
		if isFloat(u.Elem) && isInteger(s.Elem) {
			// out-of-range lanes saturate, as scalar conversions do (spec §5)
			f, r, i := g.tmp(), g.tmp(), g.tmp()
			conv := fmt.Sprintf("sq_f2i(%s[%s], %d)", f, i, sizeofBasic(s.Elem.Kind)*8)
			if isUnsigned(s.Elem) {
				conv = fmt.Sprintf("sq_f2u(%s[%s], %d)", f, i, sizeofBasic(s.Elem.Kind)*8)
			}
			return fmt.Sprintf("({ %s %s = %s; %s %s; for (int %s = 0; %s < %d; %s++) %s[%s] = (%s)%s; %s; })",
				simdC(u), f, g.expr(x), ct, r, i, i, u.Lanes, i, r, i, g.basicC(s.Elem.Kind), conv, r)
		}
		return fmt.Sprintf("__builtin_convertvector(%s, %s)", g.expr(x), ct)
	case *Slice:
		sl, v := g.tmp(), g.tmp()
		return fmt.Sprintf("({ %s %s = %s; if (__builtin_expect(%s.len < %d, 0)) sq_panic_index(%d, %s.len, __FILE__, __LINE__); %s %s; memcpy(&%s, %s.p, sizeof %s); %s; })",
			g.ctype(x.info().T), sl, g.expr(x), sl, s.Lanes, s.Lanes-1, sl, ct, v, v, sl, v, v)
	case *Array:
		a, v := g.tmp(), g.tmp()
		return fmt.Sprintf("({ %s %s = %s; %s %s; memcpy(&%s, %s.a, sizeof %s); %s; })", g.ctype(x.info().T), a, g.expr(x), ct, v, v, a, v, v)
	}
	return g.simdSplat(s, g.coerce(x, s.Elem))
}

// simdReduce combines the lanes of the C vector v (type s) pairwise: the upper half into the
// lower half, until one lane is left. That fixes the order of float additions.
func (g *gen) simdReduce(s *SIMD, v string, combine func(a, b string) string) string {
	lanes := make([]string, s.Lanes)
	for i := range lanes {
		lanes[i] = fmt.Sprintf("%s[%d]", v, i)
	}
	for len(lanes) > 1 {
		h := len(lanes) / 2
		next := make([]string, h)
		for i := range next {
			next[i] = combine(lanes[i], lanes[i+h])
		}
		lanes = next
	}
	return lanes[0]
}

func (g *gen) simdBuiltin(e *CallExpr) string {
	se := unparen(e.Fun).(*SelectorExpr)
	s := simdOf(se.X.info().T)
	v := g.tmp()
	ct, et := simdC(s), g.basicC(s.Elem.Kind)
	recv := fmt.Sprintf("%s %s = %s; ", ct, v, g.expr(se.X))
	switch e.Builtin {
	case "simd.Store":
		d := g.tmp()
		return fmt.Sprintf("({ %s%s %s = %s; if (__builtin_expect(%s.len < %d, 0)) sq_panic_index(%d, %s.len, __FILE__, __LINE__); memcpy(%s.p, &%s, sizeof %s); })",
			recv, g.ctype(e.Args[0].info().T), d, g.expr(e.Args[0]), d, s.Lanes, s.Lanes-1, d, d, v, v)
	case "simd.Sum":
		return fmt.Sprintf("({ %s(%s)(%s); })", recv, et, g.simdReduce(s, v, func(a, b string) string {
			return fmt.Sprintf("(%s)(%s + %s)", et, a, b)
		}))
	case "simd.Min", "simd.Max":
		op := "<"
		if e.Builtin == "simd.Max" {
			op = ">"
		}
		// as the scalar min and max: the later lane wins unless it is smaller (larger)
		return fmt.Sprintf("({ %s(%s)(%s); })", recv, et, g.simdReduce(s, v, func(a, b string) string {
			x, y := g.tmp(), g.tmp()
			return fmt.Sprintf("({ %s %s = %s, %s = %s; %s %s %s ? %s : %s; })", et, x, a, y, b, y, op, x, y, x)
		}))
	case "simd.Any", "simd.All":
		op := "||"
		if e.Builtin == "simd.All" {
			op = "&&"
		}
		return fmt.Sprintf("({ %s(bool)(%s); })", recv, g.simdReduce(s, v, func(a, b string) string {
			if !strings.HasPrefix(a, "(") {
				a = "(" + a + " != 0)"
			}
			if !strings.HasPrefix(b, "(") {
				b = "(" + b + " != 0)"
			}
			return fmt.Sprintf("(%s %s %s)", a, op, b)
		}))
	case "simd.Shuffle", "simd.Swizzle":
		args, other := e.Args, v
		if e.Builtin == "simd.Shuffle" {
			other = g.tmp()
			recv += fmt.Sprintf("%s %s = %s; ", ct, other, g.coerce(e.Args[0], se.X.info().T))
			args = args[1:]
		}
		lanes := make([]string, len(args))
		for i, a := range args {
			k, _ := constInt(a)
			if k < s.Lanes {
				lanes[i] = fmt.Sprintf("%s[%d]", v, k)
			} else {
				lanes[i] = fmt.Sprintf("%s[%d]", other, k-s.Lanes)
			}
		}
		return fmt.Sprintf("({ %s(%s){%s}; })", recv, ct, strings.Join(lanes, ", "))
	case "simd.Select":
		t := e.T
		vt := simdC(simdOf(t))
		m, x, y := g.tmp(), g.tmp(), g.tmp()
		return fmt.Sprintf("({ %s%s %s = (%s)(%s != 0); %s %s = %s, %s = %s; (%s)(((%s)%s & %s) | ((%s)%s & ~%s)); })",
			recv, ct, m, ct, v, vt, x, g.coerce(e.Args[0], t), y, g.coerce(e.Args[1], t), vt, ct, x, m, ct, y, m)
	}
	panic(fmt.Sprintf("squark compiler bug: no code generation for builtin %q", e.Builtin))
}

// simdMinMax is min or max lane by lane, each lane as the scalar builtin would pick it.
func (g *gen) simdMinMax(e *CallExpr, s *SIMD) string {
	t := e.T
	ct, mt := simdC(s), simdC(maskOf(s))
	op := "<"
	if e.Builtin == "max" {
		op = ">"
	}
	var b strings.Builder
	r := g.tmp()
	fmt.Fprintf(&b, "({ %s %s = %s; ", ct, r, g.coerce(e.Args[0], t))
	for _, a := range e.Args[1:] {
		v, m := g.tmp(), g.tmp()
		fmt.Fprintf(&b, "%s %s = %s; %s %s = (%s)(%s %s %s); %s = (%s)(((%s)%s & %s) | ((%s)%s & ~%s)); ",
			ct, v, g.coerce(a, t), mt, m, mt, v, op, r, r, ct, mt, v, m, mt, r, m)
	}
	fmt.Fprintf(&b, "%s; })", r)
	return b.String()
}

// simdPrint prints the C vector v as [a b c d].
func (g *gen) simdPrint(v string, s *SIMD) string {
	var b strings.Builder
	b.WriteString("({ sq_print_byte('['); ")
	for i := int64(0); i < s.Lanes; i++ {
		if i > 0 {
			b.WriteString("sq_print_byte(' '); ")
		}
		switch {
		case isFloat(s.Elem):
			fmt.Fprintf(&b, "sq_print_float((double)%s[%d]); ", v, i)
		case isUnsigned(s.Elem):
			fmt.Fprintf(&b, "sq_print_uint((uint64_t)%s[%d]); ", v, i)
		default:
			fmt.Fprintf(&b, "sq_print_int((int64_t)%s[%d]); ", v, i)
		}
	}
	b.WriteString("sq_print_byte(']'); })")
	return b.String()
}

// simdLane returns lhs's vector and lane when lhs is a lane of a SIMD variable (v[i] = x), as a
// C lvalue (*p)[i] after emitting the pointer and the checked index: C can't take a lane's
// address, but it can assign through a pointer to the vector.
func (g *gen) simdLane(lhs Expr) (string, bool) {
	ix, ok := unparen(lhs).(*IndexExpr)
	if !ok {
		return "", false
	}
	s := simdOf(ix.X.info().T)
	if s == nil {
		return "", false
	}
	p, i := g.tmp(), g.tmp()
	idx := g.coerce(ix.Index, tInt)
	if _, ok := constInt(ix.Index); !ok {
		idx = fmt.Sprintf("SQ_IDX(%s, %d)", idx, s.Lanes)
	}
	g.line("%s *%s = &(%s); int64_t %s = %s;", simdC(s), p, g.expr(ix.X), i, idx)
	return fmt.Sprintf("(*%s)[%s]", p, i), true
}
