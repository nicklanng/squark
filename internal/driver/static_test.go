package driver

import (
	"debug/elf"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestStatic: -static links a binary with no dynamic loader and no shared libraries, SQLite
// included, which runs on its own; on macOS it is an error.
func TestStatic(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "prog")
	_, err := Build(Options{Dir: "../../examples/count", Output: bin, Static: true})
	if runtime.GOOS != "linux" {
		if err == nil || !strings.Contains(err.Error(), "for Linux") {
			t.Errorf("on %s: %v, want an error", runtime.GOOS, err)
		}
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	f, err := elf.Open(bin)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	for _, p := range f.Progs {
		if p.Type == elf.PT_INTERP {
			t.Error("the binary names a dynamic loader")
		}
	}
	if libs, _ := f.ImportedLibraries(); len(libs) > 0 {
		t.Errorf("shared libraries: %v", libs)
	}
	// it runs: hello needs nothing but the kernel
	hello := filepath.Join(dir, "hello")
	if _, err := Build(Options{Dir: "../../examples/hello", Output: hello, Static: true}); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(hello).CombinedOutput()
	if err != nil || !strings.Contains(string(out), "hello") {
		t.Errorf("static hello: %v\n%s", err, out)
	}
}
