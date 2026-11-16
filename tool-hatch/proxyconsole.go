package main

import (
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/dop251/goja"
)

// Console output exists only in memory: it is never logged or written to disk,
// and contains only what scripts explicitly print.
const (
	proxyConsoleLimit      = 300
	proxyConsoleEntryLimit = 4 << 10
)

type ProxyConsoleEntry struct {
	ID      int64  `json:"id"`
	Time    string `json:"time"`
	Level   string `json:"level"`
	Message string `json:"message"`
}

// ProxyConsoleResponse returns entries after a cursor. Truncated reports that
// entries the caller had not seen were evicted (not cleared) in the meantime.
type ProxyConsoleResponse struct {
	Entries   []ProxyConsoleEntry `json:"entries"`
	LastID    int64               `json:"lastID"`
	Truncated bool                `json:"truncated"`
}

type proxyConsole struct {
	mu        sync.Mutex
	entries   []ProxyConsoleEntry
	lastID    int64
	clearedAt int64
}

func (c *proxyConsole) add(level, message string) {
	if len(message) > proxyConsoleEntryLimit {
		message = strings.ToValidUTF8(message[:proxyConsoleEntryLimit], "") + "…"
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lastID++
	c.entries = append(c.entries, ProxyConsoleEntry{ID: c.lastID, Time: time.Now().Format("2006-01-02T15:04:05.000Z07:00"), Level: level, Message: message})
	if len(c.entries) > proxyConsoleLimit {
		c.entries = append(c.entries[:0:0], c.entries[len(c.entries)-proxyConsoleLimit:]...)
	}
}

func (c *proxyConsole) since(afterID int64) ProxyConsoleResponse {
	c.mu.Lock()
	defer c.mu.Unlock()
	response := ProxyConsoleResponse{Entries: []ProxyConsoleEntry{}, LastID: c.lastID}
	for _, entry := range c.entries {
		if entry.ID > afterID {
			response.Entries = append(response.Entries, entry)
		}
	}
	if len(response.Entries) > 0 {
		first := response.Entries[0].ID
		response.Truncated = first > afterID+1 && first-1 > c.clearedAt
	}
	return response
}

func (c *proxyConsole) clear() ProxyConsoleResponse {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries, c.clearedAt = nil, c.lastID
	return ProxyConsoleResponse{Entries: []ProxyConsoleEntry{}, LastID: c.lastID}
}

func (c *proxyConsole) last() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lastID
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
