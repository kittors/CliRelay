package api

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/identity"
	modelconfigsettings "github.com/router-for-me/CLIProxyAPI/v6/internal/management/settings/modelconfig"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
	"github.com/tidwall/gjson"
)

// A turn's model is only checked through the gate the model-restriction
// middleware publishes for WebSocket upgrades, so the middleware has to
// recognise every upgrade the Responses upgrader accepts, in every header form
// the upgrader accepts.

// rawResponsesTurn performs the WebSocket handshake for /v1/responses with the
// given Upgrade header lines, sends one response.create for model, and returns
// the handshake status and the first reply. It speaks the protocol by hand
// because gorilla's Dialer always writes its own Upgrade header.
func rawResponsesTurn(t *testing.T, base, upgradeLines, model string) (int, []byte) {
	t.Helper()
	host := strings.TrimPrefix(base, "http://")
	conn, err := net.Dial("tcp", host)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))

	handshake := "GET /v1/responses HTTP/1.1\r\nHost: " + host + "\r\nConnection: Upgrade\r\n" + upgradeLines +
		"Sec-WebSocket-Version: 13\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n\r\n"
	if _, err := io.WriteString(conn, handshake); err != nil {
		t.Fatalf("write handshake: %v", err)
	}
	reader := bufio.NewReader(conn)
	resp, err := http.ReadResponse(reader, nil)
	if err != nil {
		t.Fatalf("read handshake response: %v", err)
	}
	// Closed on return, after the frames below have been read through reader.
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusSwitchingProtocols {
		return resp.StatusCode, nil
	}

	frame := fmt.Sprintf(`{"type":"response.create","model":%q,"input":[{"role":"user","content":[{"type":"input_text","text":"hi"}]}]}`, model)
	if err := writeMaskedTextFrame(conn, []byte(frame)); err != nil {
		t.Fatalf("write frame: %v", err)
	}
	reply, err := readUnmaskedFrame(reader)
	if err != nil {
		t.Fatalf("read reply: %v", err)
	}
	return resp.StatusCode, reply
}

// writeMaskedTextFrame writes one client text frame (clients must mask, RFC 6455 §5.3).
func writeMaskedTextFrame(w io.Writer, payload []byte) error {
	frame := []byte{0x81}
	if n := len(payload); n < 126 {
		frame = append(frame, 0x80|byte(n))
	} else {
		frame = append(frame, 0x80|126, byte(n>>8), byte(n))
	}
	mask := [4]byte{0x1b, 0x2c, 0x3d, 0x4e}
	frame = append(frame, mask[:]...)
	for i, b := range payload {
		frame = append(frame, b^mask[i%4])
	}
	_, err := w.Write(frame)
	return err
}

// readUnmaskedFrame reads one server frame's payload (servers never mask).
func readUnmaskedFrame(r *bufio.Reader) ([]byte, error) {
	if _, err := r.ReadByte(); err != nil {
		return nil, err
	}
	lengthByte, err := r.ReadByte()
	if err != nil {
		return nil, err
	}
	n := int(lengthByte & 0x7f)
	switch n {
	case 126:
		var ext [2]byte
		if _, err := io.ReadFull(r, ext[:]); err != nil {
			return nil, err
		}
		n = int(ext[0])<<8 | int(ext[1])
	case 127:
		return nil, fmt.Errorf("unexpectedly large frame")
	}
	payload := make([]byte, n)
	_, err = io.ReadFull(r, payload)
	return payload, err
}

func TestModelGateCoversEveryAcceptedWebsocketUpgrade(t *testing.T) {
	upgrades := []struct{ name, lines string }{
		{"single Upgrade value", "Upgrade: websocket\r\n"},
		{"Upgrade token list", "Upgrade: h2c, websocket\r\n"},
		{"second Upgrade line", "Upgrade: h2c\r\nUpgrade: websocket\r\n"},
	}
	restrictions := []struct {
		name     string
		metadata map[string]string
		setup    func(t *testing.T)
		status   int
		message  string
	}{
		{
			name:     "key allowed-models",
			metadata: map[string]string{"allowed-models": quotaWSModel},
			status:   http.StatusForbidden,
			message:  "is not allowed for this API key",
		},
		{
			name:    "model disabled in the catalog",
			setup:   func(t *testing.T) { seedModelConfig(t, quotaWSOtherModel, false) },
			status:  http.StatusNotFound,
			message: "is disabled",
		},
		{
			name:     "channel group allow list",
			metadata: map[string]string{"allowed-channel-groups": "team-a"},
			setup: func(t *testing.T) {
				routing := config.RoutingConfig{ChannelGroups: []config.RoutingChannelGroup{{Name: "team-a", AllowedModels: []string{quotaWSModel}}}}
				if err := usage.UpsertRoutingConfigForTenant(identity.SystemTenantID, routing); err != nil {
					t.Fatalf("store routing config: %v", err)
				}
			},
			status:  http.StatusForbidden,
			message: "is not allowed for this API key",
		},
	}
	for _, restriction := range restrictions {
		for _, upgrade := range upgrades {
			t.Run(restriction.name+"/"+upgrade.name, func(t *testing.T) {
				h := newQuotaWSHarness(t, restriction.metadata)
				// The catalog's disabled set is cached across database swaps.
				modelconfigsettings.InvalidateDisabledModelCache()
				t.Cleanup(modelconfigsettings.InvalidateDisabledModelCache)
				if restriction.setup != nil {
					restriction.setup(t)
				}

				status, reply := rawResponsesTurn(t, h.base, upgrade.lines, quotaWSOtherModel)
				if status != http.StatusSwitchingProtocols {
					t.Fatalf("handshake status = %d, want 101 (the upgrader accepts this form)", status)
				}
				if calls := h.exec.calls.Load(); calls != 0 {
					t.Errorf("executor calls = %d, want 0", calls)
				}
				if gjson.GetBytes(reply, "type").String() != "error" ||
					int(gjson.GetBytes(reply, "status").Int()) != restriction.status ||
					!strings.Contains(gjson.GetBytes(reply, "error.message").String(), restriction.message) {
					t.Errorf("reply = %s, want the model gate's %d refusal saying %q", reply, restriction.status, restriction.message)
				}
			})
		}
	}
}
