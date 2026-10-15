package compiler

func (c *Checker) stmtList(list []Stmt) {
	for _, s := range list {
		c.stmt(s)
	}
}

func (c *Checker) stmt(s Stmt) {
	switch s := s.(type) {
	case *EmptyStmt:
	case *ExprStmt:
		c.expr(s.X)
		x := unparen(s.X)
		for { // f(x + y) in a: a call whose arguments allocate in a
			ie, ok := x.(*InExpr)
			if !ok {
				break
			}
			x = unparen(ie.X)
		}
		switch x := x.(type) {
		case *CallExpr:
			if x.Conversion {
				c.errorf(x.P, "%s is not used", exprString(x))
			} else if x.Builtin != "" {
				switch x.Builtin {
				case "len", "cap", "append", "make", "min", "max", "__error":
					c.errorf(x.P, "%s is not used", exprString(x))
				}
			}
		case *UnaryExpr:
			if x.Op != ARROW {
				c.errorf(x.P, "%s is not used", exprString(x))
			}
		case *TryExpr:
		default:
			if s.X.info().Mode != ModeInvalid {
				c.errorf(s.X.Pos(), "%s is not used", exprString(s.X))
			}
		}
	case *SendStmt:
		if !c.valueExpr(s.Chan, nil) {
			c.expr(s.Value)
			return
		}
		c.toView(s.Chan)
		ch, ok := s.Chan.info().T.Underlying().(*Chan)
		if !ok {
			c.errorf(s.P, "invalid operation: cannot send to non-channel %s", exprString(s.Chan))
			c.expr(s.Value)
			return
		}
		c.exprHint(s.Value, ch.Elem)
		c.assignable(s.Value, ch.Elem, "send")
	case *IncDecStmt:
		if !c.valueExpr(s.X, nil) {
			return
		}
		if !isNumeric(s.X.info().T) {
			c.errorf(s.P, "invalid operation: %s%s (non-numeric type %s)", exprString(s.X), s.Tok, s.X.info().T)
		}
		c.checkAssignTarget(s.X)
	case *AssignStmt:
		c.assign(s)
	case *GoStmt:
		c.goStmt(s)
	case *DeferStmt:
		c.deferStmt(s)
	case *ReturnStmt:
		c.returnStmt(s)
	case *BranchStmt:
		c.branch(s)
	case *BlockStmt:
		c.openScope()
		c.stmtList(s.List)
		c.closeScope()
	case *IfStmt:
		c.openScope()
		if s.Init != nil {
			c.stmt(s.Init)
		}
		c.cond(s.Cond, "if")
		c.stmt(s.Body)
		if s.Else != nil {
			c.stmt(s.Else)
		}
		c.closeScope()
	case *SwitchStmt:
		c.switchStmt(s)
	case *SelectStmt:
		c.selectStmt(s)
	case *ForStmt:
		c.openScope()
		if s.Init != nil {
			c.stmt(s.Init)
		}
		if s.Cond != nil {
			c.cond(s.Cond, "for")
		}
		if s.Post != nil {
			if as, ok := s.Post.(*AssignStmt); ok && as.Tok == DEFINE {
				c.errorf(as.P, "cannot declare in post statement of for loop")
			}
			c.stmt(s.Post)
		}
		c.breakable = append(c.breakable, s)
		c.loopDepth++
		c.stmt(s.Body)
		c.loopDepth--
		c.breakable = c.breakable[:len(c.breakable)-1]
		c.closeScope()
	case *RangeStmt:
		c.rangeStmt(s)
	case *DeclStmt:
		c.declStmt(s.Decl)
	case *LabeledStmt:
		if prev, ok := c.labels[s.Label.Name]; ok && c.labels != nil {
			c.errorf(s.Label.P, "label %s already defined at %s:%d", s.Label.Name, shortFile(prev.File), prev.Line)
		} else if c.labels != nil {
			c.labels[s.Label.Name] = s.Label.P
		}
		c.stmt(s.Stmt)
	case *ArenaStmt:
		n, ok := c.constSize(s.Size, "arena size")
		if ok && n < 16 {
			c.errorf(s.Size.Pos(), "arena size must be at least 16 bytes")
		}
		c.openScope()
		v := c.declareVar(s.Name, tArena)
		v.ReadOnly = true
		v.ArenaSize = n
		s.Name.Obj.Var = v
		c.stmt(s.Body)
		c.closeScope()
	default:
		c.errorf(s.Pos(), "unexpected statement")
	}
}

