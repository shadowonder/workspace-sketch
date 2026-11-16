package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

type seen struct {
	Method, Path, RawPath, Query, Host, Body string
	Header                                   http.Header
	ContentLength                            int64
	TransferEncoding                         []string
}

// echoUpstream records each request it receives.
func echoUpstream(t *testing.T) (*httptest.Server, chan seen) {
	t.Helper()
	requests := make(chan seen, 16)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		requests <- seen{r.Method, r.URL.Path, r.URL.EscapedPath(), r.URL.RawQuery, r.Host, string(body), r.Header.Clone(), r.ContentLength, r.TransferEncoding}
		w.Header().Set("X-Upstream", "yes")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte("upstream-ok"))
	}))
	t.Cleanup(server.Close)
	return server, requests
}

func startProxy(t *testing.T, config ProxyConfig) (*ProxyService, string) {
	t.Helper()
	if config.ListenPort == 0 {
		config.ListenPort = freePort(t)
	}
	s := &ProxyService{}
	status := s.Start(config)
	if !status.Success || !status.Running {
		t.Fatalf("start failed: %s", status.Message)
	}
	t.Cleanup(func() { s.Stop() })
	return s, status.Address
}

func TestMapProxyPath(t *testing.T) {
	cases := []struct{ path, source, target, want string }{
		{"/api/users", "/api", "/v2", "/v2/users"},
		{"/api", "/api", "/v2", "/v2"},
		{"/api/", "/api", "/v2", "/v2/"},
		{"/apix/users", "/api", "/v2", "/apix/users"},
		{"/other", "/api", "/v2", "/other"},
		{"/api/users", "/api/", "/v2/", "/v2/users"},
		{"/api/users", "/api", "/", "/users"},
		{"/api", "/api", "/", "/"},
		{"/x/y", "/", "/base", "/base/x/y"},
		{"/api/a%2Fb", "/api", "/v2", "/v2/a%2Fb"},
		{"/a/b/c", "/a/b", "/z", "/z/c"},
		{"/a/bc", "/a/b", "/z", "/a/bc"},
		{"/anything", "", "", "/anything"},
	}
	for _, c := range cases {
		if got, _ := mapProxyPath(c.path, c.source, c.target); got != c.want {
			t.Errorf("map(%q, %q→%q) = %q, want %q", c.path, c.source, c.target, got, c.want)
		}
	}
}

func TestValidateProxyConfig(t *testing.T) {
	bad := []ProxyConfig{
		{ListenPort: 0, TargetURL: "http://localhost:9000"},
		{ListenPort: 70000, TargetURL: "http://localhost:9000"},
		{ListenPort: 8080, TargetURL: "ftp://localhost"},
		{ListenPort: 8080, TargetURL: "localhost:9000"},
		{ListenPort: 8080, TargetURL: "http://localhost:9000/base"},
		{ListenPort: 8080, TargetURL: "http://user:pw@localhost:9000"},
		{ListenPort: 8080, TargetURL: "http://localhost:9000?x=1"},
		{ListenPort: 8080, TargetURL: "http://localhost:9000", SourcePath: "/api"},
		{ListenPort: 8080, TargetURL: "http://localhost:9000", SourcePath: "api", TargetPath: "/v2"},
		{ListenPort: 8080, TargetURL: "http://localhost:9000", SourcePath: "/api?x", TargetPath: "/v2"},
	}
	for _, c := range bad {
		if _, err := validateProxyConfig(c); err == nil {
			t.Errorf("expected rejection: %+v", c)
		}
	}
	for _, target := range []string{"http://localhost:9000", "https://api.example.com", "https://api.example.com/", "http://[::1]:8080"} {
		if _, err := validateProxyConfig(ProxyConfig{ListenPort: 8080, TargetURL: target, SourcePath: "/a", TargetPath: "/"}); err != nil {
			t.Errorf("%s rejected: %v", target, err)
		}
	}
}

