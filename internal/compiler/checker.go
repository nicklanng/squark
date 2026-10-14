package compiler

import (
	"fmt"
	"go/constant"
	gotoken "go/token"
	"math"
	"strconv"
	"strings"
)

var universe = NewScope(nil)

var builtinNames = []string{"len", "cap", "append", "copy", "make", "delete", "close", "panic", "print", "println", "min", "max", "new"}

func init() {
	for _, t := range []*Basic{tBool, tInt, tInt8, tInt16, tInt32, tInt64, tUint, tUint8, tUint16, tUint32, tUint64, tUintptr, tFloat32, tFloat64, tString, tRawptr} {
		universe.Insert(&Object{Kind: ObjType, Name: t.Name, Type: t, state: 2})
	}
	universe.Insert(&Object{Kind: ObjType, Name: "uint8", Type: tUint8, state: 2})
	universe.Insert(&Object{Kind: ObjType, Name: "rune", Type: tInt32, state: 2})
	universe.Insert(&Object{Kind: ObjType, Name: "error", Type: tError, state: 2})
	universe.Insert(&Object{Kind: ObjConst, Name: "true", Type: tUntypedBool, Const: constant.MakeBool(true), state: 2})
	universe.Insert(&Object{Kind: ObjConst, Name: "false", Type: tUntypedBool, Const: constant.MakeBool(false), state: 2})
	universe.Insert(&Object{Kind: ObjNil, Name: "nil", Type: tUntypedNil, state: 2})
	universe.Insert(&Object{Kind: ObjConst, Name: "iota", Type: tUntypedInt, state: 2})
	for _, b := range builtinNames {
		universe.Insert(&Object{Kind: ObjBuiltin, Name: b, state: 2})
	}
}

type Checker struct {
	prog *Program
	pkg  *Package
	errs ErrorList

	fileImports map[string]*Package // imports of the file being checked
	fileScopes  map[*File]map[string]*Package

	// function context
	fn        *Func
	sig       *Signature
	scope     *Scope
	depth     int
	breakable []Stmt
	labels    map[string]Pos // labels seen in the current function
	loopDepth int
	iota      int64 // -1 when not in a const decl
	varCount  int
	namedRes  []*Var

	calleeIdent *Ident    // identifier being checked as the callee of a direct call
	goCall      *CallExpr // call being checked as the operand of a go statement
	allocArena  Expr      // innermost `in` arena, while checking its operand
}

// Check type-checks the whole program.
func Check(prog *Program) error {
	c := &Checker{prog: prog, fileScopes: map[*File]map[string]*Package{}, iota: -1}
	prog.InitFunc = &Func{Name: "__init", Sig: &Signature{}}
	for _, pkg := range prog.Packages {
		c.collect(pkg)
	}
	for _, pkg := range prog.Packages {
		c.pkg = pkg
		c.resolveDecls(pkg)
	}
	if len(c.errs) > 0 {
		return c.errs
	}
	for _, pkg := range prog.Packages {
		c.pkg = pkg
		c.checkBodies(pkg)
	}
	if len(c.errs) > 0 {
		return c.errs
	}
	main := prog.Main.Scope.LookupLocal("main")
	if main == nil || main.Kind != ObjFunc {
		return ErrorList{{Pos{File: prog.Main.Dir}, "function main is undeclared in the main package"}}
	}
	prog.MainFunc = main.Func
	if len(main.Func.Sig.Params) != 0 || len(main.Func.Sig.Results) != 0 {
		return ErrorList{{main.Pos, "func main must have no arguments and no return values"}}
	}
	return nil
}

func (c *Checker) errorf(pos Pos, format string, args ...any) {
	c.errs = append(c.errs, &Error{pos, fmt.Sprintf(format, args...)})
}

func isExported(name string) bool {
	return len(name) > 0 && name[0] >= 'A' && name[0] <= 'Z'
}