func (c *Checker) cond(x Expr, what string) {
	if !c.valueExpr(x, nil) {
		return
	}
	if !isBoolean(x.info().T) {
		c.errorf(x.Pos(), "non-boolean condition in %s statement", what)
		return
	}
	c.defaultUntyped(x)
}

func (c *Checker) checkAssignTarget(x Expr) bool {
	xi := x.info()
	if xi.Mode == ModeInvalid {
		return false
	}
	if v, root := readOnlyBase(x); v != nil {
		c.errorf(x.Pos(), "cannot assign to %s (%s)", exprString(x), constWhat(x, root))
		return false
	}
	changed(x)
	if xi.Mode == ModeVar || xi.Mode == ModeMapIndex {
		if id, ok := unparen(x).(*Ident); ok && id.Obj != nil && id.Obj.Var != nil && id.Obj.Var.Param && c.fn != nil {
			// params are assignable locals
		}
		return true
	}
	if se, ok := unparen(x).(*SelectorExpr); ok && se.Kind == SelField {
		if ix, ok := unparen(se.X).(*IndexExpr); ok && ix.Mode == ModeMapIndex {
			c.errorf(x.Pos(), "cannot assign to struct field %s in map", exprString(x))
			return false
		}
	}
	c.errorf(x.Pos(), "cannot assign to %s (neither addressable nor a map index expression)", exprString(x))
	return false
}

// lhsTypes returns the types produced by the right-hand side of an n-ary assignment.
// rhsTypes checks the right side of an assignment or declaration whose left side starts at pos.
func (c *Checker) rhsTypes(pos Pos, lhsN int, rhs []Expr, hints []Type) []Type {
	if len(rhs) == 1 && lhsN > 1 {
		x := rhs[0]
		switch u := unparen(x).(type) {
		case *IndexExpr:
			c.expr(u)
			if u.Mode == ModeMapIndex && lhsN == 2 {
				u.CommaOk = true
				return []Type{u.T, tBool}
			}
		case *UnaryExpr:
			if u.Op == ARROW {
				c.expr(u)
				if u.Mode != ModeInvalid && lhsN == 2 {
					u.CommaOk = true
					return []Type{u.T, tBool}
				}
				return nil
			}
			c.expr(u)
		default:
			c.expr(x)
		}
		if x.info().Mode == ModeInvalid {
			return nil
		}
		if tu, ok := x.info().T.(*Tuple); ok {
			if len(tu.Types) != lhsN {
				c.errorf(pos, "assignment mismatch: %d variables but %s returns %d values", lhsN, exprString(x), len(tu.Types))
				return nil
			}
			return tu.Types
		}
		c.errorf(pos, "assignment mismatch: %d variables but 1 value", lhsN)
		return nil
	}
	if len(rhs) != lhsN {
		c.errorf(pos, "assignment mismatch: %d variables but %d values", lhsN, len(rhs))
		for _, x := range rhs {
			c.expr(x)
		}
		return nil
	}
	ts := make([]Type, lhsN)
	for i, x := range rhs {
		var h Type
		if hints != nil {
			h = hints[i]
		}
		c.exprHint(x, h)
		if tu, ok := x.info().T.(*Tuple); ok && x.info().Mode != ModeInvalid {
			c.errorf(x.Pos(), "assignment mismatch: %d variable%s but %s returns %d values", lhsN, plural(lhsN), exprString(x), len(tu.Types))
			c.invalid(x)
			return nil
		}
		if !c.value(x) {
			return nil
		}
		ts[i] = x.info().T
	}
	return ts
}

func isBlank(x Expr) bool {
	id, ok := x.(*Ident)
	return ok && id.Name == "_"
}

