package executor

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"testing/iotest"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

var rateLimitNow = time.Unix(1_800_000_000, 0)

// quotaSink stands in for the scheduler's observer.
type quotaSink struct {
	mu  sync.Mutex
	got []cliproxyexecutor.QuotaObservation
}

func (s *quotaSink) observe(o cliproxyexecutor.QuotaObservation) {
	s.mu.Lock()
	s.got = append(s.got, o)
	s.mu.Unlock()
}

func (s *quotaSink) all() []cliproxyexecutor.QuotaObservation {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]cliproxyexecutor.QuotaObservation(nil), s.got...)
}

func withQuotaSink(ctx context.Context) (context.Context, *quotaSink) {
	sink := &quotaSink{}
	return cliproxyexecutor.WithQuotaObserver(ctx, sink.observe), sink
}

func rateHeaders(pairs ...string) http.Header {
	h := http.Header{}
	for i := 0; i+1 < len(pairs); i += 2 {
		h.Set(pairs[i], pairs[i+1])
	}
	return h
}

type wantWindow struct {
	used    float64
	minutes int
	label   string
	reset   time.Time
}

func checkWindows(t *testing.T, got []cliproxyexecutor.QuotaWindowObservation, want []wantWindow) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("windows = %+v, want %d of them (%+v)", got, len(want), want)
	}
	for i := range want {
		g, w := got[i], want[i]
		if g.UsedPercent != w.used || g.WindowMinutes != w.minutes || g.Label != w.label || !g.ResetAt.Equal(w.reset) {
			t.Errorf("window %d = {used:%v minutes:%d label:%q reset:%v}, want %+v", i, g.UsedPercent, g.WindowMinutes, g.Label, g.ResetAt, w)
		}
	}
}

func secs(n int64) time.Time { return rateLimitNow.Add(time.Duration(n) * time.Second) }