// collect enters all package-level objects into package scopes.
func (c *Checker) collect(pkg *Package) {
	pkg.Scope = NewScope(universe)
	c.pkg = pkg
	declare := func(o *Object) {
		if o.Name == "_" {
			return
		}
		if prev := pkg.Scope.Insert(o); prev != nil {
			c.errorf(o.Pos, "%s redeclared in this package (previous declaration at %s)", o.Name, prev.Pos)
		}
	}
	for _, f := range pkg.Files {
		imps := map[string]*Package{}
		impPaths := map[string]string{}
		for _, imp := range f.Imports {
			dep := imp.Pkg
			if dep == nil {
				c.errorf(imp.P, "unresolved import %q", imp.Path)
				continue
			}
			name := dep.Name
			if imp.Name != nil {
				name = imp.Name.Name
			}
			if prev, ok := impPaths[name]; ok {
				c.errorf(imp.P, "%s redeclared in this block (also imported from %q)", name, prev)
				continue
			}
			impPaths[name] = imp.Path
			imps[name] = dep
		}
		c.fileScopes[f] = imps
		for _, d := range f.Decls {
			switch d := d.(type) {
			case *GenDecl:
				switch d.Tok {
				case TYPE:
					for _, ts := range d.Types {
						o := &Object{Kind: ObjType, Name: ts.Name.Name, Pos: ts.Name.P, Pkg: pkg, TypeDecl: ts}
						n := &Named{Obj: o, Pkg: pkg}
						o.Type = n
						o.decl = f
						ts.Name.Obj = o
						pkg.Types = append(pkg.Types, n)
						declare(o)
					}
				case CONST:
					for _, vs := range d.Values {
						if isErrorConst(vs) || isCompositeConst(vs) {
							// error constants and constant composites (tables, structs) are static
							// read-only values, shared by every scheduler thread
							for _, name := range vs.Names {
								v := &Var{Name: name.Name, Pos: name.P, Global: true, Pkg: pkg, ReadOnly: true, ConstData: isCompositeConst(vs)}
								o := &Object{Kind: ObjVar, Name: name.Name, Pos: name.P, Pkg: pkg, Var: v, decl: f}
								v.Obj = o
								name.Obj = o
								pkg.Globals = append(pkg.Globals, v)
								declare(o)
							}
							pkg.GlobalInits = append(pkg.GlobalInits, vs)
							continue
						}
						for i, name := range vs.Names {
							o := &Object{Kind: ObjConst, Name: name.Name, Pos: name.P, Pkg: pkg, decl: constDecl{vs, i, f}}
							name.Obj = o
							declare(o)
						}
					}
				case VAR:
					for _, vs := range d.Values {
						for _, name := range vs.Names {
							v := &Var{Name: name.Name, Pos: name.P, Global: true, Pkg: pkg}
							o := &Object{Kind: ObjVar, Name: name.Name, Pos: name.P, Pkg: pkg, Var: v, decl: f}
							v.Obj = o
							name.Obj = o
							pkg.Globals = append(pkg.Globals, v)
							declare(o)
						}
						pkg.GlobalInits = append(pkg.GlobalInits, vs)
					}
				}
			case *ArenaDecl:
				v := &Var{Name: d.Name.Name, Pos: d.Name.P, Global: true, Pkg: pkg, Type: tArena, ReadOnly: true, ArenaSizeExpr: d.Size}
				o := &Object{Kind: ObjVar, Name: d.Name.Name, Pos: d.Name.P, Pkg: pkg, Var: v, Type: tArena, decl: f}
				v.Obj = o
				d.Name.Obj = o
				pkg.Globals = append(pkg.Globals, v)
				pkg.Arenas = append(pkg.Arenas, v)
				declare(o)
			case *FuncDecl:
				fn := &Func{Name: d.Name.Name, Decl: d, Pkg: pkg, Pragmas: d.Pragmas}
				for _, pr := range d.Pragmas {
					fields := strings.Fields(pr)
					if len(fields) == 0 {
						continue
					}
					switch fields[0] {
					case "noinline":
					case "extern", "stack", "goroutines":
						c.errorf(d.P, "//squark:%s is gone: use extern func ... stack N, or task ... pool N", fields[0])
					default:
						c.errorf(d.P, "unknown pragma //squark:%s", fields[0])
					}
				}
				switch d.Kind {
				case KindExtern:
					fn.Extern = d.ExternSym
					if fn.Extern == "" {
						fn.Extern = d.Name.Name
					}
					if d.Stack == nil {
						c.errorf(d.P, "extern func %s needs a stack budget (extern func %s(...) stack 8K): the stack proof depends on it", d.Name.Name, d.Name.Name)
					}
				case KindTask:
					fn.IsTask = true
					if d.Pool == nil {
						c.errorf(d.P, "task %s needs a pool size (task %s(...) pool N): the maximum number running at once", d.Name.Name, d.Name.Name)
					}
				}
				if d.Body == nil && d.Kind != KindExtern {
					c.errorf(d.P, "missing function body (use extern func to bind a C function)")
				}
				pkg.Funcs = append(pkg.Funcs, fn)
				c.prog.Funcs = append(c.prog.Funcs, fn)
				if d.Recv != nil {
					continue // attached during resolveDecls
				}
				if d.Name.Name == "init" {
					c.errorf(d.P, "init functions are not supported; initialize in main")
					continue
				}
				o := &Object{Kind: ObjFunc, Name: d.Name.Name, Pos: d.Name.P, Pkg: pkg, Func: fn, decl: f}
				d.Name.Obj = o
				declare(o)
			}
		}
	}
	// as in Go, a file's import names and the package's own names share one namespace
	for _, f := range pkg.Files {
		for _, imp := range f.Imports {
			if imp.Pkg == nil {
				continue
			}
			name := imp.Pkg.Name
			if imp.Name != nil {
				name = imp.Name.Name
			}
			if o := pkg.Scope.LookupLocal(name); o != nil {
				c.errorf(o.Pos, "%s already declared through import of %q (at %s:%d)", name, imp.Path, shortFile(imp.P.File), imp.P.Line)
			}
		}
	}
}