func TestCompileProxyScriptValidation(t *testing.T) {
	if program, err := compileProxyScript("  \n"); program != nil || err != nil {
		t.Fatal("empty script must mean plain forwarding")
	}
	for script, want := range map[string]string{
		"function onRequest(req) {": "syntax error",
		"var x = 1;":                "must define function onRequest",
		"var onRequest = 3;":        "must define function onRequest",
		"throw new Error('boom'); function onRequest(){}":                  "boom",
		"http.request({url:'http://127.0.0.1:1'}); function onRequest(){}": "only available while onRequest runs",
		"while (true) {} function onRequest(){}":                           "time limit",
	} {
		if _, err := compileProxyScript(script); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("script %q: got %v, want %q", script, err, want)
		}
	}
	if _, err := compileProxyScript(strings.Repeat(" ", proxyScriptMaxBytes) + "function onRequest(){}"); err == nil {
		t.Error("oversized script accepted")
	}
}

func TestProxyForwardsRequestsUnchanged(t *testing.T) {
	upstream, requests := echoUpstream(t)
	s, address := startProxy(t, ProxyConfig{TargetURL: upstream.URL})
	req, _ := http.NewRequest(http.MethodPatch, address+"/items/a%2Fb?secret=token-123&b=2&a=1", strings.NewReader("raw body \x00\xff"))
	req.Header.Set("X-Custom", "kept")
	req.Header.Set("Content-Type", "application/octet-stream")
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusCreated || string(body) != "upstream-ok" || response.Header.Get("X-Upstream") != "yes" {
		t.Fatalf("response not passed through: %d %q", response.StatusCode, body)
	}
	got := <-requests
	if got.Method != http.MethodPatch || got.RawPath != "/items/a%2Fb" || got.Query != "secret=token-123&b=2&a=1" ||
		got.Body != "raw body \x00\xff" || got.Header.Get("X-Custom") != "kept" || got.Host != strings.TrimPrefix(upstream.URL, "http://") {
		t.Fatalf("request modified: %+v", got)
	}
	status := s.Status()
	if status.Requests != 1 || len(status.Activity) != 1 || status.Activity[0].Status != http.StatusCreated {
		t.Fatalf("activity not recorded: %+v", status)
	}
	if raw, _ := json.Marshal(status); strings.Contains(string(raw), "token-123") || strings.Contains(string(raw), "raw body") {
		t.Fatal("activity leaked query or body")
	}
}

func TestProxyPathMappingEndToEnd(t *testing.T) {
	upstream, requests := echoUpstream(t)
	_, address := startProxy(t, ProxyConfig{TargetURL: upstream.URL, SourcePath: "/api", TargetPath: "/v2/service"})
	for path, want := range map[string]string{
		"/api/users?id=7": "/v2/service/users?id=7",
		"/api":            "/v2/service",
		"/apix/users":     "/apix/users",
		"/health":         "/health",
	} {
		response, err := http.Get(address + path)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		got := <-requests
		if full := got.RawPath + map[bool]string{true: "?" + got.Query, false: ""}[got.Query != ""]; full != want {
			t.Errorf("%s forwarded as %s, want %s", path, full, want)
		}
	}
}

func TestProxyScriptTransformsRequestWithOutboundCall(t *testing.T) {
	var ssoBody string
	sso := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		ssoBody = r.Method + " " + r.Header.Get("Content-Type") + " " + string(raw)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"abc.def","expires_in":60}`))
	}))
	defer sso.Close()
	upstream, requests := echoUpstream(t)
	script := fmt.Sprintf(`
