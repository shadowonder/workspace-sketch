package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/dop251/goja"
)

// Script limits. Go owns all I/O; JavaScript only sees buffered strings.
const (
	proxyScriptMaxBytes   = 256 << 10
	proxyBodyLimit        = 10 << 20
	proxyFetchBodyLimit   = 4 << 20
	proxyFetchMaxCalls    = 8
	proxyFetchTimeout     = 10 * time.Second
	proxyValidateTimeout  = 2 * time.Second
	proxyScriptErrorLimit = 400
)

// proxyScriptTimeout bounds onRequest including its http.request calls (a var for tests).
var proxyScriptTimeout = 20 * time.Second

// scriptFailure is returned to the client and shown in activity; status is the
// HTTP status the proxy answers with instead of forwarding.
type scriptFailure struct {
	status  int
	message string
}

func (f *scriptFailure) Error() string { return f.message }

// compileProxyScript returns nil for an empty script (plain forwarding). Otherwise
// it compiles once and runs the top level in a throwaway runtime to confirm that
// onRequest is a function. The compiled Program is immutable and shared; every
// request runs it in its own Runtime, so no Runtime is used concurrently.
func compileProxyScript(src string) (*goja.Program, error) {
	if strings.TrimSpace(src) == "" {
		return nil, nil
	}
	if len(src) > proxyScriptMaxBytes {
		return nil, errors.New("Script exceeds the 256 KiB limit.")
	}
	program, err := goja.Compile("proxy.js", src, false)
	if err != nil {
		return nil, errors.New("Script syntax error: " + clip(err.Error()))
	}
	ctx, cancel := context.WithTimeout(context.Background(), proxyValidateTimeout)
	defer cancel()
	_, stop, err := newProxyRuntime(ctx, program, nil)
	stop()
	if err != nil {
		return nil, err
	}
	return program, nil
}

type proxyRuntime struct {
	rt     *goja.Runtime
	ctx    context.Context
	client *http.Client
	active bool // http.request is only callable while onRequest runs
	calls  int
}

// newProxyRuntime runs the program's top level and returns onRequest. The returned
// stop function must always be called to release the interrupt watcher.
func newProxyRuntime(ctx context.Context, program *goja.Program, client *http.Client) (*proxyRuntime, func() bool, error) {
	p := &proxyRuntime{rt: goja.New(), ctx: ctx, client: client}
	p.rt.SetMaxCallStackSize(512)
	stop := context.AfterFunc(ctx, func() { p.rt.Interrupt(ctx.Err()) })
	httpObject := p.rt.NewObject()
	_ = httpObject.Set("request", p.request)
	_ = p.rt.Set("http", httpObject)
	if _, err := p.rt.RunProgram(program); err != nil {
		return nil, stop, scriptError(ctx, err)
	}
	if _, ok := goja.AssertFunction(p.rt.Get("onRequest")); !ok {
		return nil, stop, errors.New("Script must define function onRequest(req).")
	}
	return p, stop, nil
}

