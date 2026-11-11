// Package proxytest provides in-process proxies for tests of the code that
// tunnels upstream traffic through one.
package proxytest

import (
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/url"
	"strconv"
	"sync"
	"testing"
)

// SOCKS5 is a SOCKS5 server on the loopback interface. It counts what clients
// do to it, and a test can change, while it runs, whether it accepts their
// credentials and whether it serves their CONNECT requests.
type SOCKS5 struct {
	listener net.Listener

	mu         sync.Mutex
	accept     func(username, password string) bool
	replyCode  byte
	handshakes int
	connects   int
	conns      map[net.Conn]struct{}
	closed     bool
}

// NewSOCKS5 starts a server that asks for no credentials and tunnels every
// CONNECT. It stops when the test ends.
func NewSOCKS5(t testing.TB) *SOCKS5 {
	t.Helper()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("proxytest: listen: %v", err)
	}
	return NewSOCKS5On(t, listener)
}

// NewSOCKS5On is NewSOCKS5 on a listener the test opened itself, for a proxy
// that has to come up on a particular address.
func NewSOCKS5On(t testing.TB, listener net.Listener) *SOCKS5 {
	t.Helper()
	s := &SOCKS5{listener: listener, conns: map[net.Conn]struct{}{}}
	go s.serve()
	t.Cleanup(s.Close)
	return s
}

// Addr is the host:port the server listens on.
func (s *SOCKS5) Addr() string { return s.listener.Addr().String() }

// URL is the proxy URL of the server with the given credentials; an empty
// username gives a URL without any.
func (s *SOCKS5) URL(username, password string) string {
	u := url.URL{Scheme: "socks5", Host: s.Addr()}
	if username != "" {
		u.User = url.UserPassword(username, password)
	}
	return u.String()
}

// RequireAuth makes the server demand a username and password and accept the
// ones accept approves. Nil goes back to asking for none.
func (s *SOCKS5) RequireAuth(accept func(username, password string) bool) {
	s.mu.Lock()
	s.accept = accept
	s.mu.Unlock()
}

// ReplyToConnect makes the server answer every CONNECT with the SOCKS5 reply
// code given instead of tunnelling (0x04 is "host unreachable"). Zero goes
// back to tunnelling.
func (s *SOCKS5) ReplyToConnect(code byte) {
	s.mu.Lock()
	s.replyCode = code
	s.mu.Unlock()
}

// Handshakes counts the connections on which a client started a SOCKS5
// handshake, whatever became of it.
func (s *SOCKS5) Handshakes() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.handshakes
}

// Connects counts the CONNECT requests that got past authentication.
func (s *SOCKS5) Connects() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.connects
}

// Close stops the server and drops its connections.
func (s *SOCKS5) Close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	conns := make([]net.Conn, 0, len(s.conns))
	for conn := range s.conns {
		conns = append(conns, conn)
	}
	s.mu.Unlock()
	_ = s.listener.Close()
	for _, conn := range conns {
		_ = conn.Close()
	}
}

func (s *SOCKS5) serve() {
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			return
		}
		if !s.track(conn) {
			_ = conn.Close()
			return
		}
		go s.handle(conn)
	}
}

func (s *SOCKS5) track(conn net.Conn) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return false
	}
	s.conns[conn] = struct{}{}
	return true
}

func (s *SOCKS5) untrack(conn net.Conn) {
	s.mu.Lock()
	delete(s.conns, conn)
	s.mu.Unlock()
	_ = conn.Close()
}

const (
	socksVersion       = 0x05
	methodNone         = 0x00
	methodUserPassword = 0x02
	methodUnacceptable = 0xff
	replyRefused       = 0x05
)

