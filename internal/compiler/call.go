package compiler

import (
	"fmt"
	"go/constant"
	gotoken "go/token"
)

func (c *Checker) call(e *CallExpr) {
	e.Variadic = -1
	// method or qualified call: check selector in callee mode
	switch f := unparen(e.Fun).(type) {
	case *SelectorExpr:
		c.selector(f, true)
		e.Fun.info().T, e.Fun.info().Mode = f.T, f.Mode
		if p, ok := e.Fun.(*ParenExpr); ok {
			p.ExprInfo = f.ExprInfo
		}
	case *Ident:
		// a direct call is not an address-taking use of a function
		c.calleeIdent = f
		c.ident(f)
		c.calleeIdent = nil
		if p, ok := e.Fun.(*ParenExpr); ok {
			p.ExprInfo = f.ExprInfo
		}
	default:
		c.expr(e.Fun)
	}
	fi := e.Fun.info()
	switch fi.Mode {
	case ModeInvalid:
		c.invalid(e)
		for _, a := range e.Args {
			c.expr(a)
		}
		return
	case ModeType:
		c.conversion(e, fi.T)
		return
	case ModeBuiltin:
		c.builtin(e, unparen(e.Fun).(*Ident).Name)
		return
	case ModePkg:
		c.errorf(e.P, "use of package %s without selector", exprString(e.Fun))
		c.invalid(e)
		return
	}
	sig, ok := fi.T.Underlying().(*Signature)
	if !ok {
		c.errorf(e.P, "invalid operation: cannot call non-function %s (type %s)", exprString(e.Fun), fi.T)
		c.invalid(e)
		return
	}
	// resolve static callee
	switch f := unparen(e.Fun).(type) {
	case *Ident:
		if f.Obj != nil && f.Obj.Kind == ObjFunc {
			e.Callee = f.Obj.Func
		}
	case *SelectorExpr:
		switch f.Kind {
		case SelQualified:
			if f.Sel.Obj != nil && f.Sel.Obj.Kind == ObjFunc {
				e.Callee = f.Sel.Obj.Func
			}
		case SelMethod:
			e.Callee = f.Method
		case SelBuiltinMethod:
			switch f.X.info().T.Underlying().(type) {
			case *ArenaType:
				e.Builtin = "arena." + f.Sel.Name
			case *VecType:
				e.Builtin = "vec." + f.Sel.Name
			default:
				e.Builtin = "error." + f.Sel.Name
			}
			c.args(e, sig)
			switch len(sig.Results) {
			case 0:
				e.T, e.Mode = tInvalid, ModeNoValue
			default:
				e.T, e.Mode = sig.Results[0].Type, ModeValue
			}
			return
		}
	}
	owner := c.fn
	if owner == nil {
		owner = c.prog.InitFunc // package-level initializers
	}
	if e == c.goCall {
		// `go f()` runs f on its own stack: not a call edge for stack sizing or recursion
	} else if e.Callee != nil {
		owner.Calls = append(owner.Calls, e.Callee)
	} else {
		owner.Indirect = append(owner.Indirect, sig)
	}
	c.args(e, sig)
	switch len(sig.Results) {
	case 0:
		e.T, e.Mode = tInvalid, ModeNoValue
	case 1:
		e.T, e.Mode = sig.Results[0].Type, ModeValue
	default:
		tu := &Tuple{}
		for _, r := range sig.Results {
			tu.Types = append(tu.Types, r.Type)
		}
		e.T, e.Mode = tu, ModeValue
	}
}

