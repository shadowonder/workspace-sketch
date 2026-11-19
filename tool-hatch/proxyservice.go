package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// ProxyConfig is a complete proxy definition. The proxy always listens on
// 127.0.0.1; TargetURL is scheme://host[:port] only.
type ProxyConfig struct {
	ListenPort int    `json:"listenPort"`
	TargetURL  string `json:"targetURL"`
	SourcePath string `json:"sourcePath"`
	TargetPath string `json:"targetPath"`
	Script     string `json:"script"`
}

// ProxyActivity records metadata only: never query strings, headers or bodies.
// A pending entry is still being forwarded (for example an open SSE stream).
// Debug marks entries whose exchange was captured; the capture itself stays in
// Go and is fetched only through Capture/CopyCapture.
type ProxyActivity struct {
	ID         int64  `json:"id"`
	Time       string `json:"time"`
	Method     string `json:"method"`
	Path       string `json:"path"`
	Status     int    `json:"status"`
	DurationMs int64  `json:"durationMs"`
	Error      string `json:"error"`
	Pending    bool   `json:"pending"`
	Debug      bool   `json:"debug"`
}

type activityRecord struct {
	ProxyActivity
	capture *proxyCapture // nil unless Debug was on when the request started
}

// ProxyStatus reports the actual Go server state plus recent activity.
type ProxyStatus struct {
	Success   bool            `json:"success"`
	Message   string          `json:"message"`
	Running   bool            `json:"running"`
	Address   string          `json:"address"`
	Target    string          `json:"target"`
	Requests  int64           `json:"requests"`
	Failures  int64           `json:"failures"`
	LastError string          `json:"lastError"`
	Activity  []ProxyActivity `json:"activity"`
	// ConsoleLastID lets the UI fetch console entries only when new ones exist.
	ConsoleLastID int64 `json:"consoleLastID"`
	ConsoleLimit  int   `json:"consoleLimit"`
	// Debug is the current switch state (sampled by each request when it starts).
	Debug           bool  `json:"debug"`
	DebugBytes      int64 `json:"debugBytes"`
	DebugLimitBytes int64 `json:"debugLimitBytes"`
	ActivityLimit   int   `json:"activityLimit"`
}

type ProxyService struct {
	lifecycle sync.Mutex // serializes Start/Stop

	mu        sync.Mutex // guards the fields below
	server    *http.Server
	shared    *proxyContext // the running proxy's script context; new on every Start
	cancel    context.CancelFunc
	done      chan struct{}
	address   string
	target    string
	lastError string
	nextID    int64
	requests  int64
	failures  int64
	records   []*activityRecord
	// run identifies the current server run and epoch the current Activity
	// storage (bumped by Start, Stop, unexpected exit and ClearActivity), so late
	// handlers of an earlier run or of cleared storage cannot repopulate it.
	run, epoch    int64
	debug         bool  // Debug switch; memory-only, off after app restart
	debugBytes    int64 // memory charged by retained captures
	activityLimit int

	clipboard func(string) bool // tests; nil uses the Wails clipboard

	storeMu   sync.Mutex
	storePath string

	// console is independent of Start/Stop and Activity; it survives restarts.
	console proxyConsole
}

// Activity retention choices; captures are released with their entries.
var proxyActivityLimits = []int{100, 200, 500}

const proxyActivityDefault = 200

func NewProxyService() *ProxyService {
	path := ""
	if dir, err := os.UserConfigDir(); err == nil {
		path = filepath.Join(dir, "ToolHatch", "proxy-configs.json")
	}
	return &ProxyService{storePath: path}
}

// ServiceShutdown stops the proxy gracefully when the application quits.
func (s *ProxyService) ServiceShutdown() error {
	s.Stop()
	return nil
}

type proxyRoute struct {
	target             *url.URL
	source, targetPath string
}

