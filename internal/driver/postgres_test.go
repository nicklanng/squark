package driver

import (
	"os"
	"strings"
	"testing"
	"time"
)

// TestPostgres runs std/postgres against a real server: SQUARK_PG_URL names one (scripts/
// test-postgres.sh starts one in Docker, with SCRAM-SHA-256 authentication), and the test is
// skipped without it. It covers authentication, DDL, parameters (text with quotes, booleans,
// NULL), rows, the affected count, a server error's SQLSTATE, a pool shared by goroutines, and a
// query cut off by with deadline.
func TestPostgres(t *testing.T) {
	url := os.Getenv("SQUARK_PG_URL")
	if url == "" {
		t.Skip("SQUARK_PG_URL isn't set (scripts/test-postgres.sh sets it)")
	}
	bin := buildSource(t, `program

use os, postgres, time

thread buf u8[16K]
thread pool_mem u8[64K]
thread pool postgres.Pool
thread results int chan[8]

task worker(n int) pool 4 {
	c, err := pool.get()
	if err != nil {
		println("get:", err.message())
		results <- -1
		return
	}
	var p postgres.Params
	p.int(n)
	rows, err := c.query("SELECT $1::int * 10, pg_sleep(0.05)", &p)
	v := -1
	if err == nil {
		while rows.next() {
			v = rows.int(0)
		}
	}
	pool.put(c)
	results <- v
}

fn run() -> ! {
	if _, err := postgres.connect(os.arg(2), buf[:]); err != nil {
		println("wrong password:", err.message())
	}
	c := postgres.connect(os.arg(1), buf[:])!
	c.exec("DROP TABLE IF EXISTS squark_t", nil)!
	c.exec("CREATE TABLE squark_t (id int PRIMARY KEY, title text, done bool)", nil)!
	var p postgres.Params
	for i in 1..4 {
		p.clear()
		p.int(i)
		p.str("it's \"note\" é")
		p.bool(i%2 == 0)
		n := c.exec("INSERT INTO squark_t VALUES ($1, $2, $3)", &p)!
		println("inserted", n)
	}
	p.clear()
	p.int(1)
	rows := c.query("SELECT id, title, done, NULL FROM squark_t WHERE id > $1 ORDER BY id", &p)!
	while rows.next() {
		println(rows.int(0), rows.text(1), rows.bool(2), rows.is_null(3), rows.columns())
	}
	rows.err()!
	println("selected", rows.affected())
	p.clear()
	p.int(2)
	p.str("dup")
	p.null()
	if _, err := c.exec("INSERT INTO squark_t VALUES ($1, $2, $3)", &p); err != nil {
		println("dup:", err.message(), c.error_code())
	}
	n := c.exec("UPDATE squark_t SET done = NOT done WHERE done IS NOT NULL", nil)!
	println("updated", n)
	if _, err := c.exec("SELEC 1", nil); err != nil {
		println("syntax:", c.error_code())
	}
	n = c.exec("DELETE FROM squark_t", nil)!
	println("deleted", n)
	with deadline 100 * time.millisecond {
		_, err := c.exec("SELECT pg_sleep(2)", nil)
		println("deadline:", err.message())
	}
	c.close()

	pool.init(os.arg(1), 2, pool_mem[:])
	for i in 1..5 {
		start worker(i)
	}
	sum := 0
	for _ in 1..5 {
		sum += <-results
	}
	println("pool sum", sum)
	return nil
}

fn main() {
	if err := run(); err != nil {
		println("error:", err.message())
		os.exit(1)
	}
}
`)
	wrong := strings.Replace(url, "://squark:", "://squark:not-", 1)
	stdout, stderr, code := runFor(t, 60*time.Second, bin, url, wrong)
	want := `wrong password: postgres: the server reported an error (see Conn.error_message)
inserted 1
inserted 1
inserted 1
2 it's "note" é true true 4
3 it's "note" é false true 4
selected 2
dup: postgres: the server reported an error (see Conn.error_message) 23505
updated 3
syntax: 42601
deleted 3
deadline: i/o timeout
pool sum 100
`
	if code != 0 || stdout != want {
		t.Errorf("exit %d\n--- got\n%s--- want\n%s--- stderr\n%s", code, stdout, want, stderr)
	}
	if strings.Contains(stderr, "panic") {
		t.Error(stderr)
	}
}