func TestParseCodexRateLimitHeaders(t *testing.T) {
	const week = 7 * 24 * 3600
	cases := []struct {
		name        string
		header      http.Header
		want        []wantWindow
		wantOverage bool
	}{
		{
			name: "both windows as the official client reads them",
			header: rateHeaders(
				"x-codex-primary-used-percent", "42", "x-codex-primary-window-minutes", "300", "x-codex-primary-reset-at", "1800003600",
				"x-codex-secondary-used-percent", "7", "x-codex-secondary-window-minutes", "10080", "x-codex-secondary-reset-at", "1800259200",
			),
			want: []wantWindow{{42, 300, "5h", secs(3600)}, {7, 10080, "week", secs(259200)}},
		},
		{
			name:   "only the primary window",
			header: rateHeaders("x-codex-primary-used-percent", "12.5", "x-codex-primary-window-minutes", "300", "x-codex-primary-reset-at", "1800000600"),
			want:   []wantWindow{{12.5, 300, "5h", secs(600)}},
		},
		{
			name:   "only the secondary window",
			header: rateHeaders("x-codex-secondary-used-percent", "80", "x-codex-secondary-window-minutes", "10080", "x-codex-secondary-reset-at", "1800086400"),
			want:   []wantWindow{{80, 10080, "week", secs(86400)}},
		},
		{
			name:   "reset given as seconds from now",
			header: rateHeaders("x-codex-primary-used-percent", "100", "x-codex-primary-window-minutes", "300", "x-codex-primary-reset-after-seconds", "5400"),
			want:   []wantWindow{{100, 300, "5h", secs(5400)}},
		},
		{
			name: "an absolute reset wins over seconds-from-now",
			header: rateHeaders("x-codex-primary-used-percent", "100", "x-codex-primary-window-minutes", "300",
				"x-codex-primary-reset-at", "1800001000", "x-codex-primary-reset-after-seconds", "5400"),
			want: []wantWindow{{100, 300, "5h", secs(1000)}},
		},
		{
			name:   "a window length that is neither 5h nor a week keeps its minutes as the label",
			header: rateHeaders("x-codex-primary-used-percent", "30", "x-codex-primary-window-minutes", "60", "x-codex-primary-reset-at", "1800000600"),
			want:   []wantWindow{{30, 60, "60m", secs(600)}},
		},
		{
			name:   "usage past 100 is clamped",
			header: rateHeaders("x-codex-primary-used-percent", "130", "x-codex-primary-window-minutes", "300", "x-codex-primary-reset-at", "1800000600"),
			want:   []wantWindow{{100, 300, "5h", secs(600)}},
		},
		{
			name:   "zero percent is a real reading when the window is described",
			header: rateHeaders("x-codex-primary-used-percent", "0", "x-codex-primary-window-minutes", "300", "x-codex-primary-reset-at", "1800018000"),
			want:   []wantWindow{{0, 300, "5h", secs(18000)}},
		},
		{
			name:   "zero percent with nothing else is an empty window and is left out",
			header: rateHeaders("x-codex-primary-used-percent", "0"),
		},
		{
			name:   "a window without a length is still reported, nameless",
			header: rateHeaders("x-codex-primary-used-percent", "55", "x-codex-primary-reset-at", "1800000600"),
			want:   []wantWindow{{55, 0, "", secs(600)}},
		},
		{
			name:   "a reset in the past is passed on as it is; the scheduler ignores it",
			header: rateHeaders("x-codex-primary-used-percent", "100", "x-codex-primary-window-minutes", "300", "x-codex-primary-reset-at", "1799990000"),
			want:   []wantWindow{{100, 300, "5h", secs(-10000)}},
		},
		{
			name:   "a reset beyond any plan window is treated as no reset",
			header: rateHeaders("x-codex-primary-used-percent", "100", "x-codex-primary-window-minutes", "300", "x-codex-primary-reset-at", "1810000000"),
			want:   []wantWindow{{100, 300, "5h", time.Time{}}},
		},
		{
			name:   "a reset in milliseconds is not a reason to park an account for millennia",
			header: rateHeaders("x-codex-primary-used-percent", "100", "x-codex-primary-window-minutes", "300", "x-codex-primary-reset-at", "1800003600000"),
			want:   []wantWindow{{100, 300, "5h", time.Time{}}},
		},
		{
			name:   "a reset exactly at the horizon is believed",
			header: rateHeaders("x-codex-primary-used-percent", "100", "x-codex-primary-window-minutes", "10080", "x-codex-primary-reset-at", "1803024000"),
			want:   []wantWindow{{100, 10080, "week", rateLimitNow.Add(codexRateLimitMaxResetHorizon)}},
		},
		{
			name:   "seconds-from-now beyond the horizon is dropped",
			header: rateHeaders("x-codex-primary-used-percent", "100", "x-codex-primary-window-minutes", "300", "x-codex-primary-reset-after-seconds", "99999999"),
			want:   []wantWindow{{100, 300, "5h", time.Time{}}},
		},
		{
			name:   "zero or negative seconds-from-now is no reset",
			header: rateHeaders("x-codex-primary-used-percent", "100", "x-codex-primary-window-minutes", "300", "x-codex-primary-reset-after-seconds", "-5"),
			want:   []wantWindow{{100, 300, "5h", time.Time{}}},
		},
		{
			name:   "a nonsense window length is ignored but the usage is not",
			header: rateHeaders("x-codex-primary-used-percent", "70", "x-codex-primary-window-minutes", "banana", "x-codex-primary-reset-at", "1800000600"),
			want:   []wantWindow{{70, 0, "", secs(600)}},
		},
		{
			name:   "a window longer than a year is not believed",
			header: rateHeaders("x-codex-primary-used-percent", "70", "x-codex-primary-window-minutes", "999999999", "x-codex-primary-reset-at", "1800000600"),
			want:   []wantWindow{{70, 0, "", secs(600)}},
		},
		{
			name:   "a negative window length is ignored",
			header: rateHeaders("x-codex-primary-used-percent", "70", "x-codex-primary-window-minutes", "-300", "x-codex-primary-reset-at", "1800000600"),
			want:   []wantWindow{{70, 0, "", secs(600)}},
		},
		{
			name: "a window that cannot be read does not spoil the other one",
			header: rateHeaders("x-codex-primary-used-percent", "NaN", "x-codex-primary-window-minutes", "300", "x-codex-primary-reset-at", "1800000600",
				"x-codex-secondary-used-percent", "5", "x-codex-secondary-window-minutes", "10080", "x-codex-secondary-reset-at", "1800086400"),
			want: []wantWindow{{5, 10080, "week", secs(86400)}},
		},
		{name: "NaN usage", header: rateHeaders("x-codex-primary-used-percent", "NaN", "x-codex-primary-window-minutes", "300")},
		{name: "infinite usage", header: rateHeaders("x-codex-primary-used-percent", "+Inf", "x-codex-primary-window-minutes", "300")},
		{name: "negative usage", header: rateHeaders("x-codex-primary-used-percent", "-1", "x-codex-primary-window-minutes", "300")},
		{name: "unparsable usage", header: rateHeaders("x-codex-primary-used-percent", "lots", "x-codex-primary-window-minutes", "300")},
		{name: "empty usage", header: rateHeaders("x-codex-primary-used-percent", "", "x-codex-primary-window-minutes", "300")},
		{name: "overflowing usage", header: rateHeaders("x-codex-primary-used-percent", "1e999", "x-codex-primary-window-minutes", "300")},
		{name: "no rate limit headers at all", header: http.Header{"Content-Type": []string{"text/event-stream"}}},
		{name: "nil headers", header: nil},
		{
			name: "limits metered on their own are not the account's limit",
			header: rateHeaders(
				"x-codex-bengalfox-primary-used-percent", "100", "x-codex-bengalfox-primary-window-minutes", "300", "x-codex-bengalfox-primary-reset-at", "1800003600",
				"x-codex-other-secondary-used-percent", "100", "x-codex-other-secondary-window-minutes", "10080",
			),
		},
		{
			name: "the main limit is still read next to a separately metered one",
			header: rateHeaders(
				"x-codex-primary-used-percent", "20", "x-codex-primary-window-minutes", "300", "x-codex-primary-reset-at", "1800003600",
				"x-codex-bengalfox-primary-used-percent", "100", "x-codex-bengalfox-primary-window-minutes", "300", "x-codex-bengalfox-primary-reset-at", "1800003600",
			),
			want: []wantWindow{{20, 300, "5h", secs(3600)}},
		},
		{
			name: "credits available",
			header: rateHeaders("x-codex-primary-used-percent", "100", "x-codex-primary-window-minutes", "300", "x-codex-primary-reset-at", "1800003600",
				"x-codex-credits-has-credits", "true"),
			want:        []wantWindow{{100, 300, "5h", secs(3600)}},
			wantOverage: true,
		},
		{
			name: "credits written as 1",
			header: rateHeaders("x-codex-primary-used-percent", "100", "x-codex-primary-window-minutes", "300", "x-codex-primary-reset-at", "1800003600",
				"x-codex-credits-has-credits", "1"),
			want:        []wantWindow{{100, 300, "5h", secs(3600)}},
			wantOverage: true,
		},
		{
			name: "unlimited credits",
			header: rateHeaders("x-codex-primary-used-percent", "100", "x-codex-primary-window-minutes", "300", "x-codex-primary-reset-at", "1800003600",
				"x-codex-credits-has-credits", "false", "x-codex-credits-unlimited", "TRUE"),
			want:        []wantWindow{{100, 300, "5h", secs(3600)}},
			wantOverage: true,
		},
		{
			name: "no credits",
			header: rateHeaders("x-codex-primary-used-percent", "100", "x-codex-primary-window-minutes", "300", "x-codex-primary-reset-at", "1800003600",
				"x-codex-credits-has-credits", "false", "x-codex-credits-unlimited", "false"),
			want: []wantWindow{{100, 300, "5h", secs(3600)}},
		},
		{
			name: "credit headers that make no sense mean no credits",
			header: rateHeaders("x-codex-primary-used-percent", "100", "x-codex-primary-window-minutes", "300", "x-codex-primary-reset-at", "1800003600",
				"x-codex-credits-has-credits", "maybe", "x-codex-credits-unlimited", "yes"),
			want: []wantWindow{{100, 300, "5h", secs(3600)}},
		},
		{
			name:   "credits alone say nothing about windows",
			header: rateHeaders("x-codex-credits-has-credits", "true"),
		},
		{
			name:   "a week-long window in seconds-from-now form",
			header: rateHeaders("x-codex-secondary-used-percent", "9", "x-codex-secondary-window-minutes", "10080", "x-codex-secondary-reset-after-seconds", "604800"),
			want:   []wantWindow{{9, 10080, "week", secs(week)}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := parseCodexRateLimitHeaders(tc.header, rateLimitNow)
			if ok != (len(tc.want) > 0) {
				t.Fatalf("ok = %v with windows %+v, want windows %+v", ok, got.Windows, tc.want)
			}
			if !ok {
				if len(got.Windows) != 0 || got.OverageAvailable {
					t.Fatalf("a refused parse must return nothing, got %+v", got)
				}
				return
			}
			checkWindows(t, got.Windows, tc.want)
			if got.OverageAvailable != tc.wantOverage {
				t.Errorf("OverageAvailable = %v, want %v", got.OverageAvailable, tc.wantOverage)
			}
			if !got.ObservedAt.Equal(rateLimitNow) {
				t.Errorf("ObservedAt = %v, want the time of the answer", got.ObservedAt)
			}
		})
	}
}