func validateProxyConfig(c ProxyConfig) (proxyRoute, error) {
	if c.ListenPort < 1 || c.ListenPort > 65535 {
		return proxyRoute{}, errors.New("Local Port must be between 1 and 65535.")
	}
	target, err := url.Parse(strings.TrimSpace(c.TargetURL))
	if err != nil || (target.Scheme != "http" && target.Scheme != "https") || target.Hostname() == "" ||
		target.User != nil || target.RawQuery != "" || target.Fragment != "" || (target.Path != "" && target.Path != "/") {
		return proxyRoute{}, errors.New("Target must be http(s)://host[:port] without path, credentials, query or fragment; use Path Mapping for paths.")
	}
	if port := target.Port(); port != "" {
		if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
			return proxyRoute{}, errors.New("Target port must be between 1 and 65535.")
		}
	}
	target.Path = ""
	source, targetPath := strings.TrimSpace(c.SourcePath), strings.TrimSpace(c.TargetPath)
	if (source == "") != (targetPath == "") {
		return proxyRoute{}, errors.New("Path Mapping requires both Source Path and Target Path, or neither.")
	}
	for _, p := range []string{source, targetPath} {
		if p != "" && (!strings.HasPrefix(p, "/") || strings.ContainsAny(p, "?# \t\r\n")) {
			return proxyRoute{}, errors.New("Mapping paths must start with / and must not contain spaces, query or fragment.")
		}
		if _, err := url.PathUnescape(p); err != nil {
			return proxyRoute{}, errors.New("Mapping paths contain invalid percent-encoding.")
		}
	}
	return proxyRoute{target: target, source: source, targetPath: targetPath}, nil
}

// mapProxyPath replaces a whole-segment source prefix with the target prefix and
// keeps the suffix. It works on the escaped path so encodings like %2F survive.
// Non-matching paths are returned unchanged.
func mapProxyPath(path, source, target string) (string, bool) {
	if source == "" {
		return path, false
	}
	source = strings.TrimRight(source, "/")
	if path != source && !strings.HasPrefix(path, source+"/") {
		return path, false
	}
	mapped := strings.TrimRight(target, "/") + path[len(source):]
	if mapped == "" {
		mapped = "/"
	}
	return mapped, true
}

func (s *ProxyService) Start(config ProxyConfig) ProxyStatus {
	s.lifecycle.Lock()
	defer s.lifecycle.Unlock()
	if s.isRunning() {
		return s.statusWith(false, "Proxy is already running. Stop it before starting again.")
	}
	route, err := validateProxyConfig(config)
	if err != nil {
		return s.statusWith(false, err.Error())
	}
	script, err := compileProxyScript(config.Script, s.console.add)
	if err != nil {
		return s.statusWith(false, err.Error())
	}
	address := net.JoinHostPort("127.0.0.1", strconv.Itoa(config.ListenPort))
	listener, err := net.Listen("tcp", address)
	if err != nil {
		message := fmt.Sprintf("Could not listen on %s.", address)
		if errors.Is(err, syscall.EADDRINUSE) {
			message = fmt.Sprintf("Port %d is already in use on 127.0.0.1. Choose another Local Port or stop the other process.", config.ListenPort)
		} else if errors.Is(err, syscall.EACCES) {
			message = fmt.Sprintf("Permission denied for port %d. Choose a port above 1024.", config.ListenPort)
		}
		s.mu.Lock()
		s.lastError = message
		s.mu.Unlock()
		return s.statusWith(false, message)
	}
	ctx, cancel := context.WithCancel(context.Background())
	shared := newProxyContext()
	s.mu.Lock()
	s.run++
	s.epoch++
	run := s.run
	s.releaseAllLocked("Released when the proxy restarted.")
	s.records = nil
	s.mu.Unlock()
	server := &http.Server{
		Handler:           newProxyHandler(route, script, shared, s, run, s.console.add),
		ReadHeaderTimeout: 15 * time.Second,
		BaseContext:       func(net.Listener) context.Context { return ctx },
		ErrorLog:          log.New(io.Discard, "", 0), // never log request data
	}
	done := make(chan struct{})
	s.mu.Lock()
	s.server, s.shared, s.cancel, s.done = server, shared, cancel, done
	s.address, s.target = "http://"+address, route.target.String()
	s.lastError, s.requests, s.failures = "", 0, 0
	s.mu.Unlock()
	go func() {
		err := server.Serve(listener)
		s.mu.Lock()
		if s.server == server { // exited without Stop
			s.server, s.shared = nil, nil
			cancel()
			shared.close()
			s.epoch++
			s.releaseAllLocked("Released when the proxy stopped.")
			if !errors.Is(err, http.ErrServerClosed) {
				s.lastError = "Proxy server stopped unexpectedly."
			}
		}
		s.mu.Unlock()
		close(done)
	}()
	return s.statusWith(true, "Proxy running.")
}

