package main

import (
	"bufio"
	"compress/gzip"
	"context"
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

type hookResult struct {
	status int
	header http.Header
	body   string
}

func hookDo(t *testing.T, method, url, body string) hookResult {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	request, _ := http.NewRequest(method, url, reader)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	data, _ := io.ReadAll(response.Body)
	return hookResult{response.StatusCode, response.Header, string(data)}
}

// jsonUpstream answers with a known-length JSON body, two Set-Cookie values and
// echoes the X-Req header it received.
func jsonUpstream(t *testing.T) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := `{"ok":true}`
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		w.Header().Add("Set-Cookie", "a=1; Path=/")
		w.Header().Add("Set-Cookie", "b=2; Path=/")
		w.Header().Set("X-Seen-Req", r.Header.Get("X-Req"))
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(server.Close)
	return server
}

func TestProxyHookModes(t *testing.T) {
	upstream := jsonUpstream(t)
	cases := []struct {
		name, script         string
		wantSeenReq, wantRes string
	}{
		{"both", `function onRequest(req) { req.setHeader('X-Req', 'from-request'); context.set('k', 'shared-' + req.path); }
		          function onResponse(res) { res.setHeader('X-Res', context.get('k')); console.log('response saw', context.get('k')); }`, "from-request", "shared-/both"},
		{"request-only", `function onRequest(req) { req.setHeader('X-Req', 'only-request'); }`, "only-request", ""},
		{"response-only", `function onResponse(res) { res.setHeader('X-Res', 'only-response ' + res.request.method + ' ' + res.request.path); }`, "", "only-response GET /response-only"},
		{"neither", `console.log('top level'); var settings = { a: 1 };`, "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s, address := startProxy(t, ProxyConfig{TargetURL: upstream.URL, Script: c.script})
			got := hookDo(t, http.MethodGet, address+"/"+c.name, "")
			if got.status != 200 || got.body != `{"ok":true}` || got.header.Get("X-Seen-Req") != c.wantSeenReq || got.header.Get("X-Res") != c.wantRes {
				t.Fatalf("got %d %q seen=%q res=%q", got.status, got.body, got.header.Get("X-Seen-Req"), got.header.Get("X-Res"))
			}
			if cookies := got.header.Values("Set-Cookie"); len(cookies) != 2 || cookies[0] != "a=1; Path=/" || cookies[1] != "b=2; Path=/" {
				t.Fatalf("Set-Cookie changed: %q", cookies)
			}
			if got.header.Get("Content-Length") != "11" {
				t.Fatalf("framing changed: %q", got.header.Get("Content-Length"))
			}
			if c.name == "both" {
				if entries := s.Console(0).Entries; len(entries) == 0 || entries[len(entries)-1].Message != "response saw shared-/both" {
					t.Fatalf("console: %+v", entries)
				}
			}
			if c.name == "neither" {
				if n := len(s.Console(0).Entries); n != 1 { // only the Start validation run executes the top level
					t.Fatalf("plain forwarding still ran the script per request: %d entries", n)
				}
			}
		})
	}
}

