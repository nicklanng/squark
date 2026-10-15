// harness runs the squark-vs-Go comparison: compute benchmarks (time, peak RSS) and the notes
// HTTP server (throughput, latency, server CPU, RSS, idle-connection memory, startup).
// It expects binaries in bench/bin (see run.sh). Results go to stdout (markdown) and
// bench/results/latest.json.
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
)

var (
	binDir   = "bin"
	scratch  = os.TempDir()
	duration = envDur("DURATION", 10*time.Second)
	conc     = envInt("CONC", 64)
	idleN    = envInt("IDLE", 5000)
	runs     = envInt("RUNS", 3)
	only     = os.Getenv("ONLY") // "compute" or "http"
	// loadgen is the load generator in bin: sq-load (bench/squark/load, the default) or load (the
	// Go one in bench/load, which also has the -startup mode)
	loadgen = envStr("LOADGEN", "sq-load")
)

func envStr(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

func envDur(k string, d time.Duration) time.Duration {
	if v := os.Getenv(k); v != "" {
		if p, err := time.ParseDuration(v); err == nil {
			return p
		}
	}
	return d
}

func envInt(k string, d int) int {
	if v := os.Getenv(k); v != "" {
		if p, err := strconv.Atoi(v); err == nil {
			return p
		}
	}
	return d
}

type ComputeResult struct {
	Bench   string  `json:"bench"`
	Impl    string  `json:"impl"`
	Seconds float64 `json:"seconds"`
	MaxRSS  int64   `json:"max_rss_bytes"`
	Output  string  `json:"output"`
}

type HTTPResult struct {
	Impl      string  `json:"impl"`
	Scenario  string  `json:"scenario"`
	RPS       float64 `json:"rps"`
	P50us     float64 `json:"p50_us"`
	P99us     float64 `json:"p99_us"`
	Errors    uint64  `json:"errors"`
	Non2xx    uint64  `json:"non2xx"`
	ServerCPU float64 `json:"server_cpu_s"`
	PeakRSS   int64   `json:"server_peak_rss_bytes"`
	Threads   int     `json:"server_peak_threads"`
}

type ServerResult struct {
	Impl       string  `json:"impl"`
	StartupMS  float64 `json:"startup_ms"`
	IdleRSS    int64   `json:"idle_rss_bytes"`
	ConnsRSS   int64   `json:"rss_with_idle_conns_bytes"`
	IdleConns  int     `json:"idle_conns"`
	PerConnKiB float64 `json:"per_conn_kib"`
}

type Report struct {
	Date     string           `json:"date"`
	Machine  string           `json:"machine"`
	Duration string           `json:"duration"`
	Conc     int              `json:"connections"`
	Compute  []ComputeResult  `json:"compute"`
	HTTP     []HTTPResult     `json:"http"`
	Servers  []ServerResult   `json:"servers"`
	Sizes    map[string]int64 `json:"binary_sizes"`
	LoadGen  string           `json:"load_generator"`
}

func main() {
	raiseFDLimit()
	rep := &Report{Date: time.Now().Format(time.RFC3339), Machine: machine(), Duration: duration.String(), Conc: conc, Sizes: map[string]int64{}, LoadGen: loadgen}
	for _, b := range []string{"notes", "gonotes", "sq-sieve", "sq-nbody", "sq-wordfreq", "sq-jsonenc", "gocompute"} {
		if st, err := os.Stat(filepath.Join(binDir, b)); err == nil {
			rep.Sizes[b] = st.Size()
		}
	}
	if only != "http" {
		computeBenchmarks(rep)
	}
	if only != "compute" {
		httpBenchmarks(rep)
	}
	os.MkdirAll("results", 0o755)
	b, _ := json.MarshalIndent(rep, "", "  ")
	os.WriteFile("results/latest.json", b, 0o644)
	printReport(rep)
}

func machine() string {
	out, _ := exec.Command("sysctl", "-n", "machdep.cpu.brand_string").Output()
	ncpu, _ := exec.Command("sysctl", "-n", "hw.ncpu").Output()
	mem, _ := exec.Command("sysctl", "-n", "hw.memsize").Output()
	m, _ := strconv.ParseInt(strings.TrimSpace(string(mem)), 10, 64)
	return fmt.Sprintf("%s, %s cores, %d GiB, %s/%s", strings.TrimSpace(string(out)), strings.TrimSpace(string(ncpu)), m>>30, runtime.GOOS, runtime.GOARCH)
}

func raiseFDLimit() {
	var rl syscall.Rlimit
	if syscall.Getrlimit(syscall.RLIMIT_NOFILE, &rl) == nil {
		want := uint64(idleN*2 + 1024)
		if want > rl.Max {
			want = rl.Max
		}
		if want > rl.Cur {
			rl.Cur = want
			if err := syscall.Setrlimit(syscall.RLIMIT_NOFILE, &rl); err != nil {
				// macOS caps the soft limit at kern.maxfilesperproc
				rl.Cur = 10240
				syscall.Setrlimit(syscall.RLIMIT_NOFILE, &rl)
			}
		}
	}
}

// ---- compute ----

var reReal = regexp.MustCompile(`([0-9.]+) real`)
var reRSS = regexp.MustCompile(`(\d+)\s+maximum resident set size`)

func timed(bin string, args ...string) (float64, int64, string, error) {
	cmd := exec.Command("/usr/bin/time", append([]string{"-l", bin}, args...)...)
	var stdout, stderr strings.Builder
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return 0, 0, "", fmt.Errorf("%s %v: %v\n%s", bin, args, err, stderr.String())
	}
	m := reReal.FindStringSubmatch(stderr.String())
	r := reRSS.FindStringSubmatch(stderr.String())
	if m == nil || r == nil {
		return 0, 0, "", fmt.Errorf("cannot parse time output: %s", stderr.String())
	}
	secs, _ := strconv.ParseFloat(m[1], 64)
	rss, _ := strconv.ParseInt(r[1], 10, 64)
	return secs, rss, strings.TrimSpace(stdout.String()), nil
}