function onRequest(req) {
  const r = http.request({ method: 'POST', url: %q, body: { client: req.header('X-Client') } });
  if (r.status !== 200) throw new Error('sso failed ' + r.status);
  const token = r.json().access_token.split('.').reverse().join('-');
  req.setHeader('Authorization', 'Bearer ' + token);
  req.removeHeader('X-Client');
  req.setQuery('added', 'yes');
  const body = JSON.parse(req.body);
  body.model = 'm1';
  body.note = 'ünïcode';
  req.body = JSON.stringify(body);
  req.path = req.path + '/x';
}`, sso.URL+"/token?client=1")
	_, address := startProxy(t, ProxyConfig{TargetURL: upstream.URL, Script: script})
	req, _ := http.NewRequest(http.MethodPost, address+"/chat?keep=1", strings.NewReader(`{"prompt":"hi"}`))
	req.Header.Set("X-Client", "cli-7")
	req.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("status %d", response.StatusCode)
	}
	got := <-requests
	if ssoBody != `POST application/json {"client":"cli-7"}` {
		t.Fatalf("sso call wrong: %q", ssoBody)
	}
	want := `{"prompt":"hi","model":"m1","note":"ünïcode"}`
	if got.Header.Get("Authorization") != "Bearer def-abc" || got.Header.Get("X-Client") != "" || got.Body != want ||
		got.ContentLength != int64(len(want)) || len(got.TransferEncoding) != 0 || got.Path != "/chat/x" ||
		got.Query != "added=yes&keep=1" {
		t.Fatalf("request not transformed: %+v", got)
	}
}

func TestProxyScriptReadOnlyBodyIsForwardedExactly(t *testing.T) {
	upstream, requests := echoUpstream(t)
	_, address := startProxy(t, ProxyConfig{TargetURL: upstream.URL, Script: `function onRequest(req) { req.setHeader('X-Len', String(req.body.length)); }`})
	binary := []byte{0xff, 0xfe, 0x00, 'a', 0x80}
	response, err := http.Post(address+"/upload", "application/octet-stream", bytes.NewReader(binary))
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	got := <-requests
	if got.Body != string(binary) || got.ContentLength != int64(len(binary)) || got.Header.Get("X-Len") == "" {
		t.Fatalf("binary body altered: %+v", got)
	}
}

func TestProxyScriptFailuresDoNotForward(t *testing.T) {
	old := proxyScriptTimeout
	proxyScriptTimeout = 300 * time.Millisecond
	defer func() { proxyScriptTimeout = old }()
	upstream, requests := echoUpstream(t)
	cases := map[string]struct {
		script string
		status int
		want   string
	}{
		"throw":    {`function onRequest(req) { throw new Error('denied by script'); }`, 500, "denied by script"},
		"timeout":  {`function onRequest(req) { while (true) {} }`, 500, "time limit"},
		"rejected": {`async function onRequest(req) { throw new Error('async nope'); }`, 500, "async nope"},
		"header":   {`function onRequest(req) { req.setHeader('Bad Name', 'x'); }`, 500, "invalid header"},
		"path":     {`function onRequest(req) { req.path = 'no-slash'; }`, 500, "invalid req.path"},
		"outbound": {`function onRequest(req) { http.request({ url: 'http://127.0.0.1:1/' }); }`, 500, "could not connect"},
		"toolarge": {`function onRequest(req) { req.body.length; }`, 413, "10 MiB"},
	}
	for name, c := range cases {
		s, address := startProxy(t, ProxyConfig{TargetURL: upstream.URL, Script: c.script})
		body := io.Reader(strings.NewReader("{}"))
		if name == "toolarge" {
			body = bytes.NewReader(make([]byte, proxyBodyLimit+10))
		}
		response, err := http.Post(address+"/x", "application/json", body)
		if err != nil {
			t.Fatal(name, err)
		}
		text, _ := io.ReadAll(response.Body)
		response.Body.Close()
		if response.StatusCode != c.status || response.Header.Get("X-ToolHatch-Proxy-Error") != "script" || !strings.Contains(string(text), c.want) {
			t.Errorf("%s: %d %q", name, response.StatusCode, text)
		}
		status := s.Status()
		if status.Failures != 1 || !strings.Contains(status.Activity[0].Error, c.want) {
			t.Errorf("%s: failure not recorded: %+v", name, status)
		}
		s.Stop()
	}
	select {
	case got := <-requests:
		t.Fatalf("failed script still forwarded %+v", got)
	default:
	}
	// A successful async handler still forwards.
	_, address := startProxy(t, ProxyConfig{TargetURL: upstream.URL, Script: `async function onRequest(req) { req.setHeader('X-Async', '1'); }`})
	response, err := http.Get(address + "/ok")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if got := <-requests; got.Header.Get("X-Async") != "1" {
		t.Fatal("async onRequest result not applied")
	}
}

func TestProxyStreamsServerSentEvents(t *testing.T) {
	release, hold := make(chan struct{}), make(chan struct{})
	defer close(hold)
	upstreamDone := make(chan struct{}, 2)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() { upstreamDone <- struct{}{} }()
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if r.URL.Path == "/hold" { // never completes on its own
			fmt.Fprint(w, "data: open\n\n")
			w.(http.Flusher).Flush()
			select {
			case <-hold:
			case <-r.Context().Done():
			}
			return
		}
		for i := 1; i <= 2; i++ {
			fmt.Fprintf(w, "data: chunk-%d\n\n", i)
			w.(http.Flusher).Flush()
		}
		select { // keep the stream open until the client has read both chunks
		case <-release:
			fmt.Fprint(w, "data: [DONE]\n\n")
		case <-r.Context().Done():
		}
	}))
	defer upstream.Close()
	s, address := startProxy(t, ProxyConfig{TargetURL: upstream.URL, Script: `function onRequest(req) { req.setHeader('Accept', 'text/event-stream'); }`})
	response, err := http.Get(address + "/v1/chat")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatal("content type changed")
	}
	reader := bufio.NewReader(response.Body)
	deadline := time.AfterFunc(3*time.Second, func() { response.Body.Close() })
	for _, want := range []string{"data: chunk-1", "", "data: chunk-2", ""} {
		line, err := reader.ReadString('\n')
		if err != nil || strings.TrimRight(line, "\n") != want {
			t.Fatalf("stream buffered or broken: %q %v", line, err)
		}
	}
	deadline.Stop()
	close(release)
	if line, _ := reader.ReadString('\n'); line != "data: [DONE]\n" {
		t.Fatalf("final event missing: %q", line)
	}
	<-upstreamDone

	// Stopping the proxy ends an open stream promptly instead of waiting for it.
	open, err := http.Get(address + "/hold")
	if err != nil {
		t.Fatal(err)
	}
	defer open.Body.Close()
	openReader := bufio.NewReader(open.Body)
	if line, _ := openReader.ReadString('\n'); line != "data: open\n" {
		t.Fatalf("second stream: %q", line)
	}
	started := time.Now()
	if status := s.Stop(); status.Running || !status.Success {
		t.Fatalf("stop failed: %+v", status)
	}
	if time.Since(started) > 2*time.Second {
		t.Fatal("stop waited for the stream")
	}
	if _, err := io.ReadAll(openReader); err == nil {
		t.Log("stream closed cleanly")
	}
	select {
	case <-upstreamDone:
	case <-time.After(2 * time.Second):
		t.Fatal("upstream stream not canceled on stop")
	}
}

func TestProxyLifecycle(t *testing.T) {
	upstream, _ := echoUpstream(t)
	port := freePort(t)
	s := &ProxyService{}
	if status := s.Status(); status.Running {
		t.Fatal("initially running")
	}
	if status := s.Start(ProxyConfig{ListenPort: port, TargetURL: "nope"}); status.Success || status.Running {
		t.Fatal("invalid config started")
	}
	if status := s.Start(ProxyConfig{ListenPort: port, TargetURL: upstream.URL, Script: "function x() {}"}); status.Success || status.Running || !strings.Contains(status.Message, "onRequest") {
		t.Fatal("invalid script started")
	}
	status := s.Start(ProxyConfig{ListenPort: port, TargetURL: upstream.URL})
	if !status.Running || status.Address != fmt.Sprintf("http://127.0.0.1:%d", port) || status.Target != upstream.URL {
		t.Fatalf("start: %+v", status)
	}
	if again := s.Start(ProxyConfig{ListenPort: port, TargetURL: upstream.URL}); again.Success || !again.Running || !strings.Contains(again.Message, "already running") {
		t.Fatalf("second start: %+v", again)
	}
	other := &ProxyService{}
	if conflict := other.Start(ProxyConfig{ListenPort: port, TargetURL: upstream.URL}); conflict.Success || conflict.Running || !strings.Contains(conflict.Message, "already in use") {
		t.Fatalf("port conflict: %+v", conflict)
	}
	if stopped := s.Stop(); stopped.Running || stopped.Address != "" {
		t.Fatalf("stop: %+v", stopped)
	}
	if _, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/", port)); err == nil {
		t.Fatal("listener still open after stop")
	}
	if again := s.Stop(); !again.Success || again.Running {
		t.Fatal("repeated stop failed")
	}
	if restarted := s.Start(ProxyConfig{ListenPort: port, TargetURL: upstream.URL}); !restarted.Running {
		t.Fatalf("port not reusable after stop: %s", restarted.Message)
	}
	if err := s.ServiceShutdown(); err != nil || s.Status().Running {
		t.Fatal("service shutdown did not stop proxy")
	}
}

func TestProxyUnreachableTarget(t *testing.T) {
	dead := fmt.Sprintf("http://127.0.0.1:%d", freePort(t))
	s, address := startProxy(t, ProxyConfig{TargetURL: dead})
	response, err := http.Get(address + "/x?token=secret")
	if err != nil {
		t.Fatal(err)
	}
	text, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusBadGateway || response.Header.Get("X-ToolHatch-Proxy-Error") != "upstream" || strings.Contains(string(text), "secret") {
		t.Fatalf("unexpected: %d %q", response.StatusCode, text)
	}
	status := s.Status()
	if status.Failures != 1 || !strings.Contains(status.Activity[0].Error, "could not connect") {
		t.Fatalf("failure not recorded: %+v", status.Activity)
	}
}

func TestProxyConfigStore(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ToolHatch", "proxy-configs.json")
	s := &ProxyService{storePath: path}
	if got := s.ListConfigs(); !got.Success || got.Draft == nil || len(got.Saved) != 0 {
		t.Fatalf("empty list: %+v", got)
	}
	draft := ProxyConfig{ListenPort: 8080, TargetURL: "https://api", Script: "function onRequest(req) { /* secret */ }"}
	if got := s.SaveDraft(draft); !got.Success {
		t.Fatal(got.Message)
	}
	named := ProxyConfig{ListenPort: 9000, TargetURL: "http://localhost:3000", SourcePath: "/a", TargetPath: "/b"}
	if got := s.SaveConfig(" Local AI ", named); !got.Success || len(got.Saved) != 1 || got.Saved[0].Name != "Local AI" {
		t.Fatalf("save: %+v", got)
	}
	replacement := ProxyConfig{ListenPort: 9001, TargetURL: "http://localhost:3001"}
	if got := s.SaveConfig("local ai", replacement); len(got.Saved) != 1 || got.Saved[0].Config != replacement {
		t.Fatalf("replace by name: %+v", got)
	}
	if got := s.SaveConfig("", named); got.Success {
		t.Fatal("empty name accepted")
	}
	restarted := &ProxyService{storePath: path}
	if got := restarted.ListConfigs(); *got.Draft != replacement || len(got.Saved) != 1 {
		t.Fatalf("restore: %+v", got)
	}
	for p, mode := range map[string]os.FileMode{path: 0600, filepath.Dir(path): 0700} {
		info, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if runtime.GOOS != "windows" && info.Mode().Perm() != mode {
			t.Fatalf("%s permissions %v", p, info.Mode().Perm())
		}
	}
	if got := s.DeleteConfig("LOCAL AI"); !got.Success || len(got.Saved) != 0 {
		t.Fatalf("delete: %+v", got)
	}
	if got := s.DeleteConfig("missing"); got.Success {
		t.Fatal("deleted missing")
	}
	if err := os.WriteFile(path, []byte("{broken"), 0600); err != nil {
		t.Fatal(err)
	}
	if got := s.SaveDraft(draft); got.Success {
		t.Fatal("corrupt store overwritten")
	}
	if raw, _ := os.ReadFile(path); string(raw) != "{broken" {
		t.Fatal("corrupt store modified")
	}
}
