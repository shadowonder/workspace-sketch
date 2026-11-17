package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// runContextScript runs onRequest once through the real script path with the
// given shared context and returns the outgoing request.
func runContextScript(t *testing.T, shared *proxyContext, src string) (*http.Request, error) {
	t.Helper()
	program, err := compileProxyScript(src, nil)
	if err != nil {
		return nil, err
	}
	out := httptest.NewRequest(http.MethodGet, "http://proxy.test/p", nil)
	return out, runProxyScript(context.Background(), program, http.DefaultClient, shared, nil, out)
}

// sharedOf returns the running proxy's context.
func sharedOf(s *ProxyService) *proxyContext {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.shared
}

func waitersOf(c *proxyContext, key string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	if load := c.loads[key]; load != nil {
		return load.waiters
	}
	return -1
}

func eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

type proxyResult struct {
	status int
	body   string
	err    error
}

func proxyDo(ctx context.Context, url string) proxyResult {
	request, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return proxyResult{err: err}
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)
	return proxyResult{status: response.StatusCode, body: string(body)}
}

// authStub is a token endpoint whose calls block until released per key.
type authStub struct {
	*httptest.Server
	mu      sync.Mutex
	calls   map[string]int
	gates   map[string]chan struct{}
	failing map[string]bool
}

func newAuthStub(t *testing.T) *authStub {
	t.Helper()
	a := &authStub{calls: map[string]int{}, gates: map[string]chan struct{}{}, failing: map[string]bool{}}
	a.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.URL.Query().Get("key")
		a.mu.Lock()
		a.calls[key]++
		n, gate, failing := a.calls[key], a.gates[key], a.failing[key]
		a.mu.Unlock()
		if gate != nil {
			select {
			case <-gate:
			case <-r.Context().Done():
				return
			}
		}
		if failing {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": fmt.Sprintf("token-%s-%d", key, n), "expires_in": 300})
	}))
	t.Cleanup(a.Close)
	return a
}

func (a *authStub) count(key string) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.calls[key]
}

func (a *authStub) gate(key string) chan struct{} {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.gates[key] = make(chan struct{})
	return a.gates[key]
}

func (a *authStub) set(key string, gate chan struct{}, failing bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.gates[key], a.failing[key] = gate, failing
}

// tokenScript caches a token per ?key= with getOrLoad and attaches it.
func tokenScript(auth string) string {
	return `
function onRequest(req) {
  const key = req.query('key');
  const token = context.getOrLoad(key, function () {
    console.log('loader ran for ' + key);
    const started = Date.now();
    const r = http.request({ url: '` + auth + `/token?key=' + key });
    if (r.status !== 200) throw new Error('auth failed: ' + r.status);
    const data = r.json();
    const elapsed = (Date.now() - started) / 1000;
    return { value: data.access_token, ttlSeconds: Math.max(1, data.expires_in - elapsed - 60) };
  });
  req.setHeader('Authorization', 'Bearer ' + token);
}`
}

