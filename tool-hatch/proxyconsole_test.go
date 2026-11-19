package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"
)

func proxyGet(t *testing.T, url string) int {
	t.Helper()
	response, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, response.Body)
	response.Body.Close()
	return response.StatusCode
}

func TestProxyConsoleLevelsAndFormatting(t *testing.T) {
	upstream, _ := echoUpstream(t)
	s, address := startProxy(t, ProxyConfig{TargetURL: upstream.URL, Script: `
console.info('loaded');
function onRequest(req) {
  console.log('path', req.path, 42, true, null, undefined, { a: [1, 2] }, [3]);
  console.warn('careful');
  console.error(new TypeError('bad thing'));
  console.debug(function named() {});
  const loop = {}; loop.self = loop; console.log(loop);
  console.log();
}`})
	// Top-level code runs once when Start validates the script.
	if got := s.Console(0); len(got.Entries) != 1 || got.Entries[0].Level != "info" || got.Entries[0].Message != "loaded" {
		t.Fatalf("validation output: %+v", got.Entries)
	}
	if proxyGet(t, address+"/x?token=secret") != http.StatusCreated {
		t.Fatal("request not forwarded")
	}
	got := s.Console(1)
	want := [][2]string{
		{"info", "loaded"}, // top level runs again in the request's fresh runtime
		{"log", `path /x 42 true null undefined {"a":[1,2]} [3]`},
		{"warn", "careful"},
		{"error", "TypeError: bad thing"},
		{"debug", "function named() {}"},
		{"log", "[object Object]"},
		{"log", ""},
	}
	if len(got.Entries) != len(want) {
		t.Fatalf("entries: %+v", got.Entries)
	}
	for i, entry := range got.Entries {
		if entry.Level != want[i][0] || entry.Message != want[i][1] || entry.ID != int64(i+2) {
			t.Errorf("entry %d = %+v, want %v", i, entry, want[i])
		}
		if _, err := time.Parse("2006-01-02T15:04:05.000Z07:00", entry.Time); err != nil {
			t.Errorf("timestamp %q: %v", entry.Time, err)
		}
	}
	if got.LastID != 8 || got.Truncated || s.Status().ConsoleLastID != 8 {
		t.Fatalf("cursor: %+v", got)
	}
	for _, entry := range got.Entries {
		if strings.Contains(entry.Message, "secret") {
			t.Fatal("request data logged automatically")
		}
	}
}

func TestProxyConsoleIsIndependentOfActivityAndLifecycle(t *testing.T) {
	upstream, _ := echoUpstream(t)
	port := freePort(t)
	s := &ProxyService{}
	config := ProxyConfig{ListenPort: port, TargetURL: upstream.URL, Script: `function onRequest(req) { console.log('seen ' + req.method); }`}
	if !s.Start(config).Running {
		t.Fatal("start failed")
	}
	proxyGet(t, fmt.Sprintf("http://127.0.0.1:%d/a", port))
	s.Stop()
	if got := s.Console(0); len(got.Entries) != 1 || got.Entries[0].Message != "seen GET" {
		t.Fatalf("console lost on stop: %+v", got)
	}
	if !s.Start(config).Running {
		t.Fatal("restart failed")
	}
	defer s.Stop()
	if status := s.Status(); len(status.Activity) != 0 || status.ConsoleLastID != 1 {
		t.Fatalf("restart should reset activity only: %+v", status)
	}
	cleared := s.ClearConsole()
	if len(cleared.Entries) != 0 || cleared.LastID != 1 || s.Status().Requests != 0 {
		t.Fatalf("clear: %+v", cleared)
	}
	proxyGet(t, fmt.Sprintf("http://127.0.0.1:%d/b", port))
	if got := s.Console(0); len(got.Entries) != 1 || got.Entries[0].ID != 2 || got.Truncated || s.Status().Requests != 1 {
		t.Fatalf("after clear: %+v", got)
	}
	// A plain proxy without console calls produces no console output.
	plain, plainAddress := startProxy(t, ProxyConfig{TargetURL: upstream.URL})
	proxyGet(t, plainAddress+"/c?token=secret")
	if got := plain.Console(0); len(got.Entries) != 0 || got.LastID != 0 || len(plain.Status().Activity) != 1 {
		t.Fatalf("unexpected console output: %+v", got)
	}
}