func TestProxyResponseHookHeadersStatusAndBody(t *testing.T) {
	upstream := jsonUpstream(t)
	script := `function onResponse(res) {
		const p = res.request.path;
		if (p === '/cookies') {
			res.addHeader('Set-Cookie', 'c=3');
			res.setHeader('X-Count', String(res.headerValues('set-cookie').length));
			res.setHeader('X-Joined', res.header('Set-Cookie'));
			res.removeHeader('X-Seen-Req');
		}
		if (p === '/json') {
			const data = JSON.parse(res.body);
			data.added = 'é'.repeat(3);
			res.body = JSON.stringify(data);
			res.status = 201;
		}
		if (p === '/read-only') { res.setHeader('X-Len', String(res.body.length)); }
		if (p === '/no-content') { res.status = 204; }
		if (p === '/bad-status') { res.status = 99; }
		if (p === '/fraction') { res.status = 200.5; }
		if (p === '/length') { res.setHeader('Content-Length', '1'); }
		if (p === '/te') { res.removeHeader('Transfer-Encoding'); }
		if (p === '/too-big') { res.body = 'x'.repeat(1048577); }
		if (p === '/caught') { try { res.setHeader('Content-Length', '5'); } catch (e) { res.setHeader('X-Caught', e.name); } }
	}`
	_, address := startProxy(t, ProxyConfig{TargetURL: upstream.URL, Script: script})

	got := hookDo(t, http.MethodGet, address+"/cookies", "")
	if cookies := got.header.Values("Set-Cookie"); len(cookies) != 3 || cookies[2] != "c=3" || got.header.Get("X-Count") != "3" ||
		got.header.Get("X-Joined") != "a=1; Path=/, b=2; Path=/, c=3" || got.header.Get("X-Seen-Req") != "" || got.body != `{"ok":true}` {
		t.Fatalf("cookies: %q %+v", cookies, got.header)
	}
	got = hookDo(t, http.MethodGet, address+"/json", "")
	want := `{"ok":true,"added":"ééé"}`
	if got.status != 201 || got.body != want || got.header.Get("Content-Length") != strconv.Itoa(len(want)) {
		t.Fatalf("json: %d %q len=%q", got.status, got.body, got.header.Get("Content-Length"))
	}
	if got = hookDo(t, http.MethodGet, address+"/read-only", ""); got.body != `{"ok":true}` || got.header.Get("X-Len") != "11" || got.header.Get("Content-Length") != "11" {
		t.Fatalf("read-only body changed: %+v", got)
	}
	if got = hookDo(t, http.MethodGet, address+"/no-content", ""); got.status != 204 || got.body != "" || got.header.Get("Content-Length") != "" {
		t.Fatalf("204: %d %q", got.status, got.body)
	}
	if got = hookDo(t, http.MethodHead, address+"/json", ""); got.status != 500 || !strings.Contains(got.header.Get("X-ToolHatch-Proxy-Error"), "script") {
		t.Fatalf("HEAD body access must be an explicit script error: %d", got.status)
	}
	if got = hookDo(t, http.MethodGet, address+"/caught", ""); got.status != 200 || got.header.Get("X-Caught") != "TypeError" || got.header.Get("Content-Length") != "11" {
		t.Fatalf("caught guard: %+v", got)
	}
	for path, message := range map[string]string{
		"/bad-status": "invalid res.status",
		"/fraction":   "invalid res.status",
		"/length":     "Content-Length is managed by the proxy",
		"/te":         "Transfer-Encoding is managed by the proxy",
		"/too-big":    "exceeds the 1 MiB script limit",
	} {
		got := hookDo(t, http.MethodGet, address+path, "")
		if got.status != 500 || got.header.Get("X-ToolHatch-Proxy-Error") != "script" || !strings.Contains(got.body, message) || strings.Contains(got.body, `"ok"`) {
			t.Errorf("%s: %d %q", path, got.status, got.body)
		}
	}
}

