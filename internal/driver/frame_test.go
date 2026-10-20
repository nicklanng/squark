package driver

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
)

// TestLargeFrames (Q5): a large `with arena` is not probed page by page on entry, so only the
// pages it uses become resident; a frame of 4 GiB or more is a build error, since clang on arm64
// lays out a frame of exactly 16 GiB wrongly.
func TestLargeFrames(t *testing.T) {
	const prog = "program\n\n//squark:noinline\nfn fill(a arena, n int) -> int { s := make(int[], n) in a; s[n-1] = 1; return s[n-1] }\n" +
		"//squark:noinline\nfn run() -> int { with arena x SIZE { return fill(x, 1000) } }\nfn main() { println(run()) }\n"
	build := func(size string) (string, error) {
		dir := t.TempDir()
		os.WriteFile(filepath.Join(dir, "main.sq"), []byte(strings.Replace(prog, "SIZE", size, 1)), 0o644)
		bin := filepath.Join(dir, "p")
		_, err := Build(Options{Dir: dir, Output: bin})
		return bin, err
	}

	bin, err := build("1G")
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bin)
	out, err := cmd.CombinedOutput()
	if err != nil || string(out) != "1\n" {
		t.Fatalf("1G arena: %v, output %q", err, out)
	}
	rss := cmd.ProcessState.SysUsage().(*syscall.Rusage).Maxrss
	if runtime.GOOS == "linux" {
		rss <<= 10 // KiB on Linux, bytes on macOS
	}
	if rss > 64<<20 {
		t.Errorf("1G arena, 8 KB used: max RSS %d bytes; the whole arena was made resident", rss)
	}

	if _, err := build("16G"); err == nil || !strings.Contains(err.Error(), "must be under 4 GiB") {
		t.Errorf("16G arena: got %v, want a frame-size error", err)
	}
}

// TestLargePackageArena (Q6): package variables are mapped at start-up rather than put in the
// binary's BSS, where a multi-GiB arena collided with the dyld shared region on macOS; the
// memory report still counts them.
func TestLargePackageArena(t *testing.T) {
	size, bytes := "3G", int64(3<<30)
	if runtime.GOOS != "darwin" {
		size, bytes = "256M", 256<<20 // the collision is macOS's; keep the mapping small elsewhere
	}
	dir := t.TempDir()
	src := "program\n\narena big " + size + "\n\nfn main() { s := make(int[], 1000) in big; s[999] = 7; println(s[999]) }\n"
	os.WriteFile(filepath.Join(dir, "main.sq"), []byte(src), 0o644)
	bin := filepath.Join(dir, "p")
	res, err := Build(Options{Dir: dir, Output: bin})
	if err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(bin).CombinedOutput(); err != nil || string(out) != "7\n" {
		t.Fatalf("arena big %s: %v, output %q", size, err, out)
	}
	if res.Memory.Static < bytes {
		t.Errorf("static data %d bytes, less than the %s arena", res.Memory.Static, size)
	}
}

// TestStackProofGuards: prog.c isn't probed, so a short proof must still fault rather than write
// into the next stack. readStackUsage keeps frames whose source path contains a tab (it dropped
// them, proving 9 KB for a 1.6 MB frame), and each pool's guard is as large as its largest frame.
func TestStackProofGuards(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "tab\tdir")
	os.MkdirAll(dir, 0o755)
	src := "program\n\nthread done int chan[1]\n\n//squark:noinline\nfn deep(n int) -> int { var t int[200000]; for i in 0..200000 { t[i] = i * n }; return t[n] }\n\n" +
		"task worker(n int) pool 3 { done <- deep(n) }\n\nfn main() { start worker(5); println(<-done) }\n"
	os.WriteFile(filepath.Join(dir, "main.sq"), []byte(src), 0o644)
	work := t.TempDir()
	bin := filepath.Join(dir, "p")
	res, err := Build(Options{Dir: dir, Output: bin, WorkDir: work})
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range res.Pools {
		if p.Name == "main.worker" && p.Stack < 1600000 {
			t.Errorf("main.worker proven at %d bytes, less than deep's 1.6 MB frame", p.Stack)
		}
	}
	if out, err := exec.Command(bin).CombinedOutput(); err != nil || string(out) != "25\n" {
		t.Errorf("run: %v, output %q", err, out)
	}
	pools, err := os.ReadFile(filepath.Join(work, "pools.c"))
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(pools), "\n") {
		if !strings.Contains(line, `.name = "main.worker"`) {
			continue
		}
		var guard int64
		i := strings.Index(line, ".guard_size = ")
		if i < 0 {
			t.Fatalf("no guard_size: %s", line)
		}
		fmt.Sscanf(line[i+len(".guard_size = "):], "%d", &guard)
		if guard < 1600000 {
			t.Errorf("main.worker's guard is %d bytes, smaller than its 1.6 MB frame", guard)
		}
	}
}