func (c *Checker) args(e *CallExpr, sig *Signature) {
	params := sig.Params
	nargs := len(e.Args)
	if len(e.Args) == 1 && !e.Ellipsis {
		if call, ok := unparen(e.Args[0]).(*CallExpr); ok {
			c.expr(call)
			if tu, ok := call.T.(*Tuple); ok {
				_ = tu
				c.errorf(call.P, "passing a multi-value call as arguments is not supported; assign the results first")
				return
			}
			// already checked; fall through with the result
			c.checkArg(e.Args[0], params, 0, sig, e, true)
			c.arity(e, sig, nargs)
			return
		}
	}
	for i, a := range e.Args {
		c.checkArg(a, params, i, sig, e, false)
	}
	c.arity(e, sig, nargs)
}

func (c *Checker) arity(e *CallExpr, sig *Signature, nargs int) {
	np := len(sig.Params)
	if e.Ellipsis {
		if !sig.Variadic {
			c.errorf(e.P, "cannot use ... in call to non-variadic %s", exprString(e.Fun))
		} else if nargs != np {
			c.errorf(e.P, "wrong argument count in call to %s: have %d, want %d", exprString(e.Fun), nargs, np)
		}
		return
	}
	if sig.Variadic {
		if nargs < np-1 {
			c.errorf(e.P, "not enough arguments in call to %s: have %d, want at least %d", exprString(e.Fun), nargs, np-1)
		}
		e.Variadic = np - 1
		return
	}
	if nargs < np {
		c.errorf(e.P, "not enough arguments in call to %s: have %d, want %d", exprString(e.Fun), nargs, np)
	} else if nargs > np {
		c.errorf(e.P, "too many arguments in call to %s: have %d, want %d", exprString(e.Fun), nargs, np)
	}
}

func (c *Checker) checkArg(a Expr, params []*Var, i int, sig *Signature, e *CallExpr, checked bool) {
	var pt Type
	switch {
	case sig.Variadic && i >= len(params)-1 && !e.Ellipsis:
		pt = params[len(params)-1].Type.(*Slice).Elem
	case i < len(params):
		pt = params[i].Type
	}
	if !checked {
		c.exprHint(a, pt)
	}
	if pt == nil {
		c.value(a)
		return
	}
	c.assignable(a, pt, "argument to "+exprString(e.Fun))
}

// ---- conversions ----

func (c *Checker) conversion(e *CallExpr, t Type) {
	if _, ok := t.(*ErrorType); ok {
		// error(msg) or error(code, msg): build an error value
		c.errorCtor(e)
		return
	}
	e.Conversion = true
	if len(e.Args) != 1 {
		c.errorf(e.P, "conversion to %s needs exactly one argument", t)
		c.invalid(e)
		return
	}
	x := e.Args[0]
	if !c.valueExpr(x, t) {
		c.invalid(e)
		return
	}
	xi := x.info()
	e.T, e.Mode = t, ModeValue
	tu := t.Underlying()
	// constant conversions
	if xi.Const != nil {
		if b, ok := tu.(*Basic); ok {
			switch {
			case isString(b) && isInteger(xi.T):
				r, _ := constant.Int64Val(xi.Const)
				e.Const, e.Mode = constant.MakeString(string(rune(r))), ModeConst
				return
			case isNumeric(b) && isNumeric(xi.T):
				v := xi.Const
				if isInteger(b) && v.Kind() == constant.Float {
					iv := constant.ToInt(v)
					if iv.Kind() != constant.Int {
						c.errorf(x.Pos(), "cannot convert %s (untyped float constant) to %s (truncated)", exprString(x), t)
						c.invalid(e)
						return
					}
					v = iv
				}
				nv, ok := representable(v, t)
				if !ok {
					c.errorf(x.Pos(), "cannot convert %s (constant %s) to %s (overflows)", exprString(x), v, t)
					c.invalid(e)
					return
				}
				e.Const, e.Mode = nv, ModeConst
				setType(x, defaultType(xi.T))
				if isUntyped(xi.T) {
					xi.T = t
				}
				return
			case isString(b) && isString(xi.T), isBoolean(b) && isBoolean(xi.T):
				e.Const, e.Mode = xi.Const, ModeConst
				if isUntyped(xi.T) {
					xi.T = t
				}
				return
			}
		}
	}
	if isUntyped(xi.T) {
		if isNilType(xi.T) {
			if nilable(t) {
				xi.T = t
				return
			}
		} else if c.convertUntyped(x, defaultType(xi.T), "conversion") {
			xi = x.info()
		}
	}
	if convertible(xi.T, t) {
		return
	}
	if isString(t) && isByteSlice(xi.T) {
		// string(b) is a zero-copy view of b; `string(b) in a` copies into the arena like Go
		e.InArena = c.allocArena
		return
	}
	if isByteSlice(t) && isString(xi.T) {
		if c.allocArena != nil {
			e.InArena = c.allocArena
			return
		}
		c.errorf(e.P, "converting %s to []byte copies it: allocate the copy in an arena ([]byte(s) in a), or copy into a buffer with copy(buf[:], s)", exprString(x))
		c.invalid(e)
		return
	}
	if isString(t) && isInteger(xi.T) {
		c.errorf(e.P, "cannot convert integer %s to string at run time; use utf8.AppendRune or strconv into a buffer", exprString(x))
		c.invalid(e)
		return
	}
	c.errorf(e.P, "cannot convert %s (type %s) to type %s", exprString(x), xi.T, t)
	c.invalid(e)
}

