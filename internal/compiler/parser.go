package compiler

import (
	"fmt"
	"strings"
)

type parser struct {
	lx      *lexer
	tok     lexToken
	ahead   *lexToken // one token of lookahead (see peek)
	prevEnd Pos       // end of the last token consumed
	errs    ErrorList
	exprLev int   // < 0: in control clause (composite literals not allowed)
	methods []any // methods declared in the impl block being parsed
}

type bailout struct{}

// ParseFile parses one squark source file.
func ParseFile(path string, src []byte) (f *File, err error) {
	p := &parser{lx: newLexer(path, src)}
	defer func() {
		if r := recover(); r != nil {
			if _, ok := r.(bailout); !ok {
				panic(r)
			}
		}
		errs := append(p.lx.errs, p.errs...)
		if len(errs) > 0 {
			err = errs
			f = nil
		}
	}()
	p.next()
	f = p.file(path)
	return f, nil
}

func (p *parser) next() {
	if !(p.tok.tok == SEMICOLON && p.tok.lit == "\n") && p.tok.end.Line > 0 {
		p.prevEnd = p.tok.end
	}
	if p.ahead != nil {
		p.tok, p.ahead = *p.ahead, nil
		return
	}
	p.tok = p.lx.Next()
}

// peek returns the token after p.tok.
func (p *parser) peek() lexToken {
	if p.ahead == nil {
		t := p.lx.Next()
		p.ahead = &t
	}
	return *p.ahead
}

func (p *parser) errorf(pos Pos, format string, args ...any) {
	p.errs = append(p.errs, &Error{pos, fmt.Sprintf(format, args...)})
	if len(p.errs) > 10 {
		panic(bailout{})
	}
}

func (p *parser) found() string {
	got := p.tok.tok.String()
	if p.tok.tok == SEMICOLON && p.tok.lit == "\n" {
		got = "newline"
	} else if p.tok.lit != "" && p.tok.tok != NOT && p.tok.tok != LAND && p.tok.tok != LOR {
		got = fmt.Sprintf("%s %q", got, p.tok.lit)
	}
	return got
}

func (p *parser) expect(t Tok) Pos {
	pos := p.tok.pos
	if p.tok.tok != t {
		p.errorf(pos, "expected %s, found %s", t, p.found())
		panic(bailout{})
	}
	p.next()
	return pos
}

func (p *parser) got(t Tok) bool {
	if p.tok.tok == t {
		p.next()
		return true
	}
	return false
}

// isWord reports whether the current token is the identifier w (a contextual keyword).
func (p *parser) isWord(w string) bool { return p.tok.tok == IDENT && p.tok.lit == w }

func (p *parser) semi() {
	if p.tok.tok == RPAREN || p.tok.tok == RBRACE {
		return
	}
	p.expect(SEMICOLON)
}

func (p *parser) ident() *Ident {
	pos := p.tok.pos
	name := p.tok.lit
	p.expect(IDENT)
	return &Ident{P: pos, Name: name}
}

// isKeyword reports whether t is a keyword (or the not, and, or operators).
func isKeyword(t Tok) bool {
	return t >= BREAK && t <= lastKeyword || t == NOT || t == LAND || t == LOR
}

// memberIdent parses a field or method name, which may be spelled like a keyword (r.arena):
// after a dot, in a struct, or as a method's name, it can't mean anything else.
func (p *parser) memberIdent() *Ident {
	if isKeyword(p.tok.tok) {
		id := &Ident{P: p.tok.pos, Name: p.tok.tok.String()}
		p.next()
		return id
	}
	return p.ident()
}

var directiveWords = map[string]bool{"budget": true, "link": true, "cflags": true, "ldflags": true, "pool": true, "workers": true, "threads": true, "shutdown": true}

func (p *parser) file(path string) *File {
	f := &File{Path: path, Pragmas: p.tok.pragmas, Doc: p.tok.doc}
	switch {
	case p.isWord("program"):
		p.next()
		f.Package = "main"
	case p.isWord("module"):
		p.next()
		f.Package = p.ident().Name
	default:
		p.errorf(p.tok.pos, "a file starts with program (the main package) or module NAME, found %s", p.found())
		panic(bailout{})
	}
	if p.got(LBRACE) {
		for p.tok.tok != RBRACE && p.tok.tok != EOF {
			if p.got(SEMICOLON) {
				continue
			}
			if !(p.tok.tok == IDENT && directiveWords[p.tok.lit]) {
				p.errorf(p.tok.pos, "expected a directive (budget, pool, workers, threads, link, cflags or ldflags), found %s", p.found())
				panic(bailout{})
			}
			f.Directives = append(f.Directives, p.directive())
			p.semi()
		}
		p.expect(RBRACE)
	}
	p.semi()
	for p.isWord("use") {
		p.next()
		if p.got(LPAREN) {
			for p.tok.tok != RPAREN {
				if p.got(SEMICOLON) {
					continue
				}
				f.Imports = append(f.Imports, p.useSpec())
				p.semi()
			}
			p.expect(RPAREN)
		} else {
			f.Imports = append(f.Imports, p.useSpec())
			for p.got(COMMA) {
				f.Imports = append(f.Imports, p.useSpec())
			}
		}
		p.semi()
	}
	for p.tok.tok != EOF {
		if p.got(SEMICOLON) {
			continue
		}
		if p.tok.tok == IDENT && directiveWords[p.tok.lit] && (p.peek().tok == INT || p.peek().tok == IDENT || p.peek().tok == STRING) {
			hdr := "program"
			if f.Package != "main" {
				hdr = "module " + f.Package
			}
			p.errorf(p.tok.pos, "%s is a directive: it goes in the file's header, %s { %s ... }", p.tok.lit, hdr, p.tok.lit)
			panic(bailout{})
		}
		if p.isWord("use") {
			p.errorf(p.tok.pos, "use goes before the declarations")
			panic(bailout{})
		}
		if d := p.topDecl(); d != nil {
			f.Decls = append(f.Decls, d)
		}
		f.Decls = append(f.Decls, p.methods...)
		p.methods = nil
	}
	return f
}

func (p *parser) directive() *Directive {
	d := &Directive{Name: p.tok.lit, P: p.tok.pos}
	p.next()
	if d.Name == "pool" {
		// pool pkg.task N: resize a task's pool for this program
		d.Target = p.ident().Name
		if p.got(PERIOD) {
			d.Target += "." + p.ident().Name
		}
	}
	if d.Name == "shutdown" {
		// shutdown 20s: a number with its unit right after it
		d.Value = &BasicLit{Kind: p.tok.tok, Lit: p.tok.lit, P: p.tok.pos}
		if p.tok.tok != INT {
			p.errorf(p.tok.pos, "shutdown takes a duration: shutdown 20s (or 500ms, 2m)")
			return d
		}
		p.next()
		if p.tok.tok == IDENT && p.tok.pos == p.prevEnd {
			d.Unit = p.tok.lit
			p.next()
		}
		return d
	}
	d.Value = p.sizeExpr()
	return d
}

