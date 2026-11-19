package executor

import (
	"context"
	"errors"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
)

const (
	// codexIncrementalReadIdle is how long a connection may go without a message
	// or a ping before it is taken for dead. The ChatGPT backend pings about every
	// 20 seconds (measured 2026-11-18); a connection that misses several in a row
	// is a dead path, the failure mode the HTTP transport's ping check guards
	// against (stale_upstream_conns).
	codexIncrementalReadIdle = 90 * time.Second
	// codexIncrementalReuseQuiet is how long a connection may have been silent
	// and still take a new turn: past two missed pings it is not trusted with
	// one, since a turn sent into a dead path waits for the read timeout above
	// before it can go again.
	codexIncrementalReuseQuiet = 45 * time.Second
	// codexIncrementalWriteBuffer is the frame size for outbound messages. Pongs
	// go out between frames, so a frame must stay small enough on a slow egress
	// that a multi-megabyte upload does not starve the keepalive.
	codexIncrementalWriteBuffer = 64 << 10
	// codexIncrementalWriteFloor and codexIncrementalWriteRate bound how long one
	// message may take to send: a minute plus the time at 128 KiB/s, well under
	// the 15 Mbit/s the egress has when it is shared.
	codexIncrementalWriteFloor = time.Minute
	codexIncrementalWriteRate  = 128 << 10
)

var (
	errCodexIncrementalConnClosed = errors.New("codex incremental: upstream websocket closed")
	errCodexIncrementalConnBusy   = errors.New("codex incremental: upstream websocket busy")
	codexIncrementalConnSerial    atomic.Uint64
)

// codexIncrementalConn is one upstream websocket pinned to one chain.
//
// A reader goroutine runs for the whole life of the connection. That is what
// answers the backend's pings between turns: gorilla only replies to a ping
// while something is reading, and without replies the backend closes the
// socket with 1011 "keepalive ping timeout".
type codexIncrementalConn struct {
	serial    uint64
	ws        *websocket.Conn
	identity  codexIncrementalConnIdentity
	wsURL     string
	proxyURL  string
	createdAt time.Time
	// handshake are the headers the connection was opened with; the request
	// log shows them for every turn it carries.
	handshake http.Header

	writeMu sync.Mutex
	mu      sync.Mutex
	sink    *codexIncrementalTurnSink
	// lastSeen is when the backend last sent anything, a ping included, in
	// Unix nanoseconds.
	lastSeen  atomic.Int64
	closed    atomic.Bool
	closeOnce sync.Once
	done      chan struct{}
}

// codexIncrementalConnIdentity is what the handshake fixed for the life of a
// connection. A turn that would have been opened differently gets a new one:
// another model may need a higher Version header, a refreshed token would be
// refused, a changed proxy is a different egress.
type codexIncrementalConnIdentity struct {
	authID         string
	promptCacheKey string
	model          string
	tokenDigest    string
	proxyURL       string
}

type codexIncrementalRead struct {
	payload []byte
	err     error
}

// codexIncrementalTurnBacklog bounds the messages a turn may have waiting for a
// slow reader, in payload bytes.
const codexIncrementalTurnBacklog = 64 << 20

// codexIncrementalTurnSink receives the messages of one turn.
//
// It is a queue rather than a channel the reader blocks on: the reader must go
// back to reading at once, because that is also what answers pings. With a
// client draining the reply slowly, a blocked reader would let the backend's
// keepalive expire mid-turn.
type codexIncrementalTurnSink struct {
	mu     sync.Mutex
	queue  []codexIncrementalRead
	bytes  int
	notify chan struct{}
	done   chan struct{}
	once   sync.Once
}