func convertible(v, t Type) bool {
	if assignableType(v, t) {
		return true
	}
	vu, tu := v.Underlying(), t.Underlying()
	if Identical(vu, tu) {
		return true
	}
	if vp, ok := vu.(*Pointer); ok {
		if tp, ok := tu.(*Pointer); ok && Identical(vp.Elem.Underlying(), tp.Elem.Underlying()) {
			return true
		}
	}
	if isNumeric(vu) && isNumeric(tu) {
		return true
	}
	vk, tk := basicKind(vu), basicKind(tu)
	// unsafe-ish FFI conversions
	if tk == Rawptr {
		_, isPtr := vu.(*Pointer)
		return isPtr || vk == Uintptr || vk == Rawptr
	}
	if vk == Rawptr {
		_, isPtr := tu.(*Pointer)
		return isPtr || tk == Uintptr
	}
	return false
}

func (c *Checker) errorCtor(e *CallExpr) {
	e.Builtin = "__error"
	switch len(e.Args) {
	case 1:
		c.valueExpr(e.Args[0], tString)
		c.assignable(e.Args[0], tString, "error message")
	case 2:
		c.valueExpr(e.Args[0], tInt)
		c.assignable(e.Args[0], tInt, "error code")
		c.valueExpr(e.Args[1], tString)
		c.assignable(e.Args[1], tString, "error message")
	default:
		c.errorf(e.P, "error takes a message, or a code and a message: error(404, \"not found\")")
		c.invalid(e)
		return
	}
	e.T, e.Mode = tError, ModeValue
}

// ---- builtins ----

