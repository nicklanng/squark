package main

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"net"
	"strconv"
	"time"
)

// conn is a minimal keep-alive HTTP/1.1 client over a raw TCP connection.
// It writes pre-built request bytes and parses status line, headers and a
// Content-Length or chunked body. Bodies are discarded unless captured.
type conn struct {
	addr string
	c    net.Conn
	r    *bufio.Reader
	body []byte // last response body, only filled when keepBody is set
}

func dial(addr string) (*conn, error) {
	c, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		return nil, err
	}
	if tc, ok := c.(*net.TCPConn); ok {
		tc.SetNoDelay(true)
	}
	return &conn{addr: addr, c: c, r: bufio.NewReaderSize(c, 32<<10)}, nil
}

func (c *conn) close() {
	if c.c != nil {
		c.c.Close()
		c.c = nil
	}
}

var errMalformed = errors.New("malformed response")

// do sends req and reads one response. It returns the status code and
// whether the server asked to close the connection.
func (c *conn) do(req []byte, keepBody bool) (status int, closeConn bool, err error) {
	if _, err = c.c.Write(req); err != nil {
		return 0, true, err
	}
	line, err := c.readLine()
	if err != nil {
		return 0, true, err
	}
	// "HTTP/1.1 200 OK"
	if len(line) < 12 || !bytes.HasPrefix(line, []byte("HTTP/1.")) || line[8] != ' ' {
		return 0, true, errMalformed
	}
	status, err = atoi(line[9:12])
	if err != nil {
		return 0, true, errMalformed
	}
	http10 := line[7] == '0'
	closeConn = http10
	contentLength := -1
	chunked := false
	for {
		h, err := c.readLine()
		if err != nil {
			return status, true, err
		}
		if len(h) == 0 {
			break
		}
		colon := bytes.IndexByte(h, ':')
		if colon <= 0 {
			return status, true, errMalformed
		}
		name, val := h[:colon], bytes.TrimSpace(h[colon+1:])
		switch {
		case asciiEqualFold(name, "content-length"):
			if contentLength, err = atoi(val); err != nil {
				return status, true, errMalformed
			}
		case asciiEqualFold(name, "transfer-encoding"):
			chunked = asciiEqualFold(val, "chunked")
		case asciiEqualFold(name, "connection"):
			if asciiEqualFold(val, "close") {
				closeConn = true
			} else if asciiEqualFold(val, "keep-alive") {
				closeConn = false
			}
		}
	}
	c.body = c.body[:0]
	switch {
	case status/100 == 1 || status == 204 || status == 304:
		// no body
	case chunked:
		err = c.readChunked(keepBody)
	case contentLength >= 0:
		err = c.readN(contentLength, keepBody)
	default:
		// Body delimited by connection close.
		err = c.readToEOF(keepBody)
		closeConn = true
	}
	if err != nil {
		return status, true, err
	}
	return status, closeConn, nil
}

func (c *conn) readLine() ([]byte, error) {
	line, err := c.r.ReadSlice('\n')
	if err != nil {
		return nil, err
	}
	line = line[:len(line)-1]
	if n := len(line); n > 0 && line[n-1] == '\r' {
		line = line[:n-1]
	}
	return line, nil
}

func (c *conn) readN(n int, keep bool) error {
	if keep {
		start := len(c.body)
		c.body = append(c.body, make([]byte, n)...)
		_, err := io.ReadFull(c.r, c.body[start:])
		return err
	}
	_, err := c.r.Discard(n)
	return err
}

func (c *conn) readChunked(keep bool) error {
	for {
		line, err := c.readLine()
		if err != nil {
			return err
		}
		if i := bytes.IndexByte(line, ';'); i >= 0 {
			line = line[:i]
		}
		n, err := strconv.ParseUint(string(bytes.TrimSpace(line)), 16, 32)
		if err != nil {
			return errMalformed
		}
		if n == 0 {
			// trailers until blank line
			for {
				t, err := c.readLine()
				if err != nil {
					return err
				}
				if len(t) == 0 {
					return nil
				}
			}
		}
		if err := c.readN(int(n), keep); err != nil {
			return err
		}
		if l, err := c.readLine(); err != nil || len(l) != 0 {
			return errMalformed
		}
	}
}

func (c *conn) readToEOF(keep bool) error {
	if keep {
		b, err := io.ReadAll(c.r)
		c.body = append(c.body, b...)
		return err
	}
	_, err := io.Copy(io.Discard, c.r)
	return err
}

func atoi(b []byte) (int, error) {
	if len(b) == 0 {
		return 0, errMalformed
	}
	n := 0
	for _, ch := range b {
		if ch < '0' || ch > '9' {
			return 0, errMalformed
		}
		n = n*10 + int(ch-'0')
	}
	return n, nil
}

func asciiEqualFold(b []byte, s string) bool {
	if len(b) != len(s) {
		return false
	}
	for i := range b {
		ch := b[i]
		if 'A' <= ch && ch <= 'Z' {
			ch += 'a' - 'A'
		}
		if ch != s[i] {
			return false
		}
	}
	return true
}

func buildRequest(method, host, path string, body []byte) []byte {
	var b bytes.Buffer
	b.WriteString(method)
	b.WriteByte(' ')
	b.WriteString(path)
	b.WriteString(" HTTP/1.1\r\nHost: ")
	b.WriteString(host)
	b.WriteString("\r\nUser-Agent: squark-load\r\n")
	if body != nil || method == "POST" || method == "PUT" {
		b.WriteString("Content-Type: application/json\r\nContent-Length: ")
		b.WriteString(strconv.Itoa(len(body)))
		b.WriteString("\r\n")
	}
	b.WriteString("\r\n")
	b.Write(body)
	return b.Bytes()
}