// useSpec parses an import: a path written plainly (http, math/rand, ./lib) or as a string,
// optionally renamed with `as name`.
func (p *parser) useSpec() *ImportSpec {
	s := &ImportSpec{P: p.tok.pos}
	if p.tok.tok == STRING {
		s.Path = p.tok.lit
		p.next()
	} else {
		var b strings.Builder
		for first := true; ; first = false {
			if !first && p.tok.pos != p.prevEnd {
				break // the path ends at a space
			}
			switch p.tok.tok {
			case IDENT:
				b.WriteString(p.tok.lit)
			case PERIOD:
				b.WriteString(".")
			case DOTDOT:
				b.WriteString("..")
			case QUO:
				b.WriteString("/")
			case INT:
				b.WriteString(p.tok.lit)
			default:
				if isKeyword(p.tok.tok) {
					b.WriteString(p.tok.tok.String())
					break
				}
				if first {
					p.errorf(p.tok.pos, "expected a package path, found %s", p.found())
					panic(bailout{})
				}
				goto done
			}
			p.next()
		}
	done:
		s.Path = b.String()
	}
	if p.isWord("as") {
		p.next()
		s.Name = p.ident()
	}
	return s
}

// sizeExpr parses a constant expression in a clause (pool N, stack N, arena x N) where a
// following '{' must not be taken as a composite literal.
func (p *parser) sizeExpr() Expr {
	old := p.exprLev
	p.exprLev = -1
	x := p.binaryRest(p.unary(), 1)
	p.exprLev = old
	return x
}

// clauses parses trailing `pool N` / `stack N` / `blocking` / `preemptible` clauses after a
// signature, and an extern's C symbol (c"name").
func (p *parser) clauses(d *FuncDecl) {
	for p.tok.tok == IDENT {
		name := p.tok.lit
		pos := p.tok.pos
		if name == "c" && p.peek().tok == STRING && p.peek().pos.Line == pos.Line && p.peek().pos.Col == pos.Col+1 {
			p.next()
			if d.Kind != KindExtern {
				p.errorf(pos, "only extern functions name a C symbol")
			}
			d.ExternSym = p.tok.lit
			p.next()
			continue
		}
		if name != "pool" && name != "stack" && name != "blocking" && name != "preemptible" {
			return
		}
		p.next()
		switch name {
		case "blocking":
			d.Blocking = true
			d.BlockingPos = pos
			continue
		case "preemptible":
			d.Preemptible = true
			d.PreemptiblePos = pos
			continue
		}
		x := p.sizeExpr()
		switch name {
		case "pool":
			if d.Kind != KindTask {
				p.errorf(pos, "only tasks have a pool size")
			}
			d.Pool = x
		case "stack":
			d.Stack = x
		}
	}
}

func (p *parser) topDecl() any {
	pragmas := p.tok.pragmas
	doc := p.tok.doc
	pub := false
	pubPos := p.tok.pos
	if p.got(PUB) {
		pub = true
	}
	switch p.tok.tok {
	case FN, TASK:
		d := p.funcDecl(false)
		d.Doc = doc
		d.Pragmas = pragmas
		d.Pub = pub
		p.semi()
		return d
	case EXTERN:
		pos := p.tok.pos
		p.next()
		if p.tok.tok != FN {
			p.errorf(p.tok.pos, "expected fn after extern")
			panic(bailout{})
		}
		d := p.funcDecl(true)
		d.Doc = doc
		d.P = pos
		d.Pragmas = pragmas
		d.Pub = pub
		if d.Body != nil {
			p.errorf(pos, "extern function %s must not have a body", d.Name.Name)
		}
		p.semi()
		return d
	case ARENA:
		pos := p.tok.pos
		p.next()
		d := &ArenaDecl{P: pos, Name: p.ident(), Pub: pub}
		d.Size = p.sizeExpr()
		p.semi()
		return d
	case CONST, FAULT:
		d := p.genDecl(true, pub)
		d.Pragmas = pragmas
		p.semi()
		return d
	case ENUM:
		d := p.enumDecl(pub, doc)
		d.Pragmas = pragmas
		p.semi()
		return d
	case IMPL:
		// impl Name { methods }
		if pub {
			p.errorf(pubPos, "pub goes on each method of an impl block")
		}
		p.next()
		typ := p.ident()
		p.expect(LBRACE)
		for p.tok.tok != RBRACE {
			if p.got(SEMICOLON) {
				continue
			}
			p.method(typ)
		}
		p.expect(RBRACE)
		p.semi()
		return nil
	case STRUCT:
		// struct Name { fields }
		d := &GenDecl{Tok: TYPE, P: p.tok.pos}
		ts := &TypeSpec{P: p.peek().pos, Doc: doc, Pub: pub, Form: "struct"}
		st := &StructType{P: p.expect(STRUCT)}
		ts.Name = p.ident()
		p.structFields(st, ts.Name)
		ts.Type = st
		d.Types = []*TypeSpec{ts}
		d.Pragmas = pragmas
		p.semi()
		return d
	case VAR:
		p.errorf(p.tok.pos, "a package variable is thread (one copy per scheduler thread) or shared (one copy, read-only after start-up): write thread NAME T")
		panic(bailout{})
	case IDENT:
		if p.peek().tok == COLON {
			// Name: T, a named type with T's representation
			d := &GenDecl{Tok: TYPE, P: p.tok.pos}
			ts := &TypeSpec{P: p.tok.pos, Doc: doc, Pub: pub, Name: p.ident(), Form: "named"}
			p.next()
			if p.tok.tok == STRUCT && p.peek().tok == LBRACE {
				p.errorf(p.tok.pos, "a struct type is declared struct %s { ... }", ts.Name.Name)
			}
			ts.Type = p.typ()
			if p.tok.tok == LBRACE {
				p.errorf(p.tok.pos, "methods go in an impl block: impl %s { fn ... }", ts.Name.Name)
				panic(bailout{})
			}
			d.Types = []*TypeSpec{ts}
			d.Pragmas = pragmas
			p.semi()
			return d
		}
		if p.tok.lit == "type" && p.peek().tok == IDENT {
			p.next()
			name := p.tok.lit
			if p.peek().tok == STRUCT {
				p.errorf(p.tok.pos, "a struct type is declared struct %s { ... }", name)
			} else {
				p.errorf(p.tok.pos, "a named type is declared %s: T (struct %s { ... } for a struct)", name, name)
			}
			panic(bailout{})
		}
		if p.tok.lit == "thread" || p.tok.lit == "shared" {
			d := p.genDecl(true, pub)
			d.Pragmas = pragmas
			p.semi()
			return d
		}
	}
	if pub {
		p.errorf(pubPos, "pub goes before a declaration (fn, task, extern, struct, enum, Name: T, const, fault, thread, shared or arena) or a method in an impl block")
		panic(bailout{})
	}
	p.errorf(p.tok.pos, "expected declaration, found %s", p.found())
	panic(bailout{})
}

