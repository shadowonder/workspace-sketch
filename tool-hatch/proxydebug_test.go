package main

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// bodyUpstream echoes the request body size and answers with the body named by
// ?size= (known length), or binary bytes for /binary.
func bodyUpstream(t *testing.T) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received, _ := io.ReadAll(r.Body)
		w.Header().Set("X-Received", strconv.Itoa(len(received)))
		if r.URL.Path == "/binary" {
			w.Header().Set("Content-Type", "application/octet-stream")
			_, _ = w.Write([]byte{0, 1, 2, 0xff, 0xfe})
			return
		}
		size, _ := strconv.Atoi(r.URL.Query().Get("size"))
		body := `{"answer":"` + strings.Repeat("r", size) + `"}`
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(server.Close)
	return server
}

func debugProxy(t *testing.T, target, script string) (*ProxyService, string, *[]string) {
	t.Helper()
	clipboard := &[]string{}
	var mu sync.Mutex
	s := &ProxyService{clipboard: func(text string) bool {
		mu.Lock()
		defer mu.Unlock()
		*clipboard = append(*clipboard, text)
		return true
	}}
	status := s.Start(ProxyConfig{ListenPort: freePort(t), TargetURL: target, Script: script})
	if !status.Running {
		t.Fatal(status.Message)
	}
	t.Cleanup(func() { s.Stop() })
	return s, status.Address, clipboard
}

func post(t *testing.T, url, body string, header ...string) *http.Response {
	t.Helper()
	request, _ := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	for i := 0; i+1 < len(header); i += 2 {
		request.Header.Set(header[i], header[i+1])
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, response.Body)
	response.Body.Close()
	return response
}

func retainedBytes(s *ProxyService) (int64, int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var sum int64
	for _, r := range s.records {
		if r.capture != nil {
			sum += r.capture.bytes
		}
	}
	return sum, s.debugBytes
}

func lastEntry(t *testing.T, s *ProxyService) ProxyActivity {
	t.Helper()
	activity := s.Status().Activity
	if len(activity) == 0 {
		t.Fatal("no activity")
	}
	return activity[len(activity)-1]
}

func TestProxyDebugOffCapturesNothing(t *testing.T) {
	upstream := bodyUpstream(t)
	s, address, clipboard := debugProxy(t, upstream.URL, "")
	if status := s.Status(); status.Debug || status.DebugLimitBytes != proxyDebugBudget || status.ActivityLimit != proxyActivityDefault {
		t.Fatalf("defaults: %+v", status)
	}
	post(t, address+"/p?size=10", "secret-body", "Authorization", "Bearer secret-token")
	entry := lastEntry(t, s)
	if entry.Debug || entry.Pending || entry.Status != 200 {
		t.Fatalf("entry: %+v", entry)
	}
	if view := s.Capture(entry.ID); view.State != "none" || !strings.Contains(view.Note, "Debug was off") || view.Request.Body.Preview != "" {
		t.Fatalf("view: %+v", view)
	}
	if copied := s.CopyCapture(entry.ID, "requestBody"); copied.Success || len(*clipboard) != 0 {
		t.Fatal("copy without capture")
	}
	if _, total := retainedBytes(s); total != 0 {
		t.Fatalf("debug off retained %d bytes", total)
	}
	raw, _ := json.Marshal(s.Status())
	if strings.Contains(string(raw), "secret") {
		t.Fatal("status exposes request data")
	}
}

