package nodemetrics

import (
	"net"
	"sync/atomic"
)

// trafficCounters count since the process started; the collector turns them
// into per-window figures. They are bumped on the request path, so they are
// plain atomics and nothing else.
type trafficCounters struct {
	requests atomic.Uint64
	bytesIn  atomic.Uint64
	bytesOut atomic.Uint64
}

type trafficCounts struct {
	requests, bytesIn, bytesOut uint64
}

func (t *trafficCounters) load() trafficCounts {
	return trafficCounts{requests: t.requests.Load(), bytesIn: t.bytesIn.Load(), bytesOut: t.bytesOut.Load()}
}

// minus returns c - base, or zeros where a counter went backwards, which
// cannot happen for one process but must not wrap around.
func (c trafficCounts) minus(base trafficCounts) trafficCounts {
	sub := func(a, b uint64) uint64 {
		if a < b {
			return 0
		}
		return a - b
	}
	return trafficCounts{
		requests: sub(c.requests, base.requests),
		bytesIn:  sub(c.bytesIn, base.bytesIn),
		bytesOut: sub(c.bytesOut, base.bytesOut),
	}
}

// countingListener counts the bytes of every accepted connection as they
// cross the socket. Counting here rather than per request also covers
// response streams while they run and WebSocket frames after the upgrade,
// which never pass through the request's body or response writer counts.
type countingListener struct {
	net.Listener
	counters *trafficCounters
}

func (l countingListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return &countingConn{Conn: conn, counters: l.counters}, nil
}

type countingConn struct {
	net.Conn
	counters *trafficCounters
}

func (c *countingConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	if n > 0 {
		c.counters.bytesIn.Add(uint64(n))
	}
	return n, err
}

func (c *countingConn) Write(p []byte) (int, error) {
	n, err := c.Conn.Write(p)
	if n > 0 {
		c.counters.bytesOut.Add(uint64(n))
	}
	return n, err
}

// CloseWrite keeps the half-close net/http performs before closing a
// connection it will not reuse, which a wrapper would otherwise hide from it:
// without the FIN first, closing with unread request bytes buffered makes the
// kernel send a reset that can destroy the response still in flight.
func (c *countingConn) CloseWrite() error {
	if cw, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return cw.CloseWrite()
	}
	return nil
}