// funcDecl parses fn name(...) or task name(...). (Methods are parsed in their type's block.)
func (p *parser) funcDecl(extern bool) *FuncDecl {
	d := &FuncDecl{P: p.tok.pos}
	if p.tok.tok == TASK {
		d.Kind = KindTask
		p.next()
	} else {
		p.expect(FN)
	}
	if extern {
		d.Kind = KindExtern
	}
	name := p.ident()
	if p.tok.tok == PERIOD {
		p.next()
		m := p.memberIdent()
		p.errorf(name.P, "methods go in an impl block: impl %s { fn %s(...) { ... } }", name.Name, m.Name)
		panic(bailout{})
	}
	d.Name = name
	d.Type = p.signature(d.P)
	p.clauses(d)
	if p.tok.tok == LBRACE {
		d.Body = p.block()
	}
	return d
}

func errorField(pos Pos) *Field { return &Field{Type: &Ident{P: pos, Name: "error"}} }

func (p *parser) signature(pos Pos) *FuncType {
	return p.signatureWith(pos, p.params())
}

// signatureWith parses what follows a signature's parameters: results and their provenance.
func (p *parser) signatureWith(pos Pos, params []*Field) *FuncType {
	ft := &FuncType{P: pos}
	ft.Params = params
	if p.got(ARROWR) {
		switch p.tok.tok {
		case BANG: // -> !: just an error
			ft.Results = []*Field{errorField(p.tok.pos)}
			p.next()
		case LPAREN:
			ft.Results = p.params()
			if p.tok.tok == BANG {
				if len(ft.Results) > 0 && len(ft.Results[0].Names) > 0 {
					p.errorf(p.tok.pos, "named results declare the error by name: (..., err error)")
				}
				ft.Results = append(ft.Results, errorField(p.tok.pos))
				p.next()
			}
		default:
			ft.Results = []*Field{{Type: p.typ()}}
			if p.tok.tok == BANG {
				ft.Results = append(ft.Results, errorField(p.tok.pos))
				p.next()
			}
		}
		// result provenance: `in a, b` or `in static`
		if p.tok.tok == IN {
			p.next()
			if p.isWord("static") {
				p.next()
				ft.ResultStatic = true
			} else {
				ft.From = append(ft.From, p.ident())
				for p.got(COMMA) {
					ft.From = append(ft.From, p.ident())
				}
			}
		}
	} else if p.tok.tok == IN {
		p.errorf(p.tok.pos, "in after the parameters says where results point: the function has none")
		panic(bailout{})
	} else if p.startsResultType() {
		p.errorf(p.tok.pos, "results follow ->: write -> %s", p.found())
		panic(bailout{})
	}
	return ft
}

// startsResultType: a type right after the parameters, as Go would write a result.
func (p *parser) startsResultType() bool {
	switch p.tok.tok {
	case MUL, FN, STRUCT, LPAREN:
		return true
	case IDENT:
		switch p.tok.lit {
		case "pool", "stack", "blocking", "preemptible":
			return false
		case "c":
			return p.peek().tok != STRING
		}
		return true
	}
	return false
}

// params parses a parameter list. Either all parameters are named or none.
func (p *parser) params() []*Field {
	p.expect(LPAREN)
	return p.paramList()
}

// paramList parses parameters after the ( through the ).
func (p *parser) paramList() []*Field {
	type entry struct {
		name     *Ident
		typ      Expr
		variadic bool
		static   bool
		into     *Ident
	}
	var list []entry
	for p.tok.tok != RPAREN {
		var e entry
		if p.tok.tok == ELLIPSIS {
			p.next()
			e.variadic = true
			e.typ = p.typ()
		} else if p.tok.tok == IDENT {
			id := p.ident()
			switch p.tok.tok {
			case COMMA, RPAREN:
				e.typ = id // could be a name; resolved below
			case PERIOD, LBRACK, CHAN:
				e.typ = p.typeSuffix(p.qualified(id), id.P)
			case ELLIPSIS:
				p.next()
				e.name = id
				e.variadic = true
				e.typ = p.typ()
			default:
				e.name = id
				if p.isWord("static") {
					// a common slip: in static goes after the type
					spos := p.tok.pos
					p.next()
					e.typ = p.typ()
					p.errorf(spos, "static goes after the type: write %s %s in static", id.Name, exprString(e.typ))
					e.static = true
				} else {
					e.typ = p.typ()
				}
			}
			if e.name != nil && p.tok.tok == IN {
				p.next()
				if p.isWord("static") {
					p.next()
					e.static = true
				} else {
					e.into = p.ident()
				}
			}
		} else {
			e.typ = p.typ()
		}
		list = append(list, e)
		if !p.got(COMMA) {
			break
		}
	}
	p.expect(RPAREN)
	named := false
	for _, e := range list {
		if e.name != nil {
			named = true
		}
	}
	var fields []*Field
	if !named {
		for _, e := range list {
			fields = append(fields, &Field{Type: e.typ, Variadic: e.variadic})
		}
		return fields
	}
	// grouped names: a, b int
	var pending []*Ident
	for _, e := range list {
		if e.name == nil {
			id, ok := e.typ.(*Ident)
			if !ok {
				p.errorf(e.typ.Pos(), "mixed named and unnamed parameters")
				continue
			}
			pending = append(pending, id)
			continue
		}
		pending = append(pending, e.name)
		fields = append(fields, &Field{Names: pending, Type: e.typ, Variadic: e.variadic, Static: e.static, Into: e.into})
		pending = nil
	}
	if len(pending) > 0 {
		p.errorf(pending[0].P, "mixed named and unnamed parameters")
	}
	return fields
}