func (c *Checker) builtin(e *CallExpr, name string) {
	e.Builtin = name
	nargs := len(e.Args)
	need := func(n int) bool {
		if nargs < n {
			c.errorf(e.P, "not enough arguments for %s (expected %d, found %d)", name, n, nargs)
			c.invalid(e)
			return false
		}
		return true
	}
	exactly := func(n int) bool {
		if nargs != n {
			c.errorf(e.P, "wrong number of arguments for %s (expected %d, found %d)", name, n, nargs)
			for _, a := range e.Args {
				c.expr(a)
			}
			c.invalid(e)
			return false
		}
		return true
	}
	if e.Ellipsis && name != "append" {
		c.errorf(e.P, "invalid use of ... with built-in %s", name)
	}
	switch name {
	case "len", "cap":
		if !exactly(1) {
			return
		}
		x := e.Args[0]
		if !c.valueExpr(x, nil) {
			c.invalid(e)
			return
		}
		xi := x.info()
		if isUntyped(xi.T) && isString(xi.T) {
			c.convertUntyped(x, tString, name)
		}
		// as in Go, a fixed length is constant only if computing x calls nothing and receives nothing
		if vt, ok := xi.T.Underlying().(*VecType); ok && name == "cap" {
			e.T, e.Mode = tInt, ModeValue
			if !hasCall(x) {
				e.Mode, e.Const = ModeConst, constant.MakeInt64(vt.Len)
			}
			return
		}
		c.toView(x)
		xi = x.info()
		t := xi.T.Underlying()
		if p, ok := t.(*Pointer); ok {
			if a, ok := p.Elem.Underlying().(*Array); ok {
				t = a
			}
		}
		e.T, e.Mode = tInt, ModeValue
		switch u := t.(type) {
		case *Array:
			if !hasCall(x) {
				e.Const, e.Mode = constant.MakeInt64(u.Len), ModeConst
			}
		case *Slice, *Chan:
		case *Map:
			if name == "cap" {
				// capacity of the fixed table
			}
		case *Basic:
			if name == "cap" || !isString(u) {
				c.errorf(x.Pos(), "invalid argument %s (type %s) for %s", exprString(x), xi.T, name)
				c.invalid(e)
				return
			}
			if xi.Const != nil {
				e.Const, e.Mode = constant.MakeInt64(int64(len(constant.StringVal(xi.Const)))), ModeConst
			}
		default:
			c.errorf(x.Pos(), "invalid argument %s (type %s) for %s", exprString(x), xi.T, name)
			c.invalid(e)
		}
	case "append":
		if !need(1) {
			return
		}
		s := e.Args[0]
		if !c.valueExpr(s, nil) {
			c.invalid(e)
			return
		}
		st, ok := s.info().T.Underlying().(*Slice)
		if !ok {
			c.errorf(s.Pos(), "invalid argument: %s (type %s) is not a slice", exprString(s), s.info().T)
			c.invalid(e)
			return
		}
		e.T, e.Mode = s.info().T, ModeValue
		e.InArena = c.allocArena // with an arena, append grows instead of panicking
		if e.Ellipsis {
			if nargs != 2 {
				c.errorf(e.P, "can only use ... with final argument")
				return
			}
			y := e.Args[1]
			if !c.valueExpr(y, nil) {
				return
			}
			if isString(y.info().T) && basicKind(st.Elem) == Uint8 {
				if isUntyped(y.info().T) {
					c.convertUntyped(y, tString, "append")
				}
				return
			}
			c.assignable(y, &Slice{Elem: st.Elem}, "append")
			return
		}
		for _, a := range e.Args[1:] {
			c.exprHint(a, st.Elem)
			c.assignable(a, st.Elem, "append")
		}
	case "copy":
		if !exactly(2) {
			return
		}
		e.T, e.Mode = tInt, ModeValue
		dst, src := e.Args[0], e.Args[1]
		if !c.valueExpr(dst, nil) || !c.valueExpr(src, nil) {
			return
		}
		ds, ok := dst.info().T.Underlying().(*Slice)
		if !ok {
			c.errorf(dst.Pos(), "invalid argument: copy expects slice arguments; found %s (type %s)", exprString(dst), dst.info().T)
			return
		}
		if isString(src.info().T) && basicKind(ds.Elem) == Uint8 {
			if isUntyped(src.info().T) {
				c.convertUntyped(src, tString, "copy")
			}
			return
		}
		ss, ok := src.info().T.Underlying().(*Slice)
		if !ok || !Identical(ss.Elem, ds.Elem) {
			c.errorf(src.Pos(), "invalid argument: arguments to copy %s (type %s) and %s (type %s) have different element types", exprString(dst), dst.info().T, exprString(src), src.info().T)
		}
	case "make":
		c.makeBuiltin(e)
	case "new":
		if c.allocArena == nil {
			c.errorf(e.P, "new(T) needs memory: allocate it in an arena (new(T) in a), or declare a variable and use &x")
			c.invalid(e)
			return
		}
		if !exactly(1) {
			return
		}
		t := c.typExpr(e.Args[0])
		e.InArena = c.allocArena
		e.T, e.Mode = &Pointer{Elem: t}, ModeValue
	case "delete":
		if !exactly(2) {
			return
		}
		e.T, e.Mode = tInvalid, ModeNoValue
		if !c.valueExpr(e.Args[0], nil) {
			return
		}
		c.toView(e.Args[0])
		m, ok := e.Args[0].info().T.Underlying().(*Map)
		if !ok {
			c.errorf(e.Args[0].Pos(), "invalid argument: %s is not a map", exprString(e.Args[0]))
			return
		}
		c.valueExpr(e.Args[1], m.Key)
		c.assignable(e.Args[1], m.Key, "argument to delete")
	case "close":
		if !exactly(1) {
			return
		}
		e.T, e.Mode = tInvalid, ModeNoValue
		if c.valueExpr(e.Args[0], nil) {
			c.toView(e.Args[0])
			if _, ok := e.Args[0].info().T.Underlying().(*Chan); !ok {
				c.errorf(e.Args[0].Pos(), "invalid operation: non-chan argument to close")
			}
		}
	case "panic":
		if !exactly(1) {
			return
		}
		e.T, e.Mode = tInvalid, ModeNoValue
		if !c.valueExpr(e.Args[0], nil) {
			return
		}
		c.defaultUntyped(e.Args[0])
		t := e.Args[0].info().T
		if !isString(t) && !isNumeric(t) && !isBoolean(t) {
			if _, ok := t.Underlying().(*ErrorType); !ok {
				c.errorf(e.Args[0].Pos(), "panic argument must be a string, number, bool or error (have %s)", t)
			}
		}
	case "print", "println":
		e.T, e.Mode = tInvalid, ModeNoValue
		for _, a := range e.Args {
			if !c.valueExpr(a, nil) {
				continue
			}
			c.defaultUntyped(a)
			t := a.info().T
			switch t.Underlying().(type) {
			case *Basic, *Pointer, *ErrorType:
				if isNilType(t) {
					c.errorf(a.Pos(), "use of untyped nil in %s", name)
				}
			default:
				if !isByteSlice(t) {
					c.errorf(a.Pos(), "cannot print %s (type %s)", exprString(a), t)
				}
			}
		}
	case "min", "max":
		if !need(1) {
			return
		}
		for _, a := range e.Args {
			c.valueExpr(a, nil)
		}
		var t Type
		for _, a := range e.Args {
			if !isUntyped(a.info().T) {
				t = a.info().T
				break
			}
		}
		if t == nil {
			// all untyped: as in Go, the widest kind wins (min(3, 2.5) is 2.5), and constant
			// arguments give a constant
			k := basicKind(e.Args[0].info().T)
			allConst := true
			for _, a := range e.Args {
				if kk := basicKind(a.info().T); kk > k && kk != UntypedString && kk != UntypedBool {
					k = kk
				}
				allConst = allConst && a.info().Const != nil && isString(a.info().T) == isString(e.Args[0].info().T)
			}
			for _, b := range []*Basic{tUntypedInt, tUntypedRune, tUntypedFloat, tUntypedString} {
				if b.Kind == k {
					t = b
				}
			}
			if t == nil {
				t = defaultType(e.Args[0].info().T)
			}
			if allConst && (isNumeric(t) || isString(t)) {
				best := e.Args[0].info().Const
				for _, a := range e.Args[1:] {
					v := a.info().Const
					if (name == "min" && constant.Compare(v, gotoken.LSS, best)) || (name == "max" && constant.Compare(v, gotoken.GTR, best)) {
						best = v
					}
				}
				if isFloat(t) {
					best = constant.ToFloat(best)
				}
				e.T, e.Mode, e.Const = t, ModeConst, best
				return
			}
			t = defaultType(t)
		}
		for _, a := range e.Args {
			c.assignable(a, t, name)
		}
		if !isNumeric(t) && !isString(t) {
			c.errorf(e.P, "invalid argument for %s: %s is not ordered", name, t)
		}
		e.T, e.Mode = t, ModeValue
	case "__error":
		if !exactly(2) {
			return
		}
		c.valueExpr(e.Args[0], tInt)
		c.assignable(e.Args[0], tInt, "__error")
		c.valueExpr(e.Args[1], tString)
		c.assignable(e.Args[1], tString, "__error")
		e.T, e.Mode = tError, ModeValue
	default:
		c.errorf(e.P, "unknown builtin %s", name)
		c.invalid(e)
	}
}