func TestProxyDebugCapturesTransformedExchange(t *testing.T) {
	upstream := bodyUpstream(t)
	s, address, clipboard := debugProxy(t, upstream.URL, `
		function onRequest(req) { req.setHeader('X-Added', 'yes'); req.body = req.body.toUpperCase(); }
		function onResponse(res) { res.setHeader('X-Res', 'changed'); if (res.bodyAvailable) res.body = res.body.replace('"answer"', '"final"'); }`)
	s.SetDebug(true)
	big := strings.Repeat("q", 100<<10)
	response := post(t, address+"/v1/responses?size=20000", big, "Authorization", "Bearer secret-token", "Content-Type", "text/plain")
	if response.Header.Get("X-Received") != strconv.Itoa(len(big)) {
		t.Fatal("request not forwarded intact")
	}
	entry := lastEntry(t, s)
	if !entry.Debug || entry.Pending {
		t.Fatalf("entry: %+v", entry)
	}
	view := s.Capture(entry.ID)
	if !view.Success || view.State != "complete" || view.Request.Line != "POST /v1/responses?size=20000" || view.Response.Line != "200 OK" {
		t.Fatalf("view: state=%s note=%s req=%q res=%q", view.State, view.Note, view.Request.Line, view.Response.Line)
	}
	headers := view.Request.Headers.Preview
	if !strings.Contains(headers, "X-Added: yes") || !strings.Contains(headers, "Host: "+strings.TrimPrefix(upstream.URL, "http://")) ||
		strings.Contains(headers, "secret-token") || !strings.Contains(headers, "Authorization: ••••") {
		t.Fatalf("request headers preview: %q", headers)
	}
	body := view.Request.Body
	if body.Size != int64(len(big)) || body.Observed != body.Size || body.State != "complete" || !body.Truncated ||
		len(body.Preview) != capturePreviewLimit || body.Preview != strings.Repeat("Q", capturePreviewLimit) {
		t.Fatalf("request body: %+v", ProxyCapturePart{Size: body.Size, State: body.State, Truncated: body.Truncated})
	}
	wantResponse := `{"final":"` + strings.Repeat("r", 20000) + `"}`
	if view.Response.Body.Size != int64(len(wantResponse)) || !strings.Contains(view.Response.Headers.Preview, "X-Res: changed") || view.Response.Body.State != "complete" {
		t.Fatalf("response: %+v", view.Response.Headers.Preview)
	}
	raw, _ := json.Marshal(view)
	if len(raw) > 6*capturePreviewLimit || strings.Contains(string(raw), "secret-token") {
		t.Fatalf("Inspector payload too large or leaks credentials: %d bytes", len(raw))
	}
	for part, want := range map[string]string{"requestBody": strings.ToUpper(big), "responseBody": wantResponse} {
		copied := s.CopyCapture(entry.ID, part)
		if !copied.Success || !copied.Complete || copied.Base64 || (*clipboard)[len(*clipboard)-1] != want {
			t.Fatalf("%s copy: %+v", part, copied)
		}
	}
	if copied := s.CopyCapture(entry.ID, "requestHeaders"); !copied.Success || !strings.Contains((*clipboard)[len(*clipboard)-1], "Authorization: Bearer secret-token") {
		t.Fatal("explicit header copy must include the full values")
	}
	if unknown := s.CopyCapture(entry.ID, "everything"); unknown.Success {
		t.Fatal("unknown part accepted")
	}

	// Binary content is flagged and copied as base64.
	post(t, address+"/binary", "")
	binary := lastEntry(t, s)
	if view := s.Capture(binary.ID); !view.Response.Body.Binary || view.Response.Body.Preview != "" || view.Response.Body.Size != 5 {
		t.Fatalf("binary view: %+v", view.Response.Body)
	}
	if copied := s.CopyCapture(binary.ID, "responseBody"); !copied.Base64 || (*clipboard)[len(*clipboard)-1] != base64.StdEncoding.EncodeToString([]byte{0, 1, 2, 0xff, 0xfe}) {
		t.Fatalf("binary copy: %+v", copied)
	}
	// Script failures are recorded but nothing is forwarded or captured.
	s.Stop()
	s2, address2, _ := debugProxy(t, upstream.URL, `function onRequest(req) { throw new Error('nope'); }`)
	s2.SetDebug(true)
	post(t, address2+"/x", "body")
	if entry := lastEntry(t, s2); entry.Debug || entry.Status != 500 {
		t.Fatalf("failed request: %+v", entry)
	}
}

func TestProxyDebugStreamsAndCapturesSSE(t *testing.T) {
	release := make(chan struct{})
	upstreamDone := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(upstreamDone)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		_, _ = io.WriteString(w, "data: one\n\n")
		w.(http.Flusher).Flush()
		<-release
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer upstream.Close()
	var once sync.Once
	defer once.Do(func() { close(release) }) // never leave the upstream blocked on failure
	s, address, clipboard := debugProxy(t, upstream.URL, `function onResponse(res) { res.setHeader('X-Hooked', '1'); }`)
	s.SetDebug(true)
	response := getStream(t, address+"/v1/responses")
	defer response.Body.Close()
	reader := bufio.NewReader(response.Body)
	timer := time.AfterFunc(3*time.Second, func() { response.Body.Close() })
	if line, _ := reader.ReadString('\n'); line != "data: one\n" {
		t.Fatalf("first event delayed or lost: %q", line)
	}
	timer.Stop()
	entry := lastEntry(t, s)
	if !entry.Pending || !entry.Debug {
		t.Fatalf("open stream: %+v", entry)
	}
	view := s.Capture(entry.ID)
	if view.State != "capturing" || view.Response.Body.State != "capturing" || view.Response.Body.Preview != "data: one\n\n" || !strings.Contains(view.Response.Headers.Preview, "X-Hooked: 1") {
		t.Fatalf("live capture: %+v %+v", view.State, view.Response.Body)
	}
	if copied := s.CopyCapture(entry.ID, "responseBody"); !copied.Success || copied.Complete || !strings.Contains(copied.Message, "incomplete") {
		t.Fatalf("copy of a live stream must not claim completeness: %+v", copied)
	}
	once.Do(func() { close(release) })
	<-upstreamDone
	if rest, _ := io.ReadAll(reader); string(rest) != "\ndata: [DONE]\n\n" {
		t.Fatalf("rest: %q", rest)
	}
	eventually(t, "stream entry to complete", func() bool { return !lastEntry(t, s).Pending })
	view = s.Capture(entry.ID)
	if view.State != "complete" || view.Response.Body.Size != int64(len("data: one\n\ndata: [DONE]\n\n")) {
		t.Fatalf("final: %s %+v", view.State, view.Response.Body)
	}
	if copied := s.CopyCapture(entry.ID, "responseBody"); !copied.Complete || (*clipboard)[len(*clipboard)-1] != "data: one\n\ndata: [DONE]\n\n" {
		t.Fatalf("final copy: %+v", copied)
	}
}

