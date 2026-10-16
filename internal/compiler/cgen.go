package compiler

import (
	"fmt"
	"go/constant"
	"math"
	"sort"
	"strconv"
	"strings"
)

// Output is the result of C code generation.
type Output struct {
	MainC string
	Pools []*PoolInfo
}

// PoolInfo describes one goroutine pool (one per go-target function, plus main).
type PoolInfo struct {
	CName       string   // C symbol of the sq_pool
	Name        string   // display name (entry function)
	Count       int      // max live goroutines
	Trampolines []string // C functions that start goroutines in this pool
	Entry       *Func    // nil for the main pool
	StackSize   int64    // filled by the driver
}

type cdef struct {
	name  string
	body  string
	deps  []string
	fnptr bool
}

type gen struct {
	prog *Program

	defs     []*cdef
	defByKey map[string]*cdef
	eqFuncs  map[string]string
	eqOrder  []string
	pkgNames map[*Package]string
	cnames   map[string]bool // package-level C names in use (see uniqueCName)

	// function state
	fn          *Func
	out         *strings.Builder
	ind         int
	tmpN        int
	hoisted     []string
	topDecls    []string
	labelIDs    map[Stmt]int
	selRecv     map[*UnaryExpr][2]string // a select case's receive: the value and ok the select delivered
	preemptFns  map[*Func]bool           // reachable from a preemptible task: loops check for preemption
	fnPreempt   bool                     // the function being generated is one of them
	usesPreempt bool                     // it has a loop (needs sq_pf)
	nextLbl     int
	file        string

	goSites       strings.Builder // trampolines
	pools         map[*Func]*PoolInfo
	poolOrder     []*PoolInfo
	globals       strings.Builder // members of struct sq_globals: package variables, one copy per shard
	statics       strings.Builder // members for storage hoisted from package-level initializers
	staticNames   []string        // their names, macro'd to sq_gl-> inside the init function only
	arenaInits    strings.Builder
	constGlobals  strings.Builder // error constants: static const, shared by all shards
	sharedGlobals strings.Builder // shared vars: one static copy, set before the other shards start
	sharedInits   strings.Builder // shared channels' setup, on shard 0 before anything else
	constInit     map[*Var]Expr   // constants' initializers
	constC        map[*Var]string // their C values, inlined where one constant's initializer uses
	// another (a const variable isn't a constant expression in C); functions read them by name
	inConstInit  bool
	usesGlobals  bool                  // the function being generated reads or writes a package variable
	addressed    map[*Var]bool         // its locals whose address is taken (see addressedVars)
	unchecked    map[*IndexExpr]string // indexes a loop's unchecked copy leaves unchecked, and the local holding x's header if any (cgen_loop.go)
	headerCopies map[hdrKey]string     // in a loop's unchecked copy: the locals holding field and package slices
}

// constValue returns a constant's C value (computed once).
func (g *gen) constValue(v *Var) string {
	if c, ok := g.constC[v]; ok {
		return c
	}
	saved := g.inConstInit
	g.inConstInit = true
	c := g.coerce(g.constInit[v], v.Type)
	g.inConstInit = saved
	g.constC[v] = c
	return c
}

// uniqueCName reserves a package-level C name, adding a numeric suffix if it is already taken.
func (g *gen) uniqueCName(n string) string {
	base := n
	for i := 2; g.cnames[n]; i++ {
		n = fmt.Sprintf("%s_%d", base, i)
	}
	g.cnames[n] = true
	return n
}

