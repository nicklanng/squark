package driver

import (
	"encoding/binary"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// lookupProg prints each name's addresses from net.lookup_ip, one line per name:
//
//	prog resolv.conf hosts name...
const lookupProg = `program

use net, os

fn main() {
	net.resolv_conf = os.arg(1)
	net.hosts_file = os.arg(2)
	for i in 3..os.n_args() {
		var ips net.IP[8]
		var line u8[512]
		var t u8[46]
		b := append(append(line[:0], os.arg(i)...), ':')
		res, err := net.lookup_ip(os.arg(i), ips[:])
		if err != nil {
			b = append(append(b, " error "...), err.message()...)
		}
		for ip in res {
			b = append(append(b, ' '), ip.string(t[:])...)
		}
		println(str(b))
	}
}
`

// fakeDNS is a name server on 127.0.0.1 (UDP and TCP on one port) that answers from a table and
// records the names it is asked for.
type fakeDNS struct {
	port    int
	answers map[string]fakeAnswer // by lower-case name with its final dot
	other   int                   // the reply code for other names (3, NXDOMAIN, if 0)
	mu      sync.Mutex
	asked   []string // "name type" in arrival order
	udp     net.PacketConn
	tcp     net.Listener
}

type fakeAnswer struct {
	a, aaaa  []string // addresses
	cname    string   // answer with a CNAME to this name, followed by its addresses
	rcode    int      // reply with this code and no records
	drop     bool     // never reply
	truncate bool     // over UDP, reply with no records and the TC bit
	stray    string   // also an A record with this address owned by another name (not an answer)
	dropAAAA bool     // never answer AAAA queries
	noEDNS   bool     // answer a query with an EDNS record FORMERR, with no question (old servers)
}

func startFakeDNS(t *testing.T, answers map[string]fakeAnswer, other int) *fakeDNS {
	t.Helper()
	udp, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := udp.LocalAddr().(*net.UDPAddr).Port
	tcp, err := net.Listen("tcp4", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		udp.Close()
		t.Skipf("TCP port %d is taken: %v", port, err)
	}
	f := &fakeDNS{port: port, answers: answers, other: other, udp: udp, tcp: tcp}
	t.Cleanup(func() { udp.Close(); tcp.Close() })
	go func() {
		buf := make([]byte, 2048)
		for {
			n, from, err := udp.ReadFrom(buf)
			if err != nil {
				return
			}
			if reply := f.reply(buf[:n], true); reply != nil {
				udp.WriteTo(reply, from)
			}
		}
	}()
	go func() {
		for {
			c, err := tcp.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				var hdr [2]byte
				if _, err := c.Read(hdr[:]); err != nil {
					return
				}
				msg := make([]byte, binary.BigEndian.Uint16(hdr[:]))
				if _, err := readFull(c, msg); err != nil {
					return
				}
				if reply := f.reply(msg, false); reply != nil {
					binary.BigEndian.PutUint16(hdr[:], uint16(len(reply)))
					c.Write(append(hdr[:], reply...))
				}
			}()
		}
	}()
	return f
}

func readFull(c net.Conn, p []byte) (int, error) {
	n := 0
	for n < len(p) {
		m, err := c.Read(p[n:])
		n += m
		if err != nil {
			return n, err
		}
	}
	return n, nil
}

func (f *fakeDNS) log() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.asked...)
}

