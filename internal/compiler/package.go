package compiler

import (
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

type Scope struct {
	parent *Scope
	names  map[string]*Object
}

func NewScope(parent *Scope) *Scope {
	return &Scope{parent: parent, names: map[string]*Object{}}
}

func (s *Scope) Lookup(name string) *Object {
	for ; s != nil; s = s.parent {
		if o, ok := s.names[name]; ok {
			return o
		}
	}
	return nil
}

func (s *Scope) LookupLocal(name string) *Object { return s.names[name] }

func (s *Scope) Insert(o *Object) *Object {
	if prev, ok := s.names[o.Name]; ok {
		return prev
	}
	s.names[o.Name] = o
	return nil
}

type Package struct {
	Path    string // import path ("main" for the program package)
	Std     bool   // a standard library package
	Name    string
	Dir     string
	Files   []*File
	Scope   *Scope
	Imports []*Package

	Funcs   []*Func
	Generics []*Func // functions with anytype parameters (templates; their instances are in Funcs)
	Globals []*Var
	GlobalInits []*ValueSpec
	Types   []*Named

	CFiles  []CFile // C shims shipped with the package (.c and .h)
	Budgets []*Directive
	Arenas  []*Var // package-level arenas
	Links   []string
	CFlags  []string
	LDFlags []string
}

type CFile struct {
	Name string
	Src  []byte
}

type Program struct {
	Packages []*Package // dependency order, main last
	Main     *Package
	Funcs    []*Func
	MainFunc *Func
	InitFunc *Func // synthetic function holding package-level initializer calls
	Budget   int64 // budget N in package main (0: none)
	Workers  int64 // workers N in package main: processes to run (default 1; 0: one per core)
	Threads  int64 // threads N in package main: scheduler threads per process (default 1; 0: one per core)
	ThreadsSet bool // the program has a threads directive: it is written for threads, so SQUARK_THREADS applies

	GoSites    []*GoStmt
	MakeSites  []*CallExpr
	NumDefers  int
}

// Loader finds and parses packages.
type Loader struct {
	Std     fs.FS // standard library root (contains std/<pkg>/*.sq)
	MainDir string

	pkgs    map[string]*Package
	loading map[string]bool
	order   []*Package
}

func (l *Loader) Load() (*Program, error) {
	l.pkgs = map[string]*Package{}
	l.loading = map[string]bool{}
	mainPkg, err := l.load(l.localKey(l.MainDir), "main", l.MainDir, false)
	if err != nil {
		return nil, err
	}
	if mainPkg.Name != "main" {
		return nil, fmt.Errorf("%s: package %s is not a main package", l.MainDir, mainPkg.Name)
	}
	prog := &Program{Packages: l.order, Main: mainPkg, Workers: 1, Threads: 1}
	return prog, nil
}

func (l *Loader) readDir(dir string, std bool) (names []string, read func(string) ([]byte, error), err error) {
	if std {
		ents, err := fs.ReadDir(l.Std, dir)
		if err != nil {
			return nil, nil, err
		}
		for _, e := range ents {
			names = append(names, e.Name())
		}
		return names, func(n string) ([]byte, error) { return fs.ReadFile(l.Std, path.Join(dir, n)) }, nil
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil, nil, err
	}
	for _, e := range ents {
		if !e.IsDir() {
			names = append(names, e.Name())
		}
	}
	return names, func(n string) ([]byte, error) { return os.ReadFile(filepath.Join(dir, n)) }, nil
}

// localKey is the cache key of the package in dir: its absolute path, so the same directory reached
// through different relative paths is one package, and a local package never shadows a std one.
func (l *Loader) localKey(dir string) string {
	if abs, err := filepath.Abs(dir); err == nil {
		dir = abs
	}
	return "local:" + filepath.Clean(dir)
}

// load reads the package in dir, cached under key. importPath becomes Package.Path: the import path
// for std packages, "main" for the program, the absolute directory for other local packages.
func (l *Loader) load(key, importPath, dir string, isStd bool) (*Package, error) {
	if p, ok := l.pkgs[key]; ok {
		return p, nil
	}
	if l.loading[key] {
		return nil, fmt.Errorf("import cycle involving %q", importPath)
	}
	l.loading[key] = true
	names, read, err := l.readDir(dir, isStd)
	if err != nil {
		return nil, fmt.Errorf("cannot find package %q: %v", importPath, err)
	}
	sort.Strings(names)
	pkg := &Package{Path: importPath, Dir: dir, Std: isStd}
	var errs ErrorList
	for _, n := range names {
		switch {
		case strings.HasSuffix(n, ".sq") && !strings.HasSuffix(n, "_test.sq"):
			src, err := read(n)
			if err != nil {
				return nil, err
			}
			display := filepath.Join(dir, n)
			if isStd {
				display = "$SQUARK/" + path.Join(dir, n)
			}
			f, err := ParseFile(display, src)
			if err != nil {
				if el, ok := err.(ErrorList); ok {
					errs = append(errs, el...)
					continue
				}
				return nil, err
			}
			if pkg.Name == "" {
				pkg.Name = f.Package
			} else if pkg.Name != f.Package {
				errs = append(errs, &Error{Pos{File: display, Line: 1, Col: 1}, fmt.Sprintf("package %s; expected %s", f.Package, pkg.Name)})
			}
			for _, d := range f.Directives {
				lit, ok := d.Value.(*BasicLit)
				switch d.Name {
				case "link", "cflags", "ldflags":
					if !ok || lit.Kind != STRING {
						errs = append(errs, &Error{d.P, d.Name + " needs a string literal"})
						continue
					}
					fields := strings.Fields(lit.Lit)
					switch d.Name {
					case "link":
						pkg.Links = append(pkg.Links, fields...)
					case "cflags":
						pkg.CFlags = append(pkg.CFlags, fields...)
					case "ldflags":
						pkg.LDFlags = append(pkg.LDFlags, fields...)
					}
				case "budget", "pool", "workers", "threads":
					pkg.Budgets = append(pkg.Budgets, d)
				}
			}
			for _, pr := range f.Pragmas {
				errs = append(errs, &Error{Pos{File: display, Line: 1, Col: 1}, "file pragma //squark:" + pr + " is gone: use link \"lib\", cflags \"...\" or ldflags \"...\""})
			}
			pkg.Files = append(pkg.Files, f)
		case strings.HasSuffix(n, ".c") || strings.HasSuffix(n, ".h"):
			src, err := read(n)
			if err != nil {
				return nil, err
			}
			pkg.CFiles = append(pkg.CFiles, CFile{Name: n, Src: src})
		}
	}
	if len(errs) > 0 {
		return nil, errs
	}
	if len(pkg.Files) == 0 {
		return nil, fmt.Errorf("no squark files in %s", dir)
	}
	// load imports
	seen := map[string]*Package{}
	for _, f := range pkg.Files {
		for _, imp := range f.Imports {
			if dep, ok := seen[imp.Path]; ok {
				imp.Pkg = dep
				continue
			}
			var dep *Package
			var err error
			if strings.HasPrefix(imp.Path, "./") || strings.HasPrefix(imp.Path, "../") {
				if isStd {
					return nil, fmt.Errorf("%s: relative import in standard library", imp.P)
				}
				d := filepath.Join(dir, imp.Path)
				key := l.localKey(d)
				dep, err = l.load(key, strings.TrimPrefix(key, "local:"), d, false)
			} else {
				dep, err = l.load("std/"+imp.Path, imp.Path, path.Join("std", imp.Path), true)
			}
			if err != nil {
				return nil, &Error{imp.P, err.Error()}
			}
			seen[imp.Path] = dep
			imp.Pkg = dep
			pkg.Imports = append(pkg.Imports, dep)
		}
	}
	l.pkgs[key] = pkg
	l.order = append(l.order, pkg)
	delete(l.loading, key)
	return pkg, nil
}
