package compiler

import (
	"go/constant"
	gotoken "go/token"
	"strconv"
)

func (c *Checker) expr(x Expr) { c.exprHint(x, nil) }

func (c *Checker) invalid(x Expr) {
	x.info().T = tInvalid
	x.info().Mode = ModeInvalid
}

// exprHint checks x; hint is the expected type (used for elided composite literal types).
func (c *Checker) exprHint(x Expr, hint Type) {
	c.expr1(x, hint)
	xi := x.info()
	if xi.T == nil {
		xi.T = tInvalid
	}
	if xi.Mode == ModeType {
		// callers that accept types use typExpr; types in value position are errors except
		// where explicitly allowed (conversions, make); flagged by callers.
	}
}

// value checks that x is a single value (not a type, package, builtin or no-value call).
func (c *Checker) value(x Expr) bool {
	xi := x.info()
	switch xi.Mode {
	case ModeInvalid:
		return false
	case ModeType:
		c.errorf(x.Pos(), "%s (type) is not an expression", exprString(x))
	case ModePkg:
		c.errorf(x.Pos(), "use of package %s without selector", exprString(x))
	case ModeBuiltin:
		c.errorf(x.Pos(), "%s (built-in function) must be called", exprString(x))
	case ModeNoValue:
		c.errorf(x.Pos(), "%s (no value) used as value", exprString(x))
	default:
		if _, ok := xi.T.(*Tuple); ok {
			c.errorf(x.Pos(), "multiple-value %s in single-value context", exprString(x))
		} else {
			return true
		}
	}
	c.invalid(x)
	return false
}

func (c *Checker) valueExpr(x Expr, hint Type) bool {
	c.exprHint(x, hint)
	return c.value(x)
}

func (c *Checker) expr1(x Expr, hint Type) {
	switch e := x.(type) {
	case *Ident:
		c.ident(e)
	case *BasicLit:
		switch e.Kind {
		case INT:
			lit, mult := e.Lit, int64(1)
			switch lit[len(lit)-1] {
			case 'K':
				mult = 1 << 10
			case 'M':
				mult = 1 << 20
			case 'G':
				mult = 1 << 30
			}
			if mult > 1 {
				lit = lit[:len(lit)-1]
			}
			v := constant.MakeFromLiteral(lit, gotoken.INT, 0)
			if mult > 1 {
				v = constant.BinaryOp(v, gotoken.MUL, constant.MakeInt64(mult))
			}
			if v.Kind() == constant.Unknown {
				c.errorf(e.P, "invalid integer literal %s", e.Lit)
			}
			e.T, e.Const = tUntypedInt, v
		case FLOAT:
			v := constant.MakeFromLiteral(e.Lit, gotoken.FLOAT, 0)
			if v.Kind() == constant.Unknown {
				c.errorf(e.P, "invalid floating-point literal %s", e.Lit)
			}
			e.T, e.Const = tUntypedFloat, v
		case CHAR:
			r := []rune(e.Lit)
			e.T, e.Const = tUntypedRune, constant.MakeInt64(int64(r[0]))
		case STRING:
			e.T, e.Const = tUntypedString, constant.MakeString(e.Lit)
		default:
			c.errorf(e.P, "unexpected %s", e.Lit)
			c.invalid(e)
			return
		}
		e.Mode = ModeConst
	case *ParenExpr:
		c.exprHint(e.X, hint)
		e.ExprInfo = *e.X.info()
	case *CompositeLit:
		c.compositeLit(e, hint)
	case *SelectorExpr:
		c.selector(e, false)
	case *IndexExpr:
		c.index(e)
	case *SliceExpr:
		c.sliceExpr(e)
	case *CallExpr:
		c.call(e)
	case *StarExpr:
		c.expr(e.X)
		if e.X.info().Mode == ModeType {
			e.T = &Pointer{Elem: e.X.info().T}
			e.Mode = ModeType
			return
		}
		if !c.value(e.X) {
			c.invalid(e)
			return
		}
		p, ok := e.X.info().T.Underlying().(*Pointer)
		if !ok {
			c.errorf(e.P, "invalid indirect of %s (type %s)", exprString(e.X), e.X.info().T)
			c.invalid(e)
			return
		}
		e.T = p.Elem
		e.Mode = ModeVar
	case *UnaryExpr:
		c.unary(e, hint)
	case *BinaryExpr:
		c.binary(e)
	case *KeyValueExpr:
		c.errorf(e.Pos(), "unexpected key:value expression")
		c.invalid(e)
	case *TryExpr:
		c.tryExpr(e)
	case *InExpr:
		c.inExpr(e, hint)
	case *ArrayType, *MapType, *ChanType, *StructType, *FuncType:
		t := c.typExpr(x)
		x.info().T = t
		x.info().Mode = ModeType
	default:
		c.errorf(x.Pos(), "unexpected expression")
		c.invalid(x)
	}
}

// toView turns a storage expression ([..N]T, map ... cap N, chan ... cap N) into its view
// ([]T, map[K]V, chan T). Storage must be addressable: the view points into it.
func (c *Checker) toView(x Expr) bool {
	xi := x.info()
	v := viewOf(xi.T)
	if v == nil || xi.View != nil {
		return false
	}
	if xi.Mode != ModeVar {
		c.errorf(x.Pos(), "%s (%s) must be stored in a variable to be used as a %s", exprString(x), xi.T, v)
		return false
	}
	xi.View = xi.T
	xi.T = v
	xi.Mode = ModeValue
	if p, ok := x.(*ParenExpr); ok {
		c.toView(p.X)
	}
	return true
}

// isStorageCopy reports whether t is storage that must not be copied (maps and channels hold
// pointers into themselves).
func isStorageCopy(t Type) bool {
	switch t.Underlying().(type) {
	case *MapStore, *ChanStore:
		return true
	}
	return false
}

// storageIn returns the map or channel storage that t holds by value inside a struct field, array
// or vector element, or nil. Once used, that storage points into itself, so a copy would alias it.
func storageIn(t Type) Type {
	return storageInSeen(t, map[Type]bool{})
}