func TestProxyResponseBodyPolicy(t *testing.T) {
	response := func(method string, status int, length int64, header ...string) *http.Response {
		h := http.Header{}
		for i := 0; i+1 < len(header); i += 2 {
			h.Set(header[i], header[i+1])
		}
		return &http.Response{StatusCode: status, ContentLength: length, Header: h, Request: httptest.NewRequest(method, "/", nil)}
	}
	cases := []struct {
		res  *http.Response
		want string
	}{
		{response("GET", 200, 10, "Content-Type", "application/json; charset=utf-8"), ""},
		{response("GET", 200, 10, "Content-Type", "text/plain"), ""},
		{response("GET", 200, 10, "Content-Type", "application/problem+json"), ""},
		{response("GET", 200, -1, "Content-Type", "application/json"), "streaming"},
		{response("GET", 200, 10, "Content-Type", "text/event-stream"), "only text and JSON"},
		{response("GET", 200, 10, "Content-Type", "application/x-ndjson"), "only text and JSON"},
		{response("GET", 200, 10, "Content-Type", "image/png"), "only text and JSON"},
		{response("GET", 200, 10), "only text and JSON"},
		{response("GET", 200, 10, "Content-Type", "application/json", "Content-Encoding", "gzip"), "compressed"},
		{response("GET", 200, 10, "Content-Type", "application/json", "Content-Encoding", "identity"), ""},
		{response("GET", 200, proxyResponseBodyLimit+1, "Content-Type", "application/json"), "1 MiB"},
		{response("GET", 206, 10, "Content-Type", "text/plain"), "partial"},
		{response("HEAD", 200, 10, "Content-Type", "text/plain"), "no body"},
		{response("GET", 204, 0), "no body"},
		{response("GET", 304, 0), "no body"},
		{response("GET", 101, -1), "no body"},
	}
	for i, c := range cases {
		if got := responseBodyUnavailable(c.res); (c.want == "") != (got == "") || !strings.Contains(got, c.want) {
			t.Errorf("case %d: got %q, want %q", i, got, c.want)
		}
	}

	// End to end: unavailable bodies are passed through untouched and reading
	// them is an explicit, catchable error.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/chunked":
			w.Header().Set("Content-Type", "text/plain")
			w.WriteHeader(200)
			_, _ = io.WriteString(w, "part-1 ")
			w.(http.Flusher).Flush()
			_, _ = io.WriteString(w, "part-2")
		case "/gzip":
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Content-Encoding", "gzip")
			zw := gzip.NewWriter(w)
			_, _ = io.WriteString(zw, `{"z":1}`)
			_ = zw.Close()
		}
	}))
	defer upstream.Close()
	_, address := startProxy(t, ProxyConfig{TargetURL: upstream.URL, Script: `function onResponse(res) {
		res.setHeader('X-Available', String(res.bodyAvailable));
		try { res.body; } catch (e) { res.setHeader('X-Reason', e.message.replace(/[^ -~]/g, '')); }
	}`})
	got := hookDo(t, http.MethodGet, address+"/chunked", "")
	if got.body != "part-1 part-2" || got.header.Get("X-Available") != "false" || !strings.Contains(got.header.Get("X-Reason"), "streaming or unknown-length") {
		t.Fatalf("chunked: %+v", got)
	}
	request, _ := http.NewRequest(http.MethodGet, address+"/gzip", nil)
	request.Header.Set("Accept-Encoding", "gzip")
	raw, err := http.DefaultTransport.RoundTrip(request)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Body.Close()
	zr, err := gzip.NewReader(raw.Body)
	if err != nil {
		t.Fatal(err)
	}
	if data, _ := io.ReadAll(zr); string(data) != `{"z":1}` || raw.Header.Get("X-Available") != "false" || !strings.Contains(raw.Header.Get("X-Reason"), "compressed") {
		t.Fatalf("gzip: %q %+v", data, raw.Header)
	}
}

// getStream fails fast if response headers do not arrive promptly (for
// example because something buffered the stream before forwarding it).
func getStream(t *testing.T, url string) *http.Response {
	t.Helper()
	type result struct {
		response *http.Response
		err      error
	}
	done := make(chan result, 1)
	go func() { r, err := http.Get(url); done <- result{r, err} }()
	select {
	case r := <-done:
		if r.err != nil {
			t.Fatal(r.err)
		}
		return r.response
	case <-time.After(3 * time.Second):
		t.Fatal("stream headers were not forwarded promptly")
		return nil
	}
}

func TestProxyResponseHookKeepsStreaming(t *testing.T) {
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		_, _ = io.WriteString(w, "data: first\n\n")
		w.(http.Flusher).Flush()
		select {
		case <-release:
			_, _ = io.WriteString(w, "data: [DONE]\n\n")
		case <-r.Context().Done():
		}
	}))
	defer upstream.Close()
	defer close(release)
	s, address := startProxy(t, ProxyConfig{TargetURL: upstream.URL, Script: `
		function onRequest(req) { context.set('model', 'gpt-x'); }
		function onResponse(res) {
			res.setHeader('X-Model', context.get('model'));
			res.setHeader('X-Body', res.bodyAvailable ? 'yes' : 'no');
			console.log('stream status', res.status);
		}`})
	response := getStream(t, address+"/v1/responses")
	defer response.Body.Close()
	if response.Header.Get("X-Model") != "gpt-x" || response.Header.Get("X-Body") != "no" || response.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("headers: %+v", response.Header)
	}
	reader := bufio.NewReader(response.Body)
	timer := time.AfterFunc(3*time.Second, func() { response.Body.Close() })
	if line, err := reader.ReadString('\n'); err != nil || line != "data: first\n" {
		t.Fatalf("first event not flushed before the stream completed: %q %v", line, err)
	}
	timer.Stop()
	if entries := s.Console(0).Entries; len(entries) != 1 || entries[0].Message != "stream status 200" {
		t.Fatalf("console: %+v", entries)
	}
	if status := s.Status(); len(status.Activity) != 1 || !status.Activity[0].Pending || status.Requests != 0 {
		t.Fatalf("open stream must be a pending entry: %+v", status.Activity)
	}
}

