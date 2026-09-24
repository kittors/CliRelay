package dnswatch

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
)

const (
	testZoneID = "0123456789abcdef0123456789abcdef"
	// Deliberately not shaped like a real Cloudflare token.
	testToken = "test-cf-token-XXXXXXXXXXXXXXXXXXXXXXXXXX"
)

// fakeCloudflare implements the slice of the zone DNS records API dnswatch uses.
type fakeCloudflare struct {
	t      *testing.T
	server *httptest.Server

	mu      sync.Mutex
	records map[string]dnsRecord
	nextID  int
	// writes lists successful POST/DELETE calls in order, e.g.
	// "POST relay.dnswatch.test 203.0.113.12".
	writes   []string
	requests []string
	faults   []cfFault
	// ignoreTypeFilter returns every record with the requested name, as a
	// defensive check that dnswatch filters record types itself.
	ignoreTypeFilter bool
	// pageSize > 0 splits list replies into pages of that size.
	pageSize int
	// echoAuth copies the Authorization header into injected error replies.
	echoAuth bool
	// createLandsBeforeFault applies a POST before replying with the fault,
	// like a request whose reply was lost after Cloudflare stored the record.
	createLandsBeforeFault bool
}

type cfFault struct {
	method    string
	remaining int // < 0 means until cleared
	status    int
	code      int
	message   string
}

func newFakeCloudflare(t *testing.T) *fakeCloudflare {
	t.Helper()
	f := &fakeCloudflare{t: t, records: map[string]dnsRecord{}}
	f.server = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.server.Close)
	return f
}

func (f *fakeCloudflare) seed(name, recordType, content string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.insertLocked(dnsRecord{Type: recordType, Name: name, Content: content, TTL: 300})
}

func (f *fakeCloudflare) insertLocked(rec dnsRecord) string {
	f.nextID++
	rec.ID = fmt.Sprintf("rec%03d", f.nextID)
	f.records[rec.ID] = rec
	return rec.ID
}

// fail makes the next count calls with method (or any method when "")
// return status; count < 0 keeps failing until clearFaults.
func (f *fakeCloudflare) fail(method string, count, status int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.faults = append(f.faults, cfFault{method: method, remaining: count, status: status, code: 10000, message: "injected failure"})
}

func (f *fakeCloudflare) clearFaults() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.faults = nil
}

// aIPs returns the sorted A record contents published under name.
func (f *fakeCloudflare) aIPs(name string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var ips []string
	for _, rec := range f.records {
		if rec.Type == "A" && rec.Name == name {
			ips = append(ips, rec.Content)
		}
	}
	slices.Sort(ips)
	return ips
}

func (f *fakeCloudflare) has(id string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.records[id]
	return ok
}

func (f *fakeCloudflare) record(id string) dnsRecord {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.records[id]
}

func (f *fakeCloudflare) writeLog() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.writes)
}

func (f *fakeCloudflare) requestLog() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.requests)
}

func (f *fakeCloudflare) resetLogs() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.writes, f.requests = nil, nil
}

func (f *fakeCloudflare) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, r.Method+" "+r.URL.Path)

	if r.Header.Get("Authorization") != "Bearer "+testToken {
		writeCFError(w, http.StatusForbidden, 9109, "Invalid access token")
		return
	}
	collection := "/zones/" + testZoneID + "/dns_records"

	fault := f.takeFaultLocked(r.Method)
	if fault != nil && !(f.createLandsBeforeFault && r.Method == http.MethodPost) {
		f.writeFaultLocked(w, r, fault)
		return
	}

	switch {
	case r.Method == http.MethodGet && r.URL.Path == collection:
		f.listLocked(w, r)
	case r.Method == http.MethodPost && r.URL.Path == collection:
		f.createLocked(w, r, fault)
	case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, collection+"/"):
		f.deleteLocked(w, strings.TrimPrefix(r.URL.Path, collection+"/"))
	default:
		writeCFError(w, http.StatusNotFound, 7003, "Could not route to "+r.URL.Path)
	}
}