const rateLimitEvent = `{"type":"codex.rate_limits","plan_type":"plus","rate_limits":{"allowed":true,"limit_reached":false,` +
	`"primary":{"used_percent":42,"window_minutes":300,"reset_after_seconds":3600,"reset_at":1800003600},` +
	`"secondary":{"used_percent":7.5,"window_minutes":10080,"reset_after_seconds":259200,"reset_at":1800259200}},` +
	`"credits":{"has_credits":false,"balance":null,"unlimited":false}}`

func TestParseCodexRateLimitEvent(t *testing.T) {
	cases := []struct {
		name        string
		payload     string
		want        []wantWindow
		wantOverage bool
	}{
		{
			name:    "the event as the service sends it",
			payload: rateLimitEvent,
			want:    []wantWindow{{42, 300, "5h", secs(3600)}, {7.5, 10080, "week", secs(259200)}},
		},
		{
			name:    "reset given as seconds from now",
			payload: `{"type":"codex.rate_limits","rate_limits":{"primary":{"used_percent":100,"window_minutes":300,"reset_after_seconds":900}}}`,
			want:    []wantWindow{{100, 300, "5h", secs(900)}},
		},
		{
			name:        "credits make the account keep serving",
			payload:     `{"type":"codex.rate_limits","rate_limits":{"primary":{"used_percent":100,"window_minutes":300,"reset_at":1800003600}},"credits":{"has_credits":true,"balance":"12"}}`,
			want:        []wantWindow{{100, 300, "5h", secs(3600)}},
			wantOverage: true,
		},
		{
			name:        "unlimited credits",
			payload:     `{"type":"codex.rate_limits","rate_limits":{"primary":{"used_percent":100,"window_minutes":300,"reset_at":1800003600}},"credits":{"unlimited":true}}`,
			want:        []wantWindow{{100, 300, "5h", secs(3600)}},
			wantOverage: true,
		},
		{
			name:    "the main limit named explicitly is still the main limit",
			payload: `{"type":"codex.rate_limits","metered_limit_name":"codex","rate_limits":{"primary":{"used_percent":20,"window_minutes":300,"reset_at":1800003600}}}`,
			want:    []wantWindow{{20, 300, "5h", secs(3600)}},
		},
		{
			name:    "the name's case does not matter",
			payload: `{"type":"codex.rate_limits","limit_name":"CODEX","rate_limits":{"primary":{"used_percent":20,"window_minutes":300,"reset_at":1800003600}}}`,
			want:    []wantWindow{{20, 300, "5h", secs(3600)}},
		},
		{
			name:    "a blank name is the main limit",
			payload: `{"type":"codex.rate_limits","metered_limit_name":"  ","rate_limits":{"primary":{"used_percent":20,"window_minutes":300,"reset_at":1800003600}}}`,
			want:    []wantWindow{{20, 300, "5h", secs(3600)}},
		},
		{
			name:    "a separately metered limit is not the account's limit",
			payload: `{"type":"codex.rate_limits","metered_limit_name":"codex_bengalfox","rate_limits":{"primary":{"used_percent":100,"window_minutes":300,"reset_at":1800003600}}}`,
		},
		{
			name:    "another model's allowance named by limit_name",
			payload: `{"type":"codex.rate_limits","limit_name":"gpt-5.3-codex-spark","rate_limits":{"primary":{"used_percent":100,"window_minutes":300,"reset_at":1800003600}}}`,
		},
		{
			name:    "a main-limit name next to a foreign one still refuses the event",
			payload: `{"type":"codex.rate_limits","limit_name":"codex","metered_limit_name":"codex_other","rate_limits":{"primary":{"used_percent":100,"window_minutes":300,"reset_at":1800003600}}}`,
		},
		{
			name:    "an event of another type that merely mentions the name",
			payload: `{"type":"response.output_text.delta","delta":"the codex.rate_limits event carries rate_limits"}`,
		},
		{
			name:    "no type at all",
			payload: `{"rate_limits":{"primary":{"used_percent":100,"window_minutes":300,"reset_at":1800003600}},"note":"codex.rate_limits"}`,
		},
		{
			name:    "usage written as text is not a number",
			payload: `{"type":"codex.rate_limits","rate_limits":{"primary":{"used_percent":"100","window_minutes":300,"reset_at":1800003600}}}`,
		},
		{
			name:    "negative usage",
			payload: `{"type":"codex.rate_limits","rate_limits":{"primary":{"used_percent":-3,"window_minutes":300,"reset_at":1800003600}}}`,
		},
		{
			name:    "a null window",
			payload: `{"type":"codex.rate_limits","rate_limits":{"primary":null,"secondary":{"used_percent":4,"window_minutes":10080,"reset_at":1800086400}}}`,
			want:    []wantWindow{{4, 10080, "week", secs(86400)}},
		},
		{
			name:    "a reset in milliseconds",
			payload: `{"type":"codex.rate_limits","rate_limits":{"primary":{"used_percent":100,"window_minutes":300,"reset_at":1800003600000}}}`,
			want:    []wantWindow{{100, 300, "5h", time.Time{}}},
		},
		{
			name:    "no rate_limits object",
			payload: `{"type":"codex.rate_limits","plan_type":"plus"}`,
		},
		{
			name:    "rate_limits that is not an object",
			payload: `{"type":"codex.rate_limits","rate_limits":"none"}`,
		},
		{
			name:    "empty windows",
			payload: `{"type":"codex.rate_limits","rate_limits":{"primary":{},"secondary":{}}}`,
		},
		{name: "not json", payload: `codex.rate_limits {{{`},
		{name: "empty payload", payload: ``},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := parseCodexRateLimitEvent([]byte(tc.payload), rateLimitNow)
			if ok != (len(tc.want) > 0) {
				t.Fatalf("ok = %v with windows %+v, want windows %+v", ok, got.Windows, tc.want)
			}
			if !ok {
				return
			}
			checkWindows(t, got.Windows, tc.want)
			if got.OverageAvailable != tc.wantOverage {
				t.Errorf("OverageAvailable = %v, want %v", got.OverageAvailable, tc.wantOverage)
			}
		})
	}
}