// genDecl parses const, type, fault and (at the top level) thread and shared declarations, a
// single one or a parenthesized group. A fault is a constant error: fault Name [code] "message".
func (p *parser) genDecl(top, pub bool) *GenDecl {
	d := &GenDecl{Tok: p.tok.tok, P: p.tok.pos}
	fault := d.Tok == FAULT
	if fault {
		d.Tok = CONST
	}
	if p.tok.tok == IDENT { // thread / shared
		d.Tok = VAR
		d.Shared = p.tok.lit == "shared"
		d.Thread = !d.Shared
	}
	doc := p.tok.doc
	p.next()
	one := func(pub bool) *ValueSpec {
		if d.Tok == TYPE {
			ts := &TypeSpec{P: p.tok.pos, Doc: p.tok.doc, Pub: pub}
			ts.Name = p.ident()
			if p.tok.tok == ASSIGN {
				p.errorf(p.tok.pos, "type aliases are not supported")
			}
			if p.tok.tok == STRUCT {
				p.errorf(p.tok.pos, "a struct type is declared struct %s { ... }", ts.Name.Name)
			}
			ts.Type = p.typ()
			d.Types = append(d.Types, ts)
			return nil
		}
		vs := &ValueSpec{P: p.tok.pos, Doc: p.tok.doc, Pub: pub}
		if fault {
			vs.Names = []*Ident{p.ident()}
			pos := p.tok.pos
			args := []Expr{p.expr()}
			if p.tok.tok != SEMICOLON && p.tok.tok != RPAREN && p.tok.tok != EOF {
				args = append(args, p.expr())
			}
			vs.Values = []Expr{&CallExpr{Fun: &Ident{P: pos, Name: "error"}, Args: args, P: pos}}
			d.Values = append(d.Values, vs)
			return vs
		}
		vs.Names = append(vs.Names, p.ident())
		for p.got(COMMA) {
			vs.Names = append(vs.Names, p.ident())
		}
		if p.tok.tok != ASSIGN && p.tok.tok != SEMICOLON && p.tok.tok != RPAREN {
			vs.Type = p.typ()
		}
		if p.got(ASSIGN) {
			vs.Values = p.exprList()
		} else if d.Tok == CONST {
			p.errorf(vs.P, "constant %s needs a value (for a set of numbered names, declare an enum)", vs.Names[0].Name)
		}
		d.Values = append(d.Values, vs)
		return vs
	}
	if p.got(LPAREN) {
		for p.tok.tok != RPAREN {
			if p.got(SEMICOLON) {
				continue
			}
			spub := pub
			if p.tok.tok == PUB {
				if !top {
					p.errorf(p.tok.pos, "pub is only for package-level declarations")
				}
				spub = true
				p.next()
			}
			one(spub)
			p.semi()
		}
		p.expect(RPAREN)
	} else {
		one(pub)
		for _, vs := range d.Values { // the declaration's comment is the spec's
			vs.Doc = doc
		}
		for _, ts := range d.Types {
			ts.Doc = doc
		}
	}
	return d
}

// atMethod reports whether a method declaration (fn or pub fn) starts here.
// (In an enum, a member may be called fn: then a comma, a newline, = or } follows it.)
// Methods are declared only in impl blocks; atMethod finds them elsewhere to say so.
func (p *parser) atMethod() bool {
	if p.tok.tok == PUB {
		return p.peek().tok == FN
	}
	if p.tok.tok != FN {
		return false
	}
	switch p.peek().tok {
	case COMMA, SEMICOLON, RBRACE, ASSIGN:
		return false
	}
	return true
}

// method parses a method in impl typ { ... }: fn name(self, ...) or fn name(&self, ...).
func (p *parser) method(typ *Ident) {
	pragmas, doc := p.tok.pragmas, p.tok.doc
	pub := p.got(PUB)
	if p.tok.tok != FN {
		p.errorf(p.tok.pos, "an impl block holds methods: fn name(&self, ...) { ... }, found %s", p.found())
		panic(bailout{})
	}
	d := &FuncDecl{P: p.tok.pos, Doc: doc, Pragmas: pragmas, Pub: pub}
	p.next()
	d.Name = p.memberIdent()
	if p.tok.tok == PERIOD {
		p.errorf(p.tok.pos, "in impl %s a method is fn %s(...)", typ.Name, d.Name.Name)
		panic(bailout{})
	}
	// the receiver: self (a copy) or &self (a pointer), first
	p.expect(LPAREN)
	var recv *Field
	switch {
	case p.tok.tok == AND && p.peek().tok == IDENT && p.peek().lit == "self":
		apos := p.tok.pos
		p.next()
		recv = &Field{Names: []*Ident{p.ident()}, Type: &StarExpr{P: apos, X: &Ident{P: typ.P, Name: typ.Name}}}
	case p.isWord("self") && (p.peek().tok == COMMA || p.peek().tok == RPAREN):
		id := p.ident()
		recv = &Field{Names: []*Ident{id}, Type: &Ident{P: typ.P, Name: typ.Name}}
	}
	if recv != nil {
		var rest []*Field
		if p.got(COMMA) {
			rest = p.paramList()
		} else {
			p.expect(RPAREN)
		}
		d.Type = p.signatureWith(d.P, rest)
		d.Recv = recv
	} else {
		p.errorf(p.tok.pos, "a method takes self (a copy of the %s) or &self (a pointer to it) first: fn %s(&self, ...)", typ.Name, d.Name.Name)
		panic(bailout{})
	}
	p.clauses(d)
	if p.tok.tok != LBRACE {
		p.errorf(p.tok.pos, "method %s.%s needs a body", typ.Name, d.Name.Name)
		panic(bailout{})
	}
	d.Body = p.block()
	p.semi()
	p.methods = append(p.methods, d)
}

// enumDecl parses enum Name [T] { a, b = 5, c }: a named integer type and its members, which
// are constants of that type (Name.a). Members are separated by commas or newlines.
func (p *parser) enumDecl(pub bool, doc []string) *GenDecl {
	d := &GenDecl{Tok: TYPE, P: p.expect(ENUM)}
	ts := &TypeSpec{P: p.tok.pos, Doc: doc, Pub: pub, Name: p.ident(), Form: "enum"}
	if p.tok.tok != LBRACE {
		ts.Type = p.typ()
	} else {
		ts.Type = &Ident{P: ts.Name.P, Name: "int"}
	}
	p.expect(LBRACE)
	for p.tok.tok != RBRACE {
		if p.got(SEMICOLON) || p.got(COMMA) {
			continue
		}
		if p.atMethod() {
			p.errorf(p.tok.pos, "methods go in an impl block after the enum: impl %s { fn ... }", ts.Name.Name)
			panic(bailout{})
		}
		m := &EnumMember{Name: p.memberIdent()}
		if p.got(ASSIGN) {
			m.Value = p.expr()
		}
		ts.Enum = append(ts.Enum, m)
		if p.tok.tok != RBRACE && p.tok.tok != SEMICOLON && p.tok.tok != COMMA {
			p.errorf(p.tok.pos, "expected , or a new line after enum member %s, found %s", m.Name.Name, p.found())
			panic(bailout{})
		}
	}
	p.expect(RBRACE)
	d.Types = []*TypeSpec{ts}
	return d
}

// ---- types ----

// typ parses a type. Pointers bind tighter than the postfix forms: *T[] is a slice of *T, and a
// pointer to a slice is *(T[]).
func (p *parser) typ() Expr {
	start := p.tok.pos
	return p.typeSuffix(p.primaryType(), start)
}