func TestProxyDebugRetention(t *testing.T) {
	upstream := bodyUpstream(t)
	s, address, _ := debugProxy(t, upstream.URL, "")
	s.SetDebug(true)
	if status := s.SetActivityLimit(150); status.Success || status.ActivityLimit != proxyActivityDefault {
		t.Fatal("invalid retention accepted")
	}
	s.SetActivityLimit(100)
	for i := 0; i < 105; i++ {
		post(t, address+"/r", "x")
	}
	status := s.Status()
	if len(status.Activity) != 100 || status.Activity[0].ID != 6 || status.Requests != 105 {
		t.Fatalf("count eviction: %d entries, first %d", len(status.Activity), status.Activity[0].ID)
	}
	if view := s.Capture(1); view.Success {
		t.Fatal("evicted entry still inspectable")
	}
	if sum, total := retainedBytes(s); sum != total || total == 0 {
		t.Fatalf("accounting: retained %d, charged %d", sum, total)
	}

	// Memory budget: the oldest completed captures are released first.
	s.ClearActivity()
	old := proxyDebugBudget
	proxyDebugBudget = 600 << 10
	defer func() { proxyDebugBudget = old }()
	for i := 0; i < 6; i++ {
		post(t, address+"/m?size=10", strings.Repeat("m", 100<<10))
	}
	sum, total := retainedBytes(s)
	if total > proxyDebugBudget || sum != total {
		t.Fatalf("budget exceeded: %d > %d", total, proxyDebugBudget)
	}
	activity := s.Status().Activity
	first, latest := s.Capture(activity[0].ID), s.Capture(activity[len(activity)-1].ID)
	if first.State != "released" || !strings.Contains(first.Note, "memory budget") || latest.State != "complete" {
		t.Fatalf("eviction order: first=%s latest=%s", first.State, latest.State)
	}

	// A single exchange larger than the whole budget is cut and marked
	// incomplete, while the client still receives everything.
	s.ClearActivity()
	huge := strings.Repeat("h", 1<<20)
	response := post(t, address+"/huge?size=10", huge)
	if response.Header.Get("X-Received") != strconv.Itoa(len(huge)) {
		t.Fatal("forwarding was affected by capture limits")
	}
	view := s.Capture(lastEntry(t, s).ID)
	if view.State != "incomplete" || view.Request.Body.State != "incomplete" || view.Request.Body.Observed != int64(len(huge)) ||
		view.Request.Body.Size >= int64(len(huge)) || !strings.Contains(view.Request.Body.Note, "budget") {
		t.Fatalf("oversized: %s %+v", view.State, view.Request.Body)
	}
	if copied := s.CopyCapture(view.Entry.ID, "requestBody"); copied.Complete || !strings.Contains(copied.Message, "not the full original body") {
		t.Fatalf("partial copy: %+v", copied)
	}
	if sum, total := retainedBytes(s); total > proxyDebugBudget || sum != total {
		t.Fatalf("budget exceeded: %d", total)
	}
}

