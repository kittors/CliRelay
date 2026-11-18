package executor

import (
	"crypto/tls"
	"net/http"
	"net/http/httptrace"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/util"
)

type upstreamTraceTimings struct {
	Host          string `json:"host,omitempty"`
	Scheme        string `json:"scheme,omitempty"`
	GotConnReused bool   `json:"got_conn_reused"`
	WasIdle       bool   `json:"was_idle,omitempty"`
	IdleMs        int64  `json:"idle_ms,omitempty"`
	ConnectMs     int64  `json:"connect_ms,omitempty"`
	TLSMs         int64  `json:"tls_ms,omitempty"`
	HeaderTTFBMs  int64  `json:"header_ttfb_ms,omitempty"`

	getConnAt              time.Time
	gotConnAt              time.Time
	connectStartAt         time.Time
	connectDoneAt          time.Time
	tlsHandshakeStartAt    time.Time
	tlsHandshakeDoneAt     time.Time
	gotFirstResponseByteAt time.Time
	requestStart           time.Time
}

const upstreamTimingKey = "cliproxy.request_log.upstream_timing"

func (t *upstreamTraceTimings) clientTrace() *httptrace.ClientTrace {
	return &httptrace.ClientTrace{
		GetConn: func(hostPort string) {
			t.getConnAt = time.Now()
		},
		GotConn: func(info httptrace.GotConnInfo) {
			t.gotConnAt = time.Now()
			t.GotConnReused = info.Reused
			t.WasIdle = info.WasIdle
			if info.WasIdle {
				t.IdleMs = info.IdleTime.Milliseconds()
			}
		},
		ConnectStart: func(_, _ string) {
			t.connectStartAt = time.Now()
		},
		ConnectDone: func(_, _ string, err error) {
			t.connectDoneAt = time.Now()
			if err == nil && !t.connectStartAt.IsZero() {
				t.ConnectMs = t.connectDoneAt.Sub(t.connectStartAt).Milliseconds()
			}
		},
		TLSHandshakeStart: func() {
			t.tlsHandshakeStartAt = time.Now()
		},
		TLSHandshakeDone: func(_ tls.ConnectionState, err error) {
			t.tlsHandshakeDoneAt = time.Now()
			if err == nil && !t.tlsHandshakeStartAt.IsZero() {
				t.TLSMs = t.tlsHandshakeDoneAt.Sub(t.tlsHandshakeStartAt).Milliseconds()
			}
		},
		GotFirstResponseByte: func() {
			t.gotFirstResponseByteAt = time.Now()
			if !t.gotConnAt.IsZero() {
				t.HeaderTTFBMs = t.gotFirstResponseByteAt.Sub(t.gotConnAt).Milliseconds()
			}
		},
	}
}

type upstreamTracingTransport struct {
	base   http.RoundTripper
	ginCtx *gin.Context
}

func (t *upstreamTracingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	ginCtx := t.ginCtx
	if ginCtx == nil {
		ginCtx, _ = req.Context().Value(util.ContextKeyGin).(*gin.Context)
	}

	trace := &upstreamTraceTimings{requestStart: time.Now()}
	if req.URL != nil {
		trace.Host = req.URL.Host
		trace.Scheme = req.URL.Scheme
	}

	traceCtx := httptrace.WithClientTrace(req.Context(), trace.clientTrace())
	reqWithTrace := req.WithContext(traceCtx)

	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	resp, err := base.RoundTrip(reqWithTrace)

	if ginCtx != nil {
		storeUpstreamTiming(ginCtx, trace)
	}

	return resp, err
}

func storeUpstreamTiming(ginCtx *gin.Context, trace *upstreamTraceTimings) {
	if ginCtx == nil || trace == nil {
		return
	}
	ginCtx.Set(upstreamTimingKey, trace)
}

func upstreamTimingFromContext(ginCtx *gin.Context) map[string]any {
	if ginCtx == nil {
		return nil
	}
	value, exists := ginCtx.Get(upstreamTimingKey)
	if !exists {
		return nil
	}
	trace, ok := value.(*upstreamTraceTimings)
	if !ok || trace == nil {
		return nil
	}

	result := map[string]any{}
	if trace.Host != "" {
		result["host"] = trace.Host
	}
	if trace.Scheme != "" {
		result["scheme"] = trace.Scheme
	}
	result["got_conn_reused"] = trace.GotConnReused
	if trace.WasIdle {
		result["was_idle"] = true
		result["idle_ms"] = trace.IdleMs
	}
	if trace.ConnectMs > 0 {
		result["connect_ms"] = trace.ConnectMs
	}
	if trace.TLSMs > 0 {
		result["tls_ms"] = trace.TLSMs
	}
	if trace.HeaderTTFBMs > 0 {
		result["header_ttfb_ms"] = trace.HeaderTTFBMs
	}
	if len(result) == 0 {
		return nil
	}
	return result
}