func (p *parser) qualified(id *Ident) Expr {
	if p.tok.tok == PERIOD {
		p.next()
		return &SelectorExpr{X: id, Sel: p.ident()}
	}
	return id
}

func (p *parser) primaryType() Expr {
	pos := p.tok.pos
	switch p.tok.tok {
	case IDENT:
		return p.qualified(p.ident())
	case MUL:
		p.next()
		return &StarExpr{P: pos, X: p.primaryType()}
	case FN:
		p.next()
		return p.signature(pos)
	case STRUCT:
		return p.structType()
	case ARENA:
		p.next()
		return &ArenaTypeExpr{P: pos}
	case LPAREN:
		p.next()
		t := p.typ()
		p.expect(RPAREN)
		return t
	case LBRACK:
		p.errorf(pos, "the brackets go after the element type: T[], T[N], T[..N], V[K]")
		panic(bailout{})
	}
	p.errorf(pos, "expected type, found %s", p.found())
	panic(bailout{})
}

// typeSuffix parses the postfix forms after an element type t: T[], T[N], T[..N], T[...],
// V[K], V[K; N], T chan, T chan[N].
func (p *parser) typeSuffix(t Expr, start Pos) Expr {
	for {
		switch p.tok.tok {
		case LBRACK:
			t = p.bracketType(t, start)
		case CHAN:
			p.next()
			ct := &ChanType{P: start, Elem: t}
			if p.tok.tok == LBRACK {
				p.next()
				p.exprLev++
				ct.Cap = p.expr()
				p.exprLev--
				p.expect(RBRACK)
			}
			t = ct
		default:
			return t
		}
	}
}

// bracketType parses [...] after an element type.
func (p *parser) bracketType(t Expr, start Pos) Expr {
	p.expect(LBRACK)
	switch p.tok.tok {
	case RBRACK:
		p.next()
		return &ArrayType{P: start, Elem: t}
	case DOTDOT:
		p.next()
		p.exprLev++
		n := p.expr()
		p.exprLev--
		p.expect(RBRACK)
		return &VecTypeExpr{P: start, Len: n, Elem: t}
	case ELLIPSIS:
		epos := p.tok.pos
		p.next()
		p.expect(RBRACK)
		return &ArrayType{P: start, Len: &BasicLit{P: epos, Kind: ELLIPSIS, Lit: "..."}, Elem: t}
	}
	p.exprLev++
	var k Expr
	typeOnly := false
	switch p.tok.tok {
	case MUL, FN, STRUCT, ARENA:
		k = p.typ()
		typeOnly = true
	default:
		k = p.expr()
	}
	if p.got(SEMICOLON) {
		n := p.expr()
		p.exprLev--
		p.expect(RBRACK)
		return &MapType{P: start, Key: k, Value: t, Cap: n}
	}
	p.exprLev--
	p.expect(RBRACK)
	if typeOnly {
		return &MapType{P: start, Key: k, Value: t}
	}
	return &ArrayType{P: start, Len: k, Elem: t, MaybeMap: isTypeName(k)}
}

func (p *parser) structType() *StructType {
	st := &StructType{P: p.expect(STRUCT)}
	p.structFields(st, nil)
	return st
}

// structFields parses a struct's { fields }.
// structFields parses a struct's { fields }; in a named struct's block (typ set) also its methods.
func (p *parser) structFields(st *StructType, typ *Ident) {
	p.expect(LBRACE)
	for p.tok.tok != RBRACE {
		if p.got(SEMICOLON) {
			continue
		}
		if p.atMethod() {
			name := "T"
			if typ != nil {
				name = typ.Name
			}
			p.errorf(p.tok.pos, "methods go in an impl block after the struct: impl %s { fn ... }", name)
			panic(bailout{})
		}
		f := &Field{Doc: p.tok.doc}
		if p.got(PUB) {
			f.Pub = true
		}
		f.Names = append(f.Names, p.memberIdent())
		for p.got(COMMA) {
			f.Names = append(f.Names, p.memberIdent())
		}
		f.Type = p.typ()
		st.Fields = append(st.Fields, f)
		p.semi()
	}
	p.expect(RBRACE)
}

// ---- statements ----

func (p *parser) block() *BlockStmt {
	b := &BlockStmt{P: p.expect(LBRACE)}
	b.List = p.stmtList()
	b.End = p.expect(RBRACE)
	return b
}

func (p *parser) stmtList() []Stmt {
	var list []Stmt
	for p.tok.tok != RBRACE && p.tok.tok != EOF && p.tok.tok != CASE && p.tok.tok != DEFAULT {
		if p.tok.tok == SEMICOLON {
			p.next()
			continue
		}
		list = append(list, p.stmt())
		p.semi()
	}
	return list
}

func (p *parser) stmt() Stmt {
	pos := p.tok.pos
	switch p.tok.tok {
	case VAR, CONST, FAULT:
		return &DeclStmt{Decl: p.genDecl(false, false)}
	case ENUM:
		p.errorf(pos, "an enum is declared at package level")
		panic(bailout{})
	case PUB:
		p.errorf(pos, "pub is only for package-level declarations")
		panic(bailout{})
	case LBRACE:
		return p.block()
	case IF:
		return p.ifStmt()
	case ARENA:
		p.errorf(pos, "an arena block is with arena NAME SIZE { ... }")
		panic(bailout{})
	case FOR:
		return p.forStmt("")
	case WHILE:
		return p.whileStmt("")
	case SWITCH:
		return p.switchStmt("")
	case RETURN:
		p.next()
		r := &ReturnStmt{P: pos}
		if p.tok.tok != SEMICOLON && p.tok.tok != RBRACE {
			r.Results = p.exprList()
		}
		return r
	case BREAK, CONTINUE:
		t := p.tok.tok
		p.next()
		b := &BranchStmt{Tok: t, P: pos}
		if p.tok.tok == IDENT {
			b.Label = p.ident()
		}
		return b
	case DEFER:
		p.next()
		return p.callStmt(DEFER, pos)
	case SEMICOLON:
		return &EmptyStmt{P: pos}
	}
	if p.tok.tok == IDENT {
		switch nt := p.peek().tok; {
		case p.tok.lit == "start" && nt == IDENT:
			p.next()
			return p.callStmt(TASK, pos)
		case p.tok.lit == "loop" && nt == LBRACE:
			return p.loopStmt("")
		case p.tok.lit == "with" && nt == ARENA:
			p.next()
			p.next()
			s := &ArenaStmt{P: pos, Name: p.ident()}
			s.Size = p.sizeExpr()
			s.Body = p.block()
			return s
		case p.tok.lit == "with" && nt == IDENT && p.peek().lit == "deadline":
			p.next()
			p.next()
			s := &DeadlineStmt{P: pos}
			old := p.exprLev
			p.exprLev = -1 // a { ends the expression, as in an if condition
			s.D = p.expr()
			p.exprLev = old
			s.Body = p.block()
			return s
		case p.tok.lit == "select" && nt == LBRACE:
			return p.selectStmt("")
		case nt == COLON:
			// a label
			id := p.ident()
			p.next()
			switch {
			case p.tok.tok == FOR:
				return &LabeledStmt{Label: id, P: pos, Stmt: p.forStmt(id.Name)}
			case p.tok.tok == WHILE:
				return &LabeledStmt{Label: id, P: pos, Stmt: p.whileStmt(id.Name)}
			case p.isWord("loop") && p.peek().tok == LBRACE:
				return &LabeledStmt{Label: id, P: pos, Stmt: p.loopStmt(id.Name)}
			case p.tok.tok == SWITCH:
				return &LabeledStmt{Label: id, P: pos, Stmt: p.switchStmt(id.Name)}
			case p.isWord("select") && p.peek().tok == LBRACE:
				return &LabeledStmt{Label: id, P: pos, Stmt: p.selectStmt(id.Name)}
			}
			p.errorf(pos, "labels are only supported on for, while, loop, switch and select statements")
			return &EmptyStmt{P: pos}
		}
	}
	return p.simpleStmt()
}