func TestIsCodexMainRateLimitName(t *testing.T) {
	for name, want := range map[string]bool{
		"codex": true, "Codex": true, " CODEX ": true,
		"": false, "codex-other": false, "codex_other": false, "codex_bengalfox": false, "gpt-5.3-codex-spark": false, "codexx": false,
	} {
		if got := isCodexMainRateLimitName(name); got != want {
			t.Errorf("isCodexMainRateLimitName(%q) = %v, want %v", name, got, want)
		}
	}
}

// --- the stream tee -----------------------------------------------------

func sseEvent(data string) string { return "data: " + data + "\n\n" }

func rateLimitEventAt(used int, reset int64) string {
	return `{"type":"codex.rate_limits","rate_limits":{"primary":{"used_percent":` + strconv.Itoa(used) +
		`,"window_minutes":300,"reset_at":` + strconv.FormatInt(reset, 10) + `}}}`
}

type closeRecorder struct {
	io.Reader
	closed bool
}

func (c *closeRecorder) Close() error { c.closed = true; return nil }

func readAllThrough(t *testing.T, stream string, wrap func(io.Reader) io.Reader) ([]byte, *quotaSink) {
	t.Helper()
	ctx, sink := withQuotaSink(context.Background())
	body := &codexRateLimitBody{ReadCloser: io.NopCloser(wrap(strings.NewReader(stream))), ctx: ctx}
	out, err := io.ReadAll(body)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	return out, sink
}