func (s *SOCKS5) handle(client net.Conn) {
	defer s.untrack(client)

	methods, err := readMethods(client)
	if err != nil {
		return
	}
	s.mu.Lock()
	s.handshakes++
	accept, replyCode := s.accept, s.replyCode
	s.mu.Unlock()

	if accept == nil {
		if _, err = client.Write([]byte{socksVersion, methodNone}); err != nil {
			return
		}
	} else if !authenticate(client, methods, accept) {
		return
	}

	target, err := readConnect(client)
	if err != nil {
		return
	}
	s.mu.Lock()
	s.connects++
	s.mu.Unlock()

	if replyCode != 0 {
		_, _ = client.Write(connectReply(replyCode))
		return
	}
	upstream, err := net.Dial("tcp", target)
	if err != nil {
		_, _ = client.Write(connectReply(replyRefused))
		return
	}
	if !s.track(upstream) {
		_ = upstream.Close()
		return
	}
	defer s.untrack(upstream)
	if _, err = client.Write(connectReply(0)); err != nil {
		return
	}
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(upstream, client); done <- struct{}{} }()
	go func() { _, _ = io.Copy(client, upstream); done <- struct{}{} }()
	<-done
}

// authenticate runs the username/password sub-negotiation (RFC 1929) and
// reports whether the client may go on.
func authenticate(client net.Conn, methods []byte, accept func(username, password string) bool) bool {
	offered := false
	for _, method := range methods {
		offered = offered || method == methodUserPassword
	}
	if !offered {
		_, _ = client.Write([]byte{socksVersion, methodUnacceptable})
		return false
	}
	if _, err := client.Write([]byte{socksVersion, methodUserPassword}); err != nil {
		return false
	}
	username, password, err := readCredentials(client)
	if err != nil {
		return false
	}
	if !accept(username, password) {
		_, _ = client.Write([]byte{0x01, 0x01})
		return false
	}
	_, err = client.Write([]byte{0x01, 0x00})
	return err == nil
}

func readMethods(conn net.Conn) ([]byte, error) {
	header := make([]byte, 2)
	if _, err := io.ReadFull(conn, header); err != nil {
		return nil, err
	}
	if header[0] != socksVersion {
		return nil, errors.New("proxytest: not a SOCKS5 greeting")
	}
	methods := make([]byte, int(header[1]))
	_, err := io.ReadFull(conn, methods)
	return methods, err
}

func readCredentials(conn net.Conn) (string, string, error) {
	username, err := readLengthPrefixed(conn, 1)
	if err != nil {
		return "", "", err
	}
	password, err := readLengthPrefixed(conn, 0)
	return username, password, err
}

// readLengthPrefixed skips skip bytes, then reads one length byte and that
// many bytes of text.
func readLengthPrefixed(conn net.Conn, skip int) (string, error) {
	header := make([]byte, skip+1)
	if _, err := io.ReadFull(conn, header); err != nil {
		return "", err
	}
	text := make([]byte, int(header[skip]))
	_, err := io.ReadFull(conn, text)
	return string(text), err
}

// readConnect reads a CONNECT request and returns its target as host:port.
func readConnect(conn net.Conn) (string, error) {
	header := make([]byte, 4)
	if _, err := io.ReadFull(conn, header); err != nil {
		return "", err
	}
	if header[0] != socksVersion || header[1] != 0x01 {
		return "", errors.New("proxytest: not a SOCKS5 CONNECT")
	}
	var host string
	switch header[3] {
	case 0x01, 0x04:
		size := net.IPv4len
		if header[3] == 0x04 {
			size = net.IPv6len
		}
		addr := make([]byte, size)
		if _, err := io.ReadFull(conn, addr); err != nil {
			return "", err
		}
		host = net.IP(addr).String()
	case 0x03:
		name, err := readLengthPrefixed(conn, 0)
		if err != nil {
			return "", err
		}
		host = name
	default:
		return "", errors.New("proxytest: unknown SOCKS5 address type")
	}
	port := make([]byte, 2)
	if _, err := io.ReadFull(conn, port); err != nil {
		return "", err
	}
	return net.JoinHostPort(host, strconv.Itoa(int(binary.BigEndian.Uint16(port)))), nil
}

func connectReply(code byte) []byte {
	return []byte{socksVersion, code, 0x00, 0x01, 0, 0, 0, 0, 0, 0}
}