func (c *Checker) assign(s *AssignStmt) {
	switch s.Tok {
	case DEFINE:
		// rhs first, in the enclosing scope
		ts := c.rhsTypes(s.Lhs[0].Pos(), len(s.Lhs), s.Rhs, nil)
		s.NewVars = make([]bool, len(s.Lhs))
		anyNew := false
		for i, l := range s.Lhs {
			id, ok := l.(*Ident)
			if !ok {
				c.errorf(l.Pos(), "non-name %s on left side of :=", exprString(l))
				continue
			}
			var t Type = tInvalid
			existing := c.scope.LookupLocal(id.Name)
			if ts != nil {
				t = ts[i]
				// an existing variable gives an untyped value its type (assignable below);
				// only new names take the default type
				if len(s.Rhs) == len(s.Lhs) && (existing == nil || existing.Kind != ObjVar) {
					if isNilType(t) {
						c.errorf(s.Rhs[i].Pos(), "use of untyped nil in assignment")
						t = tInvalid
					} else {
						c.defaultUntyped(s.Rhs[i])
						t = s.Rhs[i].info().T
					}
				}
			}
			if id.Name == "_" {
				id.T, id.Mode = t, ModeVar
				continue
			}
			if o := c.scope.LookupLocal(id.Name); o != nil && o.Kind == ObjVar {
				o.Var.Changed = true // redeclared by :=, so assigned
				id.Obj = o
				id.T, id.Mode = o.Var.Type, ModeVar
				if ts != nil && len(s.Rhs) == len(s.Lhs) {
					c.assignable(s.Rhs[i], o.Var.Type, "assignment")
				} else if ts != nil && !assignableType(t, o.Var.Type) && t != tInvalid {
					c.errorf(id.P, "cannot assign %s to %s (type %s)", t, id.Name, o.Var.Type)
				}
				continue
			}
			if isStorageCopy(t) {
				c.errorf(id.P, "cannot copy %s storage into %s: use a view (%s) instead", t, id.Name, viewOf(t))
			} else if len(s.Rhs) == len(s.Lhs) {
				c.checkNestedStorageCopy(s.Rhs[i], t, " into "+id.Name)
			}
			anyNew = true
			s.NewVars[i] = true
			if _, isTuple := t.(*Tuple); isTuple {
				t = tInvalid
			}
			v := c.declareVar(id, t)
			v.NonNilInit = len(s.Rhs) == len(s.Lhs) && nonNilExpr(s.Rhs[i])
		}
		if !anyNew {
			c.errorf(s.P, "no new variables on left side of :=")
		}
	case ASSIGN:
		var hints []Type
		for _, l := range s.Lhs {
			if isBlank(l) {
				hints = append(hints, nil)
				continue
			}
			c.expr(l)
			hints = append(hints, l.info().T)
		}
		ts := c.rhsTypes(s.Lhs[0].Pos(), len(s.Lhs), s.Rhs, hints)
		for i, l := range s.Lhs {
			if isBlank(l) {
				l.info().Mode = ModeVar
				if ts != nil && len(s.Rhs) == len(s.Lhs) {
					c.defaultUntyped(s.Rhs[i])
					if isNilType(s.Rhs[i].info().T) {
						c.errorf(s.Rhs[i].Pos(), "use of untyped nil in assignment")
					}
					l.info().T = s.Rhs[i].info().T
				} else if ts != nil {
					l.info().T = ts[i]
				}
				continue
			}
			if !c.checkAssignTarget(l) || ts == nil {
				continue
			}
			if len(s.Rhs) == len(s.Lhs) {
				c.assignable(s.Rhs[i], l.info().T, "assignment")
			} else if !assignableType(ts[i], l.info().T) {
				c.errorf(l.Pos(), "cannot assign %s to %s (type %s)", ts[i], exprString(l), l.info().T)
			}
		}
	default:
		// op-assign
		if len(s.Lhs) != 1 || len(s.Rhs) != 1 {
			c.errorf(s.P, "assignment operation %s requires single-valued expressions", s.Tok)
			return
		}
		op := map[Tok]Tok{ADD_ASSIGN: ADD, SUB_ASSIGN: SUB, MUL_ASSIGN: MUL, QUO_ASSIGN: QUO, REM_ASSIGN: REM,
			AND_ASSIGN: AND, OR_ASSIGN: OR, XOR_ASSIGN: XOR, SHL_ASSIGN: SHL, SHR_ASSIGN: SHR, AND_NOT_ASSIGN: AND_NOT}[s.Tok]
		if in, ok := s.Rhs[0].(*InExpr); ok && op == ADD {
			// `in a` would only cover the right side: say how to allocate the whole result
			c.errorf(s.P, "%s can't allocate: write %s = %s + %s in %s", s.Tok, exprString(s.Lhs[0]), exprString(s.Lhs[0]), exprString(in.X), exprString(in.Arena))
			return
		}
		be := &BinaryExpr{X: s.Lhs[0], Op: op, Y: s.Rhs[0], P: s.P}
		c.binary(be)
		if be.Mode == ModeInvalid {
			return
		}
		c.checkAssignTarget(s.Lhs[0])
		if !Identical(be.T, s.Lhs[0].info().T) && !isUntyped(be.T) {
			c.errorf(s.P, "cannot assign %s to %s", be.T, s.Lhs[0].info().T)
		}
	}
}