func TestCodexRateLimitBodyPassesTheStreamThroughUntouched(t *testing.T) {
	bigDelta := strings.Repeat("x", 40_000)
	stream := strings.Join([]string{
		"event: response.created\n" + sseEvent(`{"type":"response.created"}`),
		"event: codex.rate_limits\n" + sseEvent(rateLimitEventAt(42, 1800003600)),
		"event: response.output_text.delta\n" + sseEvent(`{"type":"response.output_text.delta","delta":"`+bigDelta+`"}`),
		sseEvent(rateLimitEventAt(100, 1800007200)),
		"data: [DONE]\n\n",
	}, "")

	wraps := map[string]func(io.Reader) io.Reader{
		"whole":              func(r io.Reader) io.Reader { return r },
		"one byte at a time": iotest.OneByteReader,
		"half reads":         iotest.HalfReader,
		"data then EOF":      iotest.DataErrReader,
		"seven byte reads":   func(r io.Reader) io.Reader { return &chunkReader{r: r, n: 7} },
		"odd sized reads":    func(r io.Reader) io.Reader { return &chunkReader{r: r, n: 1021} },
	}
	for name, wrap := range wraps {
		t.Run(name, func(t *testing.T) {
			out, sink := readAllThrough(t, stream, wrap)
			if string(out) != stream {
				t.Fatalf("the downstream received different bytes (%d vs %d)", len(out), len(stream))
			}
			got := sink.all()
			if len(got) != 2 {
				t.Fatalf("observations = %d, want the two rate-limit events (however the stream is cut up)", len(got))
			}
			if g := got[0].Windows[0]; g.UsedPercent != 42 || g.WindowMinutes != 300 {
				t.Errorf("first observation = %+v", g)
			}
			if g := got[1].Windows[0]; g.UsedPercent != 100 {
				t.Errorf("second observation = %+v", g)
			}
		})
	}
}