func TestProxyContextStore(t *testing.T) {
	c := newProxyContext()
	now := time.Now() // carries a monotonic reading, like production
	c.now = func() time.Time { return now }
	if _, ok, err := c.get("missing"); ok || err != nil {
		t.Fatal("missing key found")
	}
	if err := c.set("a", "1", 0); err != nil {
		t.Fatal(err)
	}
	if err := c.set("ttl", "2", 10*time.Second); err != nil {
		t.Fatal(err)
	}
	if err := c.set("a", "", 0); err != nil { // replace; empty strings are values
		t.Fatal(err)
	}
	if value, ok, _ := c.get("a"); !ok || value != "" {
		t.Fatalf("replace: %q %v", value, ok)
	}
	now = now.Add(9 * time.Second)
	if value, ok, _ := c.get("ttl"); !ok || value != "2" {
		t.Fatal("expired too early")
	}
	now = now.Add(time.Second)
	if _, ok, _ := c.get("ttl"); ok {
		t.Fatal("expired entry returned")
	}
	if c.bytes != len("a") || len(c.entries) != 1 {
		t.Fatalf("accounting after expiry: %d bytes, %d entries", c.bytes, len(c.entries))
	}
	if removed, _ := c.delete("a"); !removed {
		t.Fatal("delete reported nothing removed")
	}
	if removed, _ := c.delete("a"); removed {
		t.Fatal("second delete reported a removal")
	}
	for _, key := range []string{"", strings.Repeat("k", proxyContextMaxKeyBytes+1)} {
		if err := c.set(key, "v", 0); err == nil {
			t.Errorf("key of %d bytes accepted", len(key))
		}
	}
	if err := c.set("big", strings.Repeat("v", proxyContextMaxValue+1), 0); err == nil {
		t.Fatal("oversized value accepted")
	}

	// Entry limit: expired entries are purged to make room; live ones are not evicted.
	for i := 0; i < proxyContextMaxEntries; i++ {
		ttl := time.Duration(0)
		if i%2 == 0 {
			ttl = time.Second
		}
		if err := c.set(fmt.Sprint(i), "v", ttl); err != nil {
			t.Fatalf("set %d: %v", i, err)
		}
	}
	if err := c.set("extra", "v", 0); !errors.Is(err, errContextFull) {
		t.Fatalf("full context accepted a new key: %v", err)
	}
	if err := c.set("1", "replaced", 0); err != nil {
		t.Fatalf("replacing an existing key in a full context: %v", err)
	}
	now = now.Add(time.Second)
	if err := c.set("extra", "v", 0); err != nil {
		t.Fatalf("expired entries not purged: %v", err)
	}
	if len(c.entries) != proxyContextMaxEntries/2+1 {
		t.Fatalf("entries after purge: %d", len(c.entries))
	}
	// Byte limit.
	d := newProxyContext()
	value := strings.Repeat("v", proxyContextMaxValue)
	for i := 0; ; i++ {
		if err := d.set(fmt.Sprint(i), value, 0); err != nil {
			if !errors.Is(err, errContextFull) || d.bytes > proxyContextMaxBytes || i != 7 {
				t.Fatalf("byte limit: %v after %d entries, %d bytes", err, i, d.bytes)
			}
			break
		}
	}

	c.close()
	if _, _, err := c.get("1"); !errors.Is(err, errContextClosed) {
		t.Fatal("closed context still readable")
	}
	if err := c.set("1", "v", 0); !errors.Is(err, errContextClosed) || len(c.entries) != 0 {
		t.Fatal("closed context accepted a value or kept entries")
	}
}

func TestProxyContextScriptAPI(t *testing.T) {
	upstream, seenRequests := echoUpstream(t)
	_, address := startProxy(t, ProxyConfig{TargetURL: upstream.URL, Script: `
function onRequest(req) {
  let r;
  if (req.path === '/set') { context.set('greeting', 'hello'); context.set('short', 'soon', 0.15); r = 'ok'; }
  if (req.path === '/get') r = JSON.stringify([context.get('greeting'), context.get('short'), context.get('missing'), typeof context.get('greeting')]);
  if (req.path === '/delete') r = JSON.stringify([context.delete('greeting'), context.delete('greeting'), context.get('greeting')]);
  req.setHeader('X-Result', r);
}`})
	step := func(path, want string) {
		t.Helper()
		if status := proxyGet(t, address+path); status != http.StatusCreated {
			t.Fatalf("%s: status %d", path, status)
		}
		if got := (<-seenRequests).Header.Get("X-Result"); got != want {
			t.Fatalf("%s: %s, want %s", path, got, want)
		}
	}
	step("/set", "ok")
	step("/get", `["hello","soon",null,"string"]`) // a separate runtime sees the values
	time.Sleep(250 * time.Millisecond)
	step("/get", `["hello",null,null,"string"]`) // ttl expired
	step("/delete", `[true,false,null]`)
}

