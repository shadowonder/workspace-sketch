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

	"github.com/dop251/goja"
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
type ProxyActivity struct {
	ID         int64  `json:"id"`
	Time       string `json:"time"`
	Method     string `json:"method"`
	Path       string `json:"path"`
	Status     int    `json:"status"`
	DurationMs int64  `json:"durationMs"`
	Error      string `json:"error"`
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
	activity  []ProxyActivity

	storeMu   sync.Mutex
	storePath string

	// console is independent of Start/Stop and Activity; it survives restarts.
	console proxyConsole
}

const proxyActivityLimit = 100

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
	program, err := compileProxyScript(config.Script, s.console.add)
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
	server := &http.Server{
		Handler:           newProxyHandler(route, program, shared, s.record, s.console.add),
		ReadHeaderTimeout: 15 * time.Second,
		BaseContext:       func(net.Listener) context.Context { return ctx },
		ErrorLog:          log.New(io.Discard, "", 0), // never log request data
	}
	done := make(chan struct{})
	s.mu.Lock()
	s.server, s.shared, s.cancel, s.done = server, shared, cancel, done
	s.address, s.target = "http://"+address, route.target.String()
	s.lastError, s.requests, s.failures, s.activity = "", 0, 0, nil
	s.mu.Unlock()
	go func() {
		err := server.Serve(listener)
		s.mu.Lock()
		if s.server == server { // exited without Stop
			s.server, s.shared = nil, nil
			cancel()
			shared.close()
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

// Console returns script console entries with ID greater than afterID.
func (s *ProxyService) Console(afterID int64) ProxyConsoleResponse { return s.console.since(afterID) }

// ClearConsole discards buffered console entries; IDs keep increasing.
func (s *ProxyService) ClearConsole() ProxyConsoleResponse { return s.console.clear() }

func (s *ProxyService) isRunning() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.server != nil
}

func (s *ProxyService) statusWith(success bool, message string) ProxyStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	status := ProxyStatus{Success: success, Message: message, Running: s.server != nil, Requests: s.requests,
		Failures: s.failures, LastError: s.lastError, Activity: make([]ProxyActivity, len(s.activity))}
	copy(status.Activity, s.activity)
	status.ConsoleLastID = s.console.last()
	if status.Running {
		status.Address, status.Target = s.address, s.target
	}
	return status
}

func (s *ProxyService) record(entry ProxyActivity) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nextID++
	entry.ID = s.nextID
	s.requests++
	if entry.Error != "" || entry.Status >= 500 {
		s.failures++
	}
	s.activity = append(s.activity, entry)
	if len(s.activity) > proxyActivityLimit {
		s.activity = s.activity[len(s.activity)-proxyActivityLimit:]
	}
}

type proxyHandler struct {
	route   proxyRoute
	program *goja.Program
	shared  *proxyContext
	client  *http.Client // script http.request calls; redirects are returned, not followed
	proxy   *httputil.ReverseProxy
	record  func(ProxyActivity)
	log     func(level, message string)
}

type proxyErrorKey struct{}

func newProxyHandler(route proxyRoute, program *goja.Program, shared *proxyContext, record func(ProxyActivity), logConsole func(level, message string)) *proxyHandler {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	h := &proxyHandler{route: route, program: program, shared: shared, record: record, log: logConsole,
		client: &http.Client{Transport: http.DefaultTransport.(*http.Transport).Clone(),
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
	h.proxy = &httputil.ReverseProxy{
		Transport: transport,
		// Responses are passed through untouched. ReverseProxy flushes
		// text/event-stream and unknown-length bodies immediately (streaming).
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme, pr.Out.URL.Host = route.target.Scheme, route.target.Host
			pr.Out.Host = route.target.Host
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			message := "Target unreachable: " + describeNetError(err, nil) + "."
			if r.Context().Err() != nil {
				message = "Canceled by client or proxy stop."
			}
			if holder, ok := r.Context().Value(proxyErrorKey{}).(*string); ok {
				*holder = message
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
	recorder := &statusRecorder{ResponseWriter: w}
	var proxyError string
	out := r.Clone(context.WithValue(r.Context(), proxyErrorKey{}, &proxyError))
	if mapped, ok := mapProxyPath(out.URL.EscapedPath(), h.route.source, h.route.targetPath); ok {
		decoded, _ := url.PathUnescape(mapped)
		out.URL.Path, out.URL.RawPath = decoded, mapped
	}
	entry := ProxyActivity{Time: started.Format(time.RFC3339), Method: r.Method, Path: out.URL.EscapedPath()}
	finish := func() {
		entry.Status, entry.DurationMs = recorder.status, time.Since(started).Milliseconds()
		h.record(entry)
	}
	defer func() {
		if v := recover(); v != nil { // ReverseProxy aborts broken streams this way
			if entry.Error == "" {
				entry.Error = "Response stream interrupted."
			}
			finish()
			panic(v)
		}
	}()
	if h.program != nil {
		if err := runProxyScript(out.Context(), h.program, h.client, h.shared, h.log, out); err != nil {
			failure := &scriptFailure{http.StatusInternalServerError, err.Error()}
			errors.As(err, &failure)
			entry.Error = failure.message
			recorder.Header().Set("X-ToolHatch-Proxy-Error", "script")
			http.Error(recorder, "ToolHatch proxy: "+failure.message, failure.status)
			finish()
			return
		}
		entry.Method, entry.Path = out.Method, out.URL.EscapedPath()
	}
	h.proxy.ServeHTTP(recorder, out)
	entry.Error = proxyError
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
