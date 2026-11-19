package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"mime"
	"net/http"
	"strconv"
	"strings"

	"github.com/dop251/goja"
)

// proxyResponseBodyLimit bounds res.body for onResponse. It is independent of
// the Debug capture budget.
const proxyResponseBodyLimit = 1 << 20

// runResponseHook applies onResponse to res when its headers arrive, in a fresh
// Runtime with its own time limit. The body is never touched unless the script
// reads or assigns res.body, which is only possible for bounded, known-length,
// uncompressed text/JSON bodies; everything else (SSE, NDJSON, chunked,
// compressed, binary, partial, upgrades) streams through unchanged.
func runResponseHook(parent context.Context, script *proxyScript, client *http.Client, shared *proxyContext, log func(level, message string), res *http.Response) error {
	ctx, cancel := context.WithTimeout(parent, proxyScriptTimeout)
	defer cancel()
	p, stop, err := newProxyRuntime(ctx, script.program, client, shared, log)
	defer stop()
	if err != nil {
		return &scriptFailure{http.StatusInternalServerError, err.Error()}
	}
	rt := p.rt
	state := &scriptResponse{res: res, unavailable: responseBodyUnavailable(res)}
	object := rt.NewObject()
	_ = object.Set("status", res.StatusCode)
	request := rt.NewObject()
	if res.Request != nil {
		_ = request.Set("method", res.Request.Method)
		_ = request.Set("path", res.Request.URL.EscapedPath())
	}
	_ = object.Set("request", request)
	installHeaders(rt, object, res.Header, func(name string) bool {
		name = http.CanonicalHeaderKey(name)
		return name != "Content-Length" && name != "Transfer-Encoding"
	})
	_ = object.Set("bodyAvailable", state.unavailable == "")
	getter := rt.ToValue(func(goja.FunctionCall) goja.Value {
		if err := state.readBody(); err != nil {
			panic(rt.NewGoError(err))
		}
		return rt.ToValue(string(state.body))
	})
	setter := rt.ToValue(func(call goja.FunctionCall) goja.Value {
		value := call.Argument(0).String()
		if state.unavailable == "" && len(value) > proxyResponseBodyLimit {
			panic(rt.NewGoError(errors.New("res.body: the new body exceeds the 1 MiB script limit")))
		}
		if err := state.readBody(); err != nil { // drain the original first
			panic(rt.NewGoError(err))
		}
		state.body, state.bodySet = []byte(value), true
		return goja.Undefined()
	})
	_ = object.DefineAccessorProperty("body", getter, setter, goja.FLAG_FALSE, goja.FLAG_TRUE)

	if err := p.callHook("onResponse", object); err != nil {
		return &scriptFailure{http.StatusInternalServerError, err.Error()}
	}
	return state.apply(object.Get("status"))
}

type scriptResponse struct {
	res               *http.Response
	unavailable       string // why res.body cannot be used; "" when it can
	body              []byte
	bodyRead, bodySet bool
	readFailed        bool // a caught read error must not forward an empty body
}

func (s *scriptResponse) readBody() error {
	if s.unavailable != "" {
		return errors.New("res.body is unavailable: " + s.unavailable + " (check res.bodyAvailable)")
	}
	if s.bodyRead {
		return nil
	}
	s.bodyRead = true
	data, err := io.ReadAll(io.LimitReader(s.res.Body, proxyResponseBodyLimit+1))
	_ = s.res.Body.Close()
	if err != nil || int64(len(data)) != s.res.ContentLength {
		s.readFailed = true
		return errors.New("res.body: could not read the upstream response body")
	}
	s.body = data
	return nil
}

// apply writes status and body changes back to the response, keeping framing
// consistent. An unread body is left exactly as it was (still streaming).
func (s *scriptResponse) apply(statusValue goja.Value) error {
	res := s.res
	if s.readFailed {
		return &scriptFailure{http.StatusBadGateway, "Could not read the upstream response body for onResponse."}
	}
	original := res.StatusCode
	status, ok := hookStatus(statusValue)
	if !ok {
		return &scriptFailure{http.StatusInternalServerError, "Script set an invalid res.status; use an integer from 200 to 599."}
	}
	if status != original {
		if original == http.StatusSwitchingProtocols {
			return &scriptFailure{http.StatusInternalServerError, "Script cannot change the status of a 101 Switching Protocols response."}
		}
		res.StatusCode, res.Status = status, fmt.Sprintf("%d %s", status, http.StatusText(status))
	}
	noBody := status == http.StatusNoContent || status == http.StatusNotModified ||
		(res.Request != nil && res.Request.Method == http.MethodHead)
	if noBody {
		if s.bodySet && len(s.body) > 0 {
			return &scriptFailure{http.StatusInternalServerError, "Script set res.body on a response that cannot have a body (HEAD, 204 or 304)."}
		}
		if status != original && res.Body != http.NoBody {
			_ = res.Body.Close() // e.g. 200 changed to 204: discard the upstream body
			res.Body, res.ContentLength, res.TransferEncoding = http.NoBody, 0, nil
			res.Header.Del("Content-Length")
		}
		return nil
	}
	if s.bodyRead {
		body := s.body
		res.Body = io.NopCloser(bytes.NewReader(body))
		res.ContentLength, res.TransferEncoding = int64(len(body)), nil
		res.Header.Set("Content-Length", strconv.Itoa(len(body)))
	}
	return nil
}

func hookStatus(value goja.Value) (int, bool) {
	var number float64
	switch v := value.Export().(type) {
	case int64:
		number = float64(v)
	case float64:
		number = v
	default:
		return 0, false
	}
	if number != math.Trunc(number) || number < 200 || number > 599 {
		return 0, false
	}
	return int(number), true
}

// responseBodyUnavailable returns why onResponse cannot read or replace the body,
// or "" when it is a bounded, known-length, uncompressed text/JSON body.
func responseBodyUnavailable(res *http.Response) string {
	switch {
	case res.Request != nil && res.Request.Method == http.MethodHead,
		res.StatusCode < 200, res.StatusCode == http.StatusNoContent, res.StatusCode == http.StatusNotModified:
		return "the response has no body"
	case res.StatusCode == http.StatusPartialContent:
		return "partial (206) responses pass through unchanged"
	case res.ContentLength < 0:
		return "streaming or unknown-length responses pass through unchanged"
	case res.ContentLength > proxyResponseBodyLimit:
		return "the body exceeds the 1 MiB script limit"
	}
	if encoding := strings.TrimSpace(res.Header.Get("Content-Encoding")); encoding != "" && !strings.EqualFold(encoding, "identity") {
		return "compressed responses pass through unchanged"
	}
	if !scriptableContentType(res.Header.Get("Content-Type")) {
		return "only text and JSON bodies are available"
	}
	return ""
}

func scriptableContentType(value string) bool {
	media, _, err := mime.ParseMediaType(value)
	if err != nil {
		return false
	}
	switch {
	case media == "text/event-stream":
		return false
	case strings.HasPrefix(media, "text/"), strings.HasSuffix(media, "+json"), strings.HasSuffix(media, "+xml"):
		return true
	}
	switch media {
	case "application/json", "application/xml", "application/javascript", "application/x-www-form-urlencoded":
		return true
	}
	return false
}