func storageInSeen(t Type, seen map[Type]bool) Type {
	if seen[t] {
		return nil
	}
	seen[t] = true
	switch u := t.Underlying().(type) {
	case *MapStore, *ChanStore:
		return t
	case *Array:
		return storageInSeen(u.Elem, seen)
	case *VecType:
		return storageInSeen(u.Elem, seen)
	case *Struct:
		for _, f := range u.Fields {
			if s := storageInSeen(f.Type, seen); s != nil {
				return s
			}
		}
	}
	return nil
}

// freshValue reports whether x builds a new value rather than reading an existing one. Storage in a
// new value has not been used yet, so copying it is safe.
func freshValue(x Expr) bool {
	switch x := unparen(x).(type) {
	case *CompositeLit:
		return true
	case *CallExpr:
		return x.Builtin == "" && !x.Conversion
	}
	return false
}

// checkNestedStorageCopy reports copying x (of type t, a struct or array holding map or channel
// storage) out of an existing variable. Top-level storage is handled by isStorageCopy.
func (c *Checker) checkNestedStorageCopy(x Expr, t Type, into string) bool {
	if isStorageCopy(t) || freshValue(x) {
		return true
	}
	if s := storageIn(t); s != nil {
		c.errorf(x.Pos(), "cannot copy %s%s: %s contains %s storage; use a pointer (*%s) instead", exprString(x), into, t, s, t)
		return false
	}
	return true
}

// inExpr checks `x in a`: operations in x that need memory allocate from arena a.
func (c *Checker) inExpr(e *InExpr, hint Type) {
	if c.valueExpr(e.Arena, nil) {
		if _, ok := e.Arena.info().T.Underlying().(*ArenaType); !ok {
			c.errorf(e.Arena.Pos(), "%s is not an arena (type %s)", exprString(e.Arena), e.Arena.info().T)
		}
	}
	saved := c.allocArena
	c.allocArena = e.Arena
	c.exprHint(e.X, hint)
	c.allocArena = saved
	e.ExprInfo = *e.X.info()
	if e.Mode == ModeVar {
		e.Mode = ModeValue
	}
}

// tryExpr checks `try call`: the call's last result must be an error, which is returned from the
// enclosing function (whose last result must also be error) when non-nil.
func (c *Checker) tryExpr(e *TryExpr) {
	c.expr(e.X)
	xi := e.X.info()
	if xi.Mode == ModeInvalid {
		c.invalid(e)
		return
	}
	if _, ok := unparen(e.X).(*CallExpr); !ok {
		c.errorf(e.P, "try needs a function call")
		c.invalid(e)
		return
	}
	var results []Type
	switch t := xi.T.(type) {
	case *Tuple:
		results = t.Types
	default:
		if xi.Mode != ModeNoValue {
			results = []Type{t}
		}
	}
	if len(results) == 0 {
		c.errorf(e.P, "try needs a call whose last result is an error; %s returns nothing", exprString(e.X))
		c.invalid(e)
		return
	}
	if _, ok := results[len(results)-1].Underlying().(*ErrorType); !ok {
		c.errorf(e.P, "try needs a call whose last result is an error; %s returns %s", exprString(e.X), results[len(results)-1])
		c.invalid(e)
		return
	}
	if c.sig == nil || len(c.sig.Results) == 0 {
		c.errorf(e.P, "try can only be used in a function whose last result is error")
	} else if _, ok := c.sig.Results[len(c.sig.Results)-1].Type.Underlying().(*ErrorType); !ok {
		c.errorf(e.P, "try can only be used in a function whose last result is error")
	}
	rest := results[:len(results)-1]
	switch len(rest) {
	case 0:
		e.T, e.Mode = tInvalid, ModeNoValue
	case 1:
		e.T, e.Mode = rest[0], ModeValue
	default:
		e.T, e.Mode = &Tuple{Types: rest}, ModeValue
	}
}

func (c *Checker) ident(e *Ident) {
	if e.Name == "_" {
		c.errorf(e.P, "cannot use _ as value")
		c.invalid(e)
		return
	}
	o := c.lookup(e.Name)
	if o == nil {
		c.errorf(e.P, "undefined: %s", e.Name)
		c.invalid(e)
		return
	}
	e.Obj = o
	switch o.Kind {
	case ObjVar:
		o.Var.Used = true
		e.T = o.Var.Type
		if e.T == nil {
			c.errorf(e.P, "%s used before its type is known (initialization order)", e.Name)
			e.T = tInvalid
		}
		e.Mode = ModeVar
	case ObjConst:
		if o.Name == "iota" && o.Pkg == nil {
			if c.iota < 0 {
				c.errorf(e.P, "cannot use iota outside constant declaration")
				c.invalid(e)
				return
			}
			e.T, e.Const, e.Mode = tUntypedInt, constant.MakeInt64(c.iota), ModeConst
			return
		}
		c.resolveConst(o)
		e.T = o.Type
		e.Const = o.Const.(constant.Value)
		e.Mode = ModeConst
	case ObjType:
		if n, ok := o.Type.(*Named); ok && n.Obj.state == 0 {
			c.resolveNamed(n)
		}
		e.T = o.Type
		e.Mode = ModeType
	case ObjFunc:
		e.T = o.Func.Sig
		if e.T == nil {
			e.T = tInvalid
		}
		e.Mode = ModeValue
		if c.calleeIdent == e {
			return
		}
		o.Func.AddrTaken = true
		if o.Func.Extern != "" {
			c.errorf(e.P, "cannot take the value of extern function %s", e.Name)
		}
	case ObjPkg:
		e.Mode = ModePkg
		e.T = tInvalid
	case ObjBuiltin:
		e.Mode = ModeBuiltin
		e.T = tInvalid
	case ObjNil:
		e.T = tUntypedNil
		e.Mode = ModeValue
	}
}

// ---- conversion of untyped values / assignability ----

func (c *Checker) assignable(x Expr, t Type, context string) bool {
	xi := x.info()
	if xi.Mode == ModeInvalid || xi.T == tInvalid || t == tInvalid {
		return false
	}
	if !c.value(x) {
		return false
	}
	if isUntyped(xi.T) {
		return c.convertUntyped(x, t, context)
	}
	if v := viewOf(xi.T); v != nil && assignableType(v, t) {
		return c.toView(x)
	}
	if assignableType(xi.T, t) {
		if isStorageCopy(t) {
			c.errorf(x.Pos(), "cannot copy %s (%s): pass or store a view (%s) instead", exprString(x), xi.T, viewOf(t))
			return false
		}
		return c.checkNestedStorageCopy(x, t, "")
	}
	c.errorf(x.Pos(), "cannot use %s (type %s) as type %s in %s", exprString(x), xi.T, t, context)
	return false
}