// Stop cancels in-flight work (including long SSE streams), then shuts down.
func (s *ProxyService) Stop() ProxyStatus {
	s.lifecycle.Lock()
	defer s.lifecycle.Unlock()
	s.mu.Lock()
	server, shared, cancel, done := s.server, s.shared, s.cancel, s.done
	s.server, s.shared = nil, nil
	if server != nil {
		// Debug payloads are released now; late writes see the released mark.
		s.epoch++
		s.releaseAllLocked("Released when the proxy stopped.")
	}
	s.mu.Unlock()
	if server == nil {
		return s.statusWith(true, "Proxy is stopped.")
	}
	cancel()       // aborts scripts, waiters and streams
	shared.close() // discards cached values; a later Start gets an empty context
	ctx, timeout := context.WithTimeout(context.Background(), 3*time.Second)
	defer timeout()
	if server.Shutdown(ctx) != nil {
		_ = server.Close()
	}
	<-done
	return s.statusWith(true, "Proxy stopped.")
}

func (s *ProxyService) Status() ProxyStatus { return s.statusWith(true, "") }

// SetDebug switches capture for requests that start afterwards. Captures already
// retained stay until eviction, ClearActivity, Stop or quit. Not persisted.
func (s *ProxyService) SetDebug(enabled bool) ProxyStatus {
	s.mu.Lock()
	s.debug = enabled
	s.mu.Unlock()
	if enabled {
		return s.statusWith(true, "Debug capture on for new requests.")
	}
	return s.statusWith(true, "Debug capture off; retained captures are kept until cleared.")
}

// SetActivityLimit changes how many Activity entries (and their captures) are kept.
func (s *ProxyService) SetActivityLimit(limit int) ProxyStatus {
	valid := false
	for _, n := range proxyActivityLimits {
		valid = valid || n == limit
	}
	if !valid {
		return s.statusWith(false, "Activity retention must be 100, 200 or 500 entries.")
	}
	s.mu.Lock()
	s.activityLimit = limit
	s.trimLocked()
	s.mu.Unlock()
	return s.statusWith(true, "")
}

// ClearActivity discards Activity entries and releases all Debug captures.
// Counters keep describing the current run.
func (s *ProxyService) ClearActivity() ProxyStatus {
	s.mu.Lock()
	s.epoch++
	s.releaseAllLocked("Released by Clear.")
	s.records = nil
	s.mu.Unlock()
	return s.statusWith(true, "Activity cleared.")
}

// Console returns script console entries with ID greater than afterID.
func (s *ProxyService) Console(afterID int64) ProxyConsoleResponse { return s.console.since(afterID) }

// ClearConsole discards buffered console entries; IDs keep increasing.
func (s *ProxyService) ClearConsole() ProxyConsoleResponse { return s.console.clear() }

// SetConsoleLimit changes how many console entries are retained (100/500/1000).
func (s *ProxyService) SetConsoleLimit(limit int) ProxyConsoleResponse {
	return s.console.setLimit(limit)
}

// CopyConsole copies one complete retained console message to the clipboard
// from Go; the frontend only ever holds previews.
func (s *ProxyService) CopyConsole(id int64) ProxyCopyResponse {
	message, incomplete, ok := s.console.full(id)
	if !ok {
		return ProxyCopyResponse{Message: "This console entry is no longer retained."}
	}
	if !s.setClipboard(message) {
		return ProxyCopyResponse{Message: "Could not write to the clipboard."}
	}
	text := "Copied " + formatSize(int64(len(message)))
	if incomplete {
		text += " — the message exceeded the per-entry limit and was cut when logged"
	}
	return ProxyCopyResponse{Success: true, Message: text + ".", Bytes: int64(len(message)), Complete: !incomplete}
}

func (s *ProxyService) isRunning() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.server != nil
}

func (s *ProxyService) statusWith(success bool, message string) ProxyStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	status := ProxyStatus{Success: success, Message: message, Running: s.server != nil, Requests: s.requests,
		Failures: s.failures, LastError: s.lastError, Activity: make([]ProxyActivity, len(s.records)),
		Debug: s.debug, DebugBytes: s.debugBytes, DebugLimitBytes: proxyDebugBudget, ActivityLimit: s.limitLocked()}
	for i, r := range s.records {
		status.Activity[i] = r.ProxyActivity
	}
	status.ConsoleLastID, status.ConsoleLimit = s.console.state()
	if status.Running {
		status.Address, status.Target = s.address, s.target
	}
	return status
}