func (c *Checker) makeBuiltin(e *CallExpr) {
	if len(e.Args) == 0 {
		c.errorf(e.P, "not enough arguments for make")
		c.invalid(e)
		return
	}
	t := c.typExpr(e.Args[0])
	if c.allocArena == nil {
		hint := "declare storage instead"
		switch u := t.Underlying().(type) {
		case *Slice:
			hint = fmt.Sprintf("declare an array (var buf [N]%s) or a vector (var v [..N]%s) and use a view of it", u.Elem, u.Elem)
		case *Map:
			hint = fmt.Sprintf("declare storage: var m map[%s]%s cap N", u.Key, u.Value)
		case *Chan:
			hint = fmt.Sprintf("declare storage at package level: var ch chan %s cap N", u.Elem)
		}
		c.errorf(e.P, "make needs an arena (make(...) in a); otherwise %s", hint)
		c.invalid(e)
		return
	}
	e.T, e.Mode = t, ModeValue
	sizes := []int64{}
	for _, a := range e.Args[1:] {
		if !c.valueExpr(a, nil) {
			c.invalid(e)
			return
		}
		if !isInteger(a.info().T) {
			c.errorf(a.Pos(), "make size %s must be an integer", exprString(a))
			c.invalid(e)
			return
		}
		v, ok := constInt(a)
		if !ok {
			if c.allocArena == nil {
				c.errorf(a.Pos(), "make size %s is not a constant: allocate it in an arena (make(...) in a), or use a fixed capacity", exprString(a))
				c.invalid(e)
				return
			}
			v = -1 // run-time size
		} else if v < 0 {
			c.errorf(a.Pos(), "negative size argument in make")
		}
		c.convertUntyped(a, tInt, "make")
		sizes = append(sizes, v)
	}
	e.InArena = c.allocArena
	switch t.Underlying().(type) {
	case *Slice:
		if len(sizes) == 0 {
			c.errorf(e.P, "make(%s) needs a length", t)
		}
		if len(sizes) == 2 && sizes[0] >= 0 && sizes[1] >= 0 && sizes[1] < sizes[0] {
			c.errorf(e.P, "len larger than cap in make(%s)", t)
		}
		if len(sizes) > 2 {
			c.errorf(e.P, "too many arguments to make(%s)", t)
		}
	case *Map:
		if len(sizes) != 1 {
			c.errorf(e.P, "make(%s) needs exactly one constant capacity argument: maps have a fixed capacity", t)
		}
	case *Chan:
		if len(sizes) > 1 {
			c.errorf(e.P, "too many arguments to make(%s)", t)
		}
		if c.fn != nil || e.InArena != nil {
			c.errorf(e.P, "make(%s) is only allowed in package-level variable declarations: channels are shared between goroutines, so their buffers live in static memory", t)
		}
	default:
		c.errorf(e.P, "invalid argument: cannot make %s; type must be slice, map, or channel", t)
		c.invalid(e)
		return
	}
	e.StorageID = len(c.prog.MakeSites)
	c.prog.MakeSites = append(c.prog.MakeSites, e)
	e.Variadic = -1
}