// callStmt parses the call of `start f(...)` (tok TASK) or `defer f(...)`.
func (p *parser) callStmt(t Tok, pos Pos) Stmt {
	x := p.expr()
	call, ok := x.(*CallExpr)
	if !ok {
		what := "defer"
		if t == TASK {
			what = "start"
		}
		p.errorf(pos, "expression in %s must be a function call", what)
		return &EmptyStmt{P: pos}
	}
	if t == TASK {
		s := &GoStmt{Call: call, P: pos}
		if p.tok.tok == ELSE {
			p.next()
			s.Else = p.block()
		}
		return s
	}
	return &DeferStmt{Call: call, P: pos}
}

func (p *parser) selectStmt(label string) Stmt {
	s := &SelectStmt{P: p.tok.pos, Label: label}
	p.next()
	p.expect(LBRACE)
	for p.tok.tok == CASE || p.tok.tok == DEFAULT {
		cc := &CommClause{P: p.tok.pos}
		if p.got(CASE) {
			cc.Comm = p.simpleStmt()
		} else {
			p.next()
		}
		p.expect(COLON)
		cc.Body = p.stmtList()
		s.Cases = append(s.Cases, cc)
	}
	p.expect(RBRACE)
	return s
}

func (p *parser) simpleStmt() Stmt {
	x := p.expr()
	return p.simpleStmtRest([]Expr{x})
}

// simpleStmtRest finishes a simple statement whose first expressions are lhs.
func (p *parser) simpleStmtRest(lhs []Expr) Stmt {
	pos := p.tok.pos
	for p.got(COMMA) {
		lhs = append(lhs, p.expr())
	}
	if len(lhs) > 1 {
		t := p.tok.tok
		if t != ASSIGN && t != DEFINE {
			p.errorf(p.tok.pos, "expected := or =, found %s", p.found())
			panic(bailout{})
		}
		p.next()
		return &AssignStmt{Lhs: lhs, Tok: t, Rhs: p.exprList(), P: pos}
	}
	x := lhs[0]
	switch p.tok.tok {
	case ASSIGN, DEFINE:
		t := p.tok.tok
		p.next()
		return &AssignStmt{Lhs: []Expr{x}, Tok: t, Rhs: p.exprList(), P: pos}
	case ADD_ASSIGN, SUB_ASSIGN, MUL_ASSIGN, QUO_ASSIGN, REM_ASSIGN, AND_ASSIGN, OR_ASSIGN, XOR_ASSIGN, SHL_ASSIGN, SHR_ASSIGN, AND_NOT_ASSIGN:
		t := p.tok.tok
		p.next()
		return &AssignStmt{Lhs: []Expr{x}, Tok: t, Rhs: []Expr{p.expr()}, P: pos}
	case INC, DEC:
		t := p.tok.tok
		p.next()
		return &IncDecStmt{X: x, Tok: t, P: pos}
	case ARROW:
		p.next()
		return &SendStmt{Chan: x, Value: p.expr(), P: pos}
	}
	return &ExprStmt{X: x}
}

func (p *parser) ifStmt() Stmt {
	s := &IfStmt{P: p.expect(IF)}
	old := p.exprLev
	p.exprLev = -1
	init := p.simpleStmt()
	if p.got(SEMICOLON) {
		s.Init = init
		cond := p.simpleStmt()
		es, ok := cond.(*ExprStmt)
		if !ok {
			p.errorf(s.P, "expected condition in if statement")
			panic(bailout{})
		}
		s.Cond = es.X
	} else {
		es, ok := init.(*ExprStmt)
		if !ok {
			p.errorf(s.P, "expected condition in if statement")
			panic(bailout{})
		}
		s.Cond = es.X
	}
	p.exprLev = old
	s.Body = p.block()
	if p.got(ELSE) {
		if p.tok.tok == IF {
			s.Else = p.ifStmt()
		} else {
			s.Else = p.block()
		}
	}
	return s
}

// forStmt parses `for x in X`, `for k, v in X`, `for i in a..b` and `for init; cond; post`.
func (p *parser) forStmt(label string) Stmt {
	pos := p.expect(FOR)
	old := p.exprLev
	p.exprLev = -1
	defer func() { p.exprLev = old }()
	switch p.tok.tok {
	case LBRACE:
		p.errorf(pos, "for needs a clause: loop { ... } runs until break, while cond { ... } while cond holds")
		panic(bailout{})
	case SEMICOLON:
		return p.forClause(nil, pos, label)
	}
	x := p.binaryRest(p.unary(), 1) // not expr: in here starts the range
	lhs := []Expr{x}
	for p.tok.tok == COMMA && len(lhs) < 3 {
		p.next()
		lhs = append(lhs, p.binaryRest(p.unary(), 1))
	}
	if p.tok.tok != IN {
		init := p.simpleStmtRest(lhs)
		if p.tok.tok != SEMICOLON {
			if _, ok := init.(*ExprStmt); ok {
				p.errorf(pos, "for in a condition is while: while %s { ... }", exprString(x))
				panic(bailout{})
			}
		}
		return p.forClause(init, pos, label)
	}
	p.next()
	rs := &RangeStmt{P: pos, Label: label, Define: true}
	rs.X = p.expr()
	if p.got(DOTDOT) {
		rs.Lo = rs.X
		rs.X = p.expr()
		if bl, ok := rs.Lo.(*BasicLit); ok && bl.Kind == INT && bl.Lit == "0" {
			rs.Lo = nil
		}
	}
	for _, v := range lhs {
		if _, ok := v.(*Ident); !ok {
			p.errorf(v.Pos(), "for ... in declares its variables: write a name, not %s", exprString(v))
		}
	}
	switch len(lhs) {
	case 1:
		rs.Key = lhs[0]
		rs.Single = true
	case 2:
		rs.Key, rs.Value = lhs[0], lhs[1]
	default:
		p.errorf(pos, "for ... in takes at most two variables")
		rs.Key, rs.Value = lhs[0], lhs[1]
	}
	p.exprLev = old
	rs.Body = p.block()
	return rs
}