func (s *ProxyService) limitLocked() int {
	if s.activityLimit == 0 {
		return proxyActivityDefault
	}
	return s.activityLimit
}

// trimLocked enforces the entry count, evicting the oldest completed entry first
// (the oldest pending one only if all are pending) and releasing its capture.
func (s *ProxyService) trimLocked() {
	for len(s.records) > s.limitLocked() {
		victim := 0
		for i, r := range s.records {
			if !r.Pending {
				victim = i
				break
			}
		}
		s.releaseLocked(s.records[victim].capture, "Evicted from Activity.")
		s.records = append(s.records[:victim], s.records[victim+1:]...)
	}
}

// proxyTicket is sampled when a request arrives: Debug applies only to requests
// that start while it is on, and only within the storage epoch they started in.
type proxyTicket struct {
	run, epoch int64
	debug      bool
}

// proxyExchange links one forwarded request to its Activity record and capture.
type proxyExchange struct {
	id, run int64
	capture *proxyCapture
	err     string // proxy error shown in Activity; set by the ReverseProxy error handler
}

type proxyExchangeKey struct{}

func exchangeOf(ctx context.Context) *proxyExchange {
	ex, _ := ctx.Value(proxyExchangeKey{}).(*proxyExchange)
	return ex
}

func (s *ProxyService) admit(run int64) proxyTicket {
	s.mu.Lock()
	defer s.mu.Unlock()
	return proxyTicket{run: run, epoch: s.epoch, debug: s.debug}
}

// begin inserts a pending Activity entry once the request is about to be
// forwarded (or rejected); requests of an earlier run are not recorded.
func (s *ProxyService) begin(t proxyTicket, entry ProxyActivity, capture bool) *proxyExchange {
	s.mu.Lock()
	defer s.mu.Unlock()
	ex := &proxyExchange{run: t.run}
	if t.run != s.run {
		return ex
	}
	s.nextID++
	ex.id, entry.ID, entry.Pending = s.nextID, s.nextID, true
	record := &activityRecord{ProxyActivity: entry}
	if capture && t.debug && t.epoch == s.epoch && s.server != nil {
		record.capture, record.Debug = &proxyCapture{}, true
		ex.capture = record.capture
	}
	s.records = append(s.records, record)
	s.trimLocked()
	return ex
}

// finish completes the entry; counters count completed requests of this run.
func (s *ProxyService) finish(ex *proxyExchange, method, path string, status int, duration time.Duration, failure string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ex.run != s.run {
		return
	}
	s.requests++
	if failure != "" || status >= 500 {
		s.failures++
	}
	if r := s.recordLocked(ex.id); r != nil && ex.id != 0 {
		r.Method, r.Path, r.Status, r.DurationMs, r.Error, r.Pending = method, path, status, duration.Milliseconds(), failure, false
	}
}

type proxyHandler struct {
	route  proxyRoute
	script *proxyScript
	shared *proxyContext
	client *http.Client // script http.request calls; redirects are returned, not followed
	proxy  *httputil.ReverseProxy
	svc    *ProxyService
	run    int64
	log    func(level, message string)
}

