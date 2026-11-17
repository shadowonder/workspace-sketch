package main

import (
	"context"
	"errors"
	"math"
	"sync"
	"time"

	"github.com/dop251/goja"
)

// The shared proxy context is a small Go-owned string cache shared by every
// request of one running proxy (for example a cached access token). Each Start
// creates an empty one; Stop discards it. It never holds JavaScript values: keys
// and values are copied into Go strings, so no object crosses runtimes. It is
// memory-only and never logged, persisted or shown in Activity/Console.
const (
	proxyContextMaxEntries  = 1000
	proxyContextMaxKeyBytes = 256
	proxyContextMaxValue    = 1 << 20
	proxyContextMaxBytes    = 8 << 20
	proxyContextMaxTTL      = 365 * 24 * time.Hour
)

var (
	errContextClosed = errors.New("context: the proxy is stopping")
	errContextFull   = errors.New("context: storage limit reached (1000 entries, 8 MiB)")
	// Shared only with requests already waiting; nothing is cached on failure.
	errContextLoadFailed = errors.New("context.getOrLoad: the loader failed in a concurrent request")
	errContextAbandoned  = errors.New("context.getOrLoad: the loading request ended")
)

// contextTypeError marks invalid script input; it is thrown as a TypeError.
type contextTypeError string

func (e contextTypeError) Error() string { return string(e) }

type contextEntry struct {
	value   string
	expires time.Time // zero means no expiry; keeps time.Now's monotonic reading
}

// contextLoad is one in-flight getOrLoad for a key. Waiters block on done.
type contextLoad struct {
	done    chan struct{}
	waiters int
	value   string
	err     error
	retry   bool // the leader was canceled or timed out: a waiter should load instead
}

type proxyContext struct {
	mu       sync.Mutex
	entries  map[string]contextEntry
	loads    map[string]*contextLoad
	bytes    int
	isClosed bool
	closed   chan struct{}
	now      func() time.Time // time.Now; replaced in tests
}

func newProxyContext() *proxyContext {
	return &proxyContext{entries: map[string]contextEntry{}, loads: map[string]*contextLoad{},
		closed: make(chan struct{}), now: time.Now}
}

func checkContextKey(key string) error {
	if key == "" || len(key) > proxyContextMaxKeyBytes {
		return contextTypeError("context: key must be a non-empty string of at most 256 bytes")
	}
	return nil
}

func checkContextValue(value string) error {
	if len(value) > proxyContextMaxValue {
		return contextTypeError("context: value must be a string of at most 1 MiB")
	}
	return nil
}

