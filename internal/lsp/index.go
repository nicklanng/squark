package lsp

import (
	"reflect"

	"squark/internal/compiler"
)

// index is what a file's identifiers name: every identifier, and for a field's name in a
// selector (p.x), the selector, whose operand's type has the field.
type index struct {
	idents    []*compiler.Ident
	selectors map[*compiler.Ident]*compiler.SelectorExpr
	keys      map[*compiler.Ident]*compiler.CompositeLit // a struct literal's keys (Point{x: 1})
}

var (
	identType    = reflect.TypeOf(&compiler.Ident{})
	selectorType = reflect.TypeOf(&compiler.SelectorExpr{})
	literalType  = reflect.TypeOf(&compiler.CompositeLit{})
	typeType     = reflect.TypeOf((*compiler.Type)(nil)).Elem()
	// what the checker hangs on the tree: followed, these lead out of the file
	skip = map[reflect.Type]bool{
		reflect.TypeOf(&compiler.Object{}):  true,
		reflect.TypeOf(&compiler.Func{}):    true,
		reflect.TypeOf(&compiler.Var{}):     true,
		reflect.TypeOf(&compiler.Package{}): true,
		reflect.TypeOf(&compiler.Scope{}):   true,
	}
)

// indexOf walks a file's syntax tree (by reflection, so every kind of node is covered).
func indexOf(f *compiler.File) *index {
	ix := &index{selectors: map[*compiler.Ident]*compiler.SelectorExpr{}, keys: map[*compiler.Ident]*compiler.CompositeLit{}}
	seen := map[uintptr]bool{}
	var walk func(v reflect.Value)
	walk = func(v reflect.Value) {
		switch v.Kind() {
		case reflect.Pointer:
			if v.IsNil() || skip[v.Type()] || v.Type().Implements(typeType) {
				return
			}
			if seen[v.Pointer()] {
				return
			}
			seen[v.Pointer()] = true
			switch v.Type() {
			case identType:
				ix.idents = append(ix.idents, v.Interface().(*compiler.Ident))
			case selectorType:
				sel := v.Interface().(*compiler.SelectorExpr)
				if sel.Sel != nil {
					ix.selectors[sel.Sel] = sel
				}
			case literalType:
				lit := v.Interface().(*compiler.CompositeLit)
				for _, el := range lit.Elts {
					if kv, ok := el.(*compiler.KeyValueExpr); ok {
						if id, ok := kv.Key.(*compiler.Ident); ok {
							ix.keys[id] = lit
						}
					}
				}
			}
			walk(v.Elem())
		case reflect.Interface:
			if !v.IsNil() {
				walk(v.Elem())
			}
		case reflect.Struct:
			for i := 0; i < v.NumField(); i++ {
				if v.Type().Field(i).IsExported() {
					walk(v.Field(i))
				}
			}
		case reflect.Slice, reflect.Array:
			for i := 0; i < v.Len(); i++ {
				walk(v.Index(i))
			}
		}
	}
	walk(reflect.ValueOf(f))
	return ix
}

// field is the declaration of the field a selector names, if it names one.
func field(sel *compiler.SelectorExpr) (*compiler.Field, *compiler.Ident) {
	if sel == nil || sel.X == nil {
		return nil, nil
	}
	return structField(compiler.ExprType(sel.X), sel.Sel.Name)
}

// fieldOf is the field id names, in a selector or as a struct literal's key.
func (ix *index) fieldOf(id *compiler.Ident) (*compiler.Field, *compiler.Ident) {
	if fd, n := field(ix.selectors[id]); fd != nil {
		return fd, n
	}
	if lit := ix.keys[id]; lit != nil {
		return structField(compiler.ExprType(lit), id.Name)
	}
	return nil, nil
}

// structField finds the declaration of field name of t (a struct type, or a pointer to one).
func structField(t compiler.Type, name string) (*compiler.Field, *compiler.Ident) {
	if p, ok := t.(*compiler.Pointer); ok {
		t = p.Elem
	}
	named, ok := t.(*compiler.Named)
	if !ok || named.Obj == nil || named.Obj.TypeDecl == nil {
		return nil, nil
	}
	st, ok := named.Obj.TypeDecl.Type.(*compiler.StructType)
	if !ok {
		return nil, nil
	}
	for _, fd := range st.Fields {
		for _, n := range fd.Names {
			if n.Name == name {
				return fd, n
			}
		}
	}
	return nil, nil
}