func newProxyHandler(route proxyRoute, script *proxyScript, shared *proxyContext, svc *ProxyService, run int64, logConsole func(level, message string)) *proxyHandler {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	h := &proxyHandler{route: route, script: script, shared: shared, svc: svc, run: run, log: logConsole,
		client: &http.Client{Transport: http.DefaultTransport.(*http.Transport).Clone(),
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
	h.proxy = &httputil.ReverseProxy{
		Transport: transport,
		// ReverseProxy flushes text/event-stream and unknown-length bodies
		// immediately (streaming); nothing here reads a body ahead of it.
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme, pr.Out.URL.Host = route.target.Scheme, route.target.Host
			pr.Out.Host = route.target.Host
			if ex := exchangeOf(pr.Out.Context()); ex != nil && ex.capture != nil {
				svc.captureRequest(ex.capture, pr.Out)
			}
		},
		// onResponse runs when the upstream headers arrive, before anything is
		// written to the client; a failure becomes an explicit proxy error.
		ModifyResponse: func(res *http.Response) error {
			if h.script != nil && h.script.onResponse {
				if err := runResponseHook(res.Request.Context(), h.script, h.client, h.shared, h.log, res); err != nil {
					return err
				}
			}
			if ex := exchangeOf(res.Request.Context()); ex != nil && ex.capture != nil {
				svc.captureResponse(ex.capture, res)
			}
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			ex := exchangeOf(r.Context())
			var failure *scriptFailure
			if errors.As(err, &failure) {
				if ex != nil {
					ex.err = failure.message
				}
				w.Header().Set("X-ToolHatch-Proxy-Error", "script")
				http.Error(w, "ToolHatch proxy: "+failure.message, failure.status)
				return
			}
			message := "Target unreachable: " + describeNetError(err, nil) + "."
			if r.Context().Err() != nil {
				message = "Canceled by client or proxy stop."
			}
			if ex != nil {
				ex.err = message
			}
			w.Header().Set("X-ToolHatch-Proxy-Error", "upstream")
			http.Error(w, "ToolHatch proxy: "+message, http.StatusBadGateway)
		},
		ErrorLog: log.New(io.Discard, "", 0),
	}
	return h
}

func (h *proxyHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	started := time.Now()
	ticket := h.svc.admit(h.run) // Debug is sampled when the request arrives
	recorder := &statusRecorder{ResponseWriter: w}
	out := r.Clone(r.Context())
	if mapped, ok := mapProxyPath(out.URL.EscapedPath(), h.route.source, h.route.targetPath); ok {
		decoded, _ := url.PathUnescape(mapped)
		out.URL.Path, out.URL.RawPath = decoded, mapped
	}
	method, path := r.Method, out.URL.EscapedPath()
	var failure *scriptFailure
	if h.script != nil && h.script.onRequest {
		if err := runProxyScript(out.Context(), h.script, h.client, h.shared, h.log, out); err != nil {
			failure = &scriptFailure{http.StatusInternalServerError, err.Error()}
			errors.As(err, &failure)
		} else {
			method, path = out.Method, out.URL.EscapedPath()
		}
	}
	ex := h.svc.begin(ticket, ProxyActivity{Time: started.Format(time.RFC3339), Method: method, Path: path}, failure == nil)
	finish := func() { h.svc.finish(ex, method, path, recorder.status, time.Since(started), ex.err) }
	if failure != nil {
		ex.err = failure.message
		recorder.Header().Set("X-ToolHatch-Proxy-Error", "script")
		http.Error(recorder, "ToolHatch proxy: "+failure.message, failure.status)
		finish()
		return
	}
	defer func() {
		if v := recover(); v != nil { // ReverseProxy aborts broken streams this way
			if ex.err == "" {
				ex.err = "Response stream interrupted."
			}
			finish()
			panic(v)
		}
	}()
	h.proxy.ServeHTTP(recorder, out.WithContext(context.WithValue(out.Context(), proxyExchangeKey{}, ex)))
	finish()
}

// statusRecorder captures the status code; Unwrap keeps Flush available to
// http.ResponseController so streaming is unaffected.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	if r.status == 0 {
		r.status = code
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(data []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	return r.ResponseWriter.Write(data)
}

func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

// Saved configurations follow the profile conventions: one private local JSON
// file (0600 file, 0700 directory, atomic replace), an autosaved draft that is
// restored on launch, and named configurations replaced as a whole. Scripts may
// contain secrets, so this file is sensitive and never logged.

type SavedProxyConfig struct {
	Name   string      `json:"name"`
	Config ProxyConfig `json:"config"`
}

type proxyConfigFile struct {
	Version int                `json:"version"`
	Draft   ProxyConfig        `json:"draft"`
	Saved   []SavedProxyConfig `json:"saved"`
}

type ProxyConfigResponse struct {
	Success bool               `json:"success"`
	Message string             `json:"message"`
	Draft   *ProxyConfig       `json:"draft"`
	Saved   []SavedProxyConfig `json:"saved"`
}

const (
	proxyStoreLimit    = 4 << 20
	proxyMaxSaved      = 50
	proxyMaxNameLength = 120
)

func proxyConfigFailure(message string) ProxyConfigResponse {
	return ProxyConfigResponse{Message: message, Saved: []SavedProxyConfig{}}
}

func (s *ProxyService) readStore() (proxyConfigFile, error) {
	empty := proxyConfigFile{Version: 1, Saved: []SavedProxyConfig{}}
	if s.storePath == "" {
		return empty, errors.New("Configuration directory is unavailable; proxy configurations cannot be saved.")
	}
	raw, err := os.ReadFile(s.storePath)
	if errors.Is(err, os.ErrNotExist) {
		return empty, nil
	}
	if err != nil || len(raw) > proxyStoreLimit {
		return empty, errors.New("Cannot read proxy configuration storage.")
	}
	var data proxyConfigFile
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&data) != nil || decoder.Decode(new(any)) != io.EOF || data.Version != 1 || data.Saved == nil || len(data.Saved) > proxyMaxSaved {
		return empty, errors.New("Invalid proxy configuration storage. Existing data was left unchanged; repair or remove the file.")
	}
	return data, nil
}