// isErrorConst reports whether a const spec initializes errors: const ErrX = error(404, "x").
func isErrorConst(vs *ValueSpec) bool {
	if len(vs.Values) == 0 {
		return false
	}
	call, ok := vs.Values[0].(*CallExpr)
	if !ok {
		return false
	}
	id, ok := call.Fun.(*Ident)
	return ok && id.Name == "error"
}

// isCompositeConst reports const X = T{...}: a constant table or struct.
func isCompositeConst(vs *ValueSpec) bool {
	if len(vs.Values) == 0 {
		return false
	}
	for _, x := range vs.Values {
		if _, ok := unparen(x).(*CompositeLit); !ok {
			return false
		}
	}
	return true
}

// checkConstData reports where a constant composite isn't made of constants: its type may hold
// numbers, strings, bools, errors and arrays or structs of them, and every element must be a
// constant expression.
func (c *Checker) checkConstData(name string, x Expr) {
	if t := constDataType(x.info().T, map[Type]bool{}); t != nil {
		c.errorf(x.Pos(), "const %s: %s can't be constant data (only numbers, strings, bools, errors and arrays or structs of them)", name, t)
		return
	}
	var walk func(x Expr)
	walk = func(x Expr) {
		switch e := unparen(x).(type) {
		case *CompositeLit:
			for _, el := range e.Elts {
				walk(el)
			}
		case *KeyValueExpr:
			walk(e.Value)
		default:
			if x.info().Const == nil && !isErrorConstExpr(x) {
				c.errorf(x.Pos(), "const %s: %s is not constant", name, exprString(x))
			}
		}
	}
	walk(x)
}

// isErrorConstExpr reports an error constant (error(...) or a name for one) inside a constant composite.
func isErrorConstExpr(x Expr) bool {
	switch e := unparen(x).(type) {
	case *CallExpr:
		return e.Builtin == "__error" || isErrorCall(e)
	}
	v := readOnlyVar(x)
	return v != nil && v.ArenaSize == 0
}

func isErrorCall(e *CallExpr) bool {
	id, ok := e.Fun.(*Ident)
	return ok && id.Name == "error"
}

// constDataType returns the part of t that constant data can't contain, or nil.
func constDataType(t Type, seen map[Type]bool) Type {
	if seen[t] {
		return nil
	}
	seen[t] = true
	switch u := t.Underlying().(type) {
	case *Basic:
		if u.Kind == Rawptr {
			return t
		}
		return nil
	case *ErrorType:
		return nil
	case *Array:
		return constDataType(u.Elem, seen)
	case *Struct:
		for _, f := range u.Fields {
			if bad := constDataType(f.Type, seen); bad != nil {
				return bad
			}
		}
		return nil
	}
	return t
}

type constDecl struct {
	spec  *ValueSpec
	index int
	file  *File
}

// findTask resolves "pkg.name" (or "name" in package main) to a task.
func (c *Checker) findTask(target string) *Func {
	pkgName, name := "main", target
	if i := strings.IndexByte(target, '.'); i >= 0 {
		pkgName, name = target[:i], target[i+1:]
	}
	for _, p := range c.prog.Packages {
		if p.Name != pkgName {
			continue
		}
		for _, f := range p.Funcs {
			if f.Name == name && f.IsTask && f.RecvType == nil {
				return f
			}
		}
	}
	return nil
}

// constSize evaluates a constant non-negative integer clause expression.
func (c *Checker) constSize(x Expr, what string) (int64, bool) {
	c.expr(x)
	v, ok := constInt(x)
	if !ok || !isInteger(x.info().T) || v < 0 {
		c.errorf(x.Pos(), "%s must be a non-negative integer constant", what)
		return 0, false
	}
	return v, true
}

func parseSize(s string) (int64, error) {
	mult := int64(1)
	switch {
	case strings.HasSuffix(s, "K"), strings.HasSuffix(s, "k"):
		mult = 1024
		s = s[:len(s)-1]
	case strings.HasSuffix(s, "M"):
		mult = 1024 * 1024
		s = s[:len(s)-1]
	}
	n, err := strconv.ParseInt(s, 10, 64)
	return n * mult, err
}

func (c *Checker) setFile(f *File) {
	c.fileImports = c.fileScopes[f]
}

func (c *Checker) fileOf(pos Pos) *File {
	for _, f := range c.pkg.Files {
		if f.Path == pos.File {
			return f
		}
	}
	return nil
}