func TestProxyResponseHookFailuresAndCancellation(t *testing.T) {
	upstream := jsonUpstream(t)
	old := proxyScriptTimeout
	proxyScriptTimeout = 300 * time.Millisecond
	defer func() { proxyScriptTimeout = old }()
	s, address := startProxy(t, ProxyConfig{TargetURL: upstream.URL, Script: `function onResponse(res) {
		if (res.request.path === '/throw') throw new Error('denied ' + res.status);
		if (res.request.path === '/loop') for (;;) {}
		if (res.request.path === '/reject') return Promise.reject('nope');
	}`})
	for path, message := range map[string]string{"/throw": "denied 200", "/loop": "time limit", "/reject": "onResponse rejected: nope"} {
		got := hookDo(t, http.MethodGet, address+path, "")
		if got.status != 500 || got.header.Get("X-ToolHatch-Proxy-Error") != "script" || !strings.Contains(got.body, message) ||
			strings.Contains(got.body, `"ok"`) || len(got.header.Values("Set-Cookie")) != 0 {
			t.Errorf("%s: %d %q", path, got.status, got.body)
		}
	}
	status := s.Status()
	for _, entry := range status.Activity {
		if entry.Status != 500 || entry.Pending || (entry.Path == "/throw" && !strings.Contains(entry.Error, "denied 200")) {
			t.Fatalf("activity: %+v", entry)
		}
	}
	if status.Failures != 3 || len(status.Activity) != 3 {
		t.Fatalf("activity: %+v", status.Activity)
	}
	s.Stop()

	// Stop interrupts an onResponse blocked in an outbound call.
	hang := make(chan struct{})
	defer close(hang)
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-hang:
		case <-r.Context().Done():
		}
	}))
	defer slow.Close()
	proxyScriptTimeout = 20 * time.Second
	s2, address2 := startProxy(t, ProxyConfig{TargetURL: upstream.URL, Script: fmt.Sprintf(`function onResponse(res) { http.request({ url: %q }); }`, slow.URL)})
	done := make(chan int, 1)
	go func() {
		response, err := http.Get(address2 + "/wait")
		if err != nil {
			done <- 0
			return
		}
		response.Body.Close()
		done <- response.StatusCode
	}()
	eventually(t, "onResponse to block", func() bool { return len(s2.Status().Activity) == 1 })
	started := time.Now()
	s2.Stop()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("onResponse kept running after Stop")
	}
	if time.Since(started) > 3*time.Second {
		t.Fatal("Stop waited for onResponse")
	}
}

func TestProxyHooksConcurrentRequests(t *testing.T) {
	upstream := jsonUpstream(t)
	_, address := startProxy(t, ProxyConfig{TargetURL: upstream.URL, Script: `
		function onRequest(req) { req.setHeader('X-Req', req.query('n')); context.set('n' + req.query('n'), 'v' + req.query('n')); }
		function onResponse(res) {
			const n = res.header('X-Seen-Req');
			const data = JSON.parse(res.body); data.n = context.get('n' + n);
			res.body = JSON.stringify(data);
		}`})
	var wg sync.WaitGroup
	errs := make(chan string, 40)
	for i := 0; i < 40; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			request, _ := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("%s/c?n=%d", address, i), nil)
			response, err := http.DefaultClient.Do(request)
			if err != nil {
				errs <- err.Error()
				return
			}
			defer response.Body.Close()
			data, _ := io.ReadAll(response.Body)
			if want := fmt.Sprintf(`{"ok":true,"n":"v%d"}`, i); string(data) != want {
				errs <- string(data) + " != " + want
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}