// Generate emits C for the program.
func Generate(prog *Program, order []*Func) (*Output, error) {
	g := &gen{
		prog:     prog,
		defByKey: map[string]*cdef{},
		eqFuncs:  map[string]string{},
		pkgNames: map[*Package]string{},
		cnames:   map[string]bool{},
		pools:    map[*Func]*PoolInfo{},
		labelIDs: map[Stmt]int{},
	}
	used := map[string]bool{}
	for _, p := range prog.Packages {
		n := sanitize(p.Name)
		for i := 2; used[n]; i++ {
			n = fmt.Sprintf("%s%d", sanitize(p.Name), i)
		}
		used[n] = true
		g.pkgNames[p] = n
	}
	// Functions, methods and globals share the C namespace with externs: a clash (method T.M and a
	// function T__M both want pkg__T__M) gets a numeric suffix.
	for _, f := range prog.Funcs {
		if f.Extern != "" {
			f.CName = f.Extern
			g.cnames[f.CName] = true
			if f.Blocking {
				// callers call a wrapper that runs the C function on a helper thread
				f.CName = g.uniqueCName("sq_blk_" + sanitize(f.Extern))
			}
		}
	}
	for _, f := range prog.Funcs {
		switch {
		case f.Extern != "":
		case f.RecvType != nil:
			f.CName = g.uniqueCName(g.pkgNames[f.Pkg] + "__" + sanitize(f.RecvType.Obj.Name) + "__" + sanitize(f.Name))
		default:
			f.CName = g.uniqueCName(g.pkgNames[f.Pkg] + "__" + sanitize(f.Name))
		}
	}
	prog.InitFunc.CName = "sq_init_globals"
	// loops in preemptible tasks and everything they call check for preemption; elsewhere none
	g.preemptFns = map[*Func]bool{}
	var mark func(f *Func)
	mark = func(f *Func) {
		if g.preemptFns[f] || f.Extern != "" {
			return
		}
		g.preemptFns[f] = true
		for _, c := range prog.Callees(f) {
			mark(c)
		}
	}
	for _, f := range prog.Funcs {
		if f.Preemptible {
			mark(f)
		}
	}
	mainPool := &PoolInfo{CName: "sq_main_pool", Name: "main.main", Count: 1, Trampolines: []string{"sq_program_main"}}
	g.poolOrder = append(g.poolOrder, mainPool)

	var funcs strings.Builder
	var protos strings.Builder
	for _, f := range prog.Funcs {
		if f.Sig == nil {
			continue
		}
		if f.Extern != "" {
			if f.Blocking {
				protos.WriteString(g.blockingWrapper(f))
				continue
			}
			protos.WriteString("extern " + g.proto(f) + ";\n")
			continue
		}
		if f.Decl.Body == nil {
			continue
		}
		protos.WriteString("static " + g.proto(f) + ";\n")
	}
	// globals
	constInit := map[*Var]Expr{} // constants: read-only, initialized once, shared by all shards
	g.constInit, g.constC = constInit, map[*Var]string{}
	for _, p := range prog.Packages {
		for _, vs := range p.GlobalInits {
			for i, n := range vs.Names {
				if v := n.Obj.Var; v != nil && v.ReadOnly && v.ArenaSize == 0 && !v.Shared && i < len(vs.Values) {
					constInit[v] = vs.Values[i]
				}
			}
		}
	}
	for _, p := range prog.Packages {
		for _, v := range p.Globals {
			if v.Type == nil {
				continue
			}
			// package variables are members of struct sq_globals (one per shard), reached through
			// sq_gl, the function's copy of the current shard's pointer
			member := g.uniqueCName("G_" + g.pkgNames[p] + "__" + sanitize(v.Name))
			if v.Name == "_" {
				member = fmt.Sprintf("G_%s__blank%d", g.pkgNames[p], g.tmpN)
				g.tmpN++
			}
			if x, ok := constInit[v]; ok {
				v.CName = member
				_ = x
				fmt.Fprintf(&g.constGlobals, "static const %s = %s;\n", g.decl(v.Type, member), g.constValue(v))
				continue
			}
			if v.Shared {
				// one copy for every shard, set by shard 0 before the others start
				v.CName = member
				fmt.Fprintf(&g.sharedGlobals, "static %s;\n", g.decl(v.Type, member))
				// shared channels (alone or in arrays and structs): set up eagerly, since lazy
				// setup on first use would race
				g.initSharedChans(member, v.Type, 2)
				continue
			}
			v.CName = "sq_gl->" + member
			if v.ArenaSize > 0 {
				// package arena: storage in the shard's globals, bump-only
				fmt.Fprintf(&g.globals, "\tuint8_t %s_buf[%d] __attribute__((aligned(16)));\n\tsq_arena %s_arena;\n\tsq_arena *%s;\n", member, v.ArenaSize, member, member)
				fmt.Fprintf(&g.arenaInits, "\tsq_gl->%s_arena = (sq_arena){sq_gl->%s_buf, 0, %d, %s};\n\tsq_gl->%s = &sq_gl->%s_arena;\n", member, member, v.ArenaSize, cString(p.Name+"."+v.Name), member, member)
				continue
			}
			fmt.Fprintf(&g.globals, "\t%s;\n", g.decl(v.Type, member))
		}
	}
	for _, f := range prog.Funcs {
		if f.Sig == nil || f.Extern != "" || f.Decl.Body == nil {
			continue
		}
		funcs.WriteString(g.function(f))
	}
	initFn := g.initFunction()

	var b strings.Builder
	b.WriteString("// Code generated by squark. DO NOT EDIT.\n#include \"sq_rt.h\"\n\n")
	b.WriteString("static uint64_t sq_zerobase[2];\n")
	b.WriteString(g.typeDecls())
	for _, k := range g.eqOrder {
		b.WriteString(g.eqFuncs[k])
	}
	b.WriteString("\n// pools\n")
	for _, p := range g.poolOrder {
		if p.CName != "sq_main_pool" {
			fmt.Fprintf(&b, "extern sq_pool %s;\n", p.CName)
		}
	}
	b.WriteString("\n// constants and shared vars: one copy for all shards\n")
	b.WriteString(g.constGlobals.String())
	b.WriteString(g.sharedGlobals.String())
	b.WriteString("\n// package variables: one copy per shard (scheduler thread)\nstruct sq_globals {\n")
	b.WriteString(g.globals.String())
	b.WriteString(g.statics.String())
	b.WriteString("};\nstatic struct sq_globals sq_globals0;\n")
	b.WriteString("static _Thread_local struct sq_globals *sq_globals_self = &sq_globals0;\n")
	b.WriteString("const size_t sq_globals_size = sizeof(struct sq_globals);\n")
	b.WriteString("void sq_globals_set(void *p) { sq_globals_self = p; }\n")
	b.WriteString("\n// prototypes\n")
	b.WriteString(protos.String())
	b.WriteString(g.goSites.String())
	b.WriteString("\n")
	b.WriteString(funcs.String())
	b.WriteString(initFn)
	mainName := prog.MainFunc.CName
	// shard 0 initializes first (including shared vars), then the other shards start (threads N)
	fmt.Fprintf(&b, "\nvoid sq_program_main(void *args) {\n\t(void)args;\n\tbool first = sq_shard_id() == 0;\n\tsq_init_globals(first);\n\tif (first) sq_start_shards();\n\t%s();\n}\n", mainName)
	return &Output{MainC: b.String(), Pools: g.poolOrder}, nil
}