// reply builds the answer to query msg, or nil for none.
func (f *fakeDNS) reply(msg []byte, udp bool) []byte {
	if len(msg) < 12 {
		return nil
	}
	// the question: labels, then type and class
	off := 12
	var labels []string
	for off < len(msg) && msg[off] != 0 {
		l := int(msg[off])
		labels = append(labels, string(msg[off+1:off+1+l]))
		off += 1 + l
	}
	off++
	qtype := binary.BigEndian.Uint16(msg[off:])
	question := msg[12 : off+4]
	name := strings.ToLower(strings.Join(labels, ".")) + "."
	f.mu.Lock()
	f.asked = append(f.asked, fmt.Sprintf("%s %d", name, qtype))
	f.mu.Unlock()
	ans, ok := f.answers[name]
	if !ok {
		ans = fakeAnswer{rcode: 3}
		if f.other != 0 {
			ans.rcode = f.other
		}
	}
	if ans.drop || ans.dropAAAA && qtype == 28 {
		return nil
	}
	if ans.noEDNS && binary.BigEndian.Uint16(msg[10:]) > 0 {
		return []byte{msg[0], msg[1], 0x81, 0x81, 0, 0, 0, 0, 0, 0, 0, 0}
	}
	out := []byte{msg[0], msg[1], 0x81, 0x80, 0, 1, 0, 0, 0, 0, 0, 0}
	out = append(out, question...)
	if ans.rcode != 0 {
		out[3] |= byte(ans.rcode)
		return out
	}
	if udp && ans.truncate {
		out[2] |= 0x02
		return out
	}
	count := 0
	owner := []byte{0xc0, 12} // a pointer to the question's name
	if ans.cname != "" {
		var target []byte
		for _, l := range strings.Split(strings.TrimSuffix(ans.cname, "."), ".") {
			target = append(append(target, byte(len(l))), l...)
		}
		target = append(target, 0)
		out = append(out, owner...)
		out = binary.BigEndian.AppendUint16(out, 5)
		out = append(out, 0, 1, 0, 0, 0, 60)
		out = binary.BigEndian.AppendUint16(out, uint16(len(target)))
		at := len(out)
		out = append(out, target...)
		owner = []byte{0xc0 | byte(at>>8), byte(at)}
		count++
		ans = f.answers[strings.ToLower(ans.cname)]
	}
	addrs := ans.a
	if qtype == 28 {
		addrs = ans.aaaa
	}
	for _, a := range addrs {
		ip := net.ParseIP(a)
		rd := ip.To4()
		if qtype == 28 {
			rd = ip.To16()
		}
		out = append(out, owner...)
		out = binary.BigEndian.AppendUint16(out, qtype)
		out = append(out, 0, 1, 0, 0, 0, 60)
		out = binary.BigEndian.AppendUint16(out, uint16(len(rd)))
		out = append(out, rd...)
		count++
	}
	if ans.stray != "" && qtype == 1 {
		out = append(out, 8)
		out = append(out, "unrelate"...)
		out = append(out, 0, 0, 1, 0, 1, 0, 0, 0, 60, 0, 4)
		out = append(out, net.ParseIP(ans.stray).To4()...)
		count++
	}
	binary.BigEndian.PutUint16(out[6:], uint16(count))
	return out
}