// forClause parses the rest of `for init; cond; post { ... }`.
func (p *parser) forClause(init Stmt, pos Pos, label string) Stmt {
	var cond Expr
	var post Stmt
	p.expect(SEMICOLON)
	if p.tok.tok != SEMICOLON {
		c := p.simpleStmt()
		es, ok := c.(*ExprStmt)
		if !ok {
			p.errorf(pos, "expected for loop condition")
			panic(bailout{})
		}
		cond = es.X
	}
	p.expect(SEMICOLON)
	if p.tok.tok != LBRACE {
		post = p.simpleStmt()
	}
	p.exprLev = 0
	return &ForStmt{Init: init, Cond: cond, Post: post, Body: p.block(), P: pos, Label: label}
}

func (p *parser) whileStmt(label string) Stmt {
	pos := p.expect(WHILE)
	old := p.exprLev
	p.exprLev = -1
	cond := p.expr()
	p.exprLev = old
	return &ForStmt{Cond: cond, Body: p.block(), P: pos, Label: label}
}

func (p *parser) loopStmt(label string) Stmt {
	pos := p.tok.pos
	p.next()
	return &ForStmt{Body: p.block(), P: pos, Label: label}
}

func (p *parser) switchStmt(label string) Stmt {
	s := &SwitchStmt{P: p.expect(SWITCH), Label: label}
	old := p.exprLev
	p.exprLev = -1
	if p.tok.tok != LBRACE {
		var first Stmt
		if p.tok.tok != SEMICOLON {
			first = p.simpleStmt()
		}
		if p.got(SEMICOLON) {
			s.Init = first
			if p.tok.tok != LBRACE {
				t := p.simpleStmt()
				es, ok := t.(*ExprStmt)
				if !ok {
					p.errorf(s.P, "expected switch expression")
					panic(bailout{})
				}
				s.Tag = es.X
			}
		} else {
			es, ok := first.(*ExprStmt)
			if !ok {
				p.errorf(s.P, "expected switch expression")
				panic(bailout{})
			}
			s.Tag = es.X
		}
	}
	p.exprLev = old
	p.expect(LBRACE)
	for p.tok.tok == CASE || p.tok.tok == DEFAULT {
		cc := &CaseClause{P: p.tok.pos}
		if p.got(CASE) {
			cc.List = p.exprList()
		} else {
			p.next()
		}
		p.expect(COLON)
		cc.Body = p.stmtList()
		s.Cases = append(s.Cases, cc)
	}
	p.expect(RBRACE)
	return s
}

// ---- expressions ----

func (p *parser) exprList() []Expr {
	list := []Expr{p.expr()}
	for p.got(COMMA) {
		list = append(list, p.expr())
	}
	return list
}

func (p *parser) expr() Expr {
	x := p.binaryRest(p.unary(), 1)
	if p.tok.tok == IN {
		pos := p.tok.pos
		p.next()
		a := p.unary()
		x = &InExpr{X: x, Arena: a, P: pos}
	}
	return x
}

func (p *parser) binaryRest(x Expr, prec1 int) Expr {
	for {
		switch p.tok.tok {
		case AMPAMP:
			p.errorf(p.tok.pos, "write and, not &&")
			p.tok.tok = LAND
		case BARBAR:
			p.errorf(p.tok.pos, "write or, not ||")
			p.tok.tok = LOR
		}
		prec := p.tok.tok.Precedence()
		if prec < prec1 {
			return x
		}
		op := p.tok.tok
		pos := p.tok.pos
		p.next()
		y := p.binaryRest(p.unary(), prec+1)
		x = &BinaryExpr{X: x, Op: op, Y: y, P: pos}
	}
}

func (p *parser) unary() Expr {
	pos := p.tok.pos
	switch p.tok.tok {
	case BANG:
		p.errorf(pos, "write not x: a ! after an expression propagates its error")
		p.tok.tok = NOT
		fallthrough
	case ADD, SUB, NOT, XOR, AND, ARROW:
		op := p.tok.tok
		p.next()
		x := p.unary()
		return &UnaryExpr{P: pos, Op: op, X: x}
	case MUL:
		p.next()
		return &StarExpr{P: pos, X: p.unary()}
	}
	return p.postfix(p.operand(), pos)
}

func (p *parser) operand() Expr {
	pos := p.tok.pos
	switch p.tok.tok {
	case IDENT:
		if p.tok.lit == "try" && p.peek().tok == IDENT {
			p.errorf(pos, "try is gone: a ! after the call propagates its error, f()!")
			panic(bailout{})
		}
		return p.ident()
	case INT, FLOAT, CHAR, STRING:
		lit := &BasicLit{P: pos, Kind: p.tok.tok, Lit: p.tok.lit}
		p.next()
		return lit
	case LPAREN:
		p.next()
		p.exprLev++
		x := p.expr()
		p.exprLev--
		p.expect(RPAREN)
		return &ParenExpr{P: pos, X: x}
	case PERIOD: // .member of the enum the context expects
		p.next()
		return &ImplicitMember{P: pos, Name: p.memberIdent()}
	case STRUCT:
		return p.structType()
	case FN:
		p.next()
		ft := p.signature(pos)
		if p.tok.tok == LBRACE {
			p.errorf(pos, "function literals (closures) are not supported in squark; declare a named function")
			panic(bailout{})
		}
		return ft
	case LBRACK:
		p.errorf(pos, "the brackets go after the element type: T[]{...}, T[N]{...}")
		panic(bailout{})
	}
	p.errorf(pos, "expected expression, found %s", p.found())
	panic(bailout{})
}

func isTypeName(x Expr) bool {
	switch t := x.(type) {
	case *Ident:
		return true
	case *SelectorExpr:
		_, ok := t.X.(*Ident)
		return ok
	}
	return false
}

// isTypeLike reports whether x may be a type, so that a following { starts a composite literal.
func isTypeLike(x Expr) bool {
	switch t := x.(type) {
	case *Ident, *ArrayType, *VecTypeExpr, *MapType, *ChanType, *StructType:
		return true
	case *SelectorExpr:
		return isTypeName(t)
	case *IndexExpr:
		return isTypeLike(t.X)
	case *ParenExpr:
		_, ok := t.X.(*StarExpr)
		return ok
	}
	return false
}