// ---- types ----

func (g *gen) basicC(k BasicKind) string {
	switch k {
	case Bool, UntypedBool:
		return "bool"
	case Int, Int64, UntypedInt:
		return "int64_t"
	case Int8:
		return "int8_t"
	case Int16:
		return "int16_t"
	case Int32, UntypedRune:
		return "int32_t"
	case Uint, Uint64:
		return "uint64_t"
	case Uint8:
		return "uint8_t"
	case Uint16:
		return "uint16_t"
	case Uint32:
		return "uint32_t"
	case Uintptr:
		return "uintptr_t"
	case Float32:
		return "float"
	case Float64, UntypedFloat:
		return "double"
	case String, UntypedString:
		return "sq_string"
	case Rawptr, UntypedNil:
		return "void*"
	}
	return "int"
}

func (g *gen) typeKey(t Type) string {
	switch u := t.(type) {
	case *Named:
		return "N" + u.Pkg.Path + "." + u.Obj.Name
	case *Basic:
		return u.Name
	case *Pointer:
		return "*" + g.typeKey(u.Elem)
	case *Slice:
		return "[]" + g.typeKey(u.Elem)
	case *Array:
		return fmt.Sprintf("[%d]%s", u.Len, g.typeKey(u.Elem))
	case *Chan:
		return "chan"
	case *Map:
		return "map"
	case *ErrorType:
		return "error"
	case *ArenaType:
		return "arena"
	case *SIMD:
		return u.Name
	case *VecType:
		return fmt.Sprintf("[..%d]%s", u.Len, g.typeKey(u.Elem))
	case *MapStore:
		return fmt.Sprintf("map[%s]%s cap %d", g.typeKey(u.Key), g.typeKey(u.Value), u.Cap)
	case *ChanStore:
		return fmt.Sprintf("chan %s cap %d", g.typeKey(u.Elem), u.Cap)
	case *Struct:
		var parts []string
		for _, f := range u.Fields {
			parts = append(parts, f.Name+" "+g.typeKey(f.Type))
		}
		return "struct{" + strings.Join(parts, ";") + "}"
	case *Signature:
		var ps, rs []string
		for _, p := range u.Params {
			ps = append(ps, g.typeKey(p.Type))
		}
		for _, r := range u.Results {
			rs = append(rs, g.typeKey(r.Type))
		}
		return "func(" + strings.Join(ps, ",") + ")(" + strings.Join(rs, ",") + ")"
	case *Tuple:
		var rs []string
		for _, r := range u.Types {
			rs = append(rs, g.typeKey(r))
		}
		return "(" + strings.Join(rs, ",") + ")"
	}
	return "?"
}