// TestLookupIP runs net.lookup_ip against fake name servers: search domains and ndots as in a
// Kubernetes pod, CNAMEs, a truncated reply retried over TCP, a failing server followed by a
// working one, NXDOMAIN, a server that never answers, and the hosts file.
func TestLookupIP(t *testing.T) {
	many := make([]string, 100) // 100 A records: too long for a 1232-byte UDP reply
	for i := range many {
		many[i] = fmt.Sprintf("10.1.%d.%d", i/250, i%250+1)
	}
	good := startFakeDNS(t, map[string]fakeAnswer{
		"web.ns.svc.cluster.local.":   {a: []string{"10.0.0.1"}, aaaa: []string{"fd00::1"}},
		"db.other.svc.cluster.local.": {a: []string{"10.0.0.2"}},
		"example.com.":                {a: []string{"93.184.216.34", "93.184.216.35"}, aaaa: []string{"2606:2800:220:1::1"}},
		"alias.example.com.":          {cname: "real.example.com."},
		"real.example.com.":           {a: []string{"10.0.0.3"}},
		"big.example.com.":            {a: many, truncate: true},
		"v6only.example.com.":         {aaaa: []string{"2001:db8::7"}},
		"empty.example.com.":          {},
		"slow.example.com.":           {drop: true},
		"flaky.example.com.":          {a: []string{"10.0.0.4"}},
		// V35: records owned by other names aren't answers (they were taken as ones)
		"stray.example.com.":   {stray: "6.6.6.6"},
		"strayed.example.com.": {cname: "real.example.com.", stray: "6.6.6.6"},
		// V53: a lost AAAA reply doesn't hold up the A answer; FORMERR to EDNS is asked again
		"noaaaa.example.com.": {a: []string{"10.0.0.5"}, dropAAAA: true},
		"old.example.com.":    {a: []string{"10.0.0.6"}, noEDNS: true},
	}, 0)
	failing := startFakeDNS(t, nil, 2) // SERVFAIL for every name, so each lookup goes on to good
	dir := t.TempDir()
	conf := filepath.Join(dir, "resolv.conf")
	os.WriteFile(conf, []byte(fmt.Sprintf(`# a Kubernetes pod's resolv.conf, with test ports
nameserver 127.0.0.1:%d
nameserver 127.0.0.1:%d
search ns.svc.cluster.local svc.cluster.local cluster.local
options ndots:5 timeout:1 attempts:1
`, failing.port, good.port)), 0o644)
	hosts := filepath.Join(dir, "hosts")
	var names20 []string // more names on a line than a fixed list once held
	for i := 1; i <= 20; i++ {
		names20 = append(names20, fmt.Sprintf("name%d", i))
	}
	os.WriteFile(hosts, []byte("# comment\n10.9.9.9 myhost myhost.local # trailing\n::2 myhost\n10.9.9.8\t"+strings.Join(names20, " ")+"\n"), 0o644)
	bin := buildSource(t, lookupProg)

	cases := []struct{ name, want string }{
		{"web", "web: 10.0.0.1 fd00::1"},
		{"db.other", "db.other: 10.0.0.2"},
		{"web.ns.svc.cluster.local.", "web.ns.svc.cluster.local.: 10.0.0.1 fd00::1"},
		{"example.com", "example.com: 93.184.216.34 93.184.216.35 2606:2800:220:1::1"},
		{"alias.example.com", "alias.example.com: 10.0.0.3"},
		{"big.example.com", "big.example.com: 10.1.0.1 10.1.0.2 10.1.0.3 10.1.0.4 10.1.0.5 10.1.0.6 10.1.0.7 10.1.0.8"},
		{"v6only.example.com", "v6only.example.com: 2001:db8::7"},
		{"empty.example.com", "empty.example.com: error no such host"},
		{"missing.example.com", "missing.example.com: error no such host"},
		{"flaky.example.com", "flaky.example.com: 10.0.0.4"},
		{"stray.example.com", "stray.example.com: error no such host"},
		{"strayed.example.com", "strayed.example.com: 10.0.0.3"},
		{"noaaaa.example.com", "noaaaa.example.com: 10.0.0.5"},
		{"old.example.com", "old.example.com: 10.0.0.6"},
		{"name20", "name20: 10.9.9.8"},
		{"MyHost", "MyHost: 10.9.9.9 ::2"},
		{"myhost.local.", "myhost.local.: 10.9.9.9"},
		{"localhost", "localhost: 127.0.0.1 ::1"},
		{"x.localhost", "x.localhost: 127.0.0.1 ::1"},
		{"10.2.3.4", "10.2.3.4: 10.2.3.4"},
		{"::ffff:1.2.3.4", "::ffff:1.2.3.4: ::ffff:1.2.3.4"},
		{"a..b", "a..b: error no such host"},
		{strings.Repeat("x", 64) + ".com", strings.Repeat("x", 64) + ".com: error no such host"},
		{"slow.example.com.", "slow.example.com.: error i/o timeout"},
	}
	args := []string{conf, hosts}
	for _, c := range cases {
		args = append(args, c.name)
	}
	stdout, stderr, code := runFor(t, 60*time.Second, bin, args...)
	if code != 0 {
		t.Fatalf("exit %d\n%s%s", code, stdout, stderr)
	}
	lines := strings.Split(strings.TrimSuffix(stdout, "\n"), "\n")
	for i, c := range cases {
		if i >= len(lines) {
			t.Errorf("%s: no output", c.name)
			continue
		}
		if lines[i] != c.want {
			t.Errorf("got  %q\nwant %q", lines[i], c.want)
		}
	}

	// a lost AAAA reply: the A answer comes back about 100ms later, not after the 1s timeout
	began := time.Now()
	if out, _, _ := runFor(t, 10*time.Second, bin, conf, hosts, "noaaaa.example.com."); !strings.Contains(out, "10.0.0.5") {
		t.Errorf("noaaaa: %q", out)
	}
	if took := time.Since(began); took > 800*time.Millisecond {
		t.Errorf("a lookup whose AAAA reply was lost took %v, want well under the 1s timeout", took)
	}

	// the order names were tried in: ndots 5, so search domains before the name alone; a rooted
	// name only alone; and nothing asked for names the hosts file or a literal answered
	var order []string
	for _, q := range good.log() {
		if strings.HasSuffix(q, " 1") { // A queries; AAAA ones go along with them
			order = append(order, strings.TrimSuffix(q, " 1"))
		}
	}
	got := strings.Join(order, " ")
	for _, want := range []string{
		"web.ns.svc.cluster.local. db.other.ns.svc.cluster.local. db.other.svc.cluster.local. web.ns.svc.cluster.local. ",
		"example.com.ns.svc.cluster.local. example.com.svc.cluster.local. example.com.cluster.local. example.com. ",
		"slow.example.com.",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("queries %q\ndon't contain %q", got, want)
		}
	}
	for _, q := range good.log() {
		if strings.Contains(q, "myhost") || strings.Contains(q, "localhost") || strings.HasPrefix(q, "10.") {
			t.Errorf("asked the name server for %q", q)
		}
	}
}

