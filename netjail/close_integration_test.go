//go:build integration

package netjail

import (
	"bufio"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// It waits on real time (a 3 s deadline), so it runs in make gate only.

// A connection the plain proxy made, upgraded (101) and so out of the
// HTTP server's hands, ends with the jail. Found by codex in review.
func TestCloseEndsUpgradedConnection(t *testing.T) {
	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, rw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		defer c.Close()
		io.WriteString(rw, "HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: echo\r\n\r\n")
		rw.Flush()
		io.Copy(c, rw)
	}))
	defer site.Close()
	prox, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	j := New(Spec{Allow: []string{"127.0.0.1"}})
	j.serve([]net.Listener{prox})
	defer j.Close()
	c, err := net.Dial("tcp", prox.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(3 * time.Second))
	io.WriteString(c, "GET "+site.URL+"/ HTTP/1.1\r\nHost: "+strings.TrimPrefix(site.URL, "http://")+"\r\nConnection: Upgrade\r\nUpgrade: echo\r\n\r\n")
	br := bufio.NewReader(c)
	resp, err := http.ReadResponse(br, nil)
	if err != nil || resp.StatusCode != 101 {
		t.Fatalf("upgrade: %v %v", resp, err)
	}
	io.WriteString(c, "before\n")
	if line, err := br.ReadString('\n'); err != nil || line != "before\n" {
		t.Fatalf("before Close: %q %v", line, err)
	}
	j.Close()
	io.WriteString(c, "after\n")
	line, err := br.ReadString('\n')
	var ne net.Error
	switch {
	case err == nil:
		t.Fatalf("bytes still pass after Close: %q", line)
	case errors.As(err, &ne) && ne.Timeout():
		t.Fatal("the connection is still open after Close")
	}
}