func computeBenchmarks(rep *Report) {
	text := filepath.Join(scratch, "squark-wordfreq-100mb.txt")
	if _, err := os.Stat(text); err != nil {
		fmt.Fprintln(os.Stderr, "generating 100 MiB wordfreq input...")
		if out, err := exec.Command(filepath.Join(binDir, "gocompute"), "gen-text", text, "100").CombinedOutput(); err != nil {
			fatal("gen-text: %v %s", err, out)
		}
	}
	type bench struct {
		name   string
		goArgs []string
		sqBin  string
		sqArgs []string
	}
	benches := []bench{
		{"sieve 50M", []string{"sieve", "50000000"}, "sq-sieve", []string{"50000000"}},
		{"nbody 50M", []string{"nbody", "50000000"}, "sq-nbody", []string{"50000000"}},
		{"wordfreq 100MiB", []string{"wordfreq", text}, "sq-wordfreq", []string{text}},
		{"json encode 2M", []string{"json", "2000000"}, "sq-jsonenc", []string{"2000000"}},
	}
	for _, b := range benches {
		for _, impl := range []string{"go", "squark"} {
			bin, args := filepath.Join(binDir, "gocompute"), b.goArgs
			if impl == "squark" {
				bin, args = filepath.Join(binDir, b.sqBin), b.sqArgs
			}
			best := ComputeResult{Bench: b.name, Impl: impl, Seconds: 1e9}
			for i := 0; i < runs; i++ {
				secs, rss, out, err := timed(bin, args...)
				if err != nil {
					fatal("%v", err)
				}
				fmt.Fprintf(os.Stderr, "  %-16s %-7s run %d: %.2fs %s\n", b.name, impl, i+1, secs, human(rss))
				if secs < best.Seconds {
					best.Seconds = secs
				}
				if rss > best.MaxRSS {
					best.MaxRSS = rss
				}
				best.Output = out
			}
			rep.Compute = append(rep.Compute, best)
		}
	}
}

// ---- http ----

type server struct {
	impl string
	bin  string
	env  []string
}

func servers() []server {
	return []server{
		{"squark", filepath.Join(binDir, "notes"), nil},
		{"go", filepath.Join(binDir, "gonotes"), nil},
		{"go GOMAXPROCS=1", filepath.Join(binDir, "gonotes"), []string{"GOMAXPROCS=1"}},
	}
}

const port = 18990

func startServer(s server, db string) (*exec.Cmd, time.Duration) {
	for _, suffix := range []string{"", "-wal", "-shm"} {
		os.Remove(db + suffix)
	}
	cmd := exec.Command(s.bin, "-port", strconv.Itoa(port), "-db", db)
	cmd.Env = append(os.Environ(), s.env...)
	start := time.Now()
	if err := cmd.Start(); err != nil {
		fatal("start %s: %v", s.impl, err)
	}
	for {
		if probe() {
			break
		}
		if time.Since(start) > 10*time.Second {
			fatal("%s did not come up", s.impl)
		}
		time.Sleep(time.Millisecond)
	}
	return cmd, time.Since(start)
}