func assignableType(v, t Type) bool {
	if Identical(v, t) {
		return true
	}
	_, vn := v.(*Named)
	_, tn := t.(*Named)
	if (!vn || !tn) && Identical(v.Underlying(), t.Underlying()) {
		if _, isBasic := v.Underlying().(*Basic); isBasic && (vn || tn) {
			return false // named basic types need explicit conversion
		}
		return true
	}
	return false
}

func nilable(t Type) bool {
	switch u := t.Underlying().(type) {
	case *Pointer, *Slice, *Map, *Chan, *Signature, *ErrorType, *ArenaType:
		return true
	case *Basic:
		return u.Kind == Rawptr
	}
	return false
}

func (c *Checker) convertUntyped(x Expr, t Type, context string) bool {
	xi := x.info()
	if isNilType(xi.T) {
		if !nilable(t) {
			c.errorf(x.Pos(), "cannot use nil as type %s in %s", t, context)
			return false
		}
		setType(x, t)
		return true
	}
	if xi.Const != nil {
		if _, ok := t.Underlying().(*Basic); !ok {
			c.errorf(x.Pos(), "cannot use %s (untyped constant) as type %s in %s", exprString(x), t, context)
			return false
		}
		k := basicKind(t)
		src := basicKind(xi.T)
		if (src == UntypedString) != (k == String || k == UntypedString) || (src == UntypedBool) != (k == Bool || k == UntypedBool) {
			c.errorf(x.Pos(), "cannot use %s (untyped %s constant) as type %s in %s", exprString(x), kindName(src), t, context)
			return false
		}
		v, ok := representable(xi.Const, t)
		if !ok {
			if v.Kind() == constant.Float && isInteger(t) {
				c.errorf(x.Pos(), "constant %s truncated to integer", xi.Const)
			} else {
				c.errorf(x.Pos(), "constant %s overflows %s", xi.Const, t)
			}
			return false
		}
		xi.Const = v
		setType(x, t)
		return true
	}
	// untyped non-constant (bool from comparison)
	if isBoolean(xi.T) && isBoolean(t) {
		setType(x, t)
		return true
	}
	// untyped non-constant number: a shift of an untyped constant by a variable (1 << n), maybe
	// inside arithmetic; the shifted constant takes the context's type
	switch u := unparen(x).(type) {
	case *BinaryExpr:
		if u.Op == SHL || u.Op == SHR {
			if !isInteger(t) {
				c.errorf(x.Pos(), "invalid operation: shifted operand %s (type %s) must be integer", exprString(u.X), t)
				return false
			}
			if !c.convertUntyped(u.X, t, context) {
				return false
			}
		} else if !isComparison(u.Op) {
			for _, o := range []Expr{u.X, u.Y} {
				if isUntyped(o.info().T) && !c.convertUntyped(o, t, context) {
					return false
				}
			}
		}
		setType(x, t)
		return true
	case *UnaryExpr:
		if isUntyped(u.X.info().T) && !c.convertUntyped(u.X, t, context) {
			return false
		}
		setType(x, t)
		return true
	}
	if !isUntyped(t) && basicKind(xi.T) == basicKind(defaultType(xi.T)) {
		setType(x, t)
		return true
	}
	c.errorf(x.Pos(), "cannot use %s (type %s) as type %s in %s", exprString(x), xi.T, t, context)
	return false
}

func setType(x Expr, t Type) {
	x.info().T = t
	if p, ok := x.(*ParenExpr); ok {
		setType(p.X, t)
	}
}

func kindName(k BasicKind) string {
	switch k {
	case UntypedString:
		return "string"
	case UntypedBool:
		return "bool"
	case UntypedFloat:
		return "float"
	case UntypedRune:
		return "rune"
	}
	return "int"
}

func (c *Checker) defaultUntyped(x Expr) {
	xi := x.info()
	if xi.Mode == ModeInvalid || !isUntyped(xi.T) || isNilType(xi.T) {
		return
	}
	d := defaultType(xi.T)
	if !c.convertUntyped(x, d, "assignment") {
		setType(x, d) // already reported; don't let the error spread to uses of a variable
	}
}

// ---- composite literals ----

