package driver

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// fakePostgres serves each connection with serve, after reading its startup message (passed as
// its parameters: user, database...), until the test ends. It returns the server's address.
func fakePostgres(t *testing.T, serve func(params map[string]string, r *bufio.Reader, w net.Conn)) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				r := bufio.NewReader(c)
				var n uint32
				if binary.Read(r, binary.BigEndian, &n) != nil || n < 4 {
					return
				}
				startup := make([]byte, n-4)
				if _, err := io.ReadFull(r, startup); err != nil || len(startup) < 4 {
					return
				}
				params := map[string]string{}
				kv := strings.Split(string(startup[4:]), "\x00")
				for i := 0; i+1 < len(kv); i += 2 {
					params[kv[i]] = kv[i+1]
				}
				serve(params, r, c)
			}()
		}
	}()
	return l.Addr().String()
}

func pgMsg(t byte, payload ...[]byte) []byte {
	body := bytes.Join(payload, nil)
	out := []byte{t, 0, 0, 0, 0}
	binary.BigEndian.PutUint32(out[1:], uint32(len(body)+4))
	return append(out, body...)
}

func pgInt32(v int32) []byte {
	b := make([]byte, 4)
	binary.BigEndian.PutUint32(b, uint32(v))
	return b
}

func pgRead(r *bufio.Reader) (byte, []byte, error) {
	t, err := r.ReadByte()
	if err != nil {
		return 0, nil, err
	}
	var n uint32
	if err := binary.Read(r, binary.BigEndian, &n); err != nil || n < 4 {
		return 0, nil, io.ErrUnexpectedEOF
	}
	p := make([]byte, n-4)
	_, err = io.ReadFull(r, p)
	return t, p, err
}

func buildPG(t *testing.T, src string) string {
	t.Helper()
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "main.sq"), []byte(src), 0o644)
	bin := filepath.Join(dir, "p")
	if _, err := Build(Options{Dir: dir, Output: bin}); err != nil {
		t.Fatal(err)
	}
	return bin
}

// TestPostgresHostileNonce (V10): a server-first message whose nonce is longer than the client's
// buffer for it is a protocol error, not a panic.
func TestPostgresHostileNonce(t *testing.T) {
	url := "postgres://app:secret@" + fakePostgres(t, func(_ map[string]string, r *bufio.Reader, w net.Conn) {
		w.Write(pgMsg('R', pgInt32(10), []byte("SCRAM-SHA-256\x00\x00")))
		_, p, err := pgRead(r)
		if err != nil {
			return
		}
		cnonce := p[strings.Index(string(p), "r=")+2:]
		first := "r=" + string(cnonce) + strings.Repeat("A", 200) + ",s=c2FsdA==,i=4096"
		w.Write(pgMsg('R', pgInt32(11), []byte(first)))
		pgRead(r)
	})
	bin := buildPG(t, "program\n\nuse os, postgres\n\nthread buf u8[16K]\n\nfn main() {\n\t_, err := postgres.connect(os.arg(1), buf[:])\n\tprintln(err.message())\n}\n")
	out, err := exec.Command(bin, url).CombinedOutput()
	if err != nil || !strings.Contains(string(out), "unexpected reply") {
		t.Errorf("long nonce: %v\n%s", err, out)
	}
}

// TestPostgresPoolBuffers (V19): a Pool's connections each get their own share of its memory: the
// first connection's buffer once reached to the end of it, so a query on one overwrote rows
// another had buffered.
func TestPostgresPoolBuffers(t *testing.T) {
	url := "postgres://app@" + fakePostgres(t, func(_ map[string]string, r *bufio.Reader, w net.Conn) {
		w.Write(append(pgMsg('R', pgInt32(0)), pgMsg('Z', []byte("I"))...))
		var query []byte
		for {
			typ, p, err := pgRead(r)
			if err != nil || typ == 'X' {
				return
			}
			switch typ {
			case 'P':
				query = p
			case 'S':
				name := "B"
				if bytes.Contains(query, []byte(" a ")) {
					name = "A"
				}
				out := append(pgMsg('1'), pgMsg('2')...)
				for i := 0; i < 3; i++ {
					v := name + "-row-" + string(rune('0'+i))
					out = append(out, pgMsg('D', []byte{0, 1}, pgInt32(int32(len(v))), []byte(v))...)
				}
				out = append(out, pgMsg('C', []byte("SELECT 3\x00"))...)
				w.Write(append(out, pgMsg('Z', []byte("I"))...))
			}
		}
	})
	src := `program

use os, postgres

thread pool postgres.Pool
thread mem u8[32K]

fn run() -> ! {
	pool.init(os.arg(1), 2, mem[:])
	a := pool.get()!
	b := pool.get()!
	rows := b.query("SELECT name FROM b", nil)!
	if rows.next() {
		println("b", rows.text(0))
	}
	ra := a.query("SELECT name FROM a WHERE this_query_text_once_landed_in_the_other_connections_buffer", nil)!
	while rows.next() {
		println("b", rows.text(0))
	}
	println("b err", rows.err() == nil)
	while ra.next() {
		println("a", ra.text(0))
	}
	return nil
}

fn main() {
	if err := run(); err != nil {
		println("error:", err.message())
	}
}
`
	bin := buildPG(t, src)
	out, err := exec.Command(bin, url).CombinedOutput()
	want := "b B-row-0\nb B-row-1\nb B-row-2\nb err true\na A-row-0\na A-row-1\na A-row-2\n"
	if err != nil || string(out) != want {
		t.Errorf("pool: %v\ngot:\n%swant:\n%s", err, out, want)
	}
}