// push queues a message, reporting false when the backlog is over its bound.
func (s *codexIncrementalTurnSink) push(read codexIncrementalRead) bool {
	s.mu.Lock()
	if s.bytes+len(read.payload) > codexIncrementalTurnBacklog {
		s.mu.Unlock()
		return false
	}
	s.queue = append(s.queue, read)
	s.bytes += len(read.payload)
	s.mu.Unlock()
	select {
	case s.notify <- struct{}{}:
	default:
	}
	return true
}

func (s *codexIncrementalTurnSink) pop() (codexIncrementalRead, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.queue) == 0 {
		return codexIncrementalRead{}, false
	}
	read := s.queue[0]
	s.queue[0] = codexIncrementalRead{}
	s.queue = s.queue[1:]
	s.bytes -= len(read.payload)
	return read, true
}

// dialCodexIncrementalConn opens an upstream websocket through the account's
// proxy and the egress breaker, the same way the websocket executor does.
func dialCodexIncrementalConn(ctx context.Context, cfg *config.Config, auth *cliproxyauth.Auth, wsURL string, identity codexIncrementalConnIdentity, headers http.Header) (*codexIncrementalConn, *http.Response, error) {
	dialer := newProxyAwareWebsocketDialer(ctx, cfg, auth)
	dialer.HandshakeTimeout = codexResponsesWebsocketHandshakeTO
	dialer.EnableCompression = true
	dialer.WriteBufferSize = codexIncrementalWriteBuffer
	proxyURL := resolveAuthProxyURL(cfg, auth)
	permit, errEgress := egressBreaker.Allow(proxyURL)
	if errEgress != nil {
		return nil, nil, errEgress
	}
	ws, resp, err := dialer.DialContext(ctx, wsURL, headers)
	err = permit.Done(err)
	if err != nil {
		return nil, resp, err
	}
	// As in the websocket executor: compression is negotiated for what the
	// backend sends, but outbound messages go uncompressed to avoid gorilla's
	// flate tail validation issues with some upstreams.
	ws.EnableWriteCompression(false)
	ws.SetReadLimit(maxCodexSSELineBytes)
	conn := &codexIncrementalConn{
		serial:    codexIncrementalConnSerial.Add(1),
		ws:        ws,
		identity:  identity,
		wsURL:     wsURL,
		proxyURL:  proxyURL,
		createdAt: time.Now(),
		done:      make(chan struct{}),
	}
	conn.lastSeen.Store(conn.createdAt.UnixNano())
	ws.SetPingHandler(func(appData string) error {
		now := time.Now()
		conn.lastSeen.Store(now.UnixNano())
		_ = ws.SetReadDeadline(now.Add(codexIncrementalReadIdle))
		// WriteControl may run beside a WriteMessage in progress (gorilla
		// interleaves control frames between data frames), so no write lock:
		// taking one would hold the pong back for a whole upload.
		errPong := ws.WriteControl(websocket.PongMessage, []byte(appData), time.Now().Add(10*time.Second))
		var netErr net.Error
		if errPong == nil || errors.Is(errPong, websocket.ErrCloseSent) || (errors.As(errPong, &netErr) && netErr.Timeout()) {
			return nil
		}
		return errPong
	})
	go conn.readLoop()
	log.Infof("codex incremental: upstream connected conn=%d auth=%s url=%s", conn.serial, identity.authID, wsURL)
	return conn, resp, nil
}

func (c *codexIncrementalConn) readLoop() {
	defer close(c.done)
	for {
		_ = c.ws.SetReadDeadline(time.Now().Add(codexIncrementalReadIdle))
		msgType, payload, err := c.ws.ReadMessage()
		if err != nil {
			c.close("read_error: " + err.Error())
			c.deliver(codexIncrementalRead{err: err})
			return
		}
		if msgType == websocket.BinaryMessage {
			errBinary := errors.New("codex incremental: unexpected binary message")
			c.close("unexpected_binary")
			c.deliver(codexIncrementalRead{err: errBinary})
			return
		}
		c.lastSeen.Store(time.Now().UnixNano())
		if msgType != websocket.TextMessage {
			continue
		}
		c.deliver(codexIncrementalRead{payload: payload})
	}
}