func (c *Checker) goStmt(s *GoStmt) {
	c.goCall = s.Call
	c.expr(s.Call)
	c.goCall = nil
	call := s.Call
	if call.Mode == ModeInvalid {
		return
	}
	if call.Builtin != "" || call.Conversion {
		c.errorf(s.P, "go requires a function or method call")
		return
	}
	if call.Callee == nil {
		c.errorf(s.P, "go requires a direct call to a named function or method (the goroutine's stack is sized from its entry function)")
		return
	}
	if !call.Callee.IsTask {
		c.errorf(s.P, "go needs a task: declare %s with `task %s(...) pool N` (tasks have a pool size and a proven stack)", call.Callee.Name, call.Callee.Name)
		return
	}
	if len(call.Callee.Sig.Results) > 0 {
		// results are discarded, like Go
	}
	if call.Ellipsis || call.Variadic >= 0 && call.Callee.Sig.Variadic {
		c.errorf(s.P, "go with a variadic call is not supported")
	}
	call.Callee.GoTarget = true
	s.ID = len(c.prog.GoSites)
	c.prog.GoSites = append(c.prog.GoSites, s)
	if c.fn == nil {
		c.errorf(s.P, "go statement outside function")
		return
	}
	c.fn.GoSites = append(c.fn.GoSites, s)
}

func (c *Checker) deferStmt(s *DeferStmt) {
	c.expr(s.Call)
	call := s.Call
	if call.Mode == ModeInvalid {
		return
	}
	if c.loopDepth > 0 {
		c.errorf(s.P, "defer inside a loop is not allowed: an unbounded number of deferred calls would need the heap")
		return
	}
	if call.Conversion {
		c.errorf(s.P, "defer requires a function call")
		return
	}
	if call.Builtin != "" {
		switch call.Builtin {
		case "close", "print", "println", "panic":
		case "len", "cap", "append", "min", "max", "new", "make", "error.Error", "error.Code", "arena.Used", "arena.Left", "arena.Cap", "vec.TryPush", "vec.TryAppend", "vec.Pop":
			c.errorf(s.P, "defer discards result of %s", exprString(call))
			return
		default:
			c.errorf(s.P, "defer %s is not supported (only close, panic, print and println among builtins); defer a function that calls it", exprString(call))
			return
		}
	}
	if call.Variadic >= 0 && call.Callee != nil && call.Callee.Sig.Variadic && !call.Ellipsis {
		c.errorf(s.P, "defer with a variadic call is not supported")
	}
	s.ID = len(c.fn.Defers)
	c.fn.Defers = append(c.fn.Defers, s)
}

