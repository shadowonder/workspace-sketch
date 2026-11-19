package main

import (
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/wailsapp/wails/v3/pkg/application"
)

// Debug capture keeps the main proxied exchange (after onRequest, and after
// onResponse as the client receives it) in memory only, attached to its Activity
// record. Bodies are teed while they stream through the existing forwarding
// path, never buffered ahead of it. All capture state is guarded by
// ProxyService.mu; the total is bounded by proxyDebugBudget, and the oldest
// completed captures are released first. A capture that cannot get memory
// stops growing and is marked incomplete while forwarding continues.

// proxyDebugBudget is the total Debug capture memory (a var for tests).
var proxyDebugBudget int64 = 200 << 20

const (
	captureChunkSize    = 64 << 10
	capturePreviewLimit = 4 << 10
	captureBudgetReason = "Stopped capturing: the Debug memory budget is full."
)

var errCaptureClosed = errors.New("closed")

type proxyCapture struct {
	request, response captureMessage
	responded         bool   // response headers were captured
	stopped           string // capturing stopped for the whole exchange
	released          string // why the data was released; "" while retained
	bytes             int64  // memory charged to the Debug budget
}

type captureMessage struct {
	line   string // "POST /path?query" or "200 OK"
	host   string
	header http.Header
	body   captureBody
}

type captureBody struct {
	chunks   [][]byte
	stored   int64 // bytes retained
	observed int64 // bytes that passed through
	expected int64 // Content-Length; -1 when unknown
	done     bool
	reason   string // why the body is incomplete
}

// captureReader tees a request or response body into its capture as the
// forwarding path reads it; it never reads ahead.
type captureReader struct {
	io.ReadCloser
	svc *ProxyService
	c   *proxyCapture
	b   *captureBody
}

func (r *captureReader) Read(p []byte) (int, error) {
	n, err := r.ReadCloser.Read(p)
	if n > 0 {
		r.svc.captureWrite(r.c, r.b, p[:n])
	}
	if err != nil {
		r.svc.captureEnd(r.c, r.b, err)
	}
	return n, err
}

func (r *captureReader) Close() error {
	r.svc.captureEnd(r.c, r.b, errCaptureClosed)
	return r.ReadCloser.Close()
}

func headerBytes(line, host string, h http.Header) int64 {
	n := len(line) + len(host) + 16
	for name, values := range h {
		for _, value := range values {
			n += len(name) + len(value) + 4
		}
	}
	return int64(n)
}

// captureHead records a start line and headers; false means capture stopped.
func (s *ProxyService) captureHead(c *proxyCapture, m *captureMessage, line, host string, h http.Header, expected int64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if c.released != "" {
		return false
	}
	m.body.expected = expected
	if c.stopped == "" && !s.reserveLocked(c, headerBytes(line, host, h)) {
		c.stopped = captureBudgetReason
	}
	if c.stopped != "" {
		m.body.reason, m.body.done = c.stopped, true
		return false
	}
	m.line, m.host, m.header = line, host, h.Clone()
	if m == &c.response {
		c.responded = true
	}
	return true
}

// captureRequest runs in Rewrite, so it sees exactly what is forwarded.
func (s *ProxyService) captureRequest(c *proxyCapture, out *http.Request) {
	if !s.captureHead(c, &c.request, out.Method+" "+out.URL.RequestURI(), out.Host, out.Header, out.ContentLength) {
		return
	}
	b := &c.request.body
	switch {
	case out.Body == nil || out.Body == http.NoBody:
		s.captureEnd(c, b, io.EOF)
	case out.GetBody != nil:
		// A body the script buffered can be re-sent by the transport, so copy it
		// from GetBody once instead of teeing reads that might repeat.
		body, err := out.GetBody()
		if err == nil {
			_, err = io.Copy(captureWriter{s, c, b}, body)
			_ = body.Close()
		}
		if err == nil {
			err = io.EOF
		}
		s.captureEnd(c, b, err)
	default:
		out.Body = &captureReader{out.Body, s, c, b}
	}
}