// chunkReader returns at most n bytes per Read, whatever the caller asks for.
type chunkReader struct {
	r io.Reader
	n int
}

func (c *chunkReader) Read(p []byte) (int, error) {
	if len(p) > c.n {
		p = p[:c.n]
	}
	return c.r.Read(p)
}

func TestCodexRateLimitBodyReadsEventsInTheShapesStreamsComeIn(t *testing.T) {
	event := rateLimitEventAt(55, 1800003600)
	cases := []struct {
		name   string
		stream string
		want   int
	}{
		{"CRLF line endings", "event: codex.rate_limits\r\ndata: " + event + "\r\n\r\n", 1},
		{"no blank line and no final newline", "data: " + event, 1},
		{"no space after the colon", "data:" + event + "\n\n", 1},
		{"indented data line", "  data: " + event + "\n\n", 1},
		{"an event line alone says nothing", "event: codex.rate_limits\n\n", 0},
		{"a comment mentioning the event", ": codex.rate_limits\n\n", 0},
		{"the marker inside ordinary content", sseEvent(`{"type":"response.output_text.delta","delta":"codex.rate_limits"}`), 0},
		{"a malformed payload", sseEvent(`{"type":"codex.rate_limits","rate_limits":`), 0},
		{"an event for a separately metered limit", sseEvent(`{"type":"codex.rate_limits","metered_limit_name":"codex_other","rate_limits":{"primary":{"used_percent":100,"window_minutes":300,"reset_at":1800003600}}}`), 0},
		{"two in a row", sseEvent(event) + sseEvent(event), 2},
		{"empty stream", "", 0},
		{"just newlines", "\n\n\n", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, sink := readAllThrough(t, tc.stream, iotest.OneByteReader)
			if string(out) != tc.stream {
				t.Fatalf("bytes changed: %q", out)
			}
			if got := len(sink.all()); got != tc.want {
				t.Fatalf("observations = %d, want %d", got, tc.want)
			}
		})
	}
}