func (c *Checker) returnStmt(s *ReturnStmt) {
	if c.sig == nil {
		c.errorf(s.P, "return outside function")
		return
	}
	res := c.sig.Results
	if len(s.Results) == 0 {
		if len(res) > 0 && len(c.namedRes) == 0 {
			c.errorf(s.P, "not enough return values\n\thave ()\n\twant %s", resultString(res))
		}
		return
	}
	if len(s.Results) == 1 && len(res) > 1 {
		c.expr(s.Results[0])
		if tu, ok := s.Results[0].info().T.(*Tuple); ok && s.Results[0].info().Mode != ModeInvalid {
			if len(tu.Types) != len(res) {
				c.errorf(s.P, "wrong number of return values (have %d, want %d)", len(tu.Types), len(res))
				return
			}
			for i, t := range tu.Types {
				if !assignableType(t, res[i].Type) {
					c.errorf(s.P, "cannot use %s as %s in return statement", t, res[i].Type)
				}
			}
			return
		}
		c.errorf(s.P, "not enough return values\n\thave (%s)\n\twant %s", exprString(s.Results[0]), resultString(res))
		return
	}
	if len(s.Results) != len(res) {
		for _, x := range s.Results {
			c.expr(x)
		}
		if len(s.Results) > len(res) {
			c.errorf(s.P, "too many return values (have %d, want %d)", len(s.Results), len(res))
		} else {
			c.errorf(s.P, "not enough return values (have %d, want %d)", len(s.Results), len(res))
		}
		return
	}
	for i, x := range s.Results {
		c.exprHint(x, res[i].Type)
		c.assignable(x, res[i].Type, "return statement")
	}
}

func resultString(res []*Var) string {
	s := "("
	for i, r := range res {
		if i > 0 {
			s += ", "
		}
		s += r.Type.String()
	}
	return s + ")"
}

func stmtLabel(s Stmt) string {
	switch s := s.(type) {
	case *ForStmt:
		return s.Label
	case *RangeStmt:
		return s.Label
	case *SwitchStmt:
		return s.Label
	case *SelectStmt:
		return s.Label
	}
	return ""
}

// isSwitchLike reports whether t is a switch or select: break leaves it, continue doesn't.
func isSwitchLike(t Stmt) bool {
	switch t.(type) {
	case *SwitchStmt, *SelectStmt:
		return true
	}
	return false
}

func (c *Checker) branch(s *BranchStmt) {
	for i := len(c.breakable) - 1; i >= 0; i-- {
		t := c.breakable[i]
		if s.Label != nil {
			if stmtLabel(t) != s.Label.Name {
				continue
			}
		} else if isSwitchLike(t) && s.Tok == CONTINUE {
			continue
		}
		if isSwitchLike(t) && s.Tok == CONTINUE {
			c.errorf(s.P, "invalid continue label %s", s.Label.Name)
			return
		}
		s.Target = t
		return
	}
	if s.Label != nil {
		c.errorf(s.P, "invalid %s label %s", s.Tok, s.Label.Name)
	} else if s.Tok == BREAK {
		c.errorf(s.P, "break is not in a loop, switch or select")
	} else {
		c.errorf(s.P, "continue is not in a loop")
	}
}

func isTimeAfter(f *Func) bool {
	return f != nil && f.Pkg != nil && f.Pkg.Path == "time" && f.Name == "After"
}

// recvOf returns the receive x is, if it is one.
func recvOf(x Expr) *UnaryExpr {
	if u, ok := unparen(x).(*UnaryExpr); ok && u.Op == ARROW {
		return u
	}
	return nil
}

func (c *Checker) selectStmt(s *SelectStmt) {
	c.breakable = append(c.breakable, s)
	defer func() { c.breakable = c.breakable[:len(c.breakable)-1] }()
	hasDefault := false
	var timeout *CommClause
	for _, cc := range s.Cases {
		c.openScope()
		var recv *UnaryExpr
		switch comm := cc.Comm.(type) {
		case nil:
			if hasDefault {
				c.errorf(cc.P, "multiple defaults in select")
			}
			hasDefault = true
		case *SendStmt:
			c.stmt(comm)
		case *ExprStmt:
			recv = recvOf(comm.X)
		case *AssignStmt:
			if len(comm.Rhs) == 1 && len(comm.Lhs) <= 2 && (comm.Tok == ASSIGN || comm.Tok == DEFINE) {
				recv = recvOf(comm.Rhs[0])
			}
		}
		if cc.Comm != nil && recv == nil {
			if _, ok := cc.Comm.(*SendStmt); !ok {
				c.errorf(cc.Comm.Pos(), "select case must be a send or a receive (case ch <- v:, case v := <-ch:, case v, ok := <-ch:)")
			}
		}
		if recv != nil {
			cc.Recv = recv
			after, _ := unparen(recv.X).(*CallExpr)
			c.afterCall = after
			c.stmt(cc.Comm)
			c.afterCall = nil
			if after != nil && isTimeAfter(after.Callee) {
				if _, ok := cc.Comm.(*ExprStmt); !ok {
					c.errorf(cc.Comm.Pos(), "time.After's case has no value to receive: write case <-time.After(d): (time.Now() gives the time)")
				}
				if timeout != nil {
					c.errorf(cc.P, "multiple time.After cases in select (previous at %s:%d)", shortFile(timeout.P.File), timeout.P.Line)
				}
				timeout = cc
				if len(after.Args) == 1 {
					cc.Timeout = after.Args[0]
				}
			}
		}
		c.stmtList(cc.Body)
		c.closeScope()
	}
}

