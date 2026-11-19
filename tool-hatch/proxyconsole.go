package main

import (
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/dop251/goja"
)

// Console output exists only in memory: it is never logged or written to disk,
// and contains only what scripts explicitly print. Go keeps each full message
// (within the budgets below); the frontend receives only a short preview and
// copies the full text by entry ID through Go.
const (
	proxyConsoleDefaultLimit = 500
	proxyConsolePreviewLimit = 2 << 10
	proxyConsoleEntryMax     = 8 << 20 // one message beyond this is cut and marked incomplete
)

var (
	proxyConsoleLimits = []int{100, 500, 1000}
	// proxyConsoleBudget bounds the retained message bytes (a var for tests).
	proxyConsoleBudget = 32 << 20
)

// ProxyConsoleEntry is what the frontend sees: Message is a preview of at most
// 2 KiB; Size is the retained full message length.
type ProxyConsoleEntry struct {
	ID               int64  `json:"id"`
	Time             string `json:"time"`
	Level            string `json:"level"`
	Message          string `json:"message"`
	Size             int    `json:"size"`
	PreviewTruncated bool   `json:"previewTruncated"`
	Incomplete       bool   `json:"incomplete"` // the retained message itself was cut at the per-entry limit
}

// ProxyConsoleResponse returns entries after a cursor. Truncated reports that
// entries the caller had not seen were evicted (not cleared) in the meantime.
type ProxyConsoleResponse struct {
	Entries   []ProxyConsoleEntry `json:"entries"`
	LastID    int64               `json:"lastID"`
	Truncated bool                `json:"truncated"`
	Limit     int                 `json:"limit"`
}

type consoleRecord struct {
	ProxyConsoleEntry
	full string
}

type proxyConsole struct {
	mu        sync.Mutex
	entries   []consoleRecord
	bytes     int
	limit     int
	lastID    int64
	clearedAt int64
}

func (c *proxyConsole) limitLocked() int {
	if c.limit == 0 {
		return proxyConsoleDefaultLimit
	}
	return c.limit
}

// utf8Prefix cuts text to at most n bytes on a valid UTF-8 boundary.
func utf8Prefix(text string, n int) string {
	if len(text) <= n {
		return text
	}
	return strings.ToValidUTF8(text[:n], "")
}

func (c *proxyConsole) add(level, message string) {
	entry := ProxyConsoleEntry{Time: time.Now().Format("2006-01-02T15:04:05.000Z07:00"), Level: level}
	if max := min(proxyConsoleEntryMax, proxyConsoleBudget); len(message) > max {
		message, entry.Incomplete = utf8Prefix(message, max), true
	}
	entry.Message = utf8Prefix(message, proxyConsolePreviewLimit)
	entry.Size, entry.PreviewTruncated = len(message), len(entry.Message) < len(message)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lastID++
	entry.ID = c.lastID
	c.entries = append(c.entries, consoleRecord{entry, message})
	c.bytes += len(message)
	c.trimLocked()
}

// trimLocked evicts the oldest entries beyond the entry count or byte budget.
func (c *proxyConsole) trimLocked() {
	drop := 0
	for len(c.entries)-drop > c.limitLocked() || (c.bytes > proxyConsoleBudget && drop < len(c.entries)) {
		c.bytes -= len(c.entries[drop].full)
		drop++
	}
	if drop > 0 {
		c.entries = append(c.entries[:0:0], c.entries[drop:]...)
	}
}

func (c *proxyConsole) since(afterID int64) ProxyConsoleResponse {
	c.mu.Lock()
	defer c.mu.Unlock()
	response := ProxyConsoleResponse{Entries: []ProxyConsoleEntry{}, LastID: c.lastID, Limit: c.limitLocked()}
	for _, entry := range c.entries {
		if entry.ID > afterID {
			response.Entries = append(response.Entries, entry.ProxyConsoleEntry)
		}
	}
	if len(response.Entries) > 0 {
		first := response.Entries[0].ID
		response.Truncated = first > afterID+1 && first-1 > c.clearedAt
	}
	return response
}

// full returns the retained message for an entry ID.
func (c *proxyConsole) full(id int64) (string, bool, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for i := len(c.entries) - 1; i >= 0; i-- {
		if c.entries[i].ID == id {
			return c.entries[i].full, c.entries[i].Incomplete, true
		}
	}
	return "", false, false
}

func (c *proxyConsole) setLimit(limit int) ProxyConsoleResponse {
	c.mu.Lock()
	defer c.mu.Unlock()
	response := ProxyConsoleResponse{Entries: []ProxyConsoleEntry{}, LastID: c.lastID}
	for _, n := range proxyConsoleLimits {
		if n == limit {
			c.limit = limit
			c.trimLocked()
		}
	}
	response.Limit = c.limitLocked()
	return response
}

func (c *proxyConsole) clear() ProxyConsoleResponse {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries, c.bytes, c.clearedAt = nil, 0, c.lastID
	return ProxyConsoleResponse{Entries: []ProxyConsoleEntry{}, LastID: c.lastID, Limit: c.limitLocked()}
}

func (c *proxyConsole) state() (int64, int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lastID, c.limitLocked()
}

// installConsole defines console.log/info/warn/error/debug. Arguments are joined
// with spaces: strings verbatim, objects via JSON.stringify, everything else via
// String(). Format specifiers such as %s are not interpreted.
func installConsole(rt *goja.Runtime, log func(level, message string)) {
	console := rt.NewObject()
	stringify, _ := goja.AssertFunction(rt.Get("JSON").ToObject(rt).Get("stringify"))
	for _, level := range []string{"log", "info", "warn", "error", "debug"} {
		level := level
		_ = console.Set(level, func(call goja.FunctionCall) goja.Value {
			parts := make([]string, 0, len(call.Arguments))
			for _, arg := range call.Arguments {
				parts = append(parts, consoleText(rt, arg, stringify))
			}
			log(level, strings.Join(parts, " "))
			return goja.Undefined()
		})
	}
	_ = rt.Set("console", console)
}

func consoleText(rt *goja.Runtime, value goja.Value, stringify goja.Callable) string {
	if value == nil || goja.IsUndefined(value) {
		return "undefined"
	}
	object, ok := value.(*goja.Object)
	if !ok {
		return value.String()
	}
	if _, isFunction := goja.AssertFunction(value); isFunction || object.ClassName() == "Error" {
		return value.String()
	}
	encoded, err := stringify(goja.Undefined(), value)
	if err == nil && !goja.IsUndefined(encoded) {
		return encoded.String()
	}
	// A timeout/cancel that fired inside stringify must still stop the script.
	var interrupted *goja.InterruptedError
	if errors.As(err, &interrupted) {
		rt.Interrupt(interrupted.Value())
	}
	return value.String() // e.g. circular structures
}