func (g *gen) ctype(t Type) string {
	switch u := t.(type) {
	case *Basic:
		return g.basicC(u.Kind)
	case *SIMD:
		return simdC(u)
	case *ErrorType:
		return "sq_error"
	case *ArenaType:
		return "sq_arena*"
	case *Named:
		if st, ok := u.Underlying().(*Struct); ok {
			key := g.typeKey(u)
			name := "T_" + g.pkgNames[u.Pkg] + "__" + sanitize(u.Obj.Name)
			return g.structDef(key, name, st)
		}
		return g.ctype(u.Underlying())
	case *Pointer:
		return g.ctype(u.Elem) + "*"
	case *Chan:
		return "sq_chan*"
	case *Map:
		return "sq_map*"
	case *Struct:
		key := g.typeKey(u)
		return g.structDef(key, fmt.Sprintf("sq_st%d", len(g.defs)), u)
	case *Slice:
		if g.ctype(u.Elem) == "uint8_t" {
			return "sq_bytes" // the runtime's own type, so its functions' declarations agree
		}
		key := g.typeKey(u)
		if d, ok := g.defByKey[key]; ok {
			return d.name
		}
		d := &cdef{name: fmt.Sprintf("sq_sl%d", len(g.defs))}
		g.defByKey[key] = d
		g.defs = append(g.defs, d)
		elem := g.ctype(u.Elem)
		d.body = fmt.Sprintf("struct %s { %s *p; int64_t len, cap; };\n", d.name, elem)
		return d.name
	case *Array:
		key := g.typeKey(u)
		if d, ok := g.defByKey[key]; ok {
			return d.name
		}
		d := &cdef{name: fmt.Sprintf("sq_ar%d", len(g.defs))}
		g.defByKey[key] = d
		g.defs = append(g.defs, d)
		elem := g.ctype(u.Elem)
		d.body = fmt.Sprintf("struct %s { %s a[%d]; };\n", d.name, elem, u.Len)
		d.deps = g.valueDeps(u.Elem)
		return d.name
	case *VecType:
		key := g.typeKey(u)
		if d, ok := g.defByKey[key]; ok {
			return d.name
		}
		d := &cdef{name: fmt.Sprintf("sq_vec%d", len(g.defs))}
		g.defByKey[key] = d
		g.defs = append(g.defs, d)
		d.body = fmt.Sprintf("struct %s { int64_t len; %s a[%d]; };\n", d.name, g.ctype(u.Elem), max(u.Len, 1))
		d.deps = g.valueDeps(u.Elem)
		return d.name
	case *MapStore:
		key := g.typeKey(u)
		if d, ok := g.defByKey[key]; ok {
			return d.name
		}
		d := &cdef{name: fmt.Sprintf("sq_mapst%d", len(g.defs))}
		g.defByKey[key] = d
		g.defs = append(g.defs, d)
		slots := mapSlots(u.Cap)
		d.body = fmt.Sprintf("struct %s { sq_map h; %s k[%d]; %s v[%d]; uint8_t c[%d]; };\n", d.name, g.ctype(u.Key), slots, g.ctype(u.Value), slots, slots)
		d.deps = append(g.valueDeps(u.Key), g.valueDeps(u.Value)...)
		return d.name
	case *ChanStore:
		key := g.typeKey(u)
		if d, ok := g.defByKey[key]; ok {
			return d.name
		}
		d := &cdef{name: fmt.Sprintf("sq_chanst%d", len(g.defs))}
		g.defByKey[key] = d
		g.defs = append(g.defs, d)
		d.body = fmt.Sprintf("struct %s { sq_chan h; %s buf[%d]; };\n", d.name, g.ctype(u.Elem), max(u.Cap, 1))
		d.deps = g.valueDeps(u.Elem)
		return d.name
	case *Tuple:
		key := g.typeKey(u)
		if d, ok := g.defByKey[key]; ok {
			return d.name
		}
		d := &cdef{name: fmt.Sprintf("sq_tu%d", len(g.defs))}
		g.defByKey[key] = d
		g.defs = append(g.defs, d)
		var fields strings.Builder
		for i, e := range u.Types {
			fmt.Fprintf(&fields, " %s r%d;", g.ctype(e), i)
			d.deps = append(d.deps, g.valueDeps(e)...)
		}
		d.body = fmt.Sprintf("struct %s {%s };\n", d.name, fields.String())
		return d.name
	case *Signature:
		key := "fn" + g.typeKey(u)
		if d, ok := g.defByKey[key]; ok {
			return d.name
		}
		ret := g.resultC(u)
		var ps []string
		if u.Recv != nil {
			ps = append(ps, g.ctype(u.Recv.Type))
		}
		for _, p := range u.Params {
			ps = append(ps, g.ctype(p.Type))
		}
		if len(ps) == 0 {
			ps = []string{"void"}
		}
		d := &cdef{name: fmt.Sprintf("sq_fn%d", len(g.defs)), fnptr: true}
		g.defByKey[key] = d
		g.defs = append(g.defs, d)
		d.body = fmt.Sprintf("typedef %s (*%s)(%s);\n", ret, d.name, strings.Join(ps, ", "))
		return d.name
	}
	return "int"
}