func (c *Checker) switchStmt(s *SwitchStmt) {
	c.openScope()
	defer c.closeScope()
	if s.Init != nil {
		c.stmt(s.Init)
	}
	var tagT Type = tBool
	if s.Tag != nil {
		if c.valueExpr(s.Tag, nil) {
			c.defaultUntyped(s.Tag)
			tagT = s.Tag.info().T
			if !Comparable(tagT) {
				c.errorf(s.Tag.Pos(), "cannot switch on %s (type %s is not comparable)", exprString(s.Tag), tagT)
			}
		} else {
			tagT = tInvalid
		}
	}
	c.breakable = append(c.breakable, s)
	defer func() { c.breakable = c.breakable[:len(c.breakable)-1] }()
	hasDefault := false
	seen := map[string]Pos{} // constant case values, as in Go
	for _, cc := range s.Cases {
		if cc.List == nil {
			if hasDefault {
				c.errorf(cc.P, "multiple defaults in switch")
			}
			hasDefault = true
		}
		for _, x := range cc.List {
			if !c.valueExpr(x, tagT) || tagT == tInvalid {
				continue
			}
			if s.Tag == nil {
				if !isBoolean(x.info().T) {
					c.errorf(x.Pos(), "invalid case %s in switch (mismatched types %s and bool)", exprString(x), x.info().T)
				}
				c.defaultUntyped(x)
				continue
			}
			if c.assignable(x, tagT, "switch case") && x.info().Const != nil {
				k := x.info().Const.ExactString()
				if prev, ok := seen[k]; ok {
					c.errorf(x.Pos(), "duplicate case %s in expression switch (previous case at %s:%d)", exprString(x), shortFile(prev.File), prev.Line)
				} else {
					seen[k] = x.Pos()
				}
			}
		}
		c.openScope()
		c.stmtList(cc.Body)
		c.closeScope()
	}
}