func (c *Checker) compositeLit(e *CompositeLit, hint Type) {
	var t Type
	if e.Type == nil {
		if hint == nil {
			c.errorf(e.P, "invalid composite literal type: missing type")
			c.invalid(e)
			return
		}
		t = hint
		if p, ok := hint.Underlying().(*Pointer); ok {
			// &T elided inside []*T{{...}} is not supported: it would need hidden storage per element
			_ = p
			c.errorf(e.P, "elided &T{...} composite literals are not supported; write &T{...} explicitly")
			c.invalid(e)
			return
		}
	} else if at, ok := e.Type.(*ArrayType); ok && at.Len != nil {
		if bl, ok := at.Len.(*BasicLit); ok && bl.Kind == ELLIPSIS {
			elem := c.typExpr(at.Elem)
			n := int64(0)
			idx := int64(0)
			for _, el := range e.Elts {
				if kv, ok := el.(*KeyValueExpr); ok {
					c.expr(kv.Key)
					if kv.Key.info().Const != nil {
						if v, ok := constant.Int64Val(constant.ToInt(kv.Key.info().Const)); ok {
							idx = v
						}
					}
					if idx < 0 { // reported by indexedElts, which skips the element the same way
						idx = 0
						continue
					}
				}
				idx++
				if idx > n {
					n = idx
				}
			}
			t = &Array{Len: n, Elem: elem}
			at.T = t
			at.Mode = ModeType
		}
	}
	if t == nil {
		t = c.typExpr(e.Type)
	}
	e.T = t
	e.Mode = ModeValue
	switch u := t.Underlying().(type) {
	case *Struct:
		if len(e.Elts) == 0 {
			return
		}
		named, _ := t.(*Named)
		foreign := named != nil && named.Pkg != c.pkg
		if _, ok := e.Elts[0].(*KeyValueExpr); ok {
			seen := map[string]bool{}
			for _, el := range e.Elts {
				kv, ok := el.(*KeyValueExpr)
				if !ok {
					c.errorf(el.Pos(), "mixture of field:value and value elements in struct literal")
					continue
				}
				id, ok := kv.Key.(*Ident)
				if !ok {
					c.errorf(kv.Key.Pos(), "invalid field name %s in struct literal", exprString(kv.Key))
					continue
				}
				fi := -1
				for i, f := range u.Fields {
					if f.Name == id.Name {
						fi = i
					}
				}
				if fi < 0 {
					c.errorf(id.P, "unknown field %s in struct literal of type %s", id.Name, t)
					continue
				}
				if foreign && !isExported(id.Name) {
					c.errorf(id.P, "cannot refer to unexported field %s in struct literal of type %s", id.Name, t)
				}
				if seen[id.Name] {
					c.errorf(id.P, "duplicate field name %s in struct literal", id.Name)
				}
				seen[id.Name] = true
				kv.Key.info().Mode = ModeValue
				kv.T = u.Fields[fi].Type
				c.exprHint(kv.Value, u.Fields[fi].Type)
				c.assignable(kv.Value, u.Fields[fi].Type, "struct literal")
				id.Obj = &Object{Kind: ObjVar, Name: id.Name, Const: fi}
			}
			return
		}
		if len(e.Elts) != len(u.Fields) {
			c.errorf(e.P, "too few values in struct literal of type %s", t)
			return
		}
		for i, el := range e.Elts {
			if _, ok := el.(*KeyValueExpr); ok {
				c.errorf(el.Pos(), "mixture of field:value and value elements in struct literal")
				continue
			}
			if foreign && !isExported(u.Fields[i].Name) {
				c.errorf(el.Pos(), "implicit assignment to unexported field %s in struct literal of type %s", u.Fields[i].Name, t)
			}
			c.exprHint(el, u.Fields[i].Type)
			c.assignable(el, u.Fields[i].Type, "struct literal")
		}
	case *Array:
		c.indexedElts(e.Elts, u.Elem, u.Len)
	case *Slice:
		c.indexedElts(e.Elts, u.Elem, -1)
	case *VecType:
		c.indexedElts(e.Elts, u.Elem, u.Len)
	case *Map:
		c.errorf(e.P, "map literals are not supported; use make(map[K]V, capacity) and assign entries")
	default:
		if t != tInvalid {
			c.errorf(e.P, "invalid composite literal type %s", t)
		}
		c.invalid(e)
	}
}

func (c *Checker) indexedElts(elts []Expr, elem Type, length int64) int64 {
	idx, max := int64(0), int64(0)
	seen := map[int64]bool{}
	for _, el := range elts {
		v := el
		if kv, ok := el.(*KeyValueExpr); ok {
			c.expr(kv.Key)
			kc := kv.Key.info().Const
			if kc == nil || !isInteger(kv.Key.info().T) {
				c.errorf(kv.Key.Pos(), "index %s must be integer constant", exprString(kv.Key))
			} else {
				c.convertUntyped(kv.Key, tInt, "index")
				idx, _ = constant.Int64Val(constant.ToInt(kc))
			}
			v = kv.Value
			kv.T = elem
			if idx < 0 {
				c.errorf(kv.Key.Pos(), "index %s must be non-negative integer constant", exprString(kv.Key))
				c.exprHint(v, elem)
				c.assignable(v, elem, "array or slice literal")
				idx = 0
				continue
			}
		}
		if length >= 0 && idx >= length {
			c.errorf(v.Pos(), "index %d out of bounds [0:%d]", idx, length)
		}
		if seen[idx] {
			c.errorf(v.Pos(), "duplicate index %d in array or slice literal", idx)
		}
		seen[idx] = true
		c.exprHint(v, elem)
		c.assignable(v, elem, "array or slice literal")
		idx++
		if idx > max {
			max = idx
		}
	}
	return max
}

// ---- selectors ----

// lookupFieldOrMethod finds name in t (auto-dereferencing one pointer level).
func lookupFieldOrMethod(t Type) (st *Struct, named *Named, viaPtr bool) {
	if p, ok := t.Underlying().(*Pointer); ok {
		if _, isNamedPtr := t.(*Named); !isNamedPtr {
			t = p.Elem
			viaPtr = true
		}
	}
	named, _ = t.(*Named)
	st, _ = t.Underlying().(*Struct)
	return
}

