package driver

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestCPUFlags(t *testing.T) {
	for _, c := range []struct {
		cpu, arch, want string // want "error" for a rejected CPU
	}{
		{"", "amd64", ""},
		{"baseline", "arm64", ""},
		{"native", "arm64", "-mcpu=native"},
		{"native", "amd64", `-march=native -DSQ_CPU="native"`},
		{"x86-64-v3", "amd64", `-march=x86-64-v3 -DSQ_CPU="x86-64-v3"`},
		{"x86-64-v3", "arm64", "error"},
		{"armv8.2-a", "arm64", "-march=armv8.2-a"},
		{"armv9-a", "arm64", "-march=armv9-a"},
		{"armv8.2-a", "amd64", "error"},
		{"avx2", "amd64", "error"},
	} {
		flags, err := cpuFlags(c.cpu, c.arch)
		got := strings.Join(flags, " ")
		if err != nil {
			got = "error"
		}
		if got != c.want {
			t.Errorf("cpuFlags(%q, %q) = %q, want %q", c.cpu, c.arch, got, c.want)
		}
	}
}

// TestCPU builds a program for the CPU running the tests and, on x86-64, for x86-64-v2 and v3:
// a binary for a CPU this one isn't must stop at start with a message, not crash.
func TestCPU(t *testing.T) {
	cpus := []string{"native"}
	if runtime.GOARCH == "amd64" {
		cpus = append(cpus, "x86-64-v2", "x86-64-v3")
	}
	info, _ := os.ReadFile("/proc/cpuinfo")
	has := func(flag string) bool {
		if runtime.GOOS != "linux" {
			return true // macOS on x86-64: assume a recent CPU
		}
		for _, line := range strings.Split(string(info), "\n") {
			if strings.HasPrefix(line, "flags") {
				return strings.Contains(line+" ", " "+flag+" ")
			}
		}
		return false
	}
	for _, cpu := range cpus {
		t.Run(cpu, func(t *testing.T) {
			bin := filepath.Join(t.TempDir(), "vec")
			if _, err := Build(Options{Dir: "../../testdata/vectorize", Output: bin, CPU: cpu}); err != nil {
				t.Fatal(err)
			}
			out, err := exec.Command(bin).CombinedOutput()
			runs := cpu == "native" || (cpu == "x86-64-v2" && has("sse4_2")) || (cpu == "x86-64-v3" && has("avx2") && has("fma"))
			if runs {
				if err != nil || string(out) != "0 0 64 64\n" {
					t.Fatalf("got %q, %v", out, err)
				}
				return
			}
			if ee, ok := err.(*exec.ExitError); !ok || ee.ExitCode() != 2 || !strings.Contains(string(out), "this program was built for x86-64 CPUs with") {
				t.Fatalf("want the CPU check to stop it (exit 2), got %v: %q", err, out)
			}
			t.Logf("stopped as expected: %s", out)
		})
	}
}