func (c *Checker) resolveDecls(pkg *Package) {
	c.scope = pkg.Scope
	// types
	for _, n := range pkg.Types {
		c.resolveNamed(n)
	}
	for _, n := range pkg.Types {
		c.checkValueCycle(n, nil)
	}
	// constants
	for _, f := range pkg.Files {
		c.setFile(f)
		for _, d := range f.Decls {
			if g, ok := d.(*GenDecl); ok && g.Tok == CONST {
				for _, vs := range g.Values {
					for _, name := range vs.Names {
						if name.Obj != nil && name.Obj.Kind == ObjConst {
							c.resolveConst(name.Obj)
						}
					}
				}
			}
		}
	}
	// package arena sizes
	for _, v := range pkg.Arenas {
		c.setFile(v.Obj.decl.(*File))
		if n, ok := c.constSize(v.ArenaSizeExpr, "arena size"); ok {
			v.ArenaSize = n
		}
	}
	// function signatures and methods
	for _, fn := range pkg.Funcs {
		f := c.fileOf(fn.Decl.P)
		c.setFile(f)
		d := fn.Decl
		var recv *Var
		if d.Recv != nil {
			rt := c.typExpr(d.Recv.Type)
			base := rt
			if p, ok := rt.(*Pointer); ok {
				base = p.Elem
			}
			named, ok := base.(*Named)
			if !ok || named.Pkg != pkg {
				c.errorf(d.Recv.Type.Pos(), "invalid receiver type %s (must be a named type declared in this package)", rt)
				continue
			}
			if _, isPtr := named.Underlying().(*Pointer); isPtr {
				c.errorf(d.Recv.Type.Pos(), "invalid receiver type %s (pointer base type)", rt)
			}
			recv = &Var{Name: d.Recv.Names[0].Name, Type: rt, Pos: d.Recv.Names[0].P, Param: true, Index: -1, Pkg: pkg}
			fn.RecvType = named
			for _, m := range named.Methods {
				if m.Name == fn.Name {
					c.errorf(d.Name.P, "method %s.%s already declared", named.Obj.Name, fn.Name)
				}
			}
			if st, ok := named.Underlying().(*Struct); ok {
				for _, fld := range st.Fields {
					if fld.Name == fn.Name {
						c.errorf(d.Name.P, "field and method with the same name %s", fn.Name)
					}
				}
			}
			named.Methods = append(named.Methods, fn)
		}
		fn.Sig = c.signature(d.Type, recv)
		if d.Pool != nil {
			if n, ok := c.constSize(d.Pool, "pool size"); ok {
				if n < 1 {
					c.errorf(d.Pool.Pos(), "pool size must be at least 1")
				}
				fn.Pool = int(n)
			}
		}
		if d.Stack != nil {
			if n, ok := c.constSize(d.Stack, "stack size"); ok {
				if fn.Extern != "" {
					fn.StackHint = n
				} else {
					fn.StackLimit = n
				}
			}
		}
		if fn.IsTask && len(fn.Sig.Results) > 0 {
			c.errorf(d.P, "task %s cannot return values (a task's results would have nowhere to go)", fn.Name)
		}
		if fn.Extern != "" && recv != nil {
			c.errorf(d.P, "extern functions cannot be methods")
		}
		if fn.Extern != "" && d.Body != nil {
			c.errorf(d.P, "extern function %s must not have a body", fn.Name)
		}
		if d.Name.Obj != nil {
			d.Name.Obj.Type = fn.Sig
		}
	}
	// budget, pool and workers directives (package main only)
	var workersDir, threadsDir *Directive
	for _, d := range pkg.Budgets {
		if pkg != c.prog.Main {
			c.errorf(d.P, "%s is only allowed in package main", d.Name)
			continue
		}
		c.setFile(c.fileOf(d.P))
		if d.Name == "workers" || d.Name == "threads" {
			dir, count := &workersDir, &c.prog.Workers
			if d.Name == "threads" {
				dir, count = &threadsDir, &c.prog.Threads
			}
			if *dir != nil {
				c.errorf(d.P, "duplicate %s", d.Name)
			}
			*dir = d
			if id, ok := d.Value.(*Ident); ok && id.Name == "auto" {
				*count = 0
				continue
			}
			if n, ok := c.constSize(d.Value, d.Name); ok {
				if n < 1 || n > 1024 {
					c.errorf(d.P, "%s must be between 1 and 1024 (or auto: one per core)", d.Name)
				}
				*count = n
			}
			continue
		}
		n, ok := c.constSize(d.Value, d.Name)
		if !ok {
			continue
		}
		switch d.Name {
		case "budget":
			if c.prog.Budget != 0 {
				c.errorf(d.P, "duplicate budget")
			}
			c.prog.Budget = n
		case "pool":
			fn := c.findTask(d.Target)
			if fn == nil {
				c.errorf(d.P, "pool %s: no task named %s (write pkg.task, e.g. pool http.serveConn 256)", d.Target, d.Target)
				continue
			}
			if n < 1 {
				c.errorf(d.P, "pool size must be at least 1")
			}
			fn.Pool = int(n)
		}
	}
	for _, d := range []*Directive{workersDir, threadsDir} {
		if d != nil && c.prog.Budget > 0 && (d == workersDir && c.prog.Workers == 0 || d == threadsDir && c.prog.Threads == 0) {
			c.errorf(d.P, "%s auto can't be checked against the budget: the number of cores isn't known when compiling; write %s N", d.Name, d.Name)
		}
	}
	// global variable types
	for _, f := range pkg.Files {
		c.setFile(f)
		for _, d := range f.Decls {
			g, ok := d.(*GenDecl)
			if !ok || (g.Tok != VAR && g.Tok != CONST) {
				continue
			}
			for _, vs := range g.Values {
				if g.Tok == CONST && !isErrorConst(vs) && !isCompositeConst(vs) {
					continue
				}
				c.globalVarSpec(vs)
			}
		}
	}
}