func (c *Checker) rangeStmt(s *RangeStmt) {
	c.openScope()
	defer c.closeScope()
	if !c.valueExpr(s.X, nil) {
		if s.Define { // declare the variables anyway, so the body doesn't report them as undefined
			for _, v := range []Expr{s.Key, s.Value} {
				if id, ok := v.(*Ident); ok && !isBlank(id) {
					c.declareVar(id, tInvalid)
				}
			}
		}
		c.stmt(s.Body)
		return
	}
	s.X = c.derefVec(s.X)
	c.toViewRead(s.X)
	xt := s.X.info().T
	var kt, vt Type
	keyChecked := false
	switch u := xt.Underlying().(type) {
	case *Basic:
		switch {
		case isInteger(u):
			// as in Go, an untyped constant takes the type of an existing iteration variable
			if isUntyped(xt) && !s.Define && s.Key != nil && !isBlank(s.Key) {
				c.expr(s.Key)
				keyChecked = true
				if t := s.Key.info().T; t != nil && isInteger(t) && !isUntyped(t) {
					if !c.convertUntyped(s.X, t, "range") {
						setType(s.X, t) // reported: don't also report the assignment
					}
				}
			}
			c.defaultUntyped(s.X)
			kt = s.X.info().T
		case isString(u):
			c.defaultUntyped(s.X)
			kt, vt = tInt, tInt32
		}
	case *Array:
		kt, vt = tInt, u.Elem
	case *Pointer:
		if a, ok := u.Elem.Underlying().(*Array); ok {
			kt, vt = tInt, a.Elem
		}
	case *Slice:
		kt, vt = tInt, u.Elem
	case *Map:
		kt, vt = u.Key, u.Value
	case *Chan:
		kt = u.Elem
		if s.Value != nil {
			c.errorf(s.Value.Pos(), "range over %s permits only one iteration variable", exprString(s.X))
		}
	}
	if kt == nil {
		c.errorf(s.X.Pos(), "cannot range over %s (type %s)", exprString(s.X), xt)
		c.stmt(s.Body)
		return
	}
	if isInteger(xt) && s.Value != nil {
		c.errorf(s.Value.Pos(), "range over %s permits only one iteration variable", exprString(s.X))
	}
	vars := []Expr{s.Key, s.Value}
	types := []Type{kt, vt}
	for i, v := range vars {
		if v == nil {
			continue
		}
		if st := storageIn(types[i]); st != nil && !isBlank(v) {
			c.errorf(v.Pos(), "cannot copy elements of %s into %s: %s contains %s storage; range over the index and use &%s[i]", exprString(s.X), exprString(v), types[i], st, exprString(s.X))
		}
		if s.Define {
			id, ok := v.(*Ident)
			if !ok {
				c.errorf(v.Pos(), "non-name %s on left side of :=", exprString(v))
				continue
			}
			c.declareVar(id, types[i])
			continue
		}
		if isBlank(v) {
			v.info().T, v.info().Mode = types[i], ModeVar
			continue
		}
		if !(i == 0 && keyChecked) {
			c.expr(v)
		}
		if c.checkAssignTarget(v) && !assignableType(types[i], v.info().T) {
			c.errorf(v.Pos(), "cannot assign %s to %s (type %s) in range", types[i], exprString(v), v.info().T)
		}
	}
	c.breakable = append(c.breakable, s)
	c.loopDepth++
	c.stmt(s.Body)
	c.loopDepth--
	c.breakable = c.breakable[:len(c.breakable)-1]
}

func (c *Checker) declStmt(d *GenDecl) {
	switch d.Tok {
	case TYPE:
		c.errorf(d.P, "local type declarations are not supported; declare types at package level")
	case CONST:
		for _, vs := range d.Values {
			for i, name := range vs.Names {
				o := &Object{Kind: ObjConst, Name: name.Name, Pos: name.P, Pkg: c.pkg, decl: constDecl{vs, i, c.fileOf(name.P)}}
				name.Obj = o
				c.resolveConst(o)
				if name.Name != "_" {
					if prev := c.scope.Insert(o); prev != nil {
						c.errorf(name.P, "%s redeclared in this block", name.Name)
					}
				}
			}
		}
	case VAR:
		for _, vs := range d.Values {
			var typ Type
			if vs.Type != nil {
				typ = c.typExpr(vs.Type)
			}
			var ts []Type
			if len(vs.Values) > 0 {
				var hints []Type
				if typ != nil {
					for range vs.Names {
						hints = append(hints, typ)
					}
				}
				ts = c.rhsTypes(vs.Names[0].P, len(vs.Names), vs.Values, hints)
				if ts != nil && len(vs.Values) == len(vs.Names) {
					for i, x := range vs.Values {
						if typ != nil {
							c.assignable(x, typ, "variable declaration")
						} else {
							if isNilType(x.info().T) {
								c.errorf(x.Pos(), "use of untyped nil in variable declaration")
							}
							c.defaultUntyped(x)
							ts[i] = x.info().T
							if isStorageCopy(ts[i]) {
								c.errorf(x.Pos(), "cannot copy %s storage into %s: use a view (%s) instead", ts[i], vs.Names[i].Name, viewOf(ts[i]))
							} else {
								c.checkNestedStorageCopy(x, ts[i], " into "+vs.Names[i].Name)
							}
						}
					}
				}
			}
			if ts != nil && typ != nil && len(vs.Values) == 1 && len(vs.Names) > 1 {
				// var a, b T = f(): each result must be assignable to T
				for i, name := range vs.Names {
					if i < len(ts) && ts[i] != tInvalid && !assignableType(ts[i], typ) {
						c.errorf(name.P, "cannot assign %s to %s (type %s)", ts[i], name.Name, typ)
					}
				}
			}
			for i, name := range vs.Names {
				t := typ
				if t == nil {
					t = tInvalid
					if ts != nil {
						t = ts[i]
					}
				}
				v := c.declareVar(name, t)
				v.NonNilInit = len(vs.Values) == len(vs.Names) && nonNilExpr(vs.Values[i])
			}
		}
	}
}