func (g *gen) structDef(key, name string, st *Struct) string {
	if d, ok := g.defByKey[key]; ok {
		return d.name
	}
	d := &cdef{name: name}
	g.defByKey[key] = d
	g.defs = append(g.defs, d)
	var fields strings.Builder
	for i, f := range st.Fields {
		fmt.Fprintf(&fields, "\t%s;\n", g.decl(f.Type, fieldName(f.Name, i)))
		d.deps = append(d.deps, g.valueDeps(f.Type)...)
	}
	if len(st.Fields) == 0 {
		fields.WriteString("\tchar sq_empty_[0];\n")
	}
	d.body = fmt.Sprintf("struct %s {\n%s};\n", name, fields.String())
	return name
}

func fieldName(name string, i int) string {
	if name == "_" {
		return fmt.Sprintf("f__blank%d", i)
	}
	return "f_" + sanitize(name)
}

// valueDeps returns struct-like C type names that a value of type t contains by value.
func (g *gen) valueDeps(t Type) []string {
	switch u := t.(type) {
	case *Named:
		if _, ok := u.Underlying().(*Struct); ok {
			return []string{g.ctype(u)}
		}
		return g.valueDeps(u.Underlying())
	case *Struct, *Slice, *Array, *Tuple, *VecType, *MapStore, *ChanStore:
		return []string{g.ctype(u)}
	}
	return nil
}

// mapSlots is the table size for a map of capacity n (load factor at most 3/4).
func mapSlots(n int64) int64 {
	return nextPow2(n + n/3 + 1)
}

func (g *gen) decl(t Type, name string) string {
	return g.ctype(t) + " " + name
}

// localDecl declares a local variable. (Locals are never over-aligned: clang's -fstack-usage
// does not count the slack that realigning a frame needs, which would break the stack proof.)
func (g *gen) localDecl(t Type, name string) string {
	return g.decl(t, name)
}

func (g *gen) resultC(sig *Signature) string {
	switch len(sig.Results) {
	case 0:
		return "void"
	case 1:
		return g.ctype(sig.Results[0].Type)
	}
	tu := &Tuple{}
	for _, r := range sig.Results {
		tu.Types = append(tu.Types, r.Type)
	}
	return g.ctype(tu)
}

func (g *gen) tupleType(sig *Signature) *Tuple {
	tu := &Tuple{}
	for _, r := range sig.Results {
		tu.Types = append(tu.Types, r.Type)
	}
	return tu
}

func (g *gen) proto(f *Func) string {
	var ps []string
	if f.Sig.Recv != nil {
		ps = append(ps, g.decl(f.Sig.Recv.Type, f.Sig.Recv.CNameOr("recv")))
	}
	for i, p := range f.Sig.Params {
		ps = append(ps, g.decl(p.Type, p.CNameOr(fmt.Sprintf("p%d", i))))
	}
	if len(ps) == 0 {
		ps = []string{"void"}
	}
	return fmt.Sprintf("%s %s(%s)", g.resultC(f.Sig), f.CName, strings.Join(ps, ", "))
}

// initSharedChans emits the setup of every channel in the shared variable (or part) expr of type t.
func (g *gen) initSharedChans(expr string, t Type, ind int) {
	tabs := strings.Repeat("\t", ind)
	switch u := t.Underlying().(type) {
	case *ChanStore:
		fmt.Fprintf(&g.sharedInits, "%ssq_chan_init_shared(&%s.h, %s.buf, sizeof(%s), %d);\n", tabs, expr, expr, g.ctype(u.Elem), u.Cap)
	case *Array:
		if containsChan(u.Elem) {
			i := g.tmp()
			fmt.Fprintf(&g.sharedInits, "%sfor (int64_t %s = 0; %s < %d; %s++) {\n", tabs, i, i, u.Len, i)
			g.initSharedChans(fmt.Sprintf("%s.a[%s]", expr, i), u.Elem, ind+1)
			fmt.Fprintf(&g.sharedInits, "%s}\n", tabs)
		}
	case *Struct:
		for i, f := range u.Fields {
			if containsChan(f.Type) {
				g.initSharedChans(expr+"."+fieldName(f.Name, i), f.Type, ind)
			}
		}
	}
}