func (c *Checker) selector(e *SelectorExpr, callee bool) {
	if id, ok := e.X.(*Ident); ok {
		if o := c.lookup(id.Name); o != nil && o.Kind == ObjPkg {
			pkg := o.PkgRef
			id.Obj = o
			id.Mode = ModePkg
			obj := pkg.Scope.LookupLocal(e.Sel.Name)
			if obj == nil {
				c.errorf(e.Sel.P, "undefined: %s.%s", id.Name, e.Sel.Name)
				c.invalid(e)
				return
			}
			if !isExported(e.Sel.Name) {
				c.errorf(e.Sel.P, "%s.%s is not exported", id.Name, e.Sel.Name)
			}
			e.Kind = SelQualified
			e.Sel.Obj = obj
			switch obj.Kind {
			case ObjConst:
				c.resolveConst(obj)
				e.T, e.Const, e.Mode = obj.Type, obj.Const.(constant.Value), ModeConst
			case ObjVar:
				e.T, e.Mode = obj.Var.Type, ModeVar
			case ObjType:
				e.T, e.Mode = obj.Type, ModeType
			case ObjFunc:
				e.T, e.Mode = obj.Func.Sig, ModeValue
				if !callee {
					obj.Func.AddrTaken = true
					if obj.Func.Extern != "" {
						c.errorf(e.Sel.P, "cannot take the value of extern function %s", exprString(e))
					}
				}
			}
			e.Sel.T = e.T
			return
		}
	}
	c.expr(e.X)
	xi := e.X.info()
	if xi.Mode == ModeInvalid {
		c.invalid(e)
		return
	}
	if xi.Mode == ModeType {
		c.errorf(e.Pos(), "method expressions are not supported (%s)", exprString(e))
		c.invalid(e)
		return
	}
	if !c.value(e.X) {
		c.invalid(e)
		return
	}
	name := e.Sel.Name
	t := xi.T
	if _, ok := t.Underlying().(*ErrorType); ok {
		switch name {
		case "Error":
			e.Kind = SelBuiltinMethod
			e.T = &Signature{Results: []*Var{{Type: tString}}}
		case "Code":
			e.Kind = SelBuiltinMethod
			e.T = &Signature{Results: []*Var{{Type: tInt}}}
		default:
			c.errorf(e.Sel.P, "%s.%s undefined (type error has methods Error and Code)", exprString(e.X), name)
			c.invalid(e)
			return
		}
		e.Mode = ModeValue
		if !callee {
			c.errorf(e.Sel.P, "method values are not supported; call %s() directly", exprString(e))
		}
		return
	}
	if vt, ok := t.Underlying().(*VecType); ok && isVecMethod(name) {
		var sig *Signature
		switch name {
		case "Push":
			sig = &Signature{Params: []*Var{{Type: vt.Elem}}}
		case "TryPush":
			sig = &Signature{Params: []*Var{{Type: vt.Elem}}, Results: []*Var{{Type: tBool}}}
		case "Pop":
			sig = &Signature{Results: []*Var{{Type: vt.Elem}}}
		case "Clear":
			sig = &Signature{}
		default:
			c.errorf(e.Sel.P, "%s.%s undefined (vectors have Push, TryPush, Pop and Clear)", exprString(e.X), name)
			c.invalid(e)
			return
		}
		if xi.Mode != ModeVar {
			c.errorf(e.Pos(), "cannot call %s on %s (not addressable)", name, exprString(e.X))
		}
		e.Kind = SelBuiltinMethod
		e.T, e.Mode = sig, ModeValue
		if !callee {
			c.errorf(e.Sel.P, "method values are not supported; call %s(...) directly", exprString(e))
		}
		return
	}
	if _, ok := t.Underlying().(*ArenaType); ok {
		switch name {
		case "Used", "Left", "Cap":
			e.Kind = SelBuiltinMethod
			e.T = &Signature{Results: []*Var{{Type: tInt}}}
			e.Mode = ModeValue
			if !callee {
				c.errorf(e.Sel.P, "method values are not supported; call %s() directly", exprString(e))
			}
			return
		}
		c.errorf(e.Sel.P, "%s.%s undefined (type arena has methods Used, Left and Cap)", exprString(e.X), name)
		c.invalid(e)
		return
	}
	st, named, viaPtr := lookupFieldOrMethod(t)
	if st != nil {
		for i, f := range st.Fields {
			if f.Name == name {
				if named != nil && named.Pkg != c.pkg && !isExported(name) {
					c.errorf(e.Sel.P, "%s.%s undefined (cannot refer to unexported field %s)", exprString(e.X), name, name)
				}
				e.Kind = SelField
				e.Path = []int{i}
				e.T = f.Type
				if viaPtr {
					e.Implicit = "*"
					e.Mode = ModeVar
				} else if xi.Mode == ModeVar {
					e.Mode = ModeVar
				} else {
					e.Mode = ModeValue
				}
				return
			}
		}
	}
	if named != nil {
		for _, m := range named.Methods {
			if m.Name != name {
				continue
			}
			if named.Pkg != c.pkg && !isExported(name) {
				c.errorf(e.Sel.P, "%s.%s undefined (cannot refer to unexported method %s)", exprString(e.X), name, name)
			}
			e.Kind = SelMethod
			e.Method = m
			e.T = m.Sig
			e.Mode = ModeValue
			_, ptrRecv := m.Sig.Recv.Type.(*Pointer)
			switch {
			case ptrRecv && !viaPtr:
				if xi.Mode != ModeVar {
					c.errorf(e.Pos(), "cannot call pointer method %s on %s (not addressable)", name, exprString(e.X))
				} else if v, root := readOnlyBase(e.X); v != nil {
					c.errorf(e.Pos(), "cannot call pointer method %s on %s (%s is a constant)", name, exprString(e.X), exprString(root))
				}
				e.Implicit = "&"
			case !ptrRecv && viaPtr:
				e.Implicit = "*"
			}
			if !callee {
				c.errorf(e.Sel.P, "method values are not supported (they would need a closure); call %s(...) directly", exprString(e))
			}
			return
		}
	}
	if _, ok := t.Underlying().(*VecType); ok {
		c.errorf(e.Sel.P, "%s.%s undefined (vectors have Push, TryPush, Pop and Clear)", exprString(e.X), name)
	} else {
		c.errorf(e.Sel.P, "%s.%s undefined (type %s has no field or method %s)", exprString(e.X), name, t, name)
	}
	c.invalid(e)
}

func isVecMethod(name string) bool {
	return name == "Push" || name == "TryPush" || name == "Pop" || name == "Clear"
}

// ---- index & slice ----

func (c *Checker) checkIndex(x Expr, length int64) {
	if !c.valueExpr(x, nil) {
		return
	}
	xi := x.info()
	if !isInteger(xi.T) {
		c.errorf(x.Pos(), "invalid index %s (type %s must be integer)", exprString(x), xi.T)
		return
	}
	if xi.Const != nil {
		c.convertUntyped(x, tInt, "index")
		v, _ := constant.Int64Val(constant.ToInt(xi.Const))
		if v < 0 {
			c.errorf(x.Pos(), "invalid index %s (index must be non-negative)", exprString(x))
		} else if length >= 0 && v >= length {
			c.errorf(x.Pos(), "invalid index %s (out of bounds for %d-element array)", exprString(x), length)
		}
	}
}

