package driver

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestManifest: squark build -manifest prints resource settings from the program: the worst case
// plus headroom as the memory limit, its scheduler threads as CPUs, and a stop timeout past its
// shutdown grace period.
func TestManifest(t *testing.T) {
	dir := t.TempDir()
	src := "program {\n\tthreads 2\n\tshutdown 20s\n}\n\nfn main() {}\n"
	if err := os.WriteFile(filepath.Join(dir, "main.sq"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := Build(Options{Dir: dir, Output: filepath.Join(dir, "svc")})
	if err != nil {
		t.Fatal(err)
	}
	if res.Shutdown != 20e9 {
		t.Errorf("shutdown %d, want 20s", res.Shutdown)
	}
	k8s, err := Manifest(res, "svc", "k8s")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"cpu: \"2\"", "memory: \"", "terminationGracePeriodSeconds: 25", "Guaranteed"} {
		if !strings.Contains(k8s, want) {
			t.Errorf("k8s manifest lacks %q:\n%s", want, k8s)
		}
	}
	sd, err := Manifest(res, "svc", "systemd")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"MemoryMax=", "CPUQuota=200%", "TimeoutStopSec=25"} {
		if !strings.Contains(sd, want) {
			t.Errorf("systemd manifest lacks %q:\n%s", want, sd)
		}
	}
	if _, err := Manifest(res, "svc", "nomad"); err == nil {
		t.Error("an unknown format was accepted")
	}
	// threads auto: the worst case depends on the machine
	os.WriteFile(filepath.Join(dir, "main.sq"), []byte("program {\n\tthreads auto\n}\n\nfn main() {}\n"), 0o644)
	res, err = Build(Options{Dir: dir, Output: filepath.Join(dir, "svc")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Manifest(res, "svc", "k8s"); err == nil || !strings.Contains(err.Error(), "threads N") {
		t.Errorf("threads auto: %v, want an error naming threads N", err)
	}
}