// deliver hands a message to the turn in progress. Between turns nothing is
// expected; an error there means the backend is closing the socket (the
// 60-minute limit arrives this way), anything else is dropped.
func (c *codexIncrementalConn) deliver(read codexIncrementalRead) {
	c.mu.Lock()
	sink := c.sink
	c.mu.Unlock()
	if sink == nil {
		if read.err == nil && gjson.GetBytes(read.payload, "type").String() == "error" {
			c.close("idle_error_event")
		}
		return
	}
	select {
	case <-sink.done:
		return
	default:
	}
	if !sink.push(read) {
		c.close("turn_backlog_exceeded")
		sink.push(codexIncrementalRead{err: errors.New("codex incremental: reply backlog exceeded")})
	}
}

// beginTurn routes the connection's messages to a new turn.
func (c *codexIncrementalConn) beginTurn() (*codexIncrementalTurnSink, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.isClosed() {
		return nil, errCodexIncrementalConnClosed
	}
	if c.sink != nil {
		return nil, errCodexIncrementalConnBusy
	}
	c.sink = &codexIncrementalTurnSink{notify: make(chan struct{}, 1), done: make(chan struct{})}
	return c.sink, nil
}

// endTurn stops routing messages to sink.
func (c *codexIncrementalConn) endTurn(sink *codexIncrementalTurnSink) {
	if sink == nil {
		return
	}
	c.mu.Lock()
	if c.sink == sink {
		c.sink = nil
	}
	c.mu.Unlock()
	sink.once.Do(func() { close(sink.done) })
}

// next returns the turn's next message.
func (s *codexIncrementalTurnSink) next(ctx context.Context, connDone <-chan struct{}) ([]byte, error) {
	for {
		if read, ok := s.pop(); ok {
			return read.payload, read.err
		}
		select {
		case <-s.notify:
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-connDone:
			// The read loop ended; it may have queued its error first.
			if read, ok := s.pop(); ok {
				return read.payload, read.err
			}
			return nil, errCodexIncrementalConnClosed
		}
	}
}

// send writes one message. A cancelled request closes the connection, which is
// what interrupts a write stuck on a slow egress.
func (c *codexIncrementalConn) send(ctx context.Context, payload []byte) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if c.isClosed() {
		return errCodexIncrementalConnClosed
	}
	timeout := codexIncrementalWriteFloor + time.Duration(len(payload)/codexIncrementalWriteRate)*time.Second
	_ = c.ws.SetWriteDeadline(time.Now().Add(timeout))
	stop := context.AfterFunc(ctx, func() { c.close("context_done") })
	defer stop()
	if err := c.ws.WriteMessage(websocket.TextMessage, payload); err != nil {
		c.close("write_error: " + err.Error())
		return err
	}
	return nil
}

// usable reports whether a new turn may go out on this connection.
func (c *codexIncrementalConn) usable(identity codexIncrementalConnIdentity, now time.Time) bool {
	return c != nil && !c.isClosed() && c.identity == identity &&
		now.Sub(c.createdAt) < codexIncrementalConnMaxAge &&
		now.Sub(time.Unix(0, c.lastSeen.Load())) < codexIncrementalReuseQuiet
}

func (c *codexIncrementalConn) isClosed() bool {
	return c == nil || c.closed.Load()
}

func (c *codexIncrementalConn) close(reason string) {
	if c == nil {
		return
	}
	c.closeOnce.Do(func() {
		c.closed.Store(true)
		if c.ws != nil {
			_ = c.ws.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), time.Now().Add(time.Second))
			_ = c.ws.Close()
		}
		log.Infof("codex incremental: upstream disconnected conn=%d auth=%s age=%s reason=%s", c.serial, c.identity.authID, time.Since(c.createdAt).Round(time.Second), reason)
	})
}