func TestProxyContextInvalidInput(t *testing.T) {
	for _, tc := range []struct{ code, want string }{
		{`context.get(1)`, "TypeError: context: key must be a string"},
		{`context.get()`, "TypeError: context: key must be a string"},
		{`context.get('')`, "TypeError: context: key must be a non-empty string"},
		{`context.get('k'.repeat(257))`, "TypeError: context: key must be a non-empty string of at most 256 bytes"},
		{`context.set('k', { token: 'x' })`, "TypeError: context: value must be a string"},
		{`context.set('k', 42)`, "TypeError: context: value must be a string"},
		{`context.set('k', 'v'.repeat(1048577))`, "TypeError: context: value must be a string of at most 1 MiB"},
		{`context.set('k', 'v', '300')`, "TypeError: context: ttlSeconds must be a number"},
		{`context.set('k', 'v', 0)`, "TypeError: context: ttlSeconds must be a positive number"},
		{`context.set('k', 'v', -5)`, "TypeError: context: ttlSeconds must be a positive number"},
		{`context.set('k', 'v', NaN)`, "TypeError: context: ttlSeconds must be a positive number"},
		{`context.set('k', 'v', Infinity)`, "TypeError: context: ttlSeconds must be a positive number"},
		{`context.set('k', 'v', -1e300)`, "TypeError: context: ttlSeconds must be a positive number"},
		{`context.set('k', 'v', 31536001)`, "TypeError: context: ttlSeconds must be a positive number"},
		{`context.delete(null)`, "TypeError: context: key must be a string"},
		{`context.getOrLoad('k')`, "TypeError: context.getOrLoad: loader must be a function"},
		{`context.getOrLoad('k', () => 'plain')`, "TypeError: " + contextLoaderShape},
		{`context.getOrLoad('k', () => ({ value: 1 }))`, "TypeError: " + contextLoaderShape},
		{`context.getOrLoad('k', () => ({ value: 'v', ttlSeconds: -1 }))`, "TypeError: context: ttlSeconds must be a positive number"},
		{`context.getOrLoad('k', async () => ({ value: 'v' }))`, "TypeError: " + contextLoaderShape},
		{`context.getOrLoad('k', () => { throw new Error('loader broke') })`, "Error: loader broke"},
		{`context.getOrLoad('k', () => ({ value: context.getOrLoad('j', () => ({ value: 'x' })) }))`, "context.getOrLoad cannot be called inside a loader"},
		{`context.getOrLoad('k', () => ({ value: context.getOrLoad('k', () => ({ value: 'x' })) }))`, "context.getOrLoad cannot be called inside a loader"},
	} {
		shared := newProxyContext()
		_, err := runContextScript(t, shared, `function onRequest(req) { `+tc.code+` }`)
		var failure *scriptFailure
		if !errors.As(err, &failure) || failure.status != http.StatusInternalServerError || !strings.Contains(failure.message, tc.want) {
			t.Errorf("%s: %v, want %q", tc.code, err, tc.want)
		}
		if len(shared.entries) != 0 || len(shared.loads) != 0 {
			t.Errorf("%s: left state behind: %v %v", tc.code, shared.entries, shared.loads)
		}
	}

	// Errors are catchable, and a failed load caches nothing.
	shared := newProxyContext()
	out, err := runContextScript(t, shared, `function onRequest(req) {
  try { context.get(1); } catch (e) { req.setHeader('X-Type', e.name); }
  try { context.getOrLoad('k', () => { throw new Error('nope'); }); } catch (e) { req.setHeader('X-Loader', e.message); }
  req.setHeader('X-After', String(context.get('k')));
  req.setHeader('X-Loaded', context.getOrLoad('k', () => ({ value: 'second' })));
  req.setHeader('X-Cached', context.getOrLoad('k', () => { throw new Error('must not run'); }));
}`)
	if err != nil || out.Header.Get("X-Type") != "TypeError" || out.Header.Get("X-Loader") != "nope" ||
		out.Header.Get("X-After") != "null" || out.Header.Get("X-Loaded") != "second" || out.Header.Get("X-Cached") != "second" {
		t.Fatalf("catchable errors: %v %v", err, out.Header)
	}

	// context, like http.request, is unavailable in top-level code.
	if _, err := compileProxyScript(`context.set('k', 'v'); function onRequest(req) {}`, nil); err == nil || !strings.Contains(err.Error(), "context.set: only available while onRequest runs") {
		t.Fatalf("top-level use: %v", err)
	}
}