// TestPostgresURL (V37, V38): a URL's user, password and database are percent-decoded, and an
// sslmode that requires TLS is refused rather than ignored (the password once went in the clear).
func TestPostgresURL(t *testing.T) {
	got := make(chan string, 4)
	addr := fakePostgres(t, func(params map[string]string, r *bufio.Reader, w net.Conn) {
		w.Write(pgMsg('R', pgInt32(3))) // cleartext password
		_, p, err := pgRead(r)
		if err != nil {
			return
		}
		got <- params["user"] + "|" + params["database"] + "|" + strings.TrimSuffix(string(p), "\x00")
		w.Write(append(pgMsg('R', pgInt32(0)), pgMsg('Z', []byte("I"))...))
		pgRead(r)
	})
	bin := buildPG(t, "program\n\nuse os, postgres\n\nthread buf u8[16K]\n\nfn main() {\n\tc, err := postgres.connect(os.arg(1), buf[:])\n\tif err != nil {\n\t\tprintln(err.message())\n\t\treturn\n\t}\n\tprintln(\"connected\")\n\tc.close()\n}\n")
	for _, tc := range []struct{ url, out, server string }{
		{"postgres://my%40user:p%40ss%2Fw%3F@" + addr + "/my%20db?sslmode=disable", "connected\n", "my@user|my db|p@ss/w?"},
		{"postgres://app:secret@" + addr + "/db?sslmode=require", "postgres: the URL's sslmode requires TLS, which this client doesn't support\n", ""},
		{"postgres://app:secret@" + addr + "/db?application_name=x&sslmode=verify-full", "postgres: the URL's sslmode requires TLS, which this client doesn't support\n", ""},
		{"postgres://a%4:secret@" + addr + "/db", "postgres: invalid URL (want postgres://user:password@host:port/database)\n", ""},
	} {
		out, err := exec.Command(bin, tc.url).CombinedOutput()
		if err != nil || string(out) != tc.out {
			t.Errorf("%s: %v, output %q, want %q", tc.url, err, out, tc.out)
		}
		if tc.server != "" {
			if s := <-got; s != tc.server {
				t.Errorf("%s: the server got %q, want %q", tc.url, s, tc.server)
			}
		}
	}
	select {
	case s := <-got:
		t.Errorf("a refused URL still connected: the server got %q", s)
	default:
	}
}

// TestPostgresWideRow (V57): a row with more columns than Rows holds is TooManyColumns, and the
// connection still runs the next query; it was once "unexpected reply" and a closed connection.
func TestPostgresWideRow(t *testing.T) {
	url := "postgres://app@" + fakePostgres(t, func(_ map[string]string, r *bufio.Reader, w net.Conn) {
		w.Write(append(pgMsg('R', pgInt32(0)), pgMsg('Z', []byte("I"))...))
		queries := 0
		for {
			typ, _, err := pgRead(r)
			if err != nil || typ == 'X' {
				return
			}
			if typ != 'S' {
				continue
			}
			queries++
			cols := 100
			if queries > 1 {
				cols = 1
			}
			row := []byte{byte(cols >> 8), byte(cols)}
			for i := 0; i < cols; i++ {
				row = append(append(row, pgInt32(1)...), 'x')
			}
			out := append(append(pgMsg('1'), pgMsg('2')...), pgMsg('D', row)...)
			out = append(out, pgMsg('C', []byte("SELECT 1\x00"))...)
			w.Write(append(out, pgMsg('Z', []byte("I"))...))
		}
	})
	src := `program

use os, postgres

thread buf u8[16K]

fn main() {
	c, err := postgres.connect(os.arg(1), buf[:])
	if err != nil {
		println(err.message())
		return
	}
	rows, _ := c.query("SELECT wide", nil)
	println(rows.next(), rows.err().message())
	rows2, err2 := c.query("SELECT narrow", nil)
	if err2 != nil {
		println(err2.message())
		return
	}
	println(rows2.next(), rows2.text(0))
	c.close()
}
`
	bin := buildPG(t, src)
	out, err := exec.Command(bin, url).CombinedOutput()
	want := "false postgres: a row with more than 64 columns (select fewer)\ntrue x\n"
	if err != nil || string(out) != want {
		t.Errorf("%v: got %q, want %q", err, out, want)
	}
}