func (c *Checker) globalVarSpec(vs *ValueSpec) {
	var typ Type
	if vs.Type != nil {
		typ = c.typExpr(vs.Type)
	}
	saveFn := c.fn
	c.fn = nil
	c.depth = 0
	defer func() { c.fn = saveFn }()
	if len(vs.Values) == 0 {
		for _, n := range vs.Names {
			n.Obj.Var.Type = typ
			n.Obj.Type = typ
		}
		return
	}
	if len(vs.Values) != len(vs.Names) {
		if len(vs.Values) == 1 && len(vs.Names) > 1 {
			c.errorf(vs.P, "multi-value initialization of package-level variables is not supported")
			return
		}
		c.errorf(vs.P, "assignment mismatch: %d variables but %d values", len(vs.Names), len(vs.Values))
		return
	}
	for i, n := range vs.Names {
		x := vs.Values[i]
		c.exprHint(x, typ)
		t := typ
		if t == nil {
			c.defaultUntyped(x)
			t = x.info().T
			if isNilType(t) {
				c.errorf(x.Pos(), "use of untyped nil in variable declaration")
				t = tInvalid
			} else if isStorageCopy(t) {
				c.errorf(x.Pos(), "cannot copy %s storage into %s: use a view (%s) instead", t, n.Name, viewOf(t))
			} else {
				c.checkNestedStorageCopy(x, t, " into "+n.Name)
			}
		} else {
			c.assignable(x, t, "variable declaration")
		}
		if n.Obj.Var.ConstData {
			c.checkConstData(n.Name, x)
		}
		n.Obj.Var.Type = t
		n.Obj.Type = t
	}
}

func isNilType(t Type) bool {
	b, ok := t.(*Basic)
	return ok && b.Kind == UntypedNil
}

func (c *Checker) resolveNamed(n *Named) {
	o := n.Obj
	if o.state == 2 {
		return
	}
	if o.state == 1 {
		return // recursive reference through pointer etc is fine; underlying resolved later
	}
	o.state = 1
	saved := c.fileImports
	c.setFile(o.decl.(*File))
	u := c.typExpr(o.TypeDecl.Type)
	c.fileImports = saved
	if nn, ok := u.(*Named); ok {
		c.resolveNamed(nn)
		if nn.Obj.state == 1 { // type A B; type B A
			c.errorf(o.Pos, "invalid recursive type %s", o.Name)
			u = tInvalid
		} else {
			u = nn.Underlying()
		}
	}
	n.underlying = u
	o.state = 2
}

// checkValueCycle reports types that contain themselves by value.
func (c *Checker) checkValueCycle(t Type, path []*Named) bool {
	switch u := t.(type) {
	case *Named:
		for _, p := range path {
			if p == u {
				c.errorf(u.Obj.Pos, "invalid recursive type %s (contains itself by value)", u.Obj.Name)
				return true
			}
		}
		return c.checkValueCycle(u.underlying, append(path, u))
	case *Array:
		return c.checkValueCycle(u.Elem, path)
	case *VecType: // vectors, map and channel storage hold their elements inline
		return c.checkValueCycle(u.Elem, path)
	case *MapStore:
		return c.checkValueCycle(u.Key, path) || c.checkValueCycle(u.Value, path)
	case *ChanStore:
		return c.checkValueCycle(u.Elem, path)
	case *Struct:
		for _, f := range u.Fields {
			if c.checkValueCycle(f.Type, path) {
				return true
			}
		}
	}
	return false
}

func (c *Checker) resolveConst(o *Object) {
	if o.state == 2 {
		return
	}
	if o.state == 1 {
		c.errorf(o.Pos, "initialization cycle for constant %s", o.Name)
		o.Type = tInvalid
		o.Const = constant.MakeUnknown()
		return
	}
	o.state = 1
	cd := o.decl.(constDecl)
	savedImports := c.fileImports
	savedIota := c.iota
	c.setFile(cd.file)
	c.iota = int64(cd.spec.Iota)
	defer func() {
		c.fileImports = savedImports
		c.iota = savedIota
		o.state = 2
	}()
	var typ Type
	if cd.spec.Type != nil {
		typ = c.typExpr(cd.spec.Type)
	}
	if cd.index >= len(cd.spec.Values) {
		c.errorf(o.Pos, "missing init expr for const declaration")
		o.Type = tInvalid
		o.Const = constant.MakeUnknown()
		return
	}
	x := cd.spec.Values[cd.index]
	// constant expressions may be shared between specs via implicit repetition; recheck with fresh info
	x = cloneExpr(x)
	c.expr(x)
	if x.info().Const == nil {
		if x.info().Mode != ModeInvalid {
			c.errorf(x.Pos(), "%s is not constant", exprString(x))
		}
		o.Type = tInvalid
		o.Const = constant.MakeUnknown()
		return
	}
	if typ != nil {
		c.assignable(x, typ, "constant declaration")
		o.Type = typ
	} else {
		o.Type = x.info().T
	}
	o.Const = x.info().Const
}