// TestDialIPv6 listens and dials on IPv6 loopback, and checks that ":port" takes connections on
// both IPv4 and IPv6.
func TestDialIPv6(t *testing.T) {
	if l, err := net.Listen("tcp6", "[::1]:0"); err != nil {
		t.Skip("no IPv6 loopback:", err)
	} else {
		l.Close()
	}
	bin := buildSource(t, `program

use net

// echo dials addr, sends "hi" to l's side and prints what arrived.
fn echo(l net.Listener, addr str) {
	if err := try_echo(l, addr); err != nil {
		println(addr, "error:", err.message())
	}
}

fn try_echo(l net.Listener, addr str) -> ! {
	c := net.dial(addr)!
	defer c.close()
	s := l.accept()!
	defer s.close()
	c.write_string("hi")!
	var b u8[2]
	n := s.read(b[:])!
	println(addr, str(b[:n]))
	return nil
}

fn main() {
	var a u8[64]
	l6, err := net.listen("[::1]:0")
	if err != nil {
		panic(err)
	}
	echo(l6, str(net.append_host_port(a[:0], "::1", l6.port())))
	l6.close()
	any, err := net.listen(":0")
	if err != nil {
		panic(err)
	}
	p := any.port()
	echo(any, str(net.append_host_port(a[:0], "127.0.0.1", p)))
	echo(any, str(net.append_host_port(a[:0], "::1", p)))
	echo(any, str(net.append_host_port(a[:0], "localhost", p)))
	echo(any, str(net.append_host_port(a[:0], "", p)))
	any.close()
	for bad in str[]{"::1:80", "[::1]80", "[1.2.3.4]:80", "[::1:80"} {
		_, err := net.dial(bad)
		println(bad, err.message())
	}
}
`)
	stdout, stderr, code := runFor(t, 20*time.Second, bin)
	if code != 0 {
		t.Fatalf("exit %d\n%s%s", code, stdout, stderr)
	}
	for _, want := range []string{"::1]:", "127.0.0.1:", "localhost:", "invalid address"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("output lacks %q:\n%s", want, stdout)
		}
	}
	if strings.Contains(stdout, "error:") {
		t.Errorf("a dial failed:\n%s", stdout)
	}
}