// nonNilExpr reports an expression whose pointer value can't be nil: &x, &T{...}, new(T) in a.
func nonNilExpr(x Expr) bool {
	switch e := unparen(x).(type) {
	case *UnaryExpr:
		return e.Op == AND
	case *InExpr:
		return nonNilExpr(e.X)
	case *CallExpr:
		return e.Builtin == "new"
	}
	return false
}

// changed records that x, if it names a variable, gets a new value (or may, through its address).
func changed(x Expr) {
	if id, ok := unparen(x).(*Ident); ok && id.Obj != nil && id.Obj.Var != nil {
		id.Obj.Var.Changed = true
	}
}

// ---- terminating statements ----

func (c *Checker) isTerminatingList(list []Stmt) bool {
	for i := len(list) - 1; i >= 0; i-- {
		if _, ok := list[i].(*EmptyStmt); ok {
			continue
		}
		return c.isTerminating(list[i])
	}
	return false
}

func (c *Checker) isTerminating(s Stmt) bool {
	switch s := s.(type) {
	case *ReturnStmt:
		return true
	case *ExprStmt:
		if call, ok := unparen(s.X).(*CallExpr); ok && call.Builtin == "panic" {
			return true
		}
		if call, ok := unparen(s.X).(*CallExpr); ok && call.Callee != nil && call.Callee.Pkg != nil && call.Callee.Pkg.Path == "os" && call.Callee.Name == "Exit" {
			return true
		}
	case *BlockStmt:
		return c.isTerminatingList(s.List)
	case *ArenaStmt:
		return c.isTerminating(s.Body)
	case *IfStmt:
		return s.Else != nil && c.isTerminating(s.Body) && c.isTerminating(s.Else)
	case *LabeledStmt:
		return c.isTerminating(s.Stmt)
	case *ForStmt:
		return s.Cond == nil && !hasBreak(s.Body, s, true)
	case *SwitchStmt:
		hasDefault := false
		for _, cc := range s.Cases {
			if cc.List == nil {
				hasDefault = true
			}
			if !c.isTerminatingList(cc.Body) || hasBreakList(cc.Body, s, true) {
				return false
			}
		}
		return hasDefault
	case *SelectStmt: // no default needed: a select waits until a case runs
		for _, cc := range s.Cases {
			if !c.isTerminatingList(cc.Body) || hasBreakList(cc.Body, s, true) {
				return false
			}
		}
		return true
	}
	return false
}

func hasBreakList(list []Stmt, target Stmt, top bool) bool {
	for _, s := range list {
		if hasBreak(s, target, top) {
			return true
		}
	}
	return false
}

// hasBreak reports whether s contains a break that targets target.
func hasBreak(s Stmt, target Stmt, top bool) bool {
	switch s := s.(type) {
	case *BranchStmt:
		return s.Tok == BREAK && s.Target == target
	case *BlockStmt:
		return hasBreakList(s.List, target, top)
	case *ArenaStmt:
		return hasBreak(s.Body, target, top)
	case *IfStmt:
		if hasBreak(s.Body, target, top) {
			return true
		}
		return s.Else != nil && hasBreak(s.Else, target, top)
	case *LabeledStmt:
		return hasBreak(s.Stmt, target, top)
	case *ForStmt:
		return hasBreak(s.Body, target, false)
	case *RangeStmt:
		return hasBreak(s.Body, target, false)
	case *SwitchStmt:
		for _, cc := range s.Cases {
			if hasBreakList(cc.Body, target, false) {
				return true
			}
		}
	case *SelectStmt:
		for _, cc := range s.Cases {
			if hasBreakList(cc.Body, target, false) {
				return true
			}
		}
	case *CaseClause:
		return hasBreakList(s.Body, target, top)
	}
	return false
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