// blockingWrapper declares a blocking extern and defines the wrapper its callers use: the
// arguments and result live in a struct on the goroutine's stack, and a helper thread runs the
// thunk (the real call) while the goroutine waits in sq_blocking.
func (g *gen) blockingWrapper(f *Func) string {
	w := f.CName
	f.CName = f.Extern
	ext := "extern " + g.proto(f) + ";\n"
	f.CName = w
	res := g.resultC(f.Sig)
	var fields, params, inits, args []string
	for i, p := range f.Sig.Params {
		n := fmt.Sprintf("p%d", i)
		fields = append(fields, "\t"+g.decl(p.Type, n)+";\n")
		params = append(params, g.decl(p.Type, n))
		inits = append(inits, "."+n+" = "+n)
		args = append(args, "a->"+n)
	}
	if res != "void" {
		fields = append(fields, "\t"+res+" r;\n")
	}
	if len(params) == 0 {
		params = []string{"void"}
	}
	var b strings.Builder
	b.WriteString(ext)
	fmt.Fprintf(&b, "typedef struct {\n%s\tchar unused;\n} %s_args;\n", strings.Join(fields, ""), w)
	call := fmt.Sprintf("%s(%s)", f.Extern, strings.Join(args, ", "))
	if res != "void" {
		call = "a->r = " + call
	}
	fmt.Fprintf(&b, "void %s_thunk(void *v) {\n\t%s_args *a = v;\n\t%s;\n}\n", w, w, call)
	fmt.Fprintf(&b, "static %s %s(%s) {\n\t%s_args a = {%s};\n\tsq_blocking(%s_thunk, &a);\n", res, w, strings.Join(params, ", "), w, strings.Join(inits, ", "), w)
	if res != "void" {
		b.WriteString("\treturn a.r;\n")
	}
	b.WriteString("}\n")
	return b.String()
}

func (v *Var) CNameOr(def string) string {
	if v.CName == "" {
		v.CName = def
	}
	return v.CName
}

func (g *gen) typeDecls() string {
	var b strings.Builder
	b.WriteString("// types\n")
	for _, d := range g.defs {
		if !d.fnptr {
			fmt.Fprintf(&b, "typedef struct %s %s;\n", d.name, d.name)
		}
	}
	for _, d := range g.defs {
		if d.fnptr {
			b.WriteString(d.body)
		}
	}
	// topological order by value containment
	done := map[string]bool{}
	byName := map[string]*cdef{}
	for _, d := range g.defs {
		byName[d.name] = d
	}
	var emit func(d *cdef)
	emit = func(d *cdef) {
		if done[d.name] || d.fnptr {
			return
		}
		done[d.name] = true
		for _, dep := range d.deps {
			if dd, ok := byName[dep]; ok {
				emit(dd)
			}
		}
		b.WriteString(d.body)
	}
	for _, d := range g.defs {
		emit(d)
	}
	return b.String()
}

// zero returns a C zero value expression of type t.
func (g *gen) zero(t Type) string {
	switch u := t.Underlying().(type) {
	case *Basic:
		switch u.Kind {
		case String, UntypedString:
			return "((sq_string){0})"
		case Rawptr, UntypedNil:
			return "NULL"
		case Bool:
			return "false"
		}
		return "0"
	case *Pointer, *Chan, *Map, *Signature, *ArenaType:
		return "NULL"
	}
	return "((" + g.ctype(t) + "){})" // {} (GNU C) also works for empty structs and [0]T
}

// ---- equality ----

func (g *gen) eqExpr(t Type, a, b string) string {
	switch u := t.Underlying().(type) {
	case *Basic:
		if u.Kind == String || u.Kind == UntypedString {
			return fmt.Sprintf("sq_str_eq(%s, %s)", a, b)
		}
		return fmt.Sprintf("(%s == %s)", a, b)
	case *ErrorType:
		return fmt.Sprintf("sq_err_eq(%s, %s)", a, b)
	case *Struct, *Array:
		return fmt.Sprintf("%s(%s, %s)", g.eqFunc(t), a, b)
	}
	return fmt.Sprintf("(%s == %s)", a, b)
}

func (g *gen) eqFunc(t Type) string {
	ct := g.ctype(t)
	key := ct
	if name, ok := g.eqFuncs["name:"+key]; ok {
		return name
	}
	name := "sq_eq_" + ct
	g.eqFuncs["name:"+key] = name
	var body strings.Builder
	fmt.Fprintf(&body, "static inline bool %s(%s a, %s b) {\n", name, ct, ct)
	switch u := t.Underlying().(type) {
	case *Struct:
		for i, f := range u.Fields {
			fn := fieldName(f.Name, i)
			fmt.Fprintf(&body, "\tif (!%s) return false;\n", g.eqExpr(f.Type, "a."+fn, "b."+fn))
		}
	case *Array:
		fmt.Fprintf(&body, "\tfor (int64_t i = 0; i < %d; i++) if (!%s) return false;\n", u.Len, g.eqExpr(u.Elem, "a.a[i]", "b.a[i]"))
	}
	body.WriteString("\treturn true;\n}\n")
	g.eqFuncs[key] = body.String()
	g.eqOrder = append(g.eqOrder, key)
	return name
}

// ---- constants ----