func TestProxyContextGetOrLoadOncePerKeyAndKeysIndependent(t *testing.T) {
	auth := newAuthStub(t)
	gateA, gateB := auth.gate("a"), auth.gate("b")
	upstream, seenRequests := echoUpstream(t)
	s, address := startProxy(t, ProxyConfig{TargetURL: upstream.URL, Script: tokenScript(auth.URL)})
	shared := sharedOf(s)

	const n = 10
	results := make(chan proxyResult, n+1)
	for i := 0; i < n; i++ {
		go func() { results <- proxyDo(context.Background(), address+"/v1/chat?key=a") }()
	}
	eventually(t, "nine waiters for key a", func() bool { return auth.count("a") == 1 && waitersOf(shared, "a") == n-1 })

	// While key a is still loading, key b loads and completes on its own.
	go func() { results <- proxyDo(context.Background(), address+"/v1/chat?key=b") }()
	eventually(t, "key b loader", func() bool { return auth.count("b") == 1 })
	close(gateB)
	if result := <-results; result.status != http.StatusCreated {
		t.Fatalf("key b: %+v", result)
	}
	if got := (<-seenRequests).Header.Get("Authorization"); got != "Bearer token-b-1" {
		t.Fatalf("key b token: %q", got)
	}
	if waitersOf(shared, "a") != n-1 {
		t.Fatal("key a load did not stay pending while key b completed")
	}

	close(gateA)
	for i := 0; i < n; i++ {
		if result := <-results; result.status != http.StatusCreated {
			t.Fatalf("key a request %d: %+v", i, result)
		}
		if got := (<-seenRequests).Header.Get("Authorization"); got != "Bearer token-a-1" {
			t.Fatalf("key a request %d token: %q", i, got)
		}
	}
	if proxyGet(t, address+"/again?key=a") != http.StatusCreated || (<-seenRequests).Header.Get("Authorization") != "Bearer token-a-1" {
		t.Fatal("cached token not reused")
	}
	if auth.count("a") != 1 || auth.count("b") != 1 {
		t.Fatalf("auth calls a=%d b=%d", auth.count("a"), auth.count("b"))
	}
	// Each loader ran only in its leader's runtime: waiters received the string, never the callback.
	loaders := 0
	for _, entry := range s.Console(0).Entries {
		if strings.HasPrefix(entry.Message, "loader ran for ") {
			loaders++
		}
	}
	if loaders != 2 || len(shared.loads) != 0 {
		t.Fatalf("loader executions %d, pending loads %d", loaders, len(shared.loads))
	}
}