func (c *Checker) index(e *IndexExpr) {
	c.expr(e.X)
	if e.X.info().Mode == ModeType {
		c.errorf(e.P, "generic types are not supported")
		c.invalid(e)
		return
	}
	if !c.value(e.X) {
		c.invalid(e)
		return
	}
	c.toView(e.X)
	xi := e.X.info()
	t := xi.T.Underlying()
	if p, ok := t.(*Pointer); ok {
		if a, ok := p.Elem.Underlying().(*Array); ok {
			c.checkIndex(e.Index, a.Len)
			e.T, e.Mode = a.Elem, ModeVar
			return
		}
	}
	switch u := t.(type) {
	case *Array:
		c.checkIndex(e.Index, u.Len)
		e.T = u.Elem
		if xi.Mode == ModeVar {
			e.Mode = ModeVar
		} else {
			e.Mode = ModeValue
		}
	case *Slice:
		c.checkIndex(e.Index, -1)
		e.T, e.Mode = u.Elem, ModeVar
	case *Basic:
		if !isString(u) {
			break
		}
		n := int64(-1)
		if xi.Const != nil {
			n = int64(len(constant.StringVal(xi.Const)))
		}
		c.checkIndex(e.Index, n)
		if xi.Const != nil && e.Index.info().Const != nil && isInteger(e.Index.info().T) {
			i, _ := constant.Int64Val(constant.ToInt(e.Index.info().Const))
			if i >= 0 && i < n {
				e.Const = constant.MakeInt64(int64(constant.StringVal(xi.Const)[i]))
				e.T, e.Mode = tUint8, ModeConst
				return
			}
		}
		if isUntyped(xi.T) {
			c.convertUntyped(e.X, tString, "index")
		}
		e.T, e.Mode = tUint8, ModeValue
	case *Map:
		c.valueExpr(e.Index, u.Key)
		c.assignable(e.Index, u.Key, "map index")
		e.T, e.Mode = u.Value, ModeMapIndex
	default:
		c.errorf(e.P, "invalid operation: cannot index %s (type %s)", exprString(e.X), xi.T)
		c.invalid(e)
		return
	}
	if e.T == nil {
		c.errorf(e.P, "invalid operation: cannot index %s (type %s)", exprString(e.X), xi.T)
		c.invalid(e)
	}
}

func (c *Checker) sliceExpr(e *SliceExpr) {
	if !c.valueExpr(e.X, nil) {
		c.invalid(e)
		return
	}
	if _, ok := e.X.info().T.Underlying().(*VecType); ok {
		c.toView(e.X)
	}
	xi := e.X.info()
	length := int64(-1)
	switch u := xi.T.Underlying().(type) {
	case *Array:
		if xi.Mode != ModeVar {
			c.errorf(e.P, "invalid operation: %s (slice of unaddressable value)", exprString(e))
			c.invalid(e)
			return
		}
		if v, root := readOnlyBase(e.X); v != nil {
			// a slice would be a writable view of read-only memory
			c.errorf(e.P, "cannot slice %s (%s is a constant; copy it into a variable first)", exprString(e.X), exprString(root))
			c.invalid(e)
			return
		}
		length = u.Len
		e.T = &Slice{Elem: u.Elem}
	case *Pointer:
		a, ok := u.Elem.Underlying().(*Array)
		if !ok {
			c.errorf(e.P, "cannot slice %s (type %s)", exprString(e.X), xi.T)
			c.invalid(e)
			return
		}
		length = a.Len
		e.T = &Slice{Elem: a.Elem}
	case *Slice:
		e.T = xi.T
	case *Basic:
		if !isString(u) {
			c.errorf(e.P, "cannot slice %s (type %s)", exprString(e.X), xi.T)
			c.invalid(e)
			return
		}
		if e.Max != nil {
			c.errorf(e.P, "invalid operation: 3-index slice of string")
		}
		if isUntyped(xi.T) {
			c.convertUntyped(e.X, tString, "slice expression")
		}
		e.T = e.X.info().T
		if xi.Const != nil {
			length = int64(len(constant.StringVal(xi.Const)))
		}
	default:
		c.errorf(e.P, "cannot slice %s (type %s)", exprString(e.X), xi.T)
		c.invalid(e)
		return
	}
	e.Mode = ModeValue
	var consts []int64
	for _, ix := range []Expr{e.Lo, e.Hi, e.Max} {
		if ix == nil {
			continue
		}
		bound := length
		if bound >= 0 {
			bound++ // slice indices may equal length
		}
		c.checkIndex(ix, bound)
		if cv := ix.info().Const; cv != nil {
			v, _ := constant.Int64Val(constant.ToInt(cv))
			for _, prev := range consts {
				if v < prev {
					c.errorf(ix.Pos(), "invalid slice indices: %d < %d", v, prev)
				}
			}
			consts = append(consts, v)
		}
	}
	if e.Max != nil && e.Hi == nil {
		c.errorf(e.P, "middle index required in 3-index slice")
	}
}

// ---- unary & binary ----

func (c *Checker) unary(e *UnaryExpr, hint Type) {
	if e.Op == AND {
		if cl, ok := unparen(e.X).(*CompositeLit); ok {
			var h Type
			if hint != nil {
				if p, ok := hint.Underlying().(*Pointer); ok {
					h = p.Elem
				}
			}
			c.compositeLit(cl, h)
			e.T, e.Mode = &Pointer{Elem: cl.T}, ModeValue
			return
		}
		if !c.valueExpr(e.X, nil) {
			c.invalid(e)
			return
		}
		if e.X.info().Mode != ModeVar {
			if e.X.info().Mode == ModeMapIndex {
				c.errorf(e.P, "cannot take address of map element %s", exprString(e.X))
			} else {
				c.errorf(e.P, "cannot take address of %s", exprString(e.X))
			}
			c.invalid(e)
			return
		}
		if v, root := readOnlyBase(e.X); v != nil {
			// constants are read-only and shared by every scheduler thread
			c.errorf(e.P, "cannot take address of %s (%s)", exprString(e.X), constWhat(e.X, root))
			c.invalid(e)
			return
		}
		e.T, e.Mode = &Pointer{Elem: e.X.info().T}, ModeValue
		return
	}
	if !c.valueExpr(e.X, nil) {
		c.invalid(e)
		return
	}
	if e.Op == ARROW {
		c.toView(e.X)
	}
	xi := e.X.info()
	if e.Op == ARROW {
		ch, ok := xi.T.Underlying().(*Chan)
		if !ok {
			c.errorf(e.P, "invalid operation: cannot receive from non-channel %s", exprString(e.X))
			c.invalid(e)
			return
		}
		e.T, e.Mode = ch.Elem, ModeValue
		return
	}
	ok := false
	switch e.Op {
	case ADD, SUB:
		ok = isNumeric(xi.T)
	case XOR:
		ok = isInteger(xi.T)
	case NOT:
		ok = isBoolean(xi.T)
	}
	if !ok {
		c.errorf(e.P, "invalid operation: operator %s not defined on %s (type %s)", e.Op, exprString(e.X), xi.T)
		c.invalid(e)
		return
	}
	e.T, e.Mode = xi.T, ModeValue
	if xi.Const != nil {
		op := map[Tok]gotoken.Token{ADD: gotoken.ADD, SUB: gotoken.SUB, XOR: gotoken.XOR, NOT: gotoken.NOT}[e.Op]
		prec := uint(0)
		if isUnsigned(xi.T) {
			prec = uint(sizeofBasic(basicKind(xi.T)) * 8)
		}
		v := constant.UnaryOp(op, xi.Const, prec)
		if !isUntyped(xi.T) {
			if _, ok := representable(v, xi.T); !ok {
				c.errorf(e.P, "constant %s overflows %s", v, xi.T)
			}
		}
		e.Const, e.Mode = v, ModeConst
	}
}