func cString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '"' || c == '\\':
			b.WriteByte('\\')
			b.WriteByte(c)
		case c == '\n':
			b.WriteString("\\n")
		case c == '\t':
			b.WriteString("\\t")
		case c == '?':
			b.WriteString("\\?") // avoid trigraphs
		case c >= 0x20 && c < 0x7f:
			b.WriteByte(c)
		default:
			fmt.Fprintf(&b, "\\%03o", c)
		}
	}
	b.WriteByte('"')
	return b.String()
}

func (g *gen) constant(v constant.Value, t Type) string {
	if isUntyped(t) {
		t = defaultType(t)
	}
	if s := simdOf(t); s != nil {
		return g.simdConstant(v, s)
	}
	k := basicKind(t)
	switch {
	case k == Bool:
		if constant.BoolVal(v) {
			return "true"
		}
		return "false"
	case k == String:
		s := constant.StringVal(v)
		return fmt.Sprintf("SQ_STR(%s, %d)", cString(s), len(s))
	case k == Float32 || k == Float64:
		f, _ := constant.Float64Val(constant.ToFloat(v))
		if math.IsInf(f, 0) || math.IsNaN(f) {
			return "0.0"
		}
		s := strconv.FormatFloat(f, 'g', -1, 64)
		if !strings.ContainsAny(s, ".eE") {
			s += ".0"
		}
		if k == Float32 {
			return "((float)" + s + ")"
		}
		return s
	case k >= Int && k <= Uintptr:
		iv := constant.ToInt(v)
		ct := g.basicC(k)
		if isUnsigned(t) {
			u, _ := constant.Uint64Val(iv)
			if k == Uint64 || k == Uint || k == Uintptr {
				return fmt.Sprintf("((%s)UINT64_C(%d))", ct, u)
			}
			return fmt.Sprintf("((%s)%dU)", ct, u)
		}
		i, _ := constant.Int64Val(iv)
		if i == math.MinInt64 {
			return "INT64_MIN"
		}
		if k == Int || k == Int64 {
			if i > math.MaxInt32 || i < math.MinInt32 {
				return fmt.Sprintf("INT64_C(%d)", i)
			}
			return fmt.Sprintf("((int64_t)%d)", i)
		}
		return fmt.Sprintf("((%s)%d)", ct, i)
	}
	return "0"
}

// ---- functions ----

func (g *gen) tmp() string {
	g.tmpN++
	return fmt.Sprintf("_t%d", g.tmpN)
}

func (g *gen) line(format string, args ...any) {
	g.out.WriteString(strings.Repeat("\t", g.ind))
	fmt.Fprintf(g.out, format, args...)
	g.out.WriteByte('\n')
}

func (g *gen) lineDirective(p Pos) {
	if p.File == "" {
		return
	}
	fmt.Fprintf(g.out, "#line %d %s\n", p.Line, cString(p.File))
}

func (g *gen) function(f *Func) string {
	g.fn = f
	g.tmpN = 0
	g.usesGlobals = false
	g.addressed = nil
	g.fnPreempt, g.usesPreempt = g.preemptFns[f], false
	g.topDecls = nil
	var body strings.Builder
	g.out = &body
	g.ind = 1
	for _, r := range f.Sig.Results {
		if r.Name != "" {
			g.line("%s = %s;", g.decl(r.Type, r.CNameOr(g.tmp())), g.zero(r.Type))
		}
	}
	for _, d := range f.Defers {
		g.topDecls = append(g.topDecls, fmt.Sprintf("bool _d%d_on = false;", d.ID))
	}
	g.stmts(f.Decl.Body.List)
	if len(f.Sig.Results) == 0 {
		g.runDefers()
	}
	var b strings.Builder
	b.WriteString("\n")
	g.lineDirectiveTo(&b, f.Decl.P)
	attr := ""
	for _, p := range f.Pragmas {
		if p == "noinline" {
			attr = "__attribute__((noinline)) "
		}
	}
	b.WriteString("static " + attr + g.proto(f) + " {\n")
	if g.usesGlobals {
		b.WriteString("\tstruct sq_globals *const sq_gl = sq_globals_self;\n")
	}
	if g.usesPreempt {
		b.WriteString("\t_Atomic int *const sq_pf = sq_preempt_flag;\n")
	}
	for _, d := range g.topDecls {
		b.WriteString("\t" + d + "\n")
	}
	b.WriteString(body.String())
	b.WriteString("}\n")
	g.fn = nil
	return b.String()
}

func (g *gen) lineDirectiveTo(b *strings.Builder, p Pos) {
	if p.File != "" {
		fmt.Fprintf(b, "#line %d %s\n", p.Line, cString(p.File))
	}
}

func (g *gen) runDefers() {
	if g.fn == nil {
		return
	}
	for i := len(g.fn.Defers) - 1; i >= 0; i-- {
		d := g.fn.Defers[i]
		g.line("if (_d%d_on) { %s; }", d.ID, g.deferCall(d))
	}
}