func TestProxyDebugSwitchAndLifecycle(t *testing.T) {
	release := make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		_, _ = io.WriteString(w, "data: "+r.URL.Path+"\n\n")
		w.(http.Flusher).Flush()
		if r.URL.Path == "/hold" {
			select {
			case <-release:
			case <-r.Context().Done():
			}
		}
	}))
	defer upstream.Close()
	s, address, _ := debugProxy(t, upstream.URL, "")

	// A request admitted before Debug was enabled is never captured.
	before, err := http.Get(address + "/hold")
	if err != nil {
		t.Fatal(err)
	}
	defer before.Body.Close()
	_, _ = bufio.NewReader(before.Body).ReadString('\n')
	s.SetDebug(true)
	get := func(path string) {
		r, err := http.Get(address + path)
		if err == nil {
			_, _ = io.ReadAll(r.Body)
			r.Body.Close()
		}
	}
	get("/on")
	s.SetDebug(false)
	get("/off")
	once.Do(func() { close(release) })
	eventually(t, "held stream to finish", func() bool { return s.Status().Requests == 3 })
	byPath := map[string]ProxyActivity{}
	for _, entry := range s.Status().Activity {
		byPath[entry.Path] = entry
	}
	if byPath["/hold"].Debug || !byPath["/on"].Debug || byPath["/off"].Debug {
		t.Fatalf("debug sampling: %+v", byPath)
	}
	if view := s.Capture(byPath["/on"].ID); view.State != "complete" {
		t.Fatal("disabling Debug must keep retained captures")
	}

	// Clear releases everything.
	s.SetDebug(true)
	get("/a")
	s.ClearActivity()
	if status := s.Status(); len(status.Activity) != 0 || status.DebugBytes != 0 {
		t.Fatalf("clear: %+v", status)
	}

	// Stop releases captures; a late write from the stopped run cannot repopulate.
	get("/b")
	id := lastEntry(t, s).ID
	s.mu.Lock()
	capture := s.recordLocked(id).capture
	s.mu.Unlock()
	s.Stop()
	if status := s.Status(); status.DebugBytes != 0 || s.Capture(id).State != "released" {
		t.Fatalf("stop: %+v", status)
	}
	s.captureWrite(capture, &capture.response.body, []byte("late"))
	if _, total := retainedBytes(s); total != 0 || capture.bytes != 0 {
		t.Fatal("late write repopulated a released capture")
	}
	// Restart: fresh storage; a ticket of the earlier run records nothing.
	status := s.Start(ProxyConfig{ListenPort: freePort(t), TargetURL: upstream.URL})
	if len(status.Activity) != 0 || !status.Debug {
		t.Fatalf("restart: %+v", status)
	}
	s.mu.Lock()
	stale := proxyTicket{run: s.run - 1, epoch: s.epoch, debug: true}
	s.mu.Unlock()
	ex := s.begin(stale, ProxyActivity{Method: "GET", Path: "/stale"}, true)
	s.finish(ex, "GET", "/stale", 200, 0, "")
	if status := s.Status(); len(status.Activity) != 0 || status.Requests != 0 {
		t.Fatalf("stale run repopulated activity: %+v", status)
	}
	// A request admitted before Clear does not start a capture after it.
	ticket := s.admit(s.runID())
	s.ClearActivity()
	if ex := s.begin(ticket, ProxyActivity{Path: "/raced"}, true); ex.capture != nil {
		t.Fatal("capture created in cleared storage")
	}
}

func (s *ProxyService) runID() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.run
}

func TestProxyDebugConcurrentRequestsAndClear(t *testing.T) {
	upstream := bodyUpstream(t)
	s, address, _ := debugProxy(t, upstream.URL, `function onResponse(res) { res.setHeader('X-N', res.request.path); }`)
	s.SetDebug(true)
	old := proxyDebugBudget
	proxyDebugBudget = 1 << 20
	defer func() { proxyDebugBudget = old }()
	stop := make(chan struct{})
	var pollers sync.WaitGroup
	pollers.Add(1)
	go func() { // concurrent Status/Capture/Clear while requests run
		defer pollers.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			for _, entry := range s.Status().Activity {
				s.Capture(entry.ID)
			}
			if i%7 == 0 {
				s.ClearActivity()
			}
		}
	}()
	var wg sync.WaitGroup
	for i := 0; i < 30; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			request, _ := http.NewRequest(http.MethodPost, fmt.Sprintf("%s/c%d?size=50000", address, i), strings.NewReader(strings.Repeat("c", 80<<10)))
			response, err := http.DefaultClient.Do(request)
			if err != nil {
				t.Error(err)
				return
			}
			data, _ := io.ReadAll(response.Body)
			response.Body.Close()
			if len(data) != 50000+len(`{"answer":""}`) || response.Header.Get("X-N") != fmt.Sprintf("/c%d", i) {
				t.Errorf("request %d: %d bytes", i, len(data))
			}
		}(i)
	}
	wg.Wait()
	close(stop)
	pollers.Wait()
	sum, total := retainedBytes(s)
	if sum != total || total > proxyDebugBudget {
		t.Fatalf("accounting after concurrent use: retained %d, charged %d", sum, total)
	}
	for _, entry := range s.Status().Activity {
		if entry.Pending {
			t.Fatalf("pending entry left behind: %+v", entry)
		}
	}
}