// A line longer than the bound is content (a big delta), not an event: it must
// cost nothing to carry and must never be mistaken for one, even when it embeds
// text that looks exactly like an event.
func TestCodexRateLimitBodyDoesNotHoldOnToHugeLines(t *testing.T) {
	// A megabyte of content: far past the bound, so holding it would show in the buffer.
	embedded := strings.Repeat("y", 1<<20) + sseEvent(rateLimitEventAt(100, 1800003600))
	huge := "data: " + `{"type":"response.output_text.delta","delta":"` + strings.ReplaceAll(embedded, "\n", " ") + `"}` + "\n\n"
	after := sseEvent(rateLimitEventAt(61, 1800003600))

	ctx, sink := withQuotaSink(context.Background())
	body := &codexRateLimitBody{ReadCloser: io.NopCloser(&chunkReader{r: strings.NewReader(huge + after), n: 4096}), ctx: ctx}
	out, err := io.ReadAll(body)
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != huge+after {
		t.Fatal("bytes changed")
	}
	if cap(body.line) > 2*codexRateLimitMaxEventLine {
		t.Fatalf("line buffer grew to %d bytes", cap(body.line))
	}
	got := sink.all()
	if len(got) != 1 || got[0].Windows[0].UsedPercent != 61 {
		t.Fatalf("observations = %+v, want only the event after the huge line", got)
	}
}

func TestCodexRateLimitBodyReadsAnEventJustUnderTheBound(t *testing.T) {
	padding := strings.Repeat("p", codexRateLimitMaxEventLine-400)
	event := `{"type":"codex.rate_limits","padding":"` + padding + `","rate_limits":{"primary":{"used_percent":77,"window_minutes":300,"reset_at":1800003600}}}`
	if len("data: "+event) >= codexRateLimitMaxEventLine {
		t.Fatalf("test event is %d bytes, over the bound", len(event))
	}
	_, sink := readAllThrough(t, sseEvent(event), func(r io.Reader) io.Reader { return &chunkReader{r: r, n: 513} })
	if got := sink.all(); len(got) != 1 || got[0].Windows[0].UsedPercent != 77 {
		t.Fatalf("observations = %+v", got)
	}
}

type failingReader struct {
	data []byte
	err  error
}

func (f *failingReader) Read(p []byte) (int, error) {
	if len(f.data) == 0 {
		return 0, f.err
	}
	n := copy(p, f.data)
	f.data = f.data[n:]
	return n, nil
}

