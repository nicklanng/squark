// procstat samples RSS and cumulative CPU time of a process on macOS.
//
//	procstat -pid N [-interval 100ms] [-o out.json] [-json]
//	procstat [-interval 100ms] [-o out.json] [-json] -- cmd args...
//
// With -pid it watches until the process exits or procstat gets SIGINT/SIGTERM.
// With a command it launches it (stdio inherited), forwards SIGINT/SIGTERM to
// it, and watches until it exits. A text summary goes to stderr.
package main

/*
#include <libproc.h>
#include <sys/proc_info.h>
#include <sys/resource.h>
#include <mach/mach_time.h>
#include <stdint.h>

typedef struct {
	uint64_t rss;        // pti_resident_size, bytes
	uint64_t footprint;  // ri_phys_footprint, bytes (what Activity Monitor shows as Memory)
	uint64_t user_ns;
	uint64_t sys_ns;
	int32_t  threads;
} sample_t;

static uint32_t tb_numer, tb_denom;

static uint64_t abs_to_ns(uint64_t t) {
	if (tb_denom == 0) {
		mach_timebase_info_data_t tb;
		mach_timebase_info(&tb);
		tb_numer = tb.numer;
		tb_denom = tb.denom;
	}
	// Times in proc_taskinfo / rusage_info are mach absolute time units
	// (on Apple Silicon numer/denom = 125/3, i.e. 41.67ns per tick).
	return (uint64_t)((__uint128_t)t * tb_numer / tb_denom);
}

static int take_sample(int pid, sample_t *out) {
	struct proc_taskinfo ti;
	int n = proc_pidinfo(pid, PROC_PIDTASKINFO, 0, &ti, sizeof ti);
	if (n != (int)sizeof ti) return -1;
	out->rss = ti.pti_resident_size;
	out->user_ns = abs_to_ns(ti.pti_total_user);
	out->sys_ns = abs_to_ns(ti.pti_total_system);
	out->threads = ti.pti_threadnum;
	struct rusage_info_v2 ri;
	if (proc_pid_rusage(pid, RUSAGE_INFO_V2, (rusage_info_t *)&ri) == 0) {
		out->footprint = ri.ri_phys_footprint;
	} else {
		out->footprint = 0;
	}
	return 0;
}
*/
import "C"

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

type Sample struct {
	T         float64 `json:"t_s"`
	RSS       uint64  `json:"rss_bytes"`
	Footprint uint64  `json:"footprint_bytes"`
	CPU       float64 `json:"cpu_s"`
	Threads   int     `json:"threads"`
}

type Summary struct {
	PID            int      `json:"pid"`
	Command        string   `json:"command,omitempty"`
	Samples        int      `json:"samples"`
	DurationS      float64  `json:"duration_s"`
	PeakRSS        uint64   `json:"peak_rss_bytes"`
	FinalRSS       uint64   `json:"final_rss_bytes"`
	PeakFootprint  uint64   `json:"peak_footprint_bytes"`
	FinalFootprint uint64   `json:"final_footprint_bytes"`
	PeakThreads    int      `json:"peak_threads"`
	CPUUserS       float64  `json:"cpu_user_s"`
	CPUSysS        float64  `json:"cpu_sys_s"`
	CPUS           float64  `json:"cpu_s"`       // cumulative user+sys at the end
	CPUDeltaS      float64  `json:"cpu_delta_s"` // CPU used while being watched
	AvgCPUPct      float64  `json:"avg_cpu_pct"`
	ExitCode       *int     `json:"exit_code,omitempty"`
	RusageMaxRSS   uint64   `json:"rusage_maxrss_bytes,omitempty"`
	Series         []Sample `json:"series,omitempty"`
}

func sample(pid int) (C.sample_t, error) {
	var s C.sample_t
	if C.take_sample(C.int(pid), &s) != 0 {
		return s, errors.New("process gone")
	}
	return s, nil
}