// initFunction generates package-level variable initialization, in package order.
func (g *gen) initFunction() string {
	var body strings.Builder
	g.out = &body
	g.ind = 1
	g.fn = nil
	g.tmpN = 0
	for _, p := range g.prog.Packages {
		for _, vs := range p.GlobalInits {
			if len(vs.Values) != len(vs.Names) {
				continue
			}
			for i, x := range vs.Values {
				v := vs.Names[i].Obj.Var
				if !strings.HasPrefix(v.CName, "sq_gl->") && v.ArenaSize == 0 && !v.Shared {
					continue // a constant, initialized statically
				}
				if v.Shared {
					g.line("if (first) { // shared var: set once, on shard 0, before the others start")
					g.ind++
				}
				g.lineDirective(x.Pos())
				g.hoisted = nil
				val := g.coerce(x, v.Type)
				for _, h := range g.hoisted {
					// package-level storage lives in the shard's globals too
					g.statics.WriteString("\t" + h + "\n")
					f := strings.Fields(strings.TrimSuffix(h, ";"))
					g.staticNames = append(g.staticNames, f[len(f)-1])
				}
				g.hoisted = nil
				g.line("%s = %s;", v.CName, val)
				if v.Shared {
					g.ind--
					g.line("}")
				}
			}
		}
	}
	var b strings.Builder
	for _, n := range g.staticNames {
		fmt.Fprintf(&b, "#define %s (sq_gl->%s)\n", n, n)
	}
	b.WriteString("\nstatic void sq_init_globals(bool first) {\n\tstruct sq_globals *const sq_gl = sq_globals_self;\n\t(void)first;\n")
	b.WriteString(g.arenaInits.String())
	if g.sharedInits.Len() > 0 {
		b.WriteString("\tif (first) {\n")
		b.WriteString(g.sharedInits.String())
		b.WriteString("\t}\n")
	}
	b.WriteString(body.String())
	b.WriteString("}\n")
	for _, n := range g.staticNames {
		fmt.Fprintf(&b, "#undef %s\n", n)
	}
	return b.String()
}

func (g *gen) poolFor(f *Func) *PoolInfo {
	if p, ok := g.pools[f]; ok {
		return p
	}
	count := f.Pool
	if count == 0 {
		count = 256
	}
	p := &PoolInfo{CName: "sq_pool_" + f.CName, Name: f.FullName(), Count: count, Entry: f}
	g.pools[f] = p
	g.poolOrder = append(g.poolOrder, p)
	return p
}

// StackPlan computes the proven stack size of every pool from per-C-function frame sizes.
// HelperStack is the stack a helper thread needs to run any of the program's blocking externs
// (0: the program has none).
func HelperStack(prog *Program, frames map[string]int64, externDefault int64) int64 {
	var need int64
	for _, f := range prog.Funcs {
		if !f.Blocking || f.CName == "" {
			continue
		}
		s := f.StackHint
		if s == 0 {
			s = externDefault
		}
		if s += frames[f.CName+"_thunk"]; s > need {
			need = s
		}
	}
	return need
}

func StackPlan(prog *Program, pools []*PoolInfo, frames map[string]int64, runtimeBudget, externDefault int64) (map[*PoolInfo]int64, []string) {
	memo := map[*Func]int64{}
	var notes []string
	var stack func(f *Func) int64
	stack = func(f *Func) int64 {
		if v, ok := memo[f]; ok {
			return v
		}
		if f.Blocking {
			// on the goroutine's stack: the wrapper and sq_blocking; the C function itself runs
			// on a helper thread (HelperStack)
			memo[f] = frames[f.CName] + runtimeBudget
			return memo[f]
		}
		if f.Extern != "" {
			if strings.HasPrefix(f.Extern, "sq_") && f.StackHint == 0 {
				memo[f] = runtimeBudget
				return runtimeBudget
			}
			if f.StackHint > 0 {
				memo[f] = f.StackHint
				return f.StackHint
			}
			memo[f] = externDefault
			return externDefault
		}
		max := runtimeBudget
		for _, c := range prog.Callees(f) {
			if s := stack(c); s > max {
				max = s
			}
		}
		v := frames[f.CName] + max
		memo[f] = v
		return v
	}
	out := map[*PoolInfo]int64{}
	for _, p := range pools {
		var need int64
		if p.Entry == nil {
			need = frames["sq_program_main"] + max(stack(prog.MainFunc), frames["sq_init_globals"]+stack(prog.InitFunc))
		} else {
			for _, t := range p.Trampolines {
				if s := frames[t] + stack(p.Entry); s > need {
					need = s
				}
			}
		}
		out[p] = need
	}
	sort.Strings(notes)
	return out, notes
}