// cloneExpr makes a shallow structural copy of an expression tree so that re-checking does not
// clobber previous results (needed for implicitly repeated const specs with iota).
func cloneExpr(x Expr) Expr {
	switch e := x.(type) {
	case *Ident:
		n := *e
		n.ExprInfo = ExprInfo{}
		return &n
	case *BasicLit:
		n := *e
		n.ExprInfo = ExprInfo{}
		return &n
	case *ParenExpr:
		return &ParenExpr{P: e.P, X: cloneExpr(e.X)}
	case *UnaryExpr:
		return &UnaryExpr{P: e.P, Op: e.Op, X: cloneExpr(e.X)}
	case *BinaryExpr:
		return &BinaryExpr{P: e.P, Op: e.Op, X: cloneExpr(e.X), Y: cloneExpr(e.Y)}
	case *CallExpr:
		n := &CallExpr{P: e.P, Fun: cloneExpr(e.Fun), Ellipsis: e.Ellipsis}
		for _, a := range e.Args {
			n.Args = append(n.Args, cloneExpr(a))
		}
		return n
	case *SelectorExpr:
		return &SelectorExpr{X: cloneExpr(e.X), Sel: &Ident{P: e.Sel.P, Name: e.Sel.Name}}
	}
	return x
}

func (c *Checker) signature(ft *FuncType, recv *Var) *Signature {
	sig := &Signature{Recv: recv}
	idx := 0
	for i, f := range ft.Params {
		t := c.typExpr(f.Type)
		if f.Variadic {
			if i != len(ft.Params)-1 || len(f.Names) > 1 {
				c.errorf(f.Type.Pos(), "can only use ... with final parameter in list")
			}
			t = &Slice{Elem: t}
			sig.Variadic = true
		}
		if len(f.Names) == 0 {
			sig.Params = append(sig.Params, &Var{Name: "_", Type: t, Param: true, Index: idx, Pos: f.Type.Pos()})
			idx++
		}
		for _, n := range f.Names {
			sig.Params = append(sig.Params, &Var{Name: n.Name, Type: t, Param: true, Index: idx, Pos: n.P})
			idx++
		}
	}
	for _, f := range ft.Results {
		t := c.typExpr(f.Type)
		if len(f.Names) == 0 {
			sig.Results = append(sig.Results, &Var{Name: "", Type: t, Pos: f.Type.Pos()})
		}
		for _, n := range f.Names {
			sig.Results = append(sig.Results, &Var{Name: n.Name, Type: t, Pos: n.P})
		}
	}
	c.contract(ft, sig, recv)
	ft.T = sig
	ft.Mode = ModeType
	return sig
}

// contract records declared provenance (from / into / static) on a signature.
func (c *Checker) contract(ft *FuncType, sig *Signature, recv *Var) {
	slots := map[string]int{}
	base := 0
	if recv != nil {
		slots[recv.Name] = 0
		base = 1
	}
	for i, p := range sig.Params {
		slots[p.Name] = base + i
	}
	slotOf := func(id *Ident) (int, bool) {
		s, ok := slots[id.Name]
		if !ok || id.Name == "_" {
			c.errorf(id.P, "%s is not a parameter of this function", id.Name)
		}
		return s, ok
	}
	idx := base
	for _, f := range ft.Params {
		n := max(len(f.Names), 1)
		for k := 0; k < n; k++ {
			if f.Static {
				sig.HasContract = true
				sig.StaticSlots = append(sig.StaticSlots, idx)
			}
			if f.Into != nil {
				sig.HasContract = true
				if dst, ok := slotOf(f.Into); ok {
					sig.Into = append(sig.Into, [2]int{idx, dst})
				}
			}
			idx++
		}
	}
	for _, id := range ft.From {
		sig.HasContract = true
		if s, ok := slotOf(id); ok {
			sig.From = append(sig.From, s)
		}
	}
	if ft.ResultStatic {
		sig.HasContract = true
		sig.ResultStatic = true
	}
}

// typExpr evaluates a type expression.
func (c *Checker) typExpr(x Expr) Type {
	t := c.typExpr1(x)
	x.info().T = t
	x.info().Mode = ModeType
	return t
}

