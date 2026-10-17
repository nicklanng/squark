package driver

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// TestLog: std/log writes one JSON object per line with the time (RFC 3339, UTC, ms), the level and
// the message first, then the fields; entries below log.level are dropped, and a line that doesn't
// fit keeps what fits and says "truncated":true.
func TestLog(t *testing.T) {
	bin := buildSource(t, `program

use log, net, strings, time

fn main() {
	e := log.info("served")
	e.str("path", "/notes/\"1\"\né")
	e.int("status", 200)
	e.duration("took", 2100 * time.microsecond)
	e.bool("cached", true)
	e.f64("ratio", 0.25)
	e.err("cause", net.Timeout)
	e.err("none", nil)
	e.send()
	e.send() // once only
	d := log.debug("hidden")
	d.send()
	if l, ok := log.parse_level("debug"); ok {
		log.level = l
	}
	d = log.debug("shown")
	d.send()
	w := log.warn("long")
	var big u8[3000]
	w.str("big", str(strings.append_repeat(big[:0], "x\"", 1500)))
	w.int("after", 1) // no room left
	w.send()
	x := log.err("failed")
	x.send()
}
`)
	before := time.Now().Add(-time.Second)
	stdout, stderr, code := runFor(t, 10*time.Second, bin)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	lines := strings.Split(strings.TrimSuffix(stdout, "\n"), "\n")
	if len(lines) != 4 {
		t.Fatalf("%d lines, want 4:\n%s", len(lines), stdout)
	}
	var recs []map[string]any
	for _, l := range lines {
		var m map[string]any
		if err := json.Unmarshal([]byte(l), &m); err != nil {
			t.Fatalf("not JSON: %v\n%s", err, l)
		}
		if len(l) > 1024 {
			t.Errorf("a line of %d bytes, over 1024", len(l))
		}
		ts, err := time.Parse("2006-01-02T15:04:05.000Z", m["time"].(string))
		if err != nil || ts.Before(before) || ts.After(time.Now().Add(time.Second)) {
			t.Errorf("time %v (%v)", m["time"], err)
		}
		recs = append(recs, m)
	}
	if !strings.HasPrefix(lines[0], `{"time":"`) || !strings.Contains(lines[0], `","level":"info","msg":"served","path":`) {
		t.Errorf("field order: %s", lines[0])
	}
	r := recs[0]
	want := map[string]any{"level": "info", "msg": "served", "path": "/notes/\"1\"\né", "status": 200.0, "took": 0.0021,
		"cached": true, "ratio": 0.25, "cause": "i/o timeout", "cause_code": -20.0, "none": nil}
	for k, v := range want {
		if r[k] != v {
			t.Errorf("%s = %v, want %v", k, r[k], v)
		}
	}
	if recs[1]["msg"] != "shown" || recs[1]["level"] != "debug" {
		t.Errorf("second line: %v", recs[1])
	}
	if recs[2]["truncated"] != true || recs[2]["after"] != nil || len(recs[2]["big"].(string)) < 400 {
		t.Errorf("truncated line: %s", lines[2])
	}
	if recs[3]["level"] != "error" {
		t.Errorf("log.err: %v", recs[3])
	}
}
