// load is a small wrk replacement: N keep-alive connections, each driven by
// a goroutine running a hand-rolled HTTP/1.1 request/response loop.
//
//	load -url http://127.0.0.1:8080/notes -c 64 -d 10s [-method GET] [-body '{...}'] [-mix] [-json]
//	load -startup 10 -url http://127.0.0.1:8080/health [-json] -- ./server args...
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"math/rand/v2"
	"net/url"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

type target struct {
	addr string // host:port for dialing
	host string // Host header
	path string // path + query
}

func parseTarget(raw string) (target, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return target{}, err
	}
	if u.Scheme != "http" {
		return target{}, fmt.Errorf("only http:// URLs are supported")
	}
	addr := u.Host
	if u.Port() == "" {
		addr += ":80"
	}
	path := u.RequestURI()
	return target{addr: addr, host: u.Host, path: path}, nil
}

type stats struct {
	h        hist
	requests uint64
	non2xx   uint64
	errors   uint64
	bytesIn  uint64
	status   [600]uint64
}

// picker returns the request bytes to send next.
type picker func(rng *rand.Rand) []byte

func worker(id int, addr string, pick picker, stop *atomic.Bool, st *stats, wg *sync.WaitGroup) {
	defer wg.Done()
	rng := rand.New(rand.NewPCG(uint64(id)+1, 0x5eed))
	var c *conn
	for !stop.Load() {
		if c == nil || c.c == nil {
			var err error
			if c, err = dial(addr); err != nil {
				st.errors++
				c = nil
				time.Sleep(10 * time.Millisecond)
				continue
			}
		}
		req := pick(rng)
		t0 := time.Now()
		status, closeConn, err := c.do(req, false)
		lat := time.Since(t0)
		if err != nil {
			st.errors++
			c.close()
			continue
		}
		st.requests++
		st.h.record(lat)
		if status < 600 {
			st.status[status]++
		}
		if status < 200 || status > 299 {
			st.non2xx++
		}
		if closeConn {
			c.close()
		}
	}
	if c != nil {
		c.close()
	}
}

// seed POSTs n notes over one connection and returns the created ids.
func seed(t target, n int, body []byte) ([]int64, error) {
	c, err := dial(t.addr)
	if err != nil {
		return nil, err
	}
	defer c.close()
	req := buildRequest("POST", t.host, "/notes", body)
	ids := make([]int64, 0, n)
	for i := 0; i < n; i++ {
		if c.c == nil {
			if c, err = dial(t.addr); err != nil {
				return nil, err
			}
		}
		status, closeConn, err := c.do(req, true)
		if err != nil {
			return nil, err
		}
		if status != 201 {
			return nil, fmt.Errorf("seed POST returned %d: %s", status, c.body)
		}
		var out struct {
			ID int64 `json:"id"`
		}
		if err := json.Unmarshal(c.body, &out); err != nil || out.ID == 0 {
			return nil, fmt.Errorf("seed POST: cannot parse id from %q", c.body)
		}
		ids = append(ids, out.ID)
		if closeConn {
			c.close()
		}
	}
	return ids, nil
}

type latencyJSON struct {
	Min  float64 `json:"min"`
	Mean float64 `json:"mean"`
	P50  float64 `json:"p50"`
	P90  float64 `json:"p90"`
	P99  float64 `json:"p99"`
	Max  float64 `json:"max"`
}

type result struct {
	URL         string            `json:"url"`
	Mode        string            `json:"mode"`
	Connections int               `json:"connections"`
	DurationS   float64           `json:"duration_s"`
	Requests    uint64            `json:"requests"`
	RPS         float64           `json:"rps"`
	LatencyUS   latencyJSON       `json:"latency_us"`
	Non2xx      uint64            `json:"non2xx"`
	Errors      uint64            `json:"errors"`
	Status      map[string]uint64 `json:"status"`
	Seeded      int               `json:"seeded,omitempty"`
}

func us(d time.Duration) float64 { return float64(d.Nanoseconds()) / 1e3 }