func TestProxyContextLoaderFailuresAndCancellation(t *testing.T) {
	auth := newAuthStub(t)
	upstream, seenRequests := echoUpstream(t)
	s, address := startProxy(t, ProxyConfig{TargetURL: upstream.URL, Script: tokenScript(auth.URL)})
	shared := sharedOf(s)

	t.Run("loader exception reaches the leader and current waiters, and is not cached", func(t *testing.T) {
		gate := make(chan struct{})
		auth.set("fail", gate, true)
		results := make(chan proxyResult, 4)
		for i := 0; i < 4; i++ {
			go func() { results <- proxyDo(context.Background(), address+"/x?key=fail") }()
		}
		eventually(t, "three waiters", func() bool { return waitersOf(shared, "fail") == 3 })
		close(gate)
		leader, waiters := 0, 0
		for i := 0; i < 4; i++ {
			result := <-results
			switch {
			case result.status == http.StatusInternalServerError && strings.Contains(result.body, "Error: auth failed: 503"):
				leader++
			case result.status == http.StatusInternalServerError && strings.Contains(result.body, errContextLoadFailed.Error()):
				waiters++
			default:
				t.Fatalf("unexpected result %+v", result)
			}
		}
		if leader != 1 || waiters != 3 || auth.count("fail") != 1 {
			t.Fatalf("leader %d waiters %d calls %d", leader, waiters, auth.count("fail"))
		}
		auth.set("fail", nil, false)
		if proxyGet(t, address+"/x?key=fail") != http.StatusCreated || (<-seenRequests).Header.Get("Authorization") != "Bearer token-fail-2" {
			t.Fatal("failed load was cached")
		}
	})

	t.Run("a canceled leader hands the load to a waiter", func(t *testing.T) {
		gate := auth.gate("lead")
		ctx, cancel := context.WithCancel(context.Background())
		leader := make(chan proxyResult, 1)
		go func() { leader <- proxyDo(ctx, address+"/x?key=lead") }()
		eventually(t, "leader loading", func() bool { return auth.count("lead") == 1 })
		waiter := make(chan proxyResult, 1)
		go func() { waiter <- proxyDo(context.Background(), address+"/x?key=lead") }()
		eventually(t, "waiter", func() bool { return waitersOf(shared, "lead") == 1 })
		auth.set("lead", nil, false) // the retried load is not gated
		cancel()
		if result := <-leader; result.err == nil {
			t.Fatalf("leader not canceled: %+v", result)
		}
		if result := <-waiter; result.status != http.StatusCreated {
			t.Fatalf("waiter after leader cancel: %+v", result)
		}
		if got := (<-seenRequests).Header.Get("Authorization"); got != "Bearer token-lead-2" || auth.count("lead") != 2 {
			t.Fatalf("waiter token %q, calls %d", got, auth.count("lead"))
		}
		close(gate)
	})

	t.Run("a canceled waiter leaves without disturbing the load", func(t *testing.T) {
		gate := auth.gate("wait")
		leader := make(chan proxyResult, 1)
		go func() { leader <- proxyDo(context.Background(), address+"/x?key=wait") }()
		eventually(t, "leader loading", func() bool { return auth.count("wait") == 1 })
		ctx, cancel := context.WithCancel(context.Background())
		waiter := make(chan proxyResult, 1)
		go func() { waiter <- proxyDo(ctx, address+"/x?key=wait") }()
		eventually(t, "waiter", func() bool { return waitersOf(shared, "wait") == 1 })
		cancel()
		if result := <-waiter; result.err == nil {
			t.Fatalf("waiter not canceled: %+v", result)
		}
		eventually(t, "waiter released", func() bool { return waitersOf(shared, "wait") == 0 })
		close(gate)
		if result := <-leader; result.status != http.StatusCreated {
			t.Fatalf("leader: %+v", result)
		}
		<-seenRequests
		if value, ok, _ := shared.get("wait"); !ok || value != "token-wait-1" {
			t.Fatal("leader result not cached")
		}
	})

	t.Run("waiters respect their own script time limit", func(t *testing.T) {
		old := proxyScriptTimeout
		proxyScriptTimeout = 300 * time.Millisecond
		defer func() { proxyScriptTimeout = old }()
		gate := auth.gate("slow")
		defer close(gate)
		results := make(chan proxyResult, 2)
		started := time.Now()
		for i := 0; i < 2; i++ {
			go func() { results <- proxyDo(context.Background(), address+"/x?key=slow") }()
		}
		for i := 0; i < 2; i++ {
			if result := <-results; result.status != http.StatusInternalServerError || !strings.Contains(result.body, "time limit") {
				t.Fatalf("timeout result: %+v", result)
			}
		}
		if elapsed := time.Since(started); elapsed > 2*time.Second {
			t.Fatalf("waiters blocked for %v", elapsed)
		}
		if _, ok, _ := shared.get("slow"); ok || len(shared.loads) != 0 {
			t.Fatal("timed-out load left state behind")
		}
	})
}