// runProxyScript applies onRequest to out. Unread bodies keep streaming from the
// client; a body that the script reads is buffered (bounded) and re-sent with a
// correct Content-Length, modified or not.
func runProxyScript(parent context.Context, program *goja.Program, client *http.Client, out *http.Request) error {
	ctx, cancel := context.WithTimeout(parent, proxyScriptTimeout)
	defer cancel()
	p, stop, err := newProxyRuntime(ctx, program, client)
	defer stop()
	if err != nil {
		return &scriptFailure{http.StatusInternalServerError, err.Error()}
	}
	rt := p.rt
	state := &scriptRequest{req: out}
	req := rt.NewObject()
	initialPath := out.URL.EscapedPath()
	_ = req.Set("method", out.Method)
	_ = req.Set("path", initialPath)
	_ = req.Set("header", func(name string) goja.Value {
		values := out.Header.Values(name)
		if len(values) == 0 {
			return goja.Undefined()
		}
		return rt.ToValue(strings.Join(values, ", "))
	})
	_ = req.Set("headers", func() map[string]string {
		all := map[string]string{}
		for name, values := range out.Header {
			all[name] = strings.Join(values, ", ")
		}
		return all
	})
	_ = req.Set("setHeader", func(name, value string) {
		if !validHeaderName(name) || !validHeaderValue(value) {
			panic(rt.NewTypeError("setHeader: invalid header name or value"))
		}
		out.Header.Set(name, value)
	})
	_ = req.Set("removeHeader", func(name string) { out.Header.Del(name) })
	_ = req.Set("query", func(name string) goja.Value {
		values := state.queryValues()
		if !values.Has(name) {
			return goja.Undefined()
		}
		return rt.ToValue(values.Get(name))
	})
	_ = req.Set("setQuery", func(name, value string) { state.queryValues().Set(name, value); state.queryDirty = true })
	_ = req.Set("removeQuery", func(name string) { state.queryValues().Del(name); state.queryDirty = true })
	getter := rt.ToValue(func(goja.FunctionCall) goja.Value {
		if err := state.readBody(); err != nil {
			panic(rt.NewGoError(err))
		}
		return rt.ToValue(string(state.body))
	})
	setter := rt.ToValue(func(call goja.FunctionCall) goja.Value {
		value := call.Argument(0).String()
		if len(value) > proxyBodyLimit {
			state.tooLarge = true
			panic(rt.NewGoError(errors.New("request body exceeds the 10 MiB script limit")))
		}
		// Drain the original body first so it is not left half-read.
		if err := state.readBody(); err != nil {
			panic(rt.NewGoError(err))
		}
		state.body, state.bodySet = []byte(value), true
		return goja.Undefined()
	})
	_ = req.DefineAccessorProperty("body", getter, setter, goja.FLAG_FALSE, goja.FLAG_TRUE)

	onRequest, _ := goja.AssertFunction(rt.Get("onRequest"))
	p.active = true
	result, err := onRequest(goja.Undefined(), req)
	p.active = false
	if err == nil {
		err = settledPromise(ctx, result)
	}
	if err != nil {
		if state.tooLarge {
			return &scriptFailure{http.StatusRequestEntityTooLarge, "Request body exceeds the 10 MiB script limit."}
		}
		return &scriptFailure{http.StatusInternalServerError, scriptError(ctx, err).Error()}
	}
	return state.apply(req.Get("method").String(), req.Get("path").String(), initialPath)
}

// settledPromise supports async onRequest: microtasks have already run, so the
// promise must be settled; a rejection is a script error.
func settledPromise(ctx context.Context, result goja.Value) error {
	promise, ok := result.Export().(*goja.Promise)
	if !ok {
		return nil
	}
	switch promise.State() {
	case goja.PromiseStateRejected:
		return errors.New("Script error: onRequest rejected: " + clip(promise.Result().String()))
	case goja.PromiseStatePending:
		return errors.New("Script error: onRequest returned a promise that never settled (timers and async I/O are not available).")
	}
	return nil
}

type scriptRequest struct {
	req                         *http.Request
	query                       url.Values
	queryDirty                  bool
	body                        []byte
	bodyRead, bodySet, tooLarge bool
}

func (s *scriptRequest) queryValues() url.Values {
	if s.query == nil {
		// Malformed pairs are dropped only if the script edits the query.
		s.query, _ = url.ParseQuery(s.req.URL.RawQuery)
	}
	return s.query
}

func (s *scriptRequest) readBody() error {
	if s.bodyRead {
		return nil
	}
	s.bodyRead = true
	if s.req.Body == nil || s.req.Body == http.NoBody {
		return nil
	}
	data, err := io.ReadAll(io.LimitReader(s.req.Body, proxyBodyLimit+1))
	if err != nil {
		return errors.New("could not read the request body")
	}
	if len(data) > proxyBodyLimit {
		s.tooLarge = true
		return errors.New("request body exceeds the 10 MiB script limit")
	}
	s.body = data
	return nil
}