func fmtDur(d time.Duration) string {
	switch {
	case d < time.Millisecond:
		return fmt.Sprintf("%.1fus", us(d))
	case d < time.Second:
		return fmt.Sprintf("%.2fms", float64(d.Nanoseconds())/1e6)
	default:
		return fmt.Sprintf("%.2fs", d.Seconds())
	}
}

func main() {
	rawURL := flag.String("url", "http://127.0.0.1:8080/notes", "target URL (in -mix mode only scheme/host/port are used)")
	conns := flag.Int("c", 64, "concurrent keep-alive connections")
	dur := flag.Duration("d", 10*time.Second, "test duration")
	method := flag.String("method", "GET", "HTTP method")
	body := flag.String("body", "", "request body (sent with Content-Type: application/json); in -mix mode the POST body")
	mix := flag.Bool("mix", false, "mixed notes workload: 80% GET /notes/{id}, 10% GET /notes, 10% POST /notes")
	seedN := flag.Int("seed", 1000, "notes to POST before a -mix run")
	asJSON := flag.Bool("json", false, "print results as one JSON object")
	startup := flag.Int("startup", 0, "startup mode: launch the command after -- this many times, report time until -url answers 2xx")
	flag.Parse()

	t, err := parseTarget(*rawURL)
	if err != nil {
		fatal(err)
	}
	if *startup > 0 {
		runStartup(t, *startup, flag.Args(), *asJSON)
		return
	}
	if *conns < 1 {
		fatal(fmt.Errorf("-c must be >= 1"))
	}

	var pick picker
	mode := *method + " " + t.path
	seeded := 0
	if *mix {
		postBody := []byte(*body)
		if *body == "" {
			postBody = []byte(`{"title":"x","body":"y"}`)
		}
		ids, err := seed(t, *seedN, postBody)
		if err != nil {
			fatal(fmt.Errorf("seeding: %w", err))
		}
		if len(ids) == 0 {
			fatal(fmt.Errorf("-mix needs -seed > 0"))
		}
		seeded = len(ids)
		gets := make([][]byte, len(ids))
		for i, id := range ids {
			gets[i] = buildRequest("GET", t.host, "/notes/"+strconv.FormatInt(id, 10), nil)
		}
		list := buildRequest("GET", t.host, "/notes", nil)
		post := buildRequest("POST", t.host, "/notes", postBody)
		pick = func(rng *rand.Rand) []byte {
			switch r := rng.IntN(100); {
			case r < 80:
				return gets[rng.IntN(len(gets))]
			case r < 90:
				return list
			default:
				return post
			}
		}
		mode = "mix"
	} else {
		var b []byte
		if *body != "" {
			b = []byte(*body)
		}
		req := buildRequest(*method, t.host, t.path, b)
		pick = func(*rand.Rand) []byte { return req }
	}

	all := make([]*stats, *conns)
	var stop atomic.Bool
	var wg sync.WaitGroup
	start := time.Now()
	timer := time.AfterFunc(*dur, func() { stop.Store(true) })
	for i := range all {
		all[i] = new(stats)
		wg.Add(1)
		go worker(i, t.addr, pick, &stop, all[i], &wg)
	}
	wg.Wait()
	timer.Stop()
	elapsed := time.Since(start)

	var tot stats
	for _, s := range all {
		tot.h.merge(&s.h)
		tot.requests += s.requests
		tot.non2xx += s.non2xx
		tot.errors += s.errors
		for code, n := range s.status {
			tot.status[code] += n
		}
	}
	res := result{
		URL:         *rawURL,
		Mode:        mode,
		Connections: *conns,
		DurationS:   elapsed.Seconds(),
		Requests:    tot.requests,
		RPS:         float64(tot.requests) / elapsed.Seconds(),
		LatencyUS: latencyJSON{
			Min:  us(time.Duration(tot.h.min)),
			Mean: us(tot.h.mean()),
			P50:  us(tot.h.quantile(0.50)),
			P90:  us(tot.h.quantile(0.90)),
			P99:  us(tot.h.quantile(0.99)),
			Max:  us(time.Duration(tot.h.max)),
		},
		Non2xx: tot.non2xx,
		Errors: tot.errors,
		Status: map[string]uint64{},
		Seeded: seeded,
	}
	for code, n := range tot.status {
		if n > 0 {
			res.Status[strconv.Itoa(code)] = n
		}
	}
	if *asJSON {
		out, _ := json.Marshal(res)
		fmt.Println(string(out))
		return
	}
	fmt.Printf("target:   %s  mode: %s  conns: %d  duration: %.2fs\n", *rawURL, mode, *conns, elapsed.Seconds())
	if seeded > 0 {
		fmt.Printf("seeded:   %d notes\n", seeded)
	}
	fmt.Printf("requests: %d  (%.0f req/s)\n", tot.requests, res.RPS)
	fmt.Printf("latency:  p50 %s  p90 %s  p99 %s  max %s  mean %s\n",
		fmtDur(tot.h.quantile(0.50)), fmtDur(tot.h.quantile(0.90)), fmtDur(tot.h.quantile(0.99)),
		fmtDur(time.Duration(tot.h.max)), fmtDur(tot.h.mean()))
	codes := make([]string, 0, len(res.Status))
	for k := range res.Status {
		codes = append(codes, k)
	}
	slices.Sort(codes)
	var sb bytes.Buffer
	for _, k := range codes {
		fmt.Fprintf(&sb, " %s=%d", k, res.Status[k])
	}
	fmt.Printf("status:  %s\n", sb.String())
	fmt.Printf("non-2xx:  %d  errors: %d\n", tot.non2xx, tot.errors)
}