type captureWriter struct {
	svc *ProxyService
	c   *proxyCapture
	b   *captureBody
}

func (w captureWriter) Write(p []byte) (int, error) {
	w.svc.captureWrite(w.c, w.b, p)
	return len(p), nil
}

// captureResponse runs in ModifyResponse after onResponse: what the client gets.
func (s *ProxyService) captureResponse(c *proxyCapture, res *http.Response) {
	line := fmt.Sprintf("%d %s", res.StatusCode, http.StatusText(res.StatusCode))
	if !s.captureHead(c, &c.response, line, "", res.Header, res.ContentLength) {
		return
	}
	b := &c.response.body
	switch {
	case res.StatusCode == http.StatusSwitchingProtocols:
		s.mu.Lock()
		b.reason, b.done = "Not captured: protocol upgrade.", true
		s.mu.Unlock()
	case res.Body == nil || res.Body == http.NoBody:
		s.captureEnd(c, b, io.EOF)
	default:
		res.Body = &captureReader{res.Body, s, c, b}
	}
}

func (s *ProxyService) captureWrite(c *proxyCapture, b *captureBody, data []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b.observed += int64(len(data))
	if c.released != "" || b.reason != "" {
		return
	}
	if c.stopped != "" {
		b.reason = c.stopped
		return
	}
	for len(data) > 0 {
		if n := len(b.chunks); n > 0 {
			last := b.chunks[n-1]
			if room := cap(last) - len(last); room > 0 {
				k := min(room, len(data))
				b.chunks[n-1] = append(last, data[:k]...)
				b.stored += int64(k)
				data = data[k:]
				continue
			}
		}
		size := max(len(data), captureChunkSize)
		if !s.reserveLocked(c, int64(size)) {
			if size = len(data); !s.reserveLocked(c, int64(size)) {
				c.stopped, b.reason = captureBudgetReason, captureBudgetReason
				return
			}
		}
		b.chunks = append(b.chunks, make([]byte, 0, size))
	}
}

func (s *ProxyService) captureEnd(c *proxyCapture, b *captureBody, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if b.done || c.released != "" {
		return
	}
	b.done = true
	if b.reason == "" && err != io.EOF && (b.expected < 0 || b.observed != b.expected) {
		b.reason = "Body was not fully transferred (canceled, interrupted or not read)."
	}
}

// reserveLocked charges n bytes to c, releasing the oldest completed captures
// (never c itself or a pending exchange) when the budget would be exceeded.
func (s *ProxyService) reserveLocked(c *proxyCapture, n int64) bool {
	for s.debugBytes+n > proxyDebugBudget {
		evicted := false
		for _, r := range s.records {
			if !r.Pending && r.capture != nil && r.capture != c && r.capture.bytes > 0 {
				s.releaseLocked(r.capture, "Released to stay within the Debug memory budget.")
				evicted = true
				break
			}
		}
		if !evicted {
			return false
		}
	}
	s.debugBytes += n
	c.bytes += n
	return true
}

func (s *ProxyService) releaseLocked(c *proxyCapture, why string) {
	if c == nil || c.released != "" {
		return
	}
	s.debugBytes -= c.bytes
	c.bytes, c.released = 0, why
	c.request, c.response = captureMessage{}, captureMessage{}
}

func (s *ProxyService) releaseAllLocked(why string) {
	for _, r := range s.records {
		s.releaseLocked(r.capture, why)
	}
}

// ProxyCapturePart describes one retained header block or body. Preview is
// short; Size is what Copy returns in full.
type ProxyCapturePart struct {
	Preview   string `json:"preview"`
	Truncated bool   `json:"truncated"` // the preview is shorter than the retained content
	Size      int64  `json:"size"`      // retained bytes
	Observed  int64  `json:"observed"`  // bytes that passed through the proxy
	State     string `json:"state"`     // complete | capturing | incomplete | none
	Binary    bool   `json:"binary"`
	Note      string `json:"note"`
}