func TestProxyContextStopDuringLoad(t *testing.T) {
	auth := newAuthStub(t)
	gate := auth.gate("k")
	defer close(gate)
	upstream, seenRequests := echoUpstream(t)
	port := freePort(t)
	config := ProxyConfig{ListenPort: port, TargetURL: upstream.URL, Script: tokenScript(auth.URL)}
	s, address := startProxy(t, config)
	old := sharedOf(s)
	if err := old.set("other", "kept-until-stop", 0); err != nil {
		t.Fatal(err)
	}
	results := make(chan proxyResult, 3)
	for i := 0; i < 3; i++ {
		go func() { results <- proxyDo(context.Background(), address+"/x?key=k") }()
	}
	eventually(t, "leader and two waiters", func() bool { return auth.count("k") == 1 && waitersOf(old, "k") == 2 })
	started := time.Now()
	if status := s.Stop(); status.Running {
		t.Fatal("still running")
	}
	for i := 0; i < 3; i++ {
		select {
		case result := <-results:
			if result.status == http.StatusCreated {
				t.Fatalf("request forwarded during stop: %+v", result)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("request hung after Stop")
		}
	}
	if elapsed := time.Since(started); elapsed > 3*time.Second {
		t.Fatalf("Stop took %v", elapsed)
	}
	if _, _, err := old.get("other"); !errors.Is(err, errContextClosed) || len(old.entries) != 0 || len(old.loads) != 0 {
		t.Fatal("stopped proxy's context not discarded")
	}
	if _, err := old.getOrLoad(context.Background(), "k", func() (string, time.Duration, error) { return "late", 0, nil }); !errors.Is(err, errContextClosed) {
		t.Fatal("closed context still loads")
	}

	// A restart gets an empty context and loads again.
	auth.set("k", nil, false)
	if status := s.Start(config); !status.Running {
		t.Fatalf("restart: %s", status.Message)
	}
	if sharedOf(s) == old {
		t.Fatal("restart reused the context")
	}
	if proxyGet(t, address+"/x?key=k") != http.StatusCreated || (<-seenRequests).Header.Get("Authorization") != "Bearer token-k-2" {
		t.Fatal("load after restart")
	}
	if _, ok, _ := sharedOf(s).get("other"); ok {
		t.Fatal("value survived restart")
	}
}

func TestProxyContextIsolationAndPrivacy(t *testing.T) {
	upstream, seenRequests := echoUpstream(t)
	dir := t.TempDir()
	script := `
function onRequest(req) {
  if (req.path === '/store') context.set('secret', req.header('X-Secret'));
  req.setHeader('X-Value', String(context.get('secret')));
}`
	config := ProxyConfig{ListenPort: freePort(t), TargetURL: upstream.URL, Script: script}
	s := &ProxyService{storePath: filepath.Join(dir, "proxy-configs.json")}
	if status := s.Start(config); !status.Running {
		t.Fatal(status.Message)
	}
	defer s.Stop()
	address := "http://127.0.0.1:" + fmt.Sprint(config.ListenPort)
	const secret = "s3cr3t-token-value"
	request, _ := http.NewRequest(http.MethodGet, address+"/store", nil)
	request.Header.Set("X-Secret", secret)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	<-seenRequests
	read := func(address string) string {
		t.Helper()
		if status := proxyGet(t, address+"/read"); status != http.StatusCreated {
			t.Fatalf("read: %d", status)
		}
		return (<-seenRequests).Header.Get("X-Value")
	}

	// Draft, saved-configuration and console operations do not touch a running context.
	edited := config
	edited.Script = "function onRequest(req) {}"
	s.SaveDraft(edited)
	s.SaveConfig("named", edited)
	s.ListConfigs()
	s.ClearConsole()
	if s.Start(edited).Success {
		t.Fatal("second Start accepted while running")
	}
	if got := read(address); got != secret {
		t.Fatalf("context changed by config operations: %q", got)
	}

	// A second proxy instance has its own context.
	other, otherAddress := startProxy(t, ProxyConfig{TargetURL: upstream.URL, Script: script})
	if got := read(otherAddress); got != "null" {
		t.Fatalf("contexts shared between proxies: %q", got)
	}
	other.Stop()

	// Nothing reaches status, activity, console or the config file.
	status, _ := json.Marshal(s.Status())
	console, _ := json.Marshal(s.Console(0))
	stored, err := os.ReadFile(s.storePath)
	if err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string][]byte{"status": status, "console": console, "config file": stored} {
		if strings.Contains(string(data), secret) {
			t.Errorf("secret found in %s", name)
		}
	}

	s.Stop()
	if status := s.Start(config); !status.Running {
		t.Fatal(status.Message)
	}
	if got := read(address); got != "null" {
		t.Fatalf("context survived restart: %q", got)
	}
}

func TestProxyContextKeepsStreaming(t *testing.T) {
	release := make(chan struct{})
	headers := make(chan string, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		headers <- r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: first\n\n")
		w.(http.Flusher).Flush()
		select {
		case <-release:
			fmt.Fprint(w, "data: [DONE]\n\n")
		case <-r.Context().Done():
		}
	}))
	defer upstream.Close()
	_, address := startProxy(t, ProxyConfig{TargetURL: upstream.URL, Script: `
function onRequest(req) {
  req.setHeader('Authorization', 'Bearer ' + context.getOrLoad('token', () => ({ value: 'cached', ttlSeconds: 60 })));
}`})
	response, err := http.Get(address + "/v1/chat")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if got := <-headers; got != "Bearer cached" {
		t.Fatalf("authorization %q", got)
	}
	reader := bufio.NewReader(response.Body)
	deadline := time.AfterFunc(3*time.Second, func() { response.Body.Close() })
	if line, err := reader.ReadString('\n'); err != nil || line != "data: first\n" {
		t.Fatalf("stream buffered: %q %v", line, err)
	}
	deadline.Stop()
	close(release)
	rest, _ := io.ReadAll(reader)
	if string(rest) != "\ndata: [DONE]\n\n" {
		t.Fatalf("stream tail %q", rest)
	}
}