func (c *proxyContext) get(key string) (string, bool, error) {
	if err := checkContextKey(key); err != nil {
		return "", false, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.isClosed {
		return "", false, errContextClosed
	}
	value, ok := c.lookupLocked(key)
	return value, ok, nil
}

// set stores value; ttl 0 keeps it until deleted or the proxy stops.
func (c *proxyContext) set(key, value string, ttl time.Duration) error {
	if err := checkContextKey(key); err != nil {
		return err
	}
	if err := checkContextValue(value); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.storeLocked(key, value, ttl)
}

// delete reports whether an unexpired entry was removed.
func (c *proxyContext) delete(key string) (bool, error) {
	if err := checkContextKey(key); err != nil {
		return false, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.isClosed {
		return false, errContextClosed
	}
	_, ok := c.lookupLocked(key)
	c.removeLocked(key)
	return ok, nil
}

// getOrLoad returns a fresh cached value or runs load, at most one at a time
// per key. Concurrent callers for the same key wait for that load, each bounded
// by its own ctx; different keys load independently. load runs on the caller's
// goroutine (its own Goja runtime). A failed load is never cached: current
// waiters receive errContextLoadFailed, or retry if the leader was canceled.
func (c *proxyContext) getOrLoad(ctx context.Context, key string, load func() (string, time.Duration, error)) (string, error) {
	if err := checkContextKey(key); err != nil {
		return "", err
	}
	for {
		c.mu.Lock()
		if c.isClosed {
			c.mu.Unlock()
			return "", errContextClosed
		}
		if value, ok := c.lookupLocked(key); ok {
			c.mu.Unlock()
			return value, nil
		}
		if pending := c.loads[key]; pending != nil {
			pending.waiters++
			c.mu.Unlock()
			var err error
			select {
			case <-pending.done:
			case <-ctx.Done():
				err = ctx.Err()
			case <-c.closed:
				err = errContextClosed
			}
			c.mu.Lock()
			pending.waiters--
			c.mu.Unlock()
			if err != nil {
				return "", err
			}
			if pending.retry {
				continue
			}
			return pending.value, pending.err
		}
		pending := &contextLoad{done: make(chan struct{})}
		c.loads[key] = pending
		c.mu.Unlock()
		return c.lead(ctx, key, pending, load)
	}
}

func (c *proxyContext) lead(ctx context.Context, key string, pending *contextLoad, load func() (string, time.Duration, error)) (value string, err error) {
	var ttl time.Duration
	err = errContextAbandoned // stays set if load panics (a thrown JS value)
	defer func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		if c.loads[key] == pending {
			delete(c.loads, key)
		}
		if err == nil {
			if err = checkContextValue(value); err == nil {
				err = c.storeLocked(key, value, ttl)
			}
		}
		if err == nil {
			pending.value = value
		} else {
			value = ""
			pending.err = errContextLoadFailed
			pending.retry = errors.Is(err, errContextAbandoned) || ctx.Err() != nil
		}
		close(pending.done)
	}()
	value, ttl, err = load()
	return value, err
}

// close discards all entries and releases waiters; later calls fail.
func (c *proxyContext) close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.isClosed {
		return
	}
	c.isClosed = true
	c.entries, c.bytes = map[string]contextEntry{}, 0
	close(c.closed)
}

func (c *proxyContext) lookupLocked(key string) (string, bool) {
	entry, ok := c.entries[key]
	if !ok {
		return "", false
	}
	if !entry.expires.IsZero() && !c.now().Before(entry.expires) {
		c.removeLocked(key)
		return "", false
	}
	return entry.value, true
}

func (c *proxyContext) removeLocked(key string) {
	if entry, ok := c.entries[key]; ok {
		c.bytes -= len(key) + len(entry.value)
		delete(c.entries, key)
	}
}

func (c *proxyContext) storeLocked(key, value string, ttl time.Duration) error {
	if c.isClosed {
		return errContextClosed
	}
	fits := func() bool {
		count, bytes := len(c.entries), c.bytes+len(key)+len(value)
		if old, ok := c.entries[key]; ok {
			bytes -= len(key) + len(old.value)
		} else {
			count++
		}
		return count <= proxyContextMaxEntries && bytes <= proxyContextMaxBytes
	}
	if !fits() {
		now := c.now()
		for k, entry := range c.entries {
			if !entry.expires.IsZero() && !now.Before(entry.expires) {
				c.removeLocked(k)
			}
		}
		if !fits() {
			return errContextFull
		}
	}
	c.removeLocked(key)
	entry := contextEntry{value: value}
	if ttl > 0 {
		entry.expires = c.now().Add(ttl)
	}
	c.entries[key] = entry
	c.bytes += len(key) + len(value)
	return nil
}