type ProxyCaptureSide struct {
	Line    string           `json:"line"`
	Headers ProxyCapturePart `json:"headers"`
	Body    ProxyCapturePart `json:"body"`
}

// ProxyCaptureView is the preview-only Inspector payload for one Activity entry.
type ProxyCaptureView struct {
	Success  bool             `json:"success"`
	Message  string           `json:"message"`
	Entry    ProxyActivity    `json:"entry"`
	State    string           `json:"state"` // capturing | complete | incomplete | released | none
	Note     string           `json:"note"`
	Request  ProxyCaptureSide `json:"request"`
	Response ProxyCaptureSide `json:"response"`
}

var sensitiveHeaders = map[string]bool{"Authorization": true, "Proxy-Authorization": true, "Cookie": true, "Set-Cookie": true, "X-Api-Key": true, "Api-Key": true}

// formatHeaders renders a header block; masked hides credential-bearing values.
func formatHeaders(m *captureMessage, masked bool) string {
	var lines []string
	if m.host != "" {
		lines = append(lines, "Host: "+m.host)
	}
	names := make([]string, 0, len(m.header))
	for name := range m.header {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		for _, value := range m.header[name] {
			if masked && sensitiveHeaders[http.CanonicalHeaderKey(name)] {
				value = fmt.Sprintf("•••• (%d characters hidden; Copy includes it)", len(value))
			}
			lines = append(lines, name+": "+value)
		}
	}
	return strings.Join(lines, "\n")
}

func (b *captureBody) bytesSnapshot(limit int64) []byte {
	out := make([]byte, 0, min(b.stored, limit))
	for _, chunk := range b.chunks {
		if int64(len(out)) >= limit {
			break
		}
		out = append(out, chunk[:min(int64(len(chunk)), limit-int64(len(out)))]...)
	}
	return out
}

func isText(data []byte) bool {
	return utf8.Valid(data) && !strings.ContainsRune(string(data), 0)
}

// textPrefix trims an incomplete trailing rune so a preview cut is still text.
func textPrefix(data []byte) []byte {
	for i := 0; i < utf8.UTFMax && len(data) > 0 && !utf8.Valid(data); i++ {
		data = data[:len(data)-1]
	}
	return data
}

func (s *ProxyService) recordLocked(id int64) *activityRecord {
	for i := len(s.records) - 1; i >= 0; i-- {
		if s.records[i].ID == id {
			return s.records[i]
		}
	}
	return nil
}

// Capture returns the Inspector view (previews and sizes only) for an entry.
func (s *ProxyService) Capture(id int64) ProxyCaptureView {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.recordLocked(id)
	if r == nil {
		return ProxyCaptureView{Message: "This entry is no longer retained.", State: "none"}
	}
	view := ProxyCaptureView{Success: true, Entry: r.ProxyActivity, Note: r.Error}
	c := r.capture
	switch {
	case c == nil:
		view.State, view.Note = "none", "Debug was off when this request started; nothing was captured."
		return view
	case c.released != "":
		view.State, view.Note = "released", c.released
		return view
	}
	view.Request = sideView(&c.request, true, "")
	missing := "No response was received."
	if r.Pending {
		missing = "Waiting for the response…"
	} else if r.Error != "" {
		missing = "No upstream response: " + r.Error
	}
	view.Response = sideView(&c.response, c.responded, missing)
	parts := []ProxyCapturePart{view.Request.Headers, view.Request.Body, view.Response.Headers, view.Response.Body}
	view.State = "complete"
	for _, part := range parts {
		if part.State == "incomplete" || part.State == "none" {
			view.State = "incomplete"
		}
	}
	for _, part := range parts {
		if part.State == "capturing" || r.Pending {
			view.State = "capturing"
		}
	}
	if c.stopped != "" && view.Note == "" {
		view.Note = c.stopped
	}
	return view
}

