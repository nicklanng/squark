package compiler

import (
	"fmt"
	"strings"
)

type parser struct {
	lx      *lexer
	tok     lexToken
	ahead   *lexToken // one token of lookahead (see peek)
	errs    ErrorList
	exprLev int // < 0: in control clause (composite literals not allowed)
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

func (p *parser) expect(t Tok) Pos {
	pos := p.tok.pos
	if p.tok.tok != t {
		got := p.tok.tok.String()
		if p.tok.tok == SEMICOLON && p.tok.lit == "\n" {
			got = "newline"
		} else if p.tok.lit != "" {
			got = fmt.Sprintf("%s %q", got, p.tok.lit)
		}
		p.errorf(pos, "expected %s, found %s", t, got)
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

func (p *parser) file(path string) *File {
	f := &File{Path: path, Pragmas: p.tok.pragmas}
	p.expect(PACKAGE)
	f.Package = p.ident().Name
	p.semi()
	for p.tok.tok == IMPORT {
		p.next()
		if p.got(LPAREN) {
			for p.tok.tok != RPAREN {
				f.Imports = append(f.Imports, p.importSpec())
				p.semi()
			}
			p.expect(RPAREN)
		} else {
			f.Imports = append(f.Imports, p.importSpec())
		}
		p.semi()
	}
	for p.tok.tok != EOF {
		if p.tok.tok == IDENT {
			switch p.tok.lit {
			case "budget", "link", "cflags", "ldflags", "pool", "workers", "threads":
				d := &Directive{Name: p.tok.lit, P: p.tok.pos}
				p.next()
				if d.Name == "pool" {
					// pool pkg.task N: resize a task's pool for this program
					d.Target = p.ident().Name
					if p.got(PERIOD) {
						d.Target += "." + p.ident().Name
					}
				}
				d.Value = p.sizeExpr()
				f.Directives = append(f.Directives, d)
				p.semi()
				continue
			}
		}
		f.Decls = append(f.Decls, p.topDecl())
	}
	return f
}

func (p *parser) importSpec() *ImportSpec {
	s := &ImportSpec{P: p.tok.pos}
	if p.tok.tok == IDENT {
		s.Name = p.ident()
	}
	s.Path = p.tok.lit
	p.expect(STRING)
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

// clauses parses trailing `pool N` / `stack N` clauses after a signature.
func (p *parser) clauses(d *FuncDecl) {
	for p.tok.tok == IDENT && (p.tok.lit == "pool" || p.tok.lit == "stack") {
		name := p.tok.lit
		pos := p.tok.pos
		p.next()
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
	switch p.tok.tok {
	case FUNC:
		d := p.funcDecl()
		d.Pragmas = pragmas
		p.semi()
		return d
	case TASK:
		d := p.funcDecl()
		d.Pragmas = pragmas
		p.semi()
		return d
	case EXTERN:
		pos := p.tok.pos
		p.next()
		sym := ""
		if p.tok.tok == STRING {
			sym = p.tok.lit
			p.next()
		}
		if p.tok.tok != FUNC {
			p.errorf(p.tok.pos, "expected func after extern")
			panic(bailout{})
		}
		d := p.funcDecl()
		d.Kind = KindExtern
		d.ExternSym = sym
		d.P = pos
		d.Pragmas = pragmas
		if d.Body != nil {
			p.errorf(pos, "extern function %s must not have a body", d.Name.Name)
		}
		p.semi()
		return d
	case ARENA:
		pos := p.tok.pos
		p.next()
		d := &ArenaDecl{P: pos, Name: p.ident()}
		d.Size = p.sizeExpr()
		p.semi()
		return d
	case VAR, CONST, TYPE:
		d := p.genDecl()
		d.Pragmas = pragmas
		p.semi()
		return d
	}
	p.errorf(p.tok.pos, "expected declaration, found %s", p.tok.tok)
	panic(bailout{})
}

func (p *parser) funcDecl() *FuncDecl {
	d := &FuncDecl{P: p.tok.pos}
	if p.tok.tok == TASK {
		d.Kind = KindTask
		p.next()
	} else {
		p.expect(FUNC)
	}
	if p.tok.tok == LPAREN {
		p.next()
		f := &Field{}
		if p.tok.tok == MUL || (p.tok.tok == IDENT && p.peek().tok == RPAREN) {
			f.Names = []*Ident{{Name: "_", P: p.tok.pos}} // unnamed receiver: func (T) M()
		} else {
			f.Names = []*Ident{p.ident()}
		}
		f.Type = p.typ()
		p.expect(RPAREN)
		d.Recv = f
	}
	d.Name = p.ident()
	d.Type = p.signature(d.P)
	p.clauses(d)
	if p.tok.tok == LBRACE {
		d.Body = p.block()
	}
	return d
}

func (p *parser) signature(pos Pos) *FuncType {
	ft := &FuncType{P: pos}
	ft.Params = p.params()
	if p.tok.tok == LPAREN {
		ft.Results = p.params()
	} else if p.startsType() && !p.atClause() {
		ft.Results = []*Field{{Type: p.typ()}}
	}
	// result provenance: `from a, b` or `static`
	if len(ft.Results) == 0 && p.tok.tok == IDENT && (p.tok.lit == "from" || p.tok.lit == "static") {
		p.errorf(p.tok.pos, "%s must follow results: it says where the results point", p.tok.lit)
		panic(bailout{})
	}
	if len(ft.Results) > 0 && p.tok.tok == IDENT {
		switch p.tok.lit {
		case "from":
			p.next()
			ft.From = append(ft.From, p.ident())
			for p.got(COMMA) {
				ft.From = append(ft.From, p.ident())
			}
		case "static":
			p.next()
			ft.ResultStatic = true
		}
	}
	return ft
}

// atClause reports whether the next tokens start a clause after the parameters (`pool N`, `stack N`,
// `from p` or `static`) rather than a result type that happens to have one of those names.
func (p *parser) atClause() bool {
	if p.tok.tok != IDENT {
		return false
	}
	switch p.tok.lit {
	case "pool", "stack":
		switch t := p.peek(); t.tok {
		case INT, LPAREN:
			return true
		case IDENT:
			return !isClauseWord(t.lit)
		}
	case "from":
		return p.peek().tok == IDENT
	case "static":
		return true
	}
	return false
}

func isClauseWord(s string) bool {
	return s == "pool" || s == "stack" || s == "from" || s == "static"
}

func (p *parser) startsType() bool {
	switch p.tok.tok {
	case IDENT, LBRACK, MUL, FUNC, STRUCT, CHAN, MAP, LPAREN, ARENA:
		return true
	}
	return false
}

// params parses a Go parameter list. Either all parameters are named or none.
func (p *parser) params() []*Field {
	p.expect(LPAREN)
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
			case PERIOD:
				p.next()
				sel := p.ident()
				e.typ = &SelectorExpr{X: id, Sel: sel}
			case ELLIPSIS:
				p.next()
				e.name = id
				e.variadic = true
				e.typ = p.typ()
			default:
				e.name = id
				e.typ = p.typ()
				// `name static T`: the qualifier parses as a type name first
				if st, ok := e.typ.(*Ident); ok && st.Name == "static" && p.startsType() {
					e.static = true
					e.typ = p.typ()
				}
				if p.tok.tok == IDENT && p.tok.lit == "into" {
					p.next()
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

func (p *parser) genDecl() *GenDecl {
	d := &GenDecl{Tok: p.tok.tok, P: p.tok.pos}
	p.next()
	one := func(iota int, prev *ValueSpec) *ValueSpec {
		if d.Tok == TYPE {
			ts := &TypeSpec{P: p.tok.pos, Name: p.ident()}
			if p.tok.tok == ASSIGN {
				p.errorf(p.tok.pos, "type aliases are not supported")
			}
			ts.Type = p.typ()
			d.Types = append(d.Types, ts)
			return nil
		}
		vs := &ValueSpec{P: p.tok.pos, Iota: iota}
		vs.Names = append(vs.Names, p.ident())
		for p.got(COMMA) {
			vs.Names = append(vs.Names, p.ident())
		}
		if p.tok.tok != ASSIGN && p.tok.tok != SEMICOLON && p.tok.tok != RPAREN {
			vs.Type = p.typ()
		}
		if p.got(ASSIGN) {
			vs.Values = p.exprList()
		} else if d.Tok == CONST && prev != nil {
			// implicit repetition of the previous expression list
			vs.Type = prev.Type
			vs.Values = prev.Values
		}
		d.Values = append(d.Values, vs)
		return vs
	}
	if p.got(LPAREN) {
		var prev *ValueSpec
		i := 0
		for p.tok.tok != RPAREN {
			if vs := one(i, prev); vs != nil {
				prev = vs
			}
			i++
			p.semi()
		}
		p.expect(RPAREN)
	} else {
		one(0, nil)
	}
	return d
}

// ---- types ----

func (p *parser) typ() Expr {
	pos := p.tok.pos
	switch p.tok.tok {
	case IDENT:
		id := p.ident()
		if p.tok.tok == PERIOD {
			p.next()
			return &SelectorExpr{X: id, Sel: p.ident()}
		}
		return id
	case LBRACK:
		p.next()
		if p.got(RBRACK) {
			return &ArrayType{P: pos, Elem: p.typ()}
		}
		if p.got(DOTDOT) {
			p.exprLev++
			n := p.expr()
			p.exprLev--
			p.expect(RBRACK)
			return &VecTypeExpr{P: pos, Len: n, Elem: p.typ()}
		}
		if p.tok.tok == ELLIPSIS {
			epos := p.tok.pos
			p.next()
			p.expect(RBRACK)
			return &ArrayType{P: pos, Len: &BasicLit{P: epos, Kind: ELLIPSIS, Lit: "..."}, Elem: p.typ()}
		}
		p.exprLev++
		n := p.expr()
		p.exprLev--
		p.expect(RBRACK)
		return &ArrayType{P: pos, Len: n, Elem: p.typ()}
	case MUL:
		p.next()
		return &StarExpr{P: pos, X: p.typ()}
	case CHAN:
		p.next()
		ct := &ChanType{P: pos, Elem: p.typ()}
		if p.tok.tok == IDENT && p.tok.lit == "cap" {
			p.next()
			ct.Cap = p.sizeExpr()
		}
		return ct
	case ARENA:
		p.next()
		return &ArenaTypeExpr{P: pos}
	case MAP:
		p.next()
		p.expect(LBRACK)
		k := p.typ()
		p.expect(RBRACK)
		mt := &MapType{P: pos, Key: k, Value: p.typ()}
		if p.tok.tok == IDENT && p.tok.lit == "cap" {
			p.next()
			mt.Cap = p.sizeExpr()
		}
		return mt
	case FUNC:
		p.next()
		return p.signature(pos)
	case STRUCT:
		return p.structType()
	case LPAREN:
		p.next()
		t := p.typ()
		p.expect(RPAREN)
		return t
	}
	p.errorf(pos, "expected type, found %s", p.tok.tok)
	panic(bailout{})
}

func (p *parser) structType() *StructType {
	st := &StructType{P: p.expect(STRUCT)}
	p.expect(LBRACE)
	for p.tok.tok != RBRACE {
		f := &Field{}
		f.Names = append(f.Names, p.ident())
		for p.got(COMMA) {
			f.Names = append(f.Names, p.ident())
		}
		f.Type = p.typ()
		if p.tok.tok == STRING { // tag, ignored
			p.next()
		}
		st.Fields = append(st.Fields, f)
		p.semi()
	}
	p.expect(RBRACE)
	return st
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
	case VAR, CONST, TYPE:
		return &DeclStmt{Decl: p.genDecl()}
	case LBRACE:
		return p.block()
	case IF:
		return p.ifStmt()
	case ARENA:
		p.next()
		s := &ArenaStmt{P: pos, Name: p.ident()}
		s.Size = p.sizeExpr()
		s.Body = p.block()
		return s
	case FOR:
		return p.forStmt("")
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
	case GO, DEFER:
		t := p.tok.tok
		p.next()
		x := p.expr()
		call, ok := x.(*CallExpr)
		if !ok {
			p.errorf(pos, "expression in %s must be a function call", t)
			return &EmptyStmt{P: pos}
		}
		if t == GO {
			return &GoStmt{Call: call, P: pos}
		}
		return &DeferStmt{Call: call, P: pos}
	case SEMICOLON:
		return &EmptyStmt{P: pos}
	}
	if p.tok.tok == IDENT {
		// label?
		id := p.ident()
		if p.tok.tok == COLON {
			p.next()
			switch p.tok.tok {
			case FOR:
				return &LabeledStmt{Label: id, P: pos, Stmt: p.forStmt(id.Name)}
			case SWITCH:
				return &LabeledStmt{Label: id, P: pos, Stmt: p.switchStmt(id.Name)}
			}
			p.errorf(pos, "labels are only supported on for and switch statements")
			return &EmptyStmt{P: pos}
		}
		x := p.binaryRest(p.postfix(p.operandRest(id)), 1)
		if p.tok.tok == IN {
			ipos := p.tok.pos
			p.next()
			x = &InExpr{X: x, Arena: p.unary(), P: ipos}
		}
		return p.simpleStmtRest(x)
	}
	return p.simpleStmt()
}

func (p *parser) simpleStmt() Stmt {
	x := p.expr()
	return p.simpleStmtRest(x)
}

func (p *parser) simpleStmtRest(x Expr) Stmt {
	pos := p.tok.pos
	switch p.tok.tok {
	case COMMA:
		lhs := []Expr{x}
		for p.got(COMMA) {
			lhs = append(lhs, p.expr())
		}
		t := p.tok.tok
		if t != ASSIGN && t != DEFINE {
			p.errorf(p.tok.pos, "expected := or =, found %s", t)
			panic(bailout{})
		}
		p.next()
		if p.tok.tok == RANGE {
			return p.rangeRest(lhs, t == DEFINE, pos)
		}
		return &AssignStmt{Lhs: lhs, Tok: t, Rhs: p.exprList(), P: pos}
	case ASSIGN, DEFINE:
		t := p.tok.tok
		p.next()
		if p.tok.tok == RANGE {
			return p.rangeRest([]Expr{x}, t == DEFINE, pos)
		}
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

// rangeSentinel marks a pending range clause returned from simpleStmt inside a for header.
type rangeClause struct {
	EmptyStmt
	lhs    []Expr
	define bool
	x      Expr
}

func (p *parser) rangeRest(lhs []Expr, define bool, pos Pos) Stmt {
	p.expect(RANGE)
	x := p.expr()
	return &rangeClause{EmptyStmt: EmptyStmt{P: pos}, lhs: lhs, define: define, x: x}
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

func (p *parser) forStmt(label string) Stmt {
	pos := p.expect(FOR)
	old := p.exprLev
	p.exprLev = -1
	var init, post Stmt
	var cond Expr
	if p.tok.tok == RANGE {
		// for range x
		p.next()
		x := p.expr()
		p.exprLev = old
		return &RangeStmt{X: x, Body: p.block(), P: pos, Label: label}
	}
	if p.tok.tok != LBRACE {
		if p.tok.tok != SEMICOLON {
			init = p.simpleStmt()
		}
		if rc, ok := init.(*rangeClause); ok {
			p.exprLev = old
			rs := &RangeStmt{Define: rc.define, X: rc.x, P: pos, Label: label}
			rs.Key = rc.lhs[0]
			if len(rc.lhs) > 1 {
				rs.Value = rc.lhs[1]
			}
			if len(rc.lhs) > 2 {
				p.errorf(pos, "range clause permits at most two iteration variables")
			}
			rs.Body = p.block()
			return rs
		}
		if p.tok.tok == SEMICOLON {
			p.next()
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
		} else {
			es, ok := init.(*ExprStmt)
			if !ok {
				p.errorf(pos, "expected for loop condition")
				panic(bailout{})
			}
			cond = es.X
			init = nil
		}
	}
	p.exprLev = old
	return &ForStmt{Init: init, Cond: cond, Post: post, Body: p.block(), P: pos, Label: label}
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
	case ADD, SUB, NOT, XOR, AND, ARROW:
		op := p.tok.tok
		p.next()
		x := p.unary()
		return &UnaryExpr{P: pos, Op: op, X: x}
	case MUL:
		p.next()
		return &StarExpr{P: pos, X: p.unary()}
	case TRY:
		p.next()
		return &TryExpr{P: pos, X: p.unary()}
	}
	return p.postfix(p.operand())
}

func (p *parser) operand() Expr {
	pos := p.tok.pos
	switch p.tok.tok {
	case IDENT:
		return p.operandRest(p.ident())
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
	case LBRACK, STRUCT, MAP, CHAN:
		t := p.typ()
		if p.tok.tok == LBRACE {
			return p.compositeLit(t)
		}
		return t
	case MUL:
		return p.typ()
	case FUNC:
		p.errorf(pos, "function literals (closures) are not supported in squark; declare a named function")
		panic(bailout{})
	}
	p.errorf(pos, "expected expression, found %s", p.tok.tok)
	panic(bailout{})
}

func (p *parser) operandRest(id *Ident) Expr { return id }

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

func (p *parser) postfix(x Expr) Expr {
	for {
		pos := p.tok.pos
		switch p.tok.tok {
		case PERIOD:
			p.next()
			x = &SelectorExpr{X: x, Sel: p.ident()}
		case LBRACK:
			p.next()
			p.exprLev++
			var lo, hi, max Expr
			if p.tok.tok != COLON {
				lo = p.expr()
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
				if p.tok.tok == LBRACK || p.tok.tok == MAP || p.tok.tok == CHAN || (p.tok.tok == MUL && false) {
					call.Args = append(call.Args, p.typeOrExpr())
				} else {
					call.Args = append(call.Args, p.expr())
				}
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
			if isTypeName(x) && p.exprLev >= 0 {
				x = p.compositeLit(x)
				continue
			}
			return x
		default:
			return x
		}
	}
}

func (p *parser) typeOrExpr() Expr {
	return p.expr()
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
	return p.expr()
}

// helper for diagnostics
func exprString(x Expr) string {
	switch e := x.(type) {
	case *Ident:
		return e.Name
	case *SelectorExpr:
		return exprString(e.X) + "." + e.Sel.Name
	case *StarExpr:
		return "*" + exprString(e.X)
	case *ParenExpr:
		return "(" + exprString(e.X) + ")"
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
			return "[]" + exprString(e.Elem)
		}
		return "[" + exprString(e.Len) + "]" + exprString(e.Elem)
	}
	return "expr"
}
