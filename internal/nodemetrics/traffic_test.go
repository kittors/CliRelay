package nodemetrics

import (
	"bufio"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

// serveCounted runs handler behind c's counting listener and returns its
// base URL.
func serveCounted(t *testing.T, c *Collector, handler http.Handler) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: handler, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = server.Serve(c.WrapListener(ln)) }()
	t.Cleanup(func() { _ = server.Close() })
	return "http://" + ln.Addr().String()
}

func TestListenerCountsRequestAndResponseBytes(t *testing.T) {
	c := New(Options{Root: t.TempDir()})
	body := strings.Repeat("u", 4096)
	reply := strings.Repeat("d", 10000)
	base := serveCounted(t, c, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		_, _ = io.WriteString(w, reply)
	}))

	resp, err := http.Post(base+"/v1/responses", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if string(got) != reply {
		t.Fatalf("the wrapper changed the response: %d bytes", len(got))
	}

	counts := c.traffic.load()
	// Headers come on top of the bodies on both sides.
	if counts.bytesIn < uint64(len(body)) || counts.bytesIn > uint64(len(body))+1024 {
		t.Fatalf("bytes in = %d for a %d-byte body", counts.bytesIn, len(body))
	}
	if counts.bytesOut < uint64(len(reply)) || counts.bytesOut > uint64(len(reply))+1024 {
		t.Fatalf("bytes out = %d for a %d-byte reply", counts.bytesOut, len(reply))
	}
}

// A WebSocket upgrade hands the connection over; the frames after it never
// pass through a response writer and must still be counted.
func TestListenerCountsHijackedConnections(t *testing.T) {
	c := New(Options{Root: t.TempDir()})
	base := serveCounted(t, c, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		conn, buf, err := http.NewResponseController(w).Hijack()
		if err != nil {
			t.Errorf("hijack: %v", err)
			return
		}
		defer conn.Close()
		_, _ = buf.WriteString("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: test\r\n\r\n")
		_ = buf.Flush()
		frame := make([]byte, 5000)
		if _, err := io.ReadFull(buf, frame); err != nil {
			t.Errorf("read frame: %v", err)
			return
		}
		_, _ = conn.Write([]byte(strings.Repeat("f", 7000)))
	}))

	conn, err := net.Dial("tcp", strings.TrimPrefix(base, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_, _ = io.WriteString(conn, "GET /ws HTTP/1.1\r\nHost: test\r\nConnection: Upgrade\r\nUpgrade: test\r\n\r\n")
	reader := bufio.NewReader(conn)
	resp, err := http.ReadResponse(reader, nil)
	if err != nil || resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("upgrade: %v %v", resp, err)
	}
	_, _ = conn.Write([]byte(strings.Repeat("c", 5000)))
	if n, _ := io.Copy(io.Discard, reader); n != 7000 {
		t.Fatalf("read %d bytes after the upgrade, want 7000", n)
	}

	counts := c.traffic.load()
	if counts.bytesIn < 5000 || counts.bytesOut < 7000 {
		t.Fatalf("frames after the upgrade were not counted: %+v", counts)
	}
}

// net/http half-closes a connection it will not reuse before closing it, so
// the client gets the response before any reset; the wrapper passes that on.
func TestCountingConnPassesCloseWriteThrough(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	accepted := make(chan net.Conn, 1)
	go func() {
		conn, err := ln.Accept()
		if err == nil {
			accepted <- conn
		}
	}()
	client, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	server := <-accepted
	defer server.Close()

	wrapped := &countingConn{Conn: server, counters: &trafficCounters{}}
	var cw interface{ CloseWrite() error } = wrapped
	if err := cw.CloseWrite(); err != nil {
		t.Fatalf("CloseWrite: %v", err)
	}
	_ = client.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := client.Read(make([]byte, 1)); err != io.EOF {
		t.Fatalf("the peer should see the half-close as EOF, got %v", err)
	}
}