func TestProxyConsoleBufferBounds(t *testing.T) {
	var c proxyConsole
	for i := 1; i <= proxyConsoleDefaultLimit+5; i++ {
		c.add("log", fmt.Sprint(i))
	}
	all := c.since(0)
	if len(all.Entries) != proxyConsoleDefaultLimit || all.Entries[0].Message != "6" || !all.Truncated || all.LastID != proxyConsoleDefaultLimit+5 || all.Limit != proxyConsoleDefaultLimit {
		t.Fatalf("ring: first=%+v len=%d truncated=%v", all.Entries[0], len(all.Entries), all.Truncated)
	}
	if recent := c.since(int64(proxyConsoleDefaultLimit)); len(recent.Entries) != 5 || recent.Truncated {
		t.Fatalf("incremental: %+v", recent)
	}
	if none := c.since(all.LastID); len(none.Entries) != 0 || none.Entries == nil {
		t.Fatal("empty fetch must be an empty list")
	}
	// Entry-count choices: lowering evicts the oldest at once; invalid values are ignored.
	if got := c.setLimit(100); got.Limit != 100 || len(c.since(0).Entries) != 100 || c.since(0).Entries[0].Message != "406" {
		t.Fatalf("setLimit(100): %+v", got.Limit)
	}
	if got := c.setLimit(123); got.Limit != 100 {
		t.Fatal("invalid limit accepted")
	}
	c.setLimit(proxyConsoleDefaultLimit)
	c.clear()

	// A message far beyond the old 4 KiB cap is retained in full; only the
	// preview sent to the frontend is short (and cut on a UTF-8 boundary).
	long := strings.Repeat("é", 300<<10) + "END"
	c.add("log", long)
	got := c.since(0)
	entry := got.Entries[0]
	if got.Truncated || len(got.Entries) != 1 || entry.Size != len(long) || !entry.PreviewTruncated || entry.Incomplete {
		t.Fatalf("entry: %+v", entry)
	}
	if len(entry.Message) > proxyConsolePreviewLimit || !strings.HasPrefix(entry.Message, "éé") || !utf8.ValidString(entry.Message) {
		t.Fatalf("preview: len=%d", len(entry.Message))
	}
	if full, incomplete, ok := c.full(entry.ID); !ok || incomplete || full != long {
		t.Fatal("full message not retained")
	}
	if short := (&proxyConsole{}); true {
		short.add("info", "small")
		if e := short.since(0).Entries[0]; e.PreviewTruncated || e.Message != "small" || e.Size != 5 {
			t.Fatalf("short entry: %+v", e)
		}
	}
}

// The aggregate byte budget evicts the oldest messages; a single message larger
// than the budget is kept cut and explicitly marked incomplete.
func TestProxyConsoleMemoryBudget(t *testing.T) {
	old := proxyConsoleBudget
	proxyConsoleBudget = 1 << 20
	defer func() { proxyConsoleBudget = old }()
	var c proxyConsole
	chunk := strings.Repeat("a", 300<<10)
	for i := 0; i < 5; i++ {
		c.add("log", fmt.Sprint(i)+chunk)
	}
	got := c.since(0)
	total := 0
	for _, e := range got.Entries {
		total += e.Size
	}
	if len(got.Entries) != 3 || got.Entries[0].Message[:1] != "2" || total > proxyConsoleBudget || !got.Truncated {
		t.Fatalf("budget eviction: %d entries, %d bytes", len(got.Entries), total)
	}
	c.add("error", strings.Repeat("b", 2<<20))
	got = c.since(0)
	last := got.Entries[len(got.Entries)-1]
	if len(got.Entries) != 1 || !last.Incomplete || last.Size != proxyConsoleBudget {
		t.Fatalf("oversized entry: %d entries, %+v", len(got.Entries), last.Size)
	}
	if full, incomplete, ok := c.full(last.ID); !ok || !incomplete || len(full) != proxyConsoleBudget {
		t.Fatal("oversized entry must be retained cut and marked incomplete")
	}
	if _, _, ok := c.full(1); ok {
		t.Fatal("evicted entry still retrievable")
	}
}

// CopyConsole copies the complete message through the Go clipboard by ID,
// while Console() only ever carries the preview.
func TestProxyConsoleCopyFull(t *testing.T) {
	var clipboard []string
	s := &ProxyService{clipboard: func(text string) bool { clipboard = append(clipboard, text); return true }}
	long := strings.Repeat("0123456789", 10<<10) // 100 KiB
	s.console.add("log", long)
	response := s.Console(0)
	raw, _ := json.Marshal(response)
	if len(raw) > 4*proxyConsolePreviewLimit || strings.Contains(string(raw), long) {
		t.Fatalf("Console response carries the full message (%d bytes)", len(raw))
	}
	copied := s.CopyConsole(response.Entries[0].ID)
	if !copied.Success || !copied.Complete || copied.Bytes != int64(len(long)) || len(clipboard) != 1 || clipboard[0] != long {
		t.Fatalf("copy: %+v", copied)
	}
	if missing := s.CopyConsole(999); missing.Success || len(clipboard) != 1 {
		t.Fatal("copy of a missing entry must fail without touching the clipboard")
	}
	s.ClearConsole()
	if again := s.CopyConsole(response.Entries[0].ID); again.Success {
		t.Fatal("cleared entry still copyable")
	}
	s.clipboard = func(string) bool { return false }
	s.console.add("log", "x")
	if failed := s.CopyConsole(s.Console(0).Entries[0].ID); failed.Success || !strings.Contains(failed.Message, "clipboard") {
		t.Fatal("clipboard failure not reported")
	}
}