// isExplicitType: a postfix type that can't be an index expression.
func isExplicitType(x Expr) bool {
	switch t := x.(type) {
	case *ArrayType:
		return !t.MaybeMap && (t.Len == nil || isEllipsis(t.Len))
	case *VecTypeExpr, *MapType:
		return true
	}
	return false
}

func isEllipsis(x Expr) bool {
	bl, ok := x.(*BasicLit)
	return ok && bl.Kind == ELLIPSIS
}

// asType turns an expression parsed before it was known to be a type (int[4] in int[4]{...})
// into a type expression.
func asType(x Expr) Expr {
	switch t := x.(type) {
	case *IndexExpr:
		_, star := t.Index.(*StarExpr)
		return &ArrayType{P: t.X.Pos(), Len: t.Index, Elem: asType(t.X), MaybeMap: isTypeName(t.Index) || star}
	case *ParenExpr:
		if s, ok := t.X.(*StarExpr); ok {
			return &StarExpr{P: s.P, X: asType(s.X)}
		}
	}
	return x
}

func (p *parser) postfix(x Expr, start Pos) Expr {
	for {
		pos := p.tok.pos
		switch p.tok.tok {
		case PERIOD:
			p.next()
			x = &SelectorExpr{X: x, Sel: p.memberIdent()}
		case LBRACK:
			switch p.peek().tok {
			case RBRACK, DOTDOT, ELLIPSIS:
				x = p.bracketType(asType(x), start)
				continue
			}
			p.next()
			p.exprLev++
			var lo, hi, max Expr
			if p.tok.tok != COLON {
				lo = p.expr()
			}
			if p.got(SEMICOLON) { // V[K; N]: map storage
				n := p.expr()
				p.exprLev--
				p.expect(RBRACK)
				x = &MapType{P: start, Key: lo, Value: asType(x), Cap: n}
				continue
			}
			if p.got(COLON) {
				if p.tok.tok != RBRACK && p.tok.tok != COLON {
					hi = p.expr()
				}
				if p.got(COLON) {
					max = p.expr()
				}
				p.exprLev--
				p.expect(RBRACK)
				x = &SliceExpr{X: x, Lo: lo, Hi: hi, Max: max, P: pos}
				continue
			}
			p.exprLev--
			p.expect(RBRACK)
			x = &IndexExpr{X: x, Index: lo, P: pos}
		case LPAREN:
			p.next()
			p.exprLev++
			call := &CallExpr{Fun: x, P: pos}
			for p.tok.tok != RPAREN {
				call.Args = append(call.Args, p.expr())
				if p.got(ELLIPSIS) {
					call.Ellipsis = true
				}
				if !p.got(COMMA) {
					break
				}
			}
			p.exprLev--
			p.expect(RPAREN)
			x = call
		case LBRACE:
			// in a control clause (exprLev < 0) { starts the block, unless the type can't be
			// anything else: T[]{...}, T[...]{...}, T[..N]{...}
			if isTypeLike(x) && (p.exprLev >= 0 || isExplicitType(x)) {
				x = p.compositeLit(asType(x))
				continue
			}
			return x
		case BANG:
			p.next()
			x = &TryExpr{P: start, X: x}
		case CHAN:
			if !isTypeLike(x) {
				return x
			}
			x = p.typeSuffix(asType(x), start)
		default:
			return x
		}
	}
}

func (p *parser) compositeLit(t Expr) Expr {
	lit := &CompositeLit{P: p.expect(LBRACE), Type: t}
	old := p.exprLev
	p.exprLev = 0
	for p.tok.tok != RBRACE {
		e := p.element()
		if p.got(COLON) {
			e = &KeyValueExpr{Key: e, Value: p.element()}
		}
		lit.Elts = append(lit.Elts, e)
		if !p.got(COMMA) {
			break
		}
	}
	p.exprLev = old
	p.expect(RBRACE)
	return lit
}

func (p *parser) element() Expr {
	if p.tok.tok == LBRACE {
		// elided type
		return p.compositeLit(nil)
	}
	if isKeyword(p.tok.tok) && p.peek().tok == COLON { // a field spelled like a keyword
		return p.memberIdent()
	}
	return p.expr()
}

// helper for diagnostics
func exprString(x Expr) string {
	elem := func(e Expr) string {
		s := exprString(e)
		switch e := e.(type) {
		case *FuncType:
			return "(" + s + ")"
		case *ChanType:
			if e.Cap == nil {
				return "(" + s + ")"
			}
		}
		return s
	}
	switch e := x.(type) {
	case *Ident:
		return e.Name
	case *SelectorExpr:
		return exprString(e.X) + "." + e.Sel.Name
	case *StarExpr:
		return "*" + exprString(e.X)
	case *ParenExpr:
		return "(" + exprString(e.X) + ")"
	case *TryExpr:
		return exprString(e.X) + "!"
	case *ImplicitMember:
		return "." + e.Name.Name
	case *InExpr:
		return exprString(e.X) + " in " + exprString(e.Arena)
	case *IndexExpr:
		return exprString(e.X) + "[" + exprString(e.Index) + "]"
	case *SliceExpr:
		s := exprString(e.X) + "["
		if e.Lo != nil {
			s += exprString(e.Lo)
		}
		s += ":"
		if e.Hi != nil {
			s += exprString(e.Hi)
		}
		return s + "]"
	case *CallExpr:
		var args []string
		for _, a := range e.Args {
			args = append(args, exprString(a))
		}
		return exprString(e.Fun) + "(" + strings.Join(args, ", ") + ")"
	case *UnaryExpr:
		if e.Op == NOT {
			return "not " + exprString(e.X)
		}
		return e.Op.String() + exprString(e.X)
	case *BinaryExpr:
		return exprString(e.X) + " " + e.Op.String() + " " + exprString(e.Y)
	case *BasicLit:
		if e.Kind == STRING {
			return fmt.Sprintf("%q", e.Lit)
		}
		return e.Lit
	case *CompositeLit:
		if e.Type != nil {
			return exprString(e.Type) + "{...}"
		}
		return "{...}"
	case *ArrayType:
		if e.Len == nil {
			return elem(e.Elem) + "[]"
		}
		return elem(e.Elem) + "[" + exprString(e.Len) + "]"
	case *VecTypeExpr:
		return elem(e.Elem) + "[.." + exprString(e.Len) + "]"
	case *MapType:
		s := elem(e.Value) + "[" + exprString(e.Key)
		if e.Cap != nil {
			s += "; " + exprString(e.Cap)
		}
		return s + "]"
	case *ChanType:
		s := elem(e.Elem) + " chan"
		if e.Cap != nil {
			s += "[" + exprString(e.Cap) + "]"
		}
		return s
	case *FuncType:
		return "fn(...)"
	case *StructType:
		return "struct{...}"
	case *ArenaTypeExpr:
		return "arena"
	}
	return "expr"
}