func unparen(x Expr) Expr {
	for {
		p, ok := x.(*ParenExpr)
		if !ok {
			return x
		}
		x = p.X
	}
}

var goOps = map[Tok]gotoken.Token{
	ADD: gotoken.ADD, SUB: gotoken.SUB, MUL: gotoken.MUL, QUO: gotoken.QUO, REM: gotoken.REM,
	AND: gotoken.AND, OR: gotoken.OR, XOR: gotoken.XOR, SHL: gotoken.SHL, SHR: gotoken.SHR, AND_NOT: gotoken.AND_NOT,
	EQL: gotoken.EQL, NEQ: gotoken.NEQ, LSS: gotoken.LSS, LEQ: gotoken.LEQ, GTR: gotoken.GTR, GEQ: gotoken.GEQ,
	LAND: gotoken.LAND, LOR: gotoken.LOR,
}

func isComparison(op Tok) bool {
	switch op {
	case EQL, NEQ, LSS, LEQ, GTR, GEQ:
		return true
	}
	return false
}

func (c *Checker) binary(e *BinaryExpr) {
	if !c.valueExpr(e.X, nil) || !c.valueExpr(e.Y, nil) {
		c.invalid(e)
		return
	}
	if e.Op == SHL || e.Op == SHR {
		c.shift(e)
		return
	}
	x, y := e.X.info(), e.Y.info()
	// match operand types
	if isUntyped(x.T) && !isUntyped(y.T) {
		if !c.convertUntyped(e.X, y.T, "binary operation") {
			c.invalid(e)
			return
		}
	} else if isUntyped(y.T) && !isUntyped(x.T) {
		if !c.convertUntyped(e.Y, x.T, "binary operation") {
			c.invalid(e)
			return
		}
	} else if isUntyped(x.T) && isUntyped(y.T) {
		if isNilType(x.T) || isNilType(y.T) {
			if !(isNilType(x.T) && isNilType(y.T)) {
				c.errorf(e.P, "invalid operation: mismatched types %s and %s", x.T, y.T)
			} else {
				c.errorf(e.P, "invalid operation: nil %s nil", e.Op)
			}
			c.invalid(e)
			return
		}
		// both untyped constants: promote to the larger kind
		kx, ky := basicKind(x.T), basicKind(y.T)
		if (kx == UntypedString) != (ky == UntypedString) || (kx == UntypedBool) != (ky == UntypedBool) {
			c.errorf(e.P, "invalid operation: mismatched types %s and %s", x.T, y.T)
			c.invalid(e)
			return
		}
		if kx < ky {
			x.T = y.T
		} else {
			y.T = x.T
		}
	}
	if isComparison(e.Op) {
		c.comparison(e)
		return
	}
	if !Identical(x.T, y.T) {
		c.errorf(e.P, "invalid operation: %s (mismatched types %s and %s)", exprString(e), x.T, y.T)
		c.invalid(e)
		return
	}
	t := x.T
	ok := false
	switch e.Op {
	case ADD:
		ok = isNumeric(t) || isString(t)
		if ok && isString(t) && (x.Const == nil || y.Const == nil) {
			if c.allocArena == nil {
				c.errorf(e.P, "string concatenation needs memory: allocate it in an arena (%s in a), or build the string in a buffer", exprString(e))
				c.invalid(e)
				return
			}
			e.InArena = c.allocArena
		}
	case SUB, MUL, QUO:
		ok = isNumeric(t)
	case REM, AND, OR, XOR, AND_NOT:
		ok = isInteger(t)
	case LAND, LOR:
		ok = isBoolean(t)
	}
	if !ok {
		c.errorf(e.P, "invalid operation: operator %s not defined on %s (type %s)", e.Op, exprString(e.X), t)
		c.invalid(e)
		return
	}
	if (e.Op == QUO || e.Op == REM) && y.Const != nil && constant.Sign(y.Const) == 0 {
		c.errorf(e.P, "invalid operation: division by zero")
		c.invalid(e)
		return
	}
	e.T, e.Mode = t, ModeValue
	if x.Const != nil && y.Const != nil {
		op := goOps[e.Op]
		if e.Op == QUO && isInteger(t) {
			op = gotoken.QUO_ASSIGN // integer division
		}
		var v constant.Value
		if op == gotoken.QUO_ASSIGN {
			v = constant.BinaryOp(constant.ToInt(x.Const), gotoken.QUO_ASSIGN, constant.ToInt(y.Const))
		} else if isFloat(t) && e.Op == QUO {
			v = constant.BinaryOp(constant.ToFloat(x.Const), gotoken.QUO, constant.ToFloat(y.Const))
		} else {
			v = constant.BinaryOp(x.Const, op, y.Const)
		}
		if !isUntyped(t) {
			if isUnsigned(t) && (e.Op == XOR || e.Op == AND_NOT) {
				// mask to width
				bits := uint(sizeofBasic(basicKind(t)) * 8)
				mask := constant.Shift(constant.MakeInt64(1), gotoken.SHL, bits)
				mask = constant.BinaryOp(mask, gotoken.SUB, constant.MakeInt64(1))
				v = constant.BinaryOp(v, gotoken.AND, mask)
			}
			nv, ok := representable(v, t)
			if !ok {
				c.errorf(e.P, "constant %s overflows %s", v, t)
			}
			v = nv
		}
		e.Const, e.Mode = v, ModeConst
	}
}