// probe sends GET /health over a fresh TCP connection and reports whether it got a 200.
func probe() bool {
	c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 100*time.Millisecond)
	if err != nil {
		return false
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(time.Second))
	c.Write([]byte("GET /health HTTP/1.1\r\nHost: x\r\nConnection: close\r\n\r\n"))
	buf := make([]byte, 64)
	n, _ := c.Read(buf)
	return strings.HasPrefix(string(buf[:n]), "HTTP/1.1 200")
}

func stopServer(cmd *exec.Cmd) {
	cmd.Process.Signal(syscall.SIGTERM)
	done := make(chan struct{})
	go func() { cmd.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		cmd.Process.Kill()
		<-done
	}
}

type procSummary struct {
	PeakRSS     int64   `json:"peak_rss_bytes"`
	CPUDeltaS   float64 `json:"cpu_delta_s"`
	PeakThreads int     `json:"peak_threads"`
}

type loadResult struct {
	RPS       float64 `json:"rps"`
	LatencyUS struct {
		P50 float64 `json:"p50"`
		P99 float64 `json:"p99"`
	} `json:"latency_us"`
	Non2xx uint64 `json:"non2xx"`
	Errors uint64 `json:"errors"`
}

func rssOf(pid int) int64 {
	out, err := exec.Command("ps", "-o", "rss=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return 0
	}
	kb, _ := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
	return kb * 1024
}

func httpBenchmarks(rep *Report) {
	scenarios := []struct {
		name string
		args []string
	}{
		{"GET /health", []string{"-url", fmt.Sprintf("http://127.0.0.1:%d/health", port)}},
		{"GET /notes/1", []string{"-url", fmt.Sprintf("http://127.0.0.1:%d/notes/1", port)}},
		{"mix 80/10/10", []string{"-url", fmt.Sprintf("http://127.0.0.1:%d/notes", port), "-mix"}},
	}
	for _, s := range servers() {
		db := filepath.Join(scratch, "squark-bench.db")
		// startup: best of 5 launches (the first launch of a new binary includes macOS's scan of it)
		startup := time.Hour
		for i := 0; i < 5; i++ {
			c, d := startServer(s, db)
			stopServer(c)
			startup = min(startup, d)
		}
		cmd, _ := startServer(s, db)
		pid := cmd.Process.Pid
		time.Sleep(200 * time.Millisecond)
		sr := ServerResult{Impl: s.impl, StartupMS: float64(startup.Microseconds()) / 1000, IdleRSS: rssOf(pid)}
		// seed one note for GET /notes/1
		resp, err := http.Post(fmt.Sprintf("http://127.0.0.1:%d/notes", port), "application/json", strings.NewReader(`{"title":"hello","body":"the quick brown fox jumps over the lazy dog"}`))
		if err != nil {
			fatal("seed: %v", err)
		}
		resp.Body.Close()
		for _, sc := range scenarios {
			statFile := filepath.Join(scratch, "procstat.json")
			ps := exec.Command(filepath.Join(binDir, "procstat"), "-pid", strconv.Itoa(pid), "-interval", "50ms", "-o", statFile)
			if err := ps.Start(); err != nil {
				fatal("procstat: %v", err)
			}
			time.Sleep(100 * time.Millisecond)
			args := append([]string{"-c", strconv.Itoa(conc), "-d", duration.String(), "-json"}, sc.args...)
			out, err := exec.Command(filepath.Join(binDir, loadgen), args...).Output()
			if err != nil {
				fatal("load %s %s: %v", s.impl, sc.name, err)
			}
			ps.Process.Signal(syscall.SIGINT)
			ps.Wait()
			var lr loadResult
			if err := json.Unmarshal(lastJSON(out), &lr); err != nil {
				fatal("load output: %v\n%s", err, out)
			}
			var pst procSummary
			b, _ := os.ReadFile(statFile)
			json.Unmarshal(b, &pst)
			r := HTTPResult{Impl: s.impl, Scenario: sc.name, RPS: lr.RPS, P50us: lr.LatencyUS.P50, P99us: lr.LatencyUS.P99,
				Errors: lr.Errors, Non2xx: lr.Non2xx, ServerCPU: pst.CPUDeltaS, PeakRSS: pst.PeakRSS, Threads: pst.PeakThreads}
			fmt.Fprintf(os.Stderr, "  %-16s %-14s %9.0f req/s  p50 %6.0fus  p99 %6.0fus  cpu %5.1fs  rss %s\n", s.impl, sc.name, r.RPS, r.P50us, r.P99us, r.ServerCPU, human(r.PeakRSS))
			rep.HTTP = append(rep.HTTP, r)
		}
		// idle keep-alive connections
		before := rssOf(pid)
		conns := openIdle(idleN)
		time.Sleep(500 * time.Millisecond)
		after := rssOf(pid)
		sr.IdleConns = len(conns)
		sr.ConnsRSS = after
		if len(conns) > 0 {
			sr.PerConnKiB = float64(after-before) / 1024 / float64(len(conns))
		}
		fmt.Fprintf(os.Stderr, "  %-16s %d idle conns: rss %s -> %s (%.1f KiB/conn), startup %.1fms\n", s.impl, len(conns), human(before), human(after), sr.PerConnKiB, sr.StartupMS)
		for _, c := range conns {
			c.Close()
		}
		rep.Servers = append(rep.Servers, sr)
		stopServer(cmd)
	}
}