// runStartup launches argv n times and measures wall time from exec until
// the target URL first answers with a 2xx status, then stops the process.
func runStartup(t target, n int, argv []string, asJSON bool) {
	if len(argv) == 0 {
		fatal(fmt.Errorf("-startup needs a command after --"))
	}
	req := buildRequest("GET", t.host, t.path, nil)
	times := make([]time.Duration, 0, n)
	for i := 0; i < n; i++ {
		cmd := exec.Command(argv[0], argv[1:]...)
		cmd.Stderr = nil
		cmd.Stdout = nil
		t0 := time.Now()
		if err := cmd.Start(); err != nil {
			fatal(err)
		}
		exited := make(chan struct{})
		go func() { cmd.Wait(); close(exited) }()
		ready := false
		deadline := t0.Add(10 * time.Second)
		for !ready && time.Now().Before(deadline) {
			select {
			case <-exited:
				fatal(fmt.Errorf("%s exited before becoming ready", argv[0]))
			default:
			}
			c, err := dial(t.addr)
			if err == nil {
				status, _, err := c.do(req, false)
				c.close()
				if err == nil && status >= 200 && status < 300 {
					ready = true
					break
				}
			}
			time.Sleep(200 * time.Microsecond)
		}
		el := time.Since(t0)
		cmd.Process.Signal(syscall.SIGTERM)
		select {
		case <-exited:
		case <-time.After(3 * time.Second):
			cmd.Process.Kill()
			<-exited
		}
		if !ready {
			fatal(fmt.Errorf("%s did not become ready within 10s", argv[0]))
		}
		times = append(times, el)
	}
	slices.Sort(times)
	med := times[len(times)/2]
	if asJSON {
		out, _ := json.Marshal(map[string]any{
			"runs": n, "min_ms": ms(times[0]), "median_ms": ms(med), "max_ms": ms(times[len(times)-1]),
		})
		fmt.Println(string(out))
		return
	}
	fmt.Printf("startup (exec -> first 2xx on %s), %d runs: min %s  median %s  max %s\n",
		t.path, n, fmtDur(times[0]), fmtDur(med), fmtDur(times[len(times)-1]))
}

func ms(d time.Duration) float64 { return float64(d.Nanoseconds()) / 1e6 }

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "load:", err)
	os.Exit(1)
}