func (s *ProxyService) writeStore(data proxyConfigFile) error {
	raw, err := json.MarshalIndent(data, "", "  ")
	if err != nil || len(raw) > proxyStoreLimit {
		return errors.New("Proxy configurations exceed the 4 MiB storage limit.")
	}
	return writePrivateFile(s.storePath, raw, "proxy")
}

func proxyConfigState(data proxyConfigFile, message string) ProxyConfigResponse {
	draft := data.Draft
	return ProxyConfigResponse{Success: true, Message: message, Draft: &draft, Saved: data.Saved}
}

// ListConfigs returns the autosaved draft and named configurations.
func (s *ProxyService) ListConfigs() ProxyConfigResponse {
	s.storeMu.Lock()
	defer s.storeMu.Unlock()
	data, err := s.readStore()
	if err != nil {
		return proxyConfigFailure(err.Error())
	}
	return proxyConfigState(data, "")
}

// SaveDraft autosaves the current, possibly incomplete, editor state.
func (s *ProxyService) SaveDraft(config ProxyConfig) ProxyConfigResponse {
	s.storeMu.Lock()
	defer s.storeMu.Unlock()
	if len(config.Script) > proxyScriptMaxBytes {
		return proxyConfigFailure("Script exceeds the 256 KiB limit.")
	}
	data, err := s.readStore()
	if err != nil {
		return proxyConfigFailure(err.Error())
	}
	data.Draft = config
	if err = s.writeStore(data); err != nil {
		return proxyConfigFailure(err.Error())
	}
	return proxyConfigState(data, "Draft saved.")
}

// SaveConfig stores a named configuration, replacing one with the same name
// (case-insensitive) completely.
func (s *ProxyService) SaveConfig(name string, config ProxyConfig) ProxyConfigResponse {
	s.storeMu.Lock()
	defer s.storeMu.Unlock()
	name = strings.TrimSpace(name)
	if name == "" || len(name) > proxyMaxNameLength {
		return proxyConfigFailure("Configuration Name is required (maximum 120 bytes).")
	}
	if len(config.Script) > proxyScriptMaxBytes {
		return proxyConfigFailure("Script exceeds the 256 KiB limit.")
	}
	data, err := s.readStore()
	if err != nil {
		return proxyConfigFailure(err.Error())
	}
	entry := SavedProxyConfig{Name: name, Config: config}
	replaced := false
	for i := range data.Saved {
		if strings.EqualFold(data.Saved[i].Name, name) {
			data.Saved[i], replaced = entry, true
		}
	}
	if !replaced {
		if len(data.Saved) >= proxyMaxSaved {
			return proxyConfigFailure("Maximum 50 saved proxy configurations.")
		}
		data.Saved = append(data.Saved, entry)
	}
	data.Draft = config
	if err = s.writeStore(data); err != nil {
		return proxyConfigFailure(err.Error())
	}
	return proxyConfigState(data, fmt.Sprintf("Saved %q.", name))
}

func (s *ProxyService) DeleteConfig(name string) ProxyConfigResponse {
	s.storeMu.Lock()
	defer s.storeMu.Unlock()
	data, err := s.readStore()
	if err != nil {
		return proxyConfigFailure(err.Error())
	}
	for i := range data.Saved {
		if strings.EqualFold(data.Saved[i].Name, name) {
			data.Saved = append(data.Saved[:i], data.Saved[i+1:]...)
			if err = s.writeStore(data); err != nil {
				return proxyConfigFailure(err.Error())
			}
			return proxyConfigState(data, fmt.Sprintf("Deleted %q.", name))
		}
	}
	return proxyConfigFailure("Saved configuration no longer exists.")
}