func main() {
	pid := flag.Int("pid", 0, "pid to watch")
	interval := flag.Duration("interval", 100*time.Millisecond, "sampling interval")
	outPath := flag.String("o", "", "write JSON summary + series to this file")
	asJSON := flag.Bool("json", false, "print the summary (without series) as JSON on stdout")
	flag.Parse()

	argv := flag.Args()
	if (*pid == 0) == (len(argv) == 0) {
		fmt.Fprintln(os.Stderr, "usage: procstat -pid N [-interval 100ms] [-o out.json] | procstat [flags] -- cmd args...")
		os.Exit(2)
	}

	sigs := make(chan os.Signal, 2)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)

	var cmd *exec.Cmd
	exited := make(chan struct{})
	if len(argv) > 0 {
		cmd = exec.Command(argv[0], argv[1:]...)
		cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
		if err := cmd.Start(); err != nil {
			fmt.Fprintln(os.Stderr, "procstat:", err)
			os.Exit(1)
		}
		*pid = cmd.Process.Pid
		go func() { cmd.Wait(); close(exited) }()
	}

	start := time.Now()
	var series []Sample
	var first, last C.sample_t
	have := false
	record := func() bool {
		s, err := sample(*pid)
		if err != nil {
			return false
		}
		if !have {
			first, have = s, true
		}
		last = s
		series = append(series, Sample{
			T:         time.Since(start).Seconds(),
			RSS:       uint64(s.rss),
			Footprint: uint64(s.footprint),
			CPU:       float64(s.user_ns+s.sys_ns) / 1e9,
			Threads:   int(s.threads),
		})
		return true
	}

	if !record() && cmd == nil {
		fmt.Fprintf(os.Stderr, "procstat: cannot read pid %d\n", *pid)
		os.Exit(1)
	}
	tick := time.NewTicker(*interval)
loop:
	for {
		select {
		case <-tick.C:
			if !record() && cmd == nil {
				break loop // watched process is gone
			}
		case sig := <-sigs:
			if cmd == nil {
				record()
				break loop
			}
			cmd.Process.Signal(sig) // keep sampling until the child exits
		case <-exited:
			break loop
		}
	}
	tick.Stop()
	end := time.Since(start)

	sum := Summary{PID: *pid, Samples: len(series), DurationS: end.Seconds(), Series: series}
	if cmd != nil {
		sum.Command = strings.Join(argv, " ")
	}
	for _, s := range series {
		sum.PeakRSS = max(sum.PeakRSS, s.RSS)
		sum.PeakFootprint = max(sum.PeakFootprint, s.Footprint)
		sum.PeakThreads = max(sum.PeakThreads, s.Threads)
	}
	if have {
		sum.FinalRSS = uint64(last.rss)
		sum.FinalFootprint = uint64(last.footprint)
		sum.CPUUserS = float64(last.user_ns) / 1e9
		sum.CPUSysS = float64(last.sys_ns) / 1e9
		sum.CPUS = sum.CPUUserS + sum.CPUSysS
		sum.CPUDeltaS = float64((last.user_ns+last.sys_ns)-(first.user_ns+first.sys_ns)) / 1e9
	}
	if cmd != nil && cmd.ProcessState != nil {
		// The child is reaped: wait4 rusage gives exact totals.
		code := cmd.ProcessState.ExitCode()
		sum.ExitCode = &code
		if ru, ok := cmd.ProcessState.SysUsage().(*syscall.Rusage); ok {
			sum.CPUUserS = tv(ru.Utime)
			sum.CPUSysS = tv(ru.Stime)
			sum.CPUS = sum.CPUUserS + sum.CPUSysS
			sum.CPUDeltaS = sum.CPUS
			sum.RusageMaxRSS = uint64(ru.Maxrss) // bytes on macOS
			sum.PeakRSS = max(sum.PeakRSS, sum.RusageMaxRSS)
		}
	}
	// Average CPU over the window the CPU delta actually covers: first to
	// last sample when sampling, the whole run when rusage totals were used.
	span := sum.DurationS
	if sum.ExitCode == nil && len(series) > 1 {
		span = series[len(series)-1].T - series[0].T
	}
	if span > 0 {
		sum.AvgCPUPct = 100 * sum.CPUDeltaS / span
	}

	fmt.Fprintf(os.Stderr, "procstat pid %d: %d samples over %.2fs  peak RSS %s  final RSS %s  peak footprint %s  CPU %.3fs (user %.3f sys %.3f, %.3fs while watched, avg %.0f%%)  threads %d\n",
		sum.PID, sum.Samples, sum.DurationS, mib(sum.PeakRSS), mib(sum.FinalRSS), mib(sum.PeakFootprint),
		sum.CPUS, sum.CPUUserS, sum.CPUSysS, sum.CPUDeltaS, sum.AvgCPUPct, sum.PeakThreads)

	if *outPath != "" {
		b, _ := json.MarshalIndent(sum, "", "  ")
		if err := os.WriteFile(*outPath, append(b, '\n'), 0o644); err != nil {
			fmt.Fprintln(os.Stderr, "procstat:", err)
			os.Exit(1)
		}
	}
	if *asJSON {
		s := sum
		s.Series = nil
		b, _ := json.Marshal(s)
		fmt.Println(string(b))
	}
	if sum.ExitCode != nil && *sum.ExitCode > 0 {
		os.Exit(*sum.ExitCode)
	}
}

func tv(t syscall.Timeval) float64 { return float64(t.Sec) + float64(t.Usec)/1e6 }

func mib(b uint64) string { return fmt.Sprintf("%.1fMiB", float64(b)/(1<<20)) }