// apply writes script changes back to the outgoing request.
func (s *scriptRequest) apply(method, path, initialPath string) error {
	if method != s.req.Method {
		if !validHeaderName(method) {
			return &scriptFailure{http.StatusInternalServerError, "Script set an invalid req.method."}
		}
		s.req.Method = method
	}
	if path != initialPath {
		decoded, err := url.PathUnescape(path)
		if err != nil || !strings.HasPrefix(path, "/") || strings.ContainsAny(path, "?# \t\r\n") {
			return &scriptFailure{http.StatusInternalServerError, "Script set an invalid req.path; it must start with / and must not contain a query."}
		}
		s.req.URL.Path, s.req.URL.RawPath = decoded, path
	}
	if s.queryDirty {
		s.req.URL.RawQuery = s.query.Encode()
	}
	if s.bodyRead {
		body := s.body
		s.req.Body = io.NopCloser(bytes.NewReader(body))
		s.req.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(body)), nil }
		s.req.ContentLength = int64(len(body))
		s.req.TransferEncoding = nil
		s.req.Header.Del("Content-Length")
		if len(body) == 0 {
			s.req.Body, s.req.GetBody = http.NoBody, nil
		}
	}
	return nil
}

// request implements http.request({url, method, headers, body, timeoutMs}).
// It returns {status, statusText, headers, body, header(name), json()}.
func (p *proxyRuntime) request(call goja.FunctionCall) goja.Value {
	rt := p.rt
	fail := func(message string) { panic(rt.NewGoError(errors.New("http.request: " + message))) }
	if !p.active || p.client == nil {
		fail("only available while onRequest runs")
	}
	if p.calls++; p.calls > proxyFetchMaxCalls {
		fail(fmt.Sprintf("at most %d calls per proxied request", proxyFetchMaxCalls))
	}
	arg := call.Argument(0)
	if goja.IsUndefined(arg) || goja.IsNull(arg) {
		fail("options object with url is required")
	}
	options := arg.ToObject(rt)
	target, err := url.Parse(optionString(options, "url", ""))
	if err != nil || (target.Scheme != "http" && target.Scheme != "https") || target.Hostname() == "" {
		fail("url must be an absolute http(s) URL")
	}
	method := strings.ToUpper(optionString(options, "method", http.MethodGet))
	if !validHeaderName(method) {
		fail("invalid method")
	}
	timeout := proxyFetchTimeout
	if value := options.Get("timeoutMs"); value != nil && !goja.IsUndefined(value) {
		if ms := value.ToInteger(); ms > 0 {
			timeout = time.Duration(ms) * time.Millisecond
		}
	}
	ctx, cancel := context.WithTimeout(p.ctx, timeout)
	defer cancel()

	var body io.Reader
	jsonBody := false
	if value := options.Get("body"); value != nil && !goja.IsUndefined(value) && !goja.IsNull(value) {
		text := value.String()
		if _, isObject := value.(*goja.Object); isObject {
			stringify, _ := goja.AssertFunction(rt.Get("JSON").ToObject(rt).Get("stringify"))
			encoded, err := stringify(goja.Undefined(), value)
			if err != nil {
				panic(err)
			}
			text, jsonBody = encoded.String(), true
		}
		if len(text) > proxyBodyLimit {
			fail("body exceeds the 10 MiB limit")
		}
		body = strings.NewReader(text)
	}
	outbound, err := http.NewRequestWithContext(ctx, method, target.String(), body)
	if err != nil {
		fail("invalid request")
	}
	if headers := options.Get("headers"); headers != nil && !goja.IsUndefined(headers) && !goja.IsNull(headers) {
		object := headers.ToObject(rt)
		for _, name := range object.Keys() {
			value := object.Get(name).String()
			if !validHeaderName(name) || !validHeaderValue(value) {
				fail("invalid header name or value")
			}
			outbound.Header.Set(name, value)
		}
	}
	if jsonBody && outbound.Header.Get("Content-Type") == "" {
		outbound.Header.Set("Content-Type", "application/json")
	}
	response, err := p.client.Do(outbound)
	if err != nil {
		if p.ctx.Err() != nil {
			panic(rt.NewGoError(p.ctx.Err())) // surfaced as the script-level timeout/cancel
		}
		fail(describeNetError(err, ctx))
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, proxyFetchBodyLimit+1))
	if err != nil {
		fail("could not read the response body")
	}
	if len(data) > proxyFetchBodyLimit {
		fail("response body exceeds the 4 MiB limit")
	}
	text := string(data)
	headers := map[string]string{}
	for name, values := range response.Header {
		headers[strings.ToLower(name)] = strings.Join(values, ", ")
	}
	result := rt.NewObject()
	_ = result.Set("status", response.StatusCode)
	_ = result.Set("statusText", http.StatusText(response.StatusCode))
	_ = result.Set("headers", headers)
	_ = result.Set("body", text)
	_ = result.Set("header", func(name string) goja.Value {
		if value, ok := headers[strings.ToLower(name)]; ok {
			return rt.ToValue(value)
		}
		return goja.Undefined()
	})
	_ = result.Set("json", func(goja.FunctionCall) goja.Value {
		parse, _ := goja.AssertFunction(rt.Get("JSON").ToObject(rt).Get("parse"))
		value, err := parse(goja.Undefined(), rt.ToValue(text))
		if err != nil {
			panic(err)
		}
		return value
	})
	return result
}