func sideView(m *captureMessage, present bool, missing string) ProxyCaptureSide {
	if !present || m.header == nil {
		note := missing
		if m.body.reason != "" {
			note = m.body.reason
		}
		none := ProxyCapturePart{State: "none", Note: note}
		return ProxyCaptureSide{Headers: none, Body: none}
	}
	side := ProxyCaptureSide{Line: m.line}
	full := formatHeaders(m, false)
	preview := formatHeaders(m, true)
	side.Headers = ProxyCapturePart{Size: int64(len(full)), Observed: int64(len(full)), State: "complete"}
	side.Headers.Preview = string(textPrefix([]byte(preview[:min(len(preview), capturePreviewLimit)])))
	side.Headers.Truncated = len(side.Headers.Preview) < len(preview)
	if preview != full {
		side.Headers.Note = "Credential headers are hidden in the preview."
	}
	b := &m.body
	part := ProxyCapturePart{Size: b.stored, Observed: b.observed, State: "complete", Note: b.reason}
	switch {
	case b.reason != "":
		part.State = "incomplete"
	case !b.done:
		part.State = "capturing"
	}
	head := b.bytesSnapshot(capturePreviewLimit)
	if prefix := textPrefix(head); isText(prefix) {
		part.Preview = string(prefix)
	} else {
		part.Binary = true
	}
	part.Truncated = int64(len(part.Preview)) < b.stored
	side.Body = part
	return side
}

// ProxyCopyResponse reports what an explicit Copy action placed on the clipboard.
type ProxyCopyResponse struct {
	Success  bool   `json:"success"`
	Message  string `json:"message"`
	Bytes    int64  `json:"bytes"`
	Base64   bool   `json:"base64"`
	Complete bool   `json:"complete"`
}

// CopyCapture copies one complete retained part (requestHeaders, requestBody,
// responseHeaders or responseBody) to the system clipboard from Go, so the full
// content never passes through the frontend. Binary bodies are copied as base64.
func (s *ProxyService) CopyCapture(id int64, part string) ProxyCopyResponse {
	s.mu.Lock()
	r := s.recordLocked(id)
	if r == nil || r.capture == nil || r.capture.released != "" {
		s.mu.Unlock()
		return ProxyCopyResponse{Message: "Capture is no longer retained."}
	}
	c := r.capture
	var m *captureMessage
	switch part {
	case "requestHeaders", "requestBody":
		m = &c.request
	case "responseHeaders", "responseBody":
		m = &c.response
	default:
		s.mu.Unlock()
		return ProxyCopyResponse{Message: "Unknown capture part."}
	}
	if m.header == nil {
		s.mu.Unlock()
		return ProxyCopyResponse{Message: "Nothing was captured for this part."}
	}
	// Body chunks are append-only, so the retained prefix can be joined after
	// unlocking even while a stream is still being captured.
	var data []byte
	var chunks [][]byte
	complete := true
	if strings.HasSuffix(part, "Headers") {
		data = []byte(formatHeaders(m, false))
	} else {
		chunks, complete = append([][]byte(nil), m.body.chunks...), m.body.done && m.body.reason == ""
		data = make([]byte, 0, m.body.stored)
	}
	s.mu.Unlock()
	for _, chunk := range chunks {
		data = append(data, chunk...)
	}
	text, encoded := string(data), false
	if !isText(data) {
		text, encoded = base64.StdEncoding.EncodeToString(data), true
	}
	if !s.setClipboard(text) {
		return ProxyCopyResponse{Message: "Could not write to the clipboard."}
	}
	message := fmt.Sprintf("Copied %s", formatSize(int64(len(data))))
	if encoded {
		message += " as base64 (binary content)"
	}
	if !complete {
		message += " — capture is incomplete; this is not the full original body"
	}
	return ProxyCopyResponse{Success: true, Message: message + ".", Bytes: int64(len(data)), Base64: encoded, Complete: complete}
}

func (s *ProxyService) setClipboard(text string) bool {
	if s.clipboard != nil {
		return s.clipboard(text)
	}
	if app := application.Get(); app != nil && app.Clipboard != nil {
		return app.Clipboard.SetText(text)
	}
	return false
}

func formatSize(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KiB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d bytes", n)
}
