package driver

import (
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// TestThreadsAutoLimits: threads auto takes as many shards as the CPUs the process may use, which on
// Linux is limited by its affinity (taskset) and by its cgroup's CPU quota (a container's --cpus,
// a pod's limits.cpu), not the machine's core count. (scripts/test-linux.sh runs this without a
// quota; ROADMAP.md 1.2 has the docker runs with --cpus and --cpuset-cpus.)
func TestThreadsAutoLimits(t *testing.T) {
	bin := buildSource(t, `program {
	threads auto
}

use runtime

fn main() {
	if runtime.shard() == 0 {
		println(runtime.shards())
	}
}
`)
	shards := func(name string, args ...string) int {
		t.Helper()
		cmd := exec.Command(name, args...)
		cmd.Env = append(os.Environ(), "SQUARK_THREADS=")
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("%s %v: %v", name, args, err)
		}
		n, err := strconv.Atoi(strings.TrimSpace(string(out)))
		if err != nil {
			t.Fatalf("output %q", out)
		}
		return n
	}
	want := runtime.NumCPU() // the affinity mask's CPUs on Linux
	if runtime.GOOS == "linux" {
		// the container's own cgroup (a cgroup namespace, as Docker and Kubernetes give one)
		if b, err := os.ReadFile("/sys/fs/cgroup/cpu.max"); err == nil {
			f := strings.Fields(string(b))
			if len(f) == 2 && f[0] != "max" {
				q, _ := strconv.Atoi(f[0])
				p, _ := strconv.Atoi(f[1])
				if n := (q + p - 1) / p; n < want {
					want = max(n, 1)
				}
			}
		}
	}
	if got := shards(bin); got != want {
		t.Errorf("threads auto: %d shards, want %d", got, want)
	}
	if runtime.GOOS != "linux" {
		return
	}
	if _, err := exec.LookPath("taskset"); err != nil {
		t.Log("no taskset: affinity not checked")
		return
	}
	if got := shards("taskset", "-c", "0", bin); got != 1 {
		t.Errorf("threads auto under taskset -c 0: %d shards, want 1", got)
	}
}