func (c *Checker) typExpr1(x Expr) Type {
	switch e := x.(type) {
	case *Ident:
		o := c.lookup(e.Name)
		if o == nil {
			c.errorf(e.P, "undefined: %s", e.Name)
			return tInvalid
		}
		if o.Kind != ObjType {
			c.errorf(e.P, "%s is not a type", e.Name)
			return tInvalid
		}
		e.Obj = o
		if n, ok := o.Type.(*Named); ok && n.Obj.state == 0 {
			c.resolveNamed(n)
		}
		return o.Type
	case *SelectorExpr:
		id, ok := e.X.(*Ident)
		if ok {
			if pkg := c.fileImports[id.Name]; pkg != nil && c.lookupLocalShadow(id.Name) == nil {
				o := pkg.Scope.LookupLocal(e.Sel.Name)
				if o == nil || o.Kind != ObjType {
					c.errorf(e.Sel.P, "undefined type: %s.%s", id.Name, e.Sel.Name)
					return tInvalid
				}
				if !isExported(e.Sel.Name) {
					c.errorf(e.Sel.P, "%s.%s is not exported", id.Name, e.Sel.Name)
				}
				e.Sel.Obj = o
				e.Kind = SelQualified
				return o.Type
			}
		}
		c.errorf(e.Pos(), "%s is not a type", exprString(e))
		return tInvalid
	case *ParenExpr:
		return c.typExpr(e.X)
	case *StarExpr:
		return &Pointer{Elem: c.typExpr(e.X)}
	case *ArrayType:
		elem := c.typExpr(e.Elem)
		if e.Len == nil {
			return &Slice{Elem: elem}
		}
		if bl, ok := e.Len.(*BasicLit); ok && bl.Kind == ELLIPSIS {
			c.errorf(e.P, "[...] array length only allowed in composite literals")
			return tInvalid
		}
		c.expr(e.Len)
		cv := e.Len.info().Const
		if cv == nil || !isInteger(e.Len.info().T) {
			c.errorf(e.Len.Pos(), "array length %s must be a constant integer", exprString(e.Len))
			return tInvalid
		}
		n, ok := constant.Int64Val(constant.ToInt(cv))
		if !ok || n < 0 {
			c.errorf(e.Len.Pos(), "invalid array length %s", exprString(e.Len))
			return tInvalid
		}
		return &Array{Len: n, Elem: elem}
	case *ChanType:
		elem := c.typExpr(e.Elem)
		if e.Cap != nil {
			n, _ := c.constSize(e.Cap, "channel capacity")
			return &ChanStore{Elem: elem, Cap: n}
		}
		return &Chan{Elem: elem}
	case *MapType:
		k := c.typExpr(e.Key)
		v := c.typExpr(e.Value)
		if !validMapKey(k) {
			c.errorf(e.Key.Pos(), "invalid map key type %s (must be an integer, string, bool or pointer)", k)
		}
		if e.Cap != nil {
			n, _ := c.constSize(e.Cap, "map capacity")
			if n < 1 {
				c.errorf(e.Cap.Pos(), "map capacity must be at least 1")
			}
			return &MapStore{Key: k, Value: v, Cap: n}
		}
		return &Map{Key: k, Value: v}
	case *VecTypeExpr:
		elem := c.typExpr(e.Elem)
		n, _ := c.constSize(e.Len, "vector capacity")
		return &VecType{Len: n, Elem: elem}
	case *FuncType:
		return c.signature(e, nil)
	case *ArenaTypeExpr:
		return tArena
	case *StructType:
		st := &Struct{}
		seen := map[string]bool{}
		for _, f := range e.Fields {
			t := c.typExpr(f.Type)
			for _, n := range f.Names {
				if seen[n.Name] && n.Name != "_" {
					c.errorf(n.P, "duplicate field %s", n.Name)
				}
				seen[n.Name] = true
				st.Fields = append(st.Fields, &StructField{Name: n.Name, Type: t, Pos: n.P})
			}
		}
		return st
	}
	c.errorf(x.Pos(), "%s is not a type", exprString(x))
	return tInvalid
}

func validMapKey(t Type) bool {
	switch u := t.Underlying().(type) {
	case *Basic:
		return u.Kind == String || u.Kind == Bool || (u.Kind >= Int && u.Kind <= Uintptr)
	case *Pointer:
		return true
	}
	return false
}

// lookupLocalShadow returns a non-package object for name if one shadows an import.
func (c *Checker) lookupLocalShadow(name string) *Object {
	for s := c.scope; s != nil && s != universe; s = s.parent {
		if o, ok := s.names[name]; ok {
			return o
		}
	}
	return nil
}

func (c *Checker) lookup(name string) *Object {
	if o := c.lookupLocalShadow(name); o != nil {
		return o
	}
	if pkg := c.fileImports[name]; pkg != nil {
		return &Object{Kind: ObjPkg, Name: name, PkgRef: pkg}
	}
	return universe.LookupLocal(name)
}

// ---- function bodies ----

func (c *Checker) checkBodies(pkg *Package) {
	for _, fn := range pkg.Funcs {
		if fn.Sig == nil || fn.Decl.Body == nil {
			continue
		}
		c.setFile(c.fileOf(fn.Decl.P))
		c.funcBody(fn)
	}
	// global initializers are checked in resolveDecls via globalVarSpec.
}

// newVarName returns the C name of a local: unique through the counter, and never containing "__",
// which every package-level C name (pkg__F, G_pkg__v) does, so a local cannot shadow one.
func (c *Checker) newVarName(name string) string {
	c.varCount++
	n := fmt.Sprintf("%s_%d", sanitize(name), c.varCount)
	for strings.Contains(n, "__") {
		n = strings.ReplaceAll(n, "__", "_")
	}
	return n
}

func sanitize(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r < 128 && (r == '_' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9') {
			b.WriteRune(r)
		} else {
			fmt.Fprintf(&b, "_u%x", r)
		}
	}
	return b.String()
}