func optionString(object *goja.Object, name, fallback string) string {
	value := object.Get(name)
	if value == nil || goja.IsUndefined(value) || goja.IsNull(value) {
		return fallback
	}
	return value.String()
}

// scriptError converts Goja failures to short messages without Go internals.
func scriptError(ctx context.Context, err error) error {
	var interrupted *goja.InterruptedError
	if errors.As(err, &interrupted) || ctx.Err() != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return fmt.Errorf("Script exceeded the %d s time limit.", int(proxyScriptTimeout/time.Second))
		}
		if ctx.Err() != nil {
			return errors.New("Request was canceled while the script was running.")
		}
	}
	var exception *goja.Exception
	if errors.As(err, &exception) {
		return errors.New("Script error: " + clip(exception.Error()))
	}
	return errors.New("Script error: " + clip(err.Error()))
}

func clip(text string) string {
	text = strings.TrimSpace(text)
	if len(text) > proxyScriptErrorLimit {
		return text[:proxyScriptErrorLimit] + "…"
	}
	return text
}

// describeNetError never includes URLs (queries may contain tokens).
func describeNetError(err error, ctx context.Context) string {
	var verification *tls.CertificateVerificationError
	var unknown x509.UnknownAuthorityError
	var hostname x509.HostnameError
	var dns *net.DNSError
	var timeout net.Error
	switch {
	case errors.Is(err, context.DeadlineExceeded) || (ctx != nil && errors.Is(ctx.Err(), context.DeadlineExceeded)):
		return "request timed out"
	case errors.Is(err, context.Canceled):
		return "request canceled"
	case errors.As(err, &verification), errors.As(err, &unknown), errors.As(err, &hostname):
		return "TLS certificate verification failed"
	case errors.As(err, &dns):
		return "hostname could not be resolved"
	case errors.As(err, &timeout) && timeout.Timeout():
		return "request timed out"
	default:
		return "could not connect"
	}
}

func validHeaderName(name string) bool {
	if name == "" {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.IndexByte("!#$%&'*+-.^_`|~", c) >= 0) {
			return false
		}
	}
	return true
}

func validHeaderValue(value string) bool {
	return !strings.ContainsAny(value, "\r\n\x00")
}