func TestProxyConsoleConcurrentRequestsAndTimeout(t *testing.T) {
	upstream, requests := echoUpstream(t)
	go func() { // echoUpstream's channel is bounded; keep it drained
		for range requests {
		}
	}()
	s, address := startProxy(t, ProxyConfig{TargetURL: upstream.URL, Script: `function onRequest(req) { console.log(req.query('n')); }`})
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			proxyGet(t, fmt.Sprintf("%s/p?n=%d", address, i))
		}(i)
	}
	wg.Wait()
	got := s.Console(0)
	seen := map[string]bool{}
	for i, entry := range got.Entries {
		seen[entry.Message] = true
		if entry.ID != int64(i+1) {
			t.Fatal("IDs not strictly increasing")
		}
	}
	if len(seen) != 20 {
		t.Fatalf("expected 20 distinct entries, got %d", len(seen))
	}
	s.Stop()

	old := proxyScriptTimeout
	proxyScriptTimeout = 200 * time.Millisecond
	defer func() { proxyScriptTimeout = old }()
	loud, loudAddress := startProxy(t, ProxyConfig{TargetURL: upstream.URL, Script: `function onRequest(req) { for (;;) console.log({ big: 'x'.repeat(10000) }); }`})
	if status := proxyGet(t, loudAddress+"/spin"); status != http.StatusInternalServerError {
		t.Fatalf("console loop escaped the time limit: %d", status)
	}
	// Eviction itself is covered by TestProxyConsoleBufferBounds; here only the bounds.
	if got := loud.Console(0); len(got.Entries) == 0 || len(got.Entries) > proxyConsoleDefaultLimit || len(got.Entries[0].Message) > proxyConsolePreviewLimit {
		t.Fatalf("buffer not bounded: %d", len(got.Entries))
	}
}

// Regression for the native report "console is not defined": exercises the
// production path (NewProxyService → Start → real HTTP → handler → a fresh
// per-request Goja runtime) with the tab's default template plus a console
// call inside onRequest at line 12, as in the report.
func TestProxyConsoleOnRealRequestPath(t *testing.T) {
	upstream, requests := echoUpstream(t)
	template := `// onRequest runs before each request is forwarded. Leave the script empty for plain forwarding.
// req.method, req.path (writable), req.body (string; read/write, max 10 MiB)
// req.header(name), req.headers(), req.setHeader(name, value), req.removeHeader(name)
// req.query(name), req.setQuery(name, value), req.removeQuery(name)
// http.request({ url, method, headers, body, timeoutMs })
//   -> { status, statusText, headers, body, header(name), json() }
// Throwing an error rejects the request with HTTP 500 instead of forwarding it.
function onRequest(req) {
  // Example: obtain a token and attach it.
  // const r = http.request({ method: 'POST', url: 'https://sso.example.com/token', body: { client: 'cli' } });
  // req.setHeader('Authorization', 'Bearer ' + r.json().access_token);
  %s
}
`
	s := NewProxyService() // the constructor main.go registers with Wails
	port := freePort(t)
	config := ProxyConfig{ListenPort: port, TargetURL: upstream.URL, Script: fmt.Sprintf(template, `console.log("haha")`)}
	if status := s.Start(config); !status.Running {
		t.Fatalf("start: %s", status.Message)
	}
	address := fmt.Sprintf("http://127.0.0.1:%d", port)
	for i := 1; i <= 3; i++ { // every request gets a newly created runtime
		if code := proxyGet(t, fmt.Sprintf("%s/r%d", address, i)); code != http.StatusCreated {
			t.Fatalf("request %d not forwarded: %d", i, code)
		}
		if got := <-requests; got.Path != fmt.Sprintf("/r%d", i) {
			t.Fatalf("upstream saw %q", got.Path)
		}
	}
	s.Stop()
	if !s.Start(config).Running { // runtimes created by a restarted server too
		t.Fatal("restart failed")
	}
	proxyGet(t, address+"/r4")
	<-requests
	got := s.Console(0)
	if len(got.Entries) != 4 || s.Status().ConsoleLastID != 4 {
		t.Fatalf("console entries: %+v", got.Entries)
	}
	for _, entry := range got.Entries {
		if entry.Level != "log" || entry.Message != "haha" {
			t.Fatalf("entry: %+v", entry)
		}
	}
	if activity := s.Status().Activity; len(activity) != 1 || activity[0].Error != "" || activity[0].Status != http.StatusCreated {
		t.Fatalf("console must not affect forwarding: %+v", activity)
	}
	s.Stop()

	// A misspelled identifier fails with its own name in the ReferenceError,
	// while console still exists in that same runtime.
	config.Script = fmt.Sprintf(template, `console.log("before"); concole.log("haha")`)
	if !s.Start(config).Running {
		t.Fatal("start failed")
	}
	defer s.Stop()
	response, err := http.Get(address + "/typo")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusInternalServerError || !strings.Contains(string(body), "ReferenceError: concole is not defined at onRequest (proxy.js:12:") {
		t.Fatalf("typo: %d %q", response.StatusCode, body)
	}
	if last := s.Console(4).Entries; len(last) != 1 || last[0].Message != "before" {
		t.Fatalf("console missing in failing runtime: %+v", last)
	}
}