func (c *Checker) funcBody(fn *Func) {
	c.fn = fn
	c.sig = fn.Sig
	c.scope = NewScope(c.pkg.Scope)
	c.depth = 1
	c.varCount = 0
	c.breakable = nil
	c.labels = map[string]Pos{}
	c.loopDepth = 0
	c.namedRes = nil
	defer func() { c.fn = nil; c.scope = c.pkg.Scope }()

	declareParam := func(v *Var) {
		v.Depth = 1
		v.CName = c.newVarName(v.Name)
		if v.Name == "_" || v.Name == "" {
			return
		}
		o := &Object{Kind: ObjVar, Name: v.Name, Type: v.Type, Pos: v.Pos, Var: v, Pkg: c.pkg}
		v.Obj = o
		if prev := c.scope.Insert(o); prev != nil {
			c.errorf(v.Pos, "duplicate argument %s", v.Name)
		}
	}
	if fn.Sig.Recv != nil {
		declareParam(fn.Sig.Recv)
	}
	for _, p := range fn.Sig.Params {
		declareParam(p)
	}
	for _, r := range fn.Sig.Results {
		if r.Name != "" {
			r.Depth = 2
			r.CName = c.newVarName(r.Name)
			if r.Name != "_" {
				o := &Object{Kind: ObjVar, Name: r.Name, Type: r.Type, Pos: r.Pos, Var: r, Pkg: c.pkg}
				r.Obj = o
				if prev := c.scope.Insert(o); prev != nil {
					c.errorf(r.Pos, "duplicate result %s", r.Name)
				}
			}
			c.namedRes = append(c.namedRes, r)
		}
	}
	c.depth = 2
	c.stmtList(fn.Decl.Body.List)
	if len(fn.Sig.Results) > 0 && !c.isTerminatingList(fn.Decl.Body.List) {
		c.errorf(fn.Decl.Body.End, "missing return")
	}
}

func (c *Checker) openScope() {
	c.scope = NewScope(c.scope)
	c.depth++
}

func (c *Checker) closeScope() {
	c.scope = c.scope.parent
	c.depth--
}

func (c *Checker) declareVar(id *Ident, t Type) *Var {
	v := &Var{Name: id.Name, Type: t, Pos: id.P, Depth: c.depth, Pkg: c.pkg}
	v.CName = c.newVarName(id.Name)
	o := &Object{Kind: ObjVar, Name: id.Name, Type: t, Pos: id.P, Var: v, Pkg: c.pkg}
	v.Obj = o
	id.Obj = o
	id.T = t
	id.Mode = ModeVar
	if id.Name != "_" {
		if prev := c.scope.Insert(o); prev != nil {
			c.errorf(id.P, "%s redeclared in this block", id.Name)
		}
	}
	return v
}

// ---- constants ----

func representable(v constant.Value, t Type) (constant.Value, bool) {
	k := basicKind(t)
	switch {
	case k == Bool || k == UntypedBool:
		return v, v.Kind() == constant.Bool
	case k == String || k == UntypedString:
		return v, v.Kind() == constant.String
	case k >= Int && k <= Uintptr:
		if v.Kind() == constant.Float {
			iv := constant.ToInt(v)
			if iv.Kind() != constant.Int {
				return v, false
			}
			v = iv
		}
		if v.Kind() != constant.Int {
			return v, false
		}
		var lo, hi constant.Value
		switch k {
		case Int8:
			lo, hi = constant.MakeInt64(-128), constant.MakeInt64(127)
		case Int16:
			lo, hi = constant.MakeInt64(-32768), constant.MakeInt64(32767)
		case Int32:
			lo, hi = constant.MakeInt64(-1<<31), constant.MakeInt64(1<<31-1)
		case Int, Int64:
			lo, hi = constant.MakeInt64(-1<<63), constant.MakeInt64(1<<63-1)
		case Uint8:
			lo, hi = constant.MakeInt64(0), constant.MakeInt64(255)
		case Uint16:
			lo, hi = constant.MakeInt64(0), constant.MakeInt64(65535)
		case Uint32:
			lo, hi = constant.MakeInt64(0), constant.MakeInt64(1<<32-1)
		default:
			lo, hi = constant.MakeInt64(0), constant.MakeUint64(1<<64-1)
		}
		ok := constant.Compare(v, gotoken.GEQ, lo) && constant.Compare(v, gotoken.LEQ, hi)
		return v, ok
	case k == Float32 || k == Float64:
		if v.Kind() != constant.Int && v.Kind() != constant.Float {
			return v, false
		}
		// round to the type's precision, as the C value will be; too large is an overflow
		f := constant.ToFloat(v)
		var r float64
		if k == Float32 {
			r32, _ := constant.Float32Val(f)
			r = float64(r32)
		} else {
			r, _ = constant.Float64Val(f)
		}
		if math.IsInf(r, 0) {
			return v, false
		}
		return constant.MakeFloat64(r), true
	case k == UntypedFloat:
		if v.Kind() == constant.Int || v.Kind() == constant.Float {
			return constant.ToFloat(v), true
		}
		return v, false
	case k == UntypedInt || k == UntypedRune:
		return v, v.Kind() == constant.Int
	}
	return v, false
}
