package driver

import (
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestWorstSaturates: a worst case too large for int64 saturates instead of wrapping (a wrapped
// product once made 2^52 goroutines × 16 KiB report 0 B and pass a 64M budget).
func TestWorstSaturates(t *testing.T) {
	m := &MemoryReport{Budget: 64 << 20, Workers: 1024, Threads: 1024, Static: 1 << 40,
		Pools: []PoolReport{{Name: "t", Count: 1 << 24, Touchable: 1 << 40}}}
	if w := m.Worst(); w != math.MaxInt64 {
		t.Errorf("Worst() = %d, want MaxInt64", w)
	}
	small := &MemoryReport{Code: 100, Static: 10, Pools: []PoolReport{{Count: 2, Touchable: 5}}}
	if w := small.Worst(); w != 120 {
		t.Errorf("Worst() = %d, want 120", w)
	}
}

// TestCountedMemory: connections N with sockbuf N and heap LIB N enter the worst case (once per
// worker process), the report shows them, and a program's heap overrides a library's default.
func TestCountedMemory(t *testing.T) {
	m := &MemoryReport{Code: 100, Workers: 2, Threads: 1, SockBuf: 16 << 10, Conns: 10, UsesNet: true,
		CLibs: []string{"sqlite3"}, Heaps: map[string]int64{"sqlite3": 1 << 20}}
	want := int64(100) + 2*(10*(4*16<<10+4<<10)+1<<20)
	if w := m.Worst(); w != want {
		t.Errorf("Worst() = %d, want %d", w, want)
	}
	if !m.AllCounted() {
		t.Error("everything is declared, but AllCounted is false")
	}
	report := FormatMemory(m)
	for _, s := range []string{"sockets", "at most 10 sockets (sockbuf 16.0 KiB), per worker", "C library sqlite3", "its heap limit"} {
		if !strings.Contains(report, s) {
			t.Errorf("report lacks %q:\n%s", s, report)
		}
	}
	m.SockBuf = 0
	if m.AllCounted() || !strings.Contains(FormatMemory(m), "declare connections N and sockbuf N") {
		t.Errorf("undeclared sockets: AllCounted %v, report\n%s", m.AllCounted(), FormatMemory(m))
	}

	dir := t.TempDir()
	src := "program {\n\theap sqlite3 32M\n\tconnections 100\n\tsockbuf 8K\n}\n\nuse sqlite\n\nfn main() { _ = sqlite.ok }\n"
	os.WriteFile(filepath.Join(dir, "main.sq"), []byte(src), 0o644)
	res, err := Build(Options{Dir: dir, Output: filepath.Join(dir, "p")})
	if err != nil {
		t.Fatal(err)
	}
	if h := res.Memory.Heaps["sqlite3"]; h != 32<<20 {
		t.Errorf("heap sqlite3 = %d, want the program's 32M over the package's 16M", h)
	}
	if res.Memory.Conns != 100 || res.Memory.SockBuf != 8<<10 {
		t.Errorf("connections %d, sockbuf %d", res.Memory.Conns, res.Memory.SockBuf)
	}
}