func (f *fakeCloudflare) takeFaultLocked(method string) *cfFault {
	for i := range f.faults {
		fault := &f.faults[i]
		if fault.remaining == 0 || (fault.method != "" && fault.method != method) {
			continue
		}
		if fault.remaining > 0 {
			fault.remaining--
		}
		copied := *fault
		return &copied
	}
	return nil
}

func (f *fakeCloudflare) writeFaultLocked(w http.ResponseWriter, r *http.Request, fault *cfFault) {
	message := fault.message
	if f.echoAuth {
		message += " (received " + r.Header.Get("Authorization") + ")"
	}
	writeCFError(w, fault.status, fault.code, message)
}

func (f *fakeCloudflare) listLocked(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	name, recordType := query.Get("name"), query.Get("type")
	var matched []dnsRecord
	for _, rec := range f.records {
		if !strings.EqualFold(rec.Name, name) {
			continue
		}
		if !f.ignoreTypeFilter && recordType != "" && rec.Type != recordType {
			continue
		}
		matched = append(matched, rec)
	}
	slices.SortFunc(matched, func(a, b dnsRecord) int { return strings.Compare(a.ID, b.ID) })

	page, totalPages := 1, 1
	if f.pageSize > 0 && len(matched) > 0 {
		page, _ = strconv.Atoi(query.Get("page"))
		page = max(page, 1)
		totalPages = (len(matched) + f.pageSize - 1) / f.pageSize
		start := min((page-1)*f.pageSize, len(matched))
		matched = matched[start:min(start+f.pageSize, len(matched))]
	}
	if matched == nil {
		matched = []dnsRecord{}
	}
	writeCFJSON(w, http.StatusOK, map[string]any{
		"success":     true,
		"errors":      []any{},
		"result":      matched,
		"result_info": map[string]int{"page": page, "total_pages": totalPages},
	})
}

func (f *fakeCloudflare) createLocked(w http.ResponseWriter, r *http.Request, fault *cfFault) {
	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeCFError(w, http.StatusBadRequest, 9207, "Request body is invalid")
		return
	}
	// dnswatch must send exactly the documented shape.
	if body["type"] != "A" || body["proxied"] != false {
		f.t.Errorf("create request must be an unproxied A record, got %v", body)
	}
	for _, key := range []string{"type", "name", "content", "ttl", "proxied"} {
		if _, ok := body[key]; !ok {
			f.t.Errorf("create request lacks %q: %v", key, body)
		}
	}
	name, _ := body["name"].(string)
	content, _ := body["content"].(string)
	ttl, _ := body["ttl"].(float64)
	for _, rec := range f.records {
		if rec.Type == "A" && rec.Name == name && rec.Content == content {
			writeCFError(w, http.StatusBadRequest, 81058, "An identical record already exists.")
			return
		}
	}
	id := f.insertLocked(dnsRecord{Type: "A", Name: name, Content: content, TTL: int(ttl)})
	f.writes = append(f.writes, "POST "+name+" "+content)
	if fault != nil {
		// The record landed, but the client sees an error.
		f.writeFaultLocked(w, r, fault)
		return
	}
	writeCFJSON(w, http.StatusOK, map[string]any{"success": true, "errors": []any{}, "result": f.records[id]})
}

func (f *fakeCloudflare) deleteLocked(w http.ResponseWriter, id string) {
	rec, ok := f.records[id]
	if !ok {
		writeCFError(w, http.StatusNotFound, 81044, "Record does not exist.")
		return
	}
	delete(f.records, id)
	f.writes = append(f.writes, "DELETE "+rec.Name+" "+rec.Content)
	writeCFJSON(w, http.StatusOK, map[string]any{"success": true, "errors": []any{}, "result": map[string]string{"id": id}})
}

func writeCFError(w http.ResponseWriter, status, code int, message string) {
	writeCFJSON(w, status, map[string]any{
		"success": false,
		"errors":  []map[string]any{{"code": code, "message": message}},
		"result":  nil,
	})
}

func writeCFJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}