func TestCodexRateLimitBodyPassesReadErrorsAndCloseThrough(t *testing.T) {
	boom := errors.New("connection reset by peer")
	ctx, sink := withQuotaSink(context.Background())
	rec := &closeRecorder{Reader: &failingReader{data: []byte(sseEvent(rateLimitEventAt(30, 1800003600))), err: boom}}
	body := &codexRateLimitBody{ReadCloser: rec, ctx: ctx}

	out, err := io.ReadAll(body)
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the transport's own error", err)
	}
	if !bytes.HasPrefix(out, []byte("data: ")) {
		t.Fatalf("bytes read before the error were lost: %q", out)
	}
	if got := sink.all(); len(got) != 1 {
		t.Fatalf("the event that arrived before the error was not read: %d observations", len(got))
	}
	if err := body.Close(); err != nil || !rec.closed {
		t.Fatalf("Close err=%v closed=%v: it must reach the connection", err, rec.closed)
	}
}

// The observer runs on the request path, so a slow or panicking-free fast path
// matters; this pins that a body with no events costs one pass and no allocation
// growth beyond the line buffer.
func TestCodexRateLimitBodyWithoutEventsDoesNotCallTheObserver(t *testing.T) {
	var stream strings.Builder
	for i := 0; i < 500; i++ {
		stream.WriteString(sseEvent(`{"type":"response.output_text.delta","delta":"token ` + strconv.Itoa(i) + `"}`))
	}
	out, sink := readAllThrough(t, stream.String(), func(r io.Reader) io.Reader { return r })
	if string(out) != stream.String() {
		t.Fatal("bytes changed")
	}
	if len(sink.all()) != 0 {
		t.Fatal("the observer was called for a stream without rate-limit events")
	}
}

// --- websocket helpers --------------------------------------------------

func TestObserveCodexWebsocketMessage(t *testing.T) {
	event := []byte(rateLimitEvent)
	cases := []struct {
		name    string
		backend bool
		payload []byte
		want    int
	}{
		{"event on the ChatGPT backend", true, event, 1},
		{"the same event from another host is someone else's allowance", false, event, 0},
		{"an ordinary message", true, []byte(`{"type":"response.output_text.delta","delta":"hi"}`), 0},
		{"a separately metered limit", true, []byte(`{"type":"codex.rate_limits","metered_limit_name":"codex_other","rate_limits":{"primary":{"used_percent":100,"window_minutes":300,"reset_at":1800003600}}}`), 0},
		{"an empty message", true, nil, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx, sink := withQuotaSink(context.Background())
			observeCodexWebsocketMessage(ctx, tc.backend, tc.payload)
			if got := len(sink.all()); got != tc.want {
				t.Fatalf("observations = %d, want %d", got, tc.want)
			}
		})
	}
	// With nobody listening the message is not even parsed.
	observeCodexWebsocketMessage(context.Background(), true, event)
}

func TestObserveCodexWebsocketHandshake(t *testing.T) {
	full := &http.Response{StatusCode: http.StatusSwitchingProtocols, Header: rateHeaders(
		"x-codex-primary-used-percent", "100", "x-codex-primary-window-minutes", "300", "x-codex-primary-reset-at", "1800003600")}

	t.Run("ChatGPT backend", func(t *testing.T) {
		ctx, sink := withQuotaSink(context.Background())
		observeCodexWebsocketHandshake(ctx, true, full)
		got := sink.all()
		if len(got) != 1 || got[0].Windows[0].UsedPercent != 100 {
			t.Fatalf("observations = %+v", got)
		}
	})
	t.Run("another host", func(t *testing.T) {
		ctx, sink := withQuotaSink(context.Background())
		observeCodexWebsocketHandshake(ctx, false, full)
		if len(sink.all()) != 0 {
			t.Fatal("a relay's headers describe someone else's allowance")
		}
	})
	t.Run("a failed dial has no response", func(t *testing.T) {
		ctx, sink := withQuotaSink(context.Background())
		observeCodexWebsocketHandshake(ctx, true, nil)
		if len(sink.all()) != 0 {
			t.Fatal("observed a nil response")
		}
	})
	t.Run("nobody listening", func(t *testing.T) {
		observeCodexWebsocketHandshake(context.Background(), true, full) // must not panic
	})
}