// installContext defines the global context object. Like http.request it works
// only while onRequest runs; the Start validation runtime has no context.
func (p *proxyRuntime) installContext() {
	rt := p.rt
	fail := func(err error) {
		var interrupted *goja.InterruptedError
		var exception *goja.Exception
		var typeErr contextTypeError
		switch {
		case errors.As(err, &interrupted):
			panic(interrupted) // keep the time limit/cancel uncatchable
		case errors.As(err, &exception):
			panic(exception) // rethrow the loader's own error unchanged
		case p.ctx.Err() != nil:
			panic(rt.NewGoError(p.ctx.Err())) // surfaced as the script-level timeout/cancel
		case errors.As(err, &typeErr):
			panic(rt.NewTypeError(typeErr.Error()))
		default:
			panic(rt.NewGoError(err))
		}
	}
	shared := func(method string) *proxyContext {
		if !p.active || p.shared == nil {
			panic(rt.NewGoError(errors.New("context." + method + ": only available while onRequest runs")))
		}
		return p.shared
	}
	str := func(value goja.Value, what string) string {
		text, ok := value.Export().(string)
		if !ok {
			panic(rt.NewTypeError("context: " + what + " must be a string"))
		}
		return text
	}
	object := rt.NewObject()
	_ = object.Set("get", func(call goja.FunctionCall) goja.Value {
		value, ok, err := shared("get").get(str(call.Argument(0), "key"))
		if err != nil {
			fail(err)
		}
		if !ok {
			return goja.Null()
		}
		return rt.ToValue(value)
	})
	_ = object.Set("set", func(call goja.FunctionCall) goja.Value {
		c := shared("set")
		key, value := str(call.Argument(0), "key"), str(call.Argument(1), "value")
		ttl, err := contextTTL(call.Argument(2))
		if err == nil {
			err = c.set(key, value, ttl)
		}
		if err != nil {
			fail(err)
		}
		return goja.Undefined()
	})
	_ = object.Set("delete", func(call goja.FunctionCall) goja.Value {
		removed, err := shared("delete").delete(str(call.Argument(0), "key"))
		if err != nil {
			fail(err)
		}
		return rt.ToValue(removed)
	})
	_ = object.Set("getOrLoad", func(call goja.FunctionCall) goja.Value {
		c := shared("getOrLoad")
		key := str(call.Argument(0), "key")
		loader, ok := goja.AssertFunction(call.Argument(1))
		if !ok {
			panic(rt.NewTypeError("context.getOrLoad: loader must be a function"))
		}
		if p.loading {
			// Nested loads could wait on each other across requests; forbid them.
			panic(rt.NewGoError(errors.New("context.getOrLoad cannot be called inside a loader")))
		}
		value, err := c.getOrLoad(p.ctx, key, func() (string, time.Duration, error) {
			p.loading = true
			result, err := loader(goja.Undefined())
			p.loading = false
			if err != nil {
				return "", 0, err
			}
			return contextLoaderResult(result)
		})
		if err != nil {
			fail(err)
		}
		return rt.ToValue(value)
	})
	_ = rt.Set("context", object)
}

const contextLoaderShape = "context.getOrLoad: loader must return { value: string, ttlSeconds?: number }"

func contextLoaderResult(result goja.Value) (string, time.Duration, error) {
	object, ok := result.(*goja.Object)
	if !ok {
		return "", 0, contextTypeError(contextLoaderShape)
	}
	raw := object.Get("value") // nil when the property is missing
	if raw == nil {
		return "", 0, contextTypeError(contextLoaderShape)
	}
	value, ok := raw.Export().(string)
	if !ok {
		return "", 0, contextTypeError(contextLoaderShape)
	}
	ttl, err := contextTTL(object.Get("ttlSeconds"))
	return value, ttl, err
}

// contextTTL converts optional ttlSeconds (undefined/null: no expiry) to a
// duration: a positive number of seconds, fractions allowed, at most one year.
func contextTTL(value goja.Value) (time.Duration, error) {
	if value == nil || goja.IsUndefined(value) || goja.IsNull(value) {
		return 0, nil
	}
	var seconds float64
	switch n := value.Export().(type) {
	case int64:
		seconds = float64(n)
	case float64:
		seconds = n
	default:
		return 0, contextTypeError("context: ttlSeconds must be a number")
	}
	invalid := contextTypeError("context: ttlSeconds must be a positive number of seconds, at most 31536000 (one year)")
	if math.IsNaN(seconds) || seconds <= 0 || seconds > proxyContextMaxTTL.Seconds() {
		return 0, invalid
	}
	ttl := time.Duration(seconds * float64(time.Second))
	if ttl <= 0 {
		return 0, invalid
	}
	return ttl, nil
}
