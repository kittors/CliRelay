package alerting

import (
	"context"
	"net"
	"net/http"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
)

// deliveryTimeout bounds one delivery to one channel, connection and answer
// included. It is the same order as the other outbound notifications (see
// internal/ipaccess), and long enough for a chat service on a slow day.
const deliveryTimeout = 10 * time.Second

// Delivery is the outcome of sending one message to one channel.
type Delivery struct {
	ChannelID   string `json:"channel_id"`
	ChannelName string `json:"channel_name"`
	Kind        string `json:"kind"`
	Status      string `json:"status"`
	// Error is a short reason on failure. It never contains the address or a
	// token (see sendError).
	Error    string        `json:"error"`
	Duration time.Duration `json:"-"`
}

// OK reports whether the channel accepted the message.
func (d Delivery) OK() bool { return d.Status == DeliverySent }

// Dispatcher sends messages. It holds no queue: the only callers are the
// evaluation loop and the management "send a test" request, both off the
// request path, and each sends channel by channel at its own pace. What the
// dispatcher guarantees is the part that keeps a bad channel from hurting
// anything else: every delivery has a hard timeout, redirects are not followed,
// the receiver's answer is read only up to a few kilobytes and never repeated,
// and a failure is reduced to a reason that cannot leak the address.
type Dispatcher struct {
	// direct reaches the public internet only: it has no proxy and refuses private
	// addresses at connection time. It carries the generic webhooks of tenants
	// that are not the platform operator.
	direct *http.Client
	// proxied honours the standard proxy environment, like the other outbound
	// notifications of this server (the IP-access alert, the DNS watch), and does
	// not use the upstream proxy-url, which exists for the AI providers. It
	// carries the operator's own webhooks (which may point into the local
	// network) and the chat channels, whose hosts are fixed.
	proxied *http.Client
	now     func() time.Time
}

// NewDispatcher builds a dispatcher with the production transports.
func NewDispatcher() *Dispatcher {
	return &Dispatcher{direct: newClient(false), proxied: newClient(true), now: time.Now}
}

// newDispatcherWith builds a dispatcher whose requests all go through rt, for
// tests that answer for the receivers.
func newDispatcherWith(rt http.RoundTripper) *Dispatcher {
	client := &http.Client{Transport: rt, Timeout: deliveryTimeout, CheckRedirect: noRedirect}
	return &Dispatcher{direct: client, proxied: client, now: time.Now}
}

func noRedirect(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

func newClient(proxied bool) *http.Client {
	dialer := &net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}
	transport := &http.Transport{
		DialContext:           dialer.DialContext,
		TLSHandshakeTimeout:   5 * time.Second,
		ResponseHeaderTimeout: deliveryTimeout,
		MaxIdleConns:          4,
		IdleConnTimeout:       30 * time.Second,
		ForceAttemptHTTP2:     true,
	}
	if proxied {
		transport.Proxy = http.ProxyFromEnvironment
	} else {
		dialer.Control = publicOnlyControl
	}
	// A 3xx is not followed: a receiver that redirects is either misconfigured or
	// trying to bounce the request somewhere it was not sent, and for the guarded
	// client a redirect target would also dodge the address check on the URL.
	return &http.Client{Transport: transport, Timeout: deliveryTimeout, CheckRedirect: noRedirect}
}

// Send delivers msg to every channel at the same time and returns one result
// per channel, in channel order. trusted says the channels belong to the
// platform operator, who may aim a webhook into the local network.
func (d *Dispatcher) Send(ctx context.Context, channels []Channel, msg Message, trusted bool) []Delivery {
	results := make([]Delivery, len(channels))
	var wg sync.WaitGroup
	for i, channel := range channels {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i] = d.sendOne(ctx, channel, msg, trusted)
		}()
	}
	wg.Wait()
	return results
}

func (d *Dispatcher) sendOne(ctx context.Context, channel Channel, msg Message, trusted bool) (result Delivery) {
	result = Delivery{ChannelID: channel.ID, ChannelName: channel.Name, Kind: channel.Kind, Status: DeliverySent}
	started := time.Now()
	defer func() {
		if recovered := recover(); recovered != nil {
			// A bug in a channel must cost one failed delivery, not the loop.
			log.Errorf("alerting: panic delivering to a %s channel: %v", channel.Kind, recovered)
			result.Status, result.Error = DeliveryFailed, "internal error"
		}
		result.Duration = time.Since(started)
	}()

	ctx, cancel := context.WithTimeout(ctx, deliveryTimeout)
	defer cancel()
	req, err := buildRequest(ctx, channel, msg, d.now())
	if err != nil {
		result.Status, result.Error = DeliveryFailed, transportReason(err)
		return result
	}
	client := d.proxied
	if channel.Kind == KindWebhook && !trusted {
		client = d.direct
	}
	resp, err := client.Do(req)
	if err != nil {
		result.Status, result.Error = DeliveryFailed, transportReason(err)
		return result
	}
	defer func() { _ = resp.Body.Close() }()
	if err := checkResponse(channel.Kind, resp.StatusCode, readLimited(resp.Body)); err != nil {
		result.Status, result.Error = DeliveryFailed, transportReason(err)
	}
	return result
}