func (c *Checker) comparison(e *BinaryExpr) {
	x, y := e.X.info(), e.Y.info()
	okTypes := assignableType(x.T, y.T) || assignableType(y.T, x.T)
	if isNilType(x.T) {
		okTypes = nilable(y.T)
		setType(e.X, y.T)
	} else if isNilType(y.T) {
		okTypes = nilable(x.T)
		setType(e.Y, x.T)
	}
	if !okTypes {
		c.errorf(e.P, "invalid operation: %s (mismatched types %s and %s)", exprString(e), x.T, y.T)
		c.invalid(e)
		return
	}
	t := x.T
	switch e.Op {
	case EQL, NEQ:
		_, isSlice := t.Underlying().(*Slice)
		_, isMap := t.Underlying().(*Map)
		_, isFunc := t.Underlying().(*Signature)
		_, isArena := t.Underlying().(*ArenaType)
		isFunc = isFunc || isArena // like functions, arenas compare only with nil
		if (isSlice || isMap || isFunc) && !isNilType(x.T) && !isNilType(y.T) && !(isNilLit(e.X) || isNilLit(e.Y)) {
			c.errorf(e.P, "invalid operation: %s (%s can only be compared to nil)", exprString(e), t)
			c.invalid(e)
			return
		}
		if !isSlice && !isMap && !isFunc && !Comparable(t) {
			c.errorf(e.P, "invalid operation: %s (%s cannot be compared)", exprString(e), t)
			c.invalid(e)
			return
		}
	default:
		if !isNumeric(t) && !isString(t) {
			c.errorf(e.P, "invalid operation: %s (operator %s not defined on %s)", exprString(e), e.Op, t)
			c.invalid(e)
			return
		}
	}
	e.T, e.Mode = tUntypedBool, ModeValue // as in Go: x > 1 can be assigned to a named bool type
	if x.Const != nil && y.Const != nil {
		e.Const = constant.MakeBool(constant.Compare(x.Const, goOps[e.Op], y.Const))
		e.T, e.Mode = tUntypedBool, ModeConst
	}
}

func isNilLit(x Expr) bool {
	id, ok := unparen(x).(*Ident)
	return ok && id.Obj != nil && id.Obj.Kind == ObjNil
}

func (c *Checker) shift(e *BinaryExpr) {
	x, y := e.X.info(), e.Y.info()
	if !isInteger(y.T) {
		if y.Const != nil && isNumeric(y.T) {
			if _, ok := representable(y.Const, tUint); ok {
				c.convertUntyped(e.Y, tUint, "shift count")
			}
		}
		if !isInteger(y.T) {
			c.errorf(e.Y.Pos(), "invalid shift count %s (type %s)", exprString(e.Y), y.T)
			c.invalid(e)
			return
		}
	}
	if y.Const != nil {
		if constant.Sign(y.Const) < 0 {
			c.errorf(e.Y.Pos(), "invalid negative shift count %s", exprString(e.Y))
			c.invalid(e)
			return
		}
		if isUntyped(y.T) {
			c.convertUntyped(e.Y, tUint, "shift count")
		}
	} else if isUntyped(y.T) {
		c.convertUntyped(e.Y, tUint, "shift count")
	}
	if x.Const != nil && isUntyped(x.T) && y.Const == nil {
		// untyped constant shifted by a variable amount: as in Go, the shift is untyped and takes
		// its type from the context (var m uint64 = 1 << n), int by default; see convertUntyped
		if iv := constant.ToInt(x.Const); iv.Kind() == constant.Int && basicKind(x.T) == UntypedFloat {
			x.Const, x.T = iv, tUntypedInt
		}
		if !isInteger(x.T) {
			c.errorf(e.P, "invalid operation: shifted operand %s (type %s) must be integer", exprString(e.X), x.T)
			c.invalid(e)
			return
		}
		e.T, e.Mode = x.T, ModeValue
		return
	}
	if x.Const != nil && isUntyped(x.T) && basicKind(x.T) == UntypedFloat {
		if iv := constant.ToInt(x.Const); iv.Kind() == constant.Int {
			x.Const, x.T = iv, tUntypedInt
		}
	}
	if !isInteger(x.T) {
		c.errorf(e.P, "invalid operation: shifted operand %s (type %s) must be integer", exprString(e.X), x.T)
		c.invalid(e)
		return
	}
	e.T, e.Mode = x.T, ModeValue
	if x.Const != nil && y.Const != nil {
		s, _ := constant.Uint64Val(y.Const)
		v := constant.Shift(x.Const, goOps[e.Op], uint(s))
		if !isUntyped(e.T) {
			nv, ok := representable(v, e.T)
			if !ok {
				c.errorf(e.P, "constant %s overflows %s", v, e.T)
			}
			v = nv
		}
		e.Const, e.Mode = v, ModeConst
	}
}

func constInt(x Expr) (int64, bool) {
	cv := x.info().Const
	if cv == nil {
		return 0, false
	}
	v, ok := constant.Int64Val(constant.ToInt(cv))
	return v, ok
}

var _ = strconv.Itoa

// constWhat says why x is read-only: "it is a constant", or "T is a constant" for part of T.
func constWhat(x, root Expr) string {
	if unparen(x) == unparen(root) {
		return "it is a constant"
	}
	return exprString(root) + " is a constant"
}

// readOnlyBase returns the constant (error constant or constant composite) that x is, or is an
// element or field of by value, and the expression naming it; nil if x is writable. Package arenas
// are read-only names but hold writable memory, so they don't count.
func readOnlyBase(x Expr) (*Var, Expr) {
	for {
		switch e := unparen(x).(type) {
		case *IndexExpr:
			if _, ok := e.X.info().T.Underlying().(*Array); !ok {
				return nil, nil // through a slice, pointer or map: not this storage
			}
			x = e.X
			continue
		case *SelectorExpr:
			if e.Kind == SelField && e.Implicit != "*" {
				x = e.X
				continue
			}
		}
		if v := readOnlyVar(x); v != nil && v.ArenaSize == 0 {
			return v, x
		}
		return nil, nil
	}
}

// readOnlyVar returns the read-only package variable (an error constant or a package arena) that x
// names, or nil.
func readOnlyVar(x Expr) *Var {
	switch x := unparen(x).(type) {
	case *Ident:
		if x.Obj != nil && x.Obj.Var != nil && x.Obj.Var.ReadOnly {
			return x.Obj.Var
		}
	case *SelectorExpr:
		if x.Kind == SelQualified && x.Sel.Obj != nil && x.Sel.Obj.Var != nil && x.Sel.Obj.Var.ReadOnly {
			return x.Sel.Obj.Var
		}
	}
	return nil
}