// openIdle opens n keep-alive connections, each having completed one request.
func openIdle(n int) []net.Conn {
	var conns []net.Conn
	req := []byte("GET /health HTTP/1.1\r\nHost: x\r\n\r\n")
	for i := 0; i < n; i++ {
		c, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", port))
		if err != nil {
			fmt.Fprintf(os.Stderr, "  idle conns: stopped at %d: %v\n", i, err)
			break
		}
		c.Write(req)
		br := bufio.NewReader(c)
		resp, err := http.ReadResponse(br, nil)
		if err != nil {
			c.Close()
			fmt.Fprintf(os.Stderr, "  idle conns: stopped at %d: %v\n", i, err)
			break
		}
		resp.Body.Close()
		conns = append(conns, c)
	}
	return conns
}

func lastJSON(b []byte) []byte {
	s := strings.TrimSpace(string(b))
	if i := strings.LastIndex(s, "\n{"); i >= 0 {
		s = s[i+1:]
	}
	return []byte(s)
}

// ---- report ----

func human(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.2f GiB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.0f KiB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}

func printReport(rep *Report) {
	fmt.Printf("# squark vs Go\n\n%s, %s\nHTTP: %d keep-alive connections, %s per scenario; load generator (%s) on the same machine.\n\n", rep.Machine, rep.Date, rep.Conc, rep.Duration, rep.LoadGen)
	if len(rep.Compute) > 0 {
		fmt.Printf("## Compute (best of %d)\n\n| benchmark | Go time | squark time | Go peak RSS | squark peak RSS |\n|---|---:|---:|---:|---:|\n", runs)
		for i := 0; i+1 < len(rep.Compute); i += 2 {
			g, s := rep.Compute[i], rep.Compute[i+1]
			same := ""
			if g.Output != s.Output {
				same = " ⚠ outputs differ"
			}
			fmt.Printf("| %s%s | %.2fs | %.2fs | %s | %s |\n", g.Bench, same, g.Seconds, s.Seconds, human(g.MaxRSS), human(s.MaxRSS))
		}
		fmt.Println()
	}
	if len(rep.HTTP) > 0 {
		fmt.Printf("## Notes server\n\n| server | scenario | req/s | p50 | p99 | server CPU | peak RSS | threads |\n|---|---|---:|---:|---:|---:|---:|---:|\n")
		for _, r := range rep.HTTP {
			fmt.Printf("| %s | %s | %.0f | %.0fµs | %.0fµs | %.1fs | %s | %d |\n", r.Impl, r.Scenario, r.RPS, r.P50us, r.P99us, r.ServerCPU, human(r.PeakRSS), r.Threads)
		}
		fmt.Printf("\n| server | startup | idle RSS | RSS with %d idle conns | per conn |\n|---|---:|---:|---:|---:|\n", idleN)
		for _, s := range rep.Servers {
			fmt.Printf("| %s | %.1fms | %s | %s | %.1f KiB |\n", s.Impl, s.StartupMS, human(s.IdleRSS), human(s.ConnsRSS), s.PerConnKiB)
		}
		fmt.Println()
	}
	fmt.Printf("## Binary sizes\n\n| binary | size |\n|---|---:|\n")
	for _, k := range []string{"notes", "gonotes", "sq-sieve", "gocompute"} {
		if v, ok := rep.Sizes[k]; ok {
			fmt.Printf("| %s | %s |\n", k, human(v))
		}
	}
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "harness: "+format+"\n", args...)
	os.Exit(1)
}
