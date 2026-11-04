//go:build js && wasm

// Command linkly-wasm runs linkly inside a web browser for the static demo
// on GitHub Pages. It wires the real service, HTTP handler, rate limiter,
// click recorder and metrics exactly as the server does, but on top of an
// in-memory store that is snapshotted to localStorage instead of SQLite.
//
// There is no network listener. The page calls into Go through a small
// JavaScript API installed on globalThis.linkly:
//
//	linkly.handle(method, path, headers, body) -> Promise<{status, headers, body}>
//	    Runs one request through the real http.Handler. headers is a plain
//	    object, body a string, Uint8Array or null; the response body is
//	    always a Uint8Array so binary responses (QR PNGs) round-trip.
//	linkly.flush() -> Promise<boolean>
//	    Resolves once every recorded click has been written to the store
//	    (and so persisted), or false after a timeout.
//	linkly.storage() -> "ok" | "unavailable" | "error"
//	    Whether snapshots are reaching localStorage.
//
// Configuration is read from globalThis.linklyConfig = {baseURL, storageKey}
// before main runs, and globalThis.linklyConfig.onReady() is called once
// the API is installed.
package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"sync"
	"syscall/js"
	"time"

	"github.com/SAMARTHJAISWAL/linkly/internal/analytics"
	"github.com/SAMARTHJAISWAL/linkly/internal/config"
	"github.com/SAMARTHJAISWAL/linkly/internal/httpapi"
	"github.com/SAMARTHJAISWAL/linkly/internal/memstore"
	"github.com/SAMARTHJAISWAL/linkly/internal/metrics"
	"github.com/SAMARTHJAISWAL/linkly/internal/ratelimit"
	"github.com/SAMARTHJAISWAL/linkly/internal/service"
	"github.com/SAMARTHJAISWAL/linkly/internal/store"
)

const (
	defaultStorageKey = "linkly-demo:v1"
	// clickFlushInterval is much shorter than the server's default (1s):
	// in the browser a redirect leaves the page, so clicks should reach
	// storage quickly. Redirects also wait for flush() explicitly.
	clickFlushInterval = 100 * time.Millisecond
	flushTimeout       = 3 * time.Second
	// remoteAddr is the "client address" every request appears to come
	// from. There is only one client, the visitor, so one rate-limit bucket
	// is exactly right.
	remoteAddr = "127.0.0.1:0"
)

func main() {
	cfgJS := js.Global().Get("linklyConfig")
	baseURL := jsString(cfgJS, "baseURL")
	if baseURL == "" {
		baseURL = "http://localhost/#"
	}
	key := jsString(cfgJS, "storageKey")
	if key == "" {
		key = defaultStorageKey
	}

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	defaults := config.Default()
	cfg := config.Config{BaseURL: baseURL}

	st := memstore.New()
	p := &persister{key: key, st: st, log: log}
	p.sync()
	st.OnChange(p.save)

	rec := analytics.NewRecorder(syncingSink{p}, analytics.Options{
		Buffer:        defaults.ClickBuffer,
		BatchSize:     defaults.ClickBatch,
		FlushInterval: clickFlushInterval,
		Logger:        log,
	})
	// The same limits the server ships with: bursts of 10 creations, then
	// one per second.
	lim := ratelimit.New(defaults.RateLimit, defaults.RateBurst, defaults.RateIdleTTL)
	go lim.Run(context.Background(), time.Minute)

	reg := metrics.NewRegistry()
	registerMetrics(reg, rec, lim)

	h := httpapi.New(httpapi.Options{
		Service:  service.New(syncingStore{st, p}, service.Options{SelfHost: cfg.BaseHost()}),
		Clicks:   rec,
		Health:   st,
		Limiter:  lim,
		Registry: reg,
		Logger:   log,
		BaseURL:  baseURL,
	})

	api := js.Global().Get("Object").New()
	api.Set("handle", js.FuncOf(func(_ js.Value, args []js.Value) any {
		return handle(h, args)
	}))
	api.Set("flush", js.FuncOf(func(js.Value, []js.Value) any {
		return promise(func() (any, error) {
			deadline := time.Now().Add(flushTimeout)
			for rec.Pending() > 0 {
				if time.Now().After(deadline) {
					return false, nil
				}
				time.Sleep(10 * time.Millisecond)
			}
			return true, nil
		})
	}))
	api.Set("storage", js.FuncOf(func(js.Value, []js.Value) any { return p.status() }))
	api.Set("goVersion", runtime.Version())
	js.Global().Set("linkly", api)

	if ready := cfgJS.Get("onReady"); cfgJS.Truthy() && ready.Type() == js.TypeFunction {
		ready.Invoke()
	}
	select {} // keep the Go runtime alive for callbacks
}

// handle runs one request through the handler; see the package comment.
func handle(h http.Handler, args []js.Value) any {
	if len(args) < 2 {
		return rejected("linkly.handle(method, path, headers, body): missing arguments")
	}
	method, path := args[0].String(), args[1].String()
	header := http.Header{}
	if len(args) > 2 && args[2].Type() == js.TypeObject {
		entries := js.Global().Get("Object").Call("entries", args[2])
		for i := range entries.Length() {
			kv := entries.Index(i)
			header.Add(kv.Index(0).String(), kv.Index(1).String())
		}
	}
	// Copy the body now: the JS value may be reused once we return.
	var body []byte
	if len(args) > 3 {
		switch b := args[3]; {
		case b.Type() == js.TypeString:
			body = []byte(b.String())
		case b.InstanceOf(js.Global().Get("Uint8Array")):
			body = make([]byte, b.Length())
			js.CopyBytesToGo(body, b)
		}
	}

	return promise(func() (any, error) {
		req, err := http.NewRequest(method, "http://linkly.invalid"+path, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		req.Header = header
		req.RemoteAddr = remoteAddr
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		res := w.Result()
		data, _ := io.ReadAll(res.Body)

		hdrs := js.Global().Get("Object").New()
		for k, v := range res.Header {
			if len(v) > 0 {
				hdrs.Set(k, v[0])
			}
		}
		out := js.Global().Get("Uint8Array").New(len(data))
		js.CopyBytesToJS(out, data)
		obj := js.Global().Get("Object").New()
		obj.Set("status", res.StatusCode)
		obj.Set("headers", hdrs)
		obj.Set("body", out)
		return obj, nil
	})
}

// promise runs fn on a goroutine (callbacks must never block the JS event
// loop) and settles a JS Promise with its result.
func promise(fn func() (any, error)) js.Value {
	executor := js.FuncOf(func(_ js.Value, args []js.Value) any {
		resolve, reject := args[0], args[1]
		go func() {
			defer func() {
				if v := recover(); v != nil {
					reject.Invoke(js.Global().Get("Error").New(fmt.Sprint("linkly: panic: ", v)))
				}
			}()
			v, err := fn()
			if err != nil {
				reject.Invoke(js.Global().Get("Error").New(err.Error()))
				return
			}
			resolve.Invoke(v)
		}()
		return nil
	})
	p := js.Global().Get("Promise").New(executor)
	executor.Release() // the executor runs synchronously inside the constructor
	return p
}

func rejected(msg string) js.Value {
	return js.Global().Get("Promise").Call("reject", js.Global().Get("Error").New(msg))
}

func jsString(obj js.Value, key string) string {
	if !obj.Truthy() {
		return ""
	}
	if v := obj.Get(key); v.Type() == js.TypeString {
		return v.String()
	}
	return ""
}

// persister mirrors the store to localStorage. Other tabs of the demo
// share the same key, so before every operation that reads or mutates
// links the latest snapshot is pulled in (sync), and after every mutation
// the store is written back (save). All storage access tolerates
// localStorage being absent, blocked or full: the demo then simply runs
// from memory.
type persister struct {
	key string
	st  *memstore.Store
	log *slog.Logger

	mu       sync.Mutex
	lastSeen string // the snapshot we last wrote or restored
	state    string // "ok", "unavailable" or "error"
}

func (p *persister) status() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.state
}

// sync restores the stored snapshot if another tab (or a previous page
// load) changed it since we last looked.
func (p *persister) sync() {
	p.mu.Lock()
	defer p.mu.Unlock()
	raw, ok := storageGet(p.key)
	if !ok {
		p.state = "unavailable"
		return
	}
	if p.state == "" {
		p.state = "ok"
	}
	if raw == "" || raw == p.lastSeen {
		return
	}
	p.lastSeen = raw
	if err := p.st.Restore([]byte(raw)); err != nil {
		p.log.Warn("ignoring unreadable demo state in localStorage", "err", err)
	}
}

func (p *persister) save() {
	data, err := p.st.Snapshot()
	if err != nil {
		p.log.Error("snapshotting demo state", "err", err)
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if storageSet(p.key, string(data)) {
		p.lastSeen = string(data)
		p.state = "ok"
	} else if p.state != "unavailable" {
		p.state = "error" // e.g. quota exceeded; keep running from memory
	}
}

// storageGet returns localStorage[key] ("" when unset) and whether
// localStorage could be used at all. Touching localStorage can throw
// (blocked cookies, sandboxed frames), which syscall/js turns into a panic.
func storageGet(key string) (val string, ok bool) {
	defer func() {
		if recover() != nil {
			val, ok = "", false
		}
	}()
	ls := js.Global().Get("localStorage")
	if !ls.Truthy() {
		return "", false
	}
	v := ls.Call("getItem", key)
	if v.Type() != js.TypeString {
		return "", true
	}
	return v.String(), true
}

func storageSet(key, val string) (ok bool) {
	defer func() {
		if recover() != nil {
			ok = false
		}
	}()
	ls := js.Global().Get("localStorage")
	if !ls.Truthy() {
		return false
	}
	ls.Call("setItem", key, val)
	return true
}

// syncingStore pulls in other tabs' changes before each store call, so a
// stale tab never overwrites newer state when it next saves.
type syncingStore struct {
	*memstore.Store
	p *persister
}

func (s syncingStore) CreateLink(ctx context.Context, l store.Link) (store.Link, error) {
	s.p.sync()
	return s.Store.CreateLink(ctx, l)
}

func (s syncingStore) GetLink(ctx context.Context, code string) (store.Link, error) {
	s.p.sync()
	return s.Store.GetLink(ctx, code)
}

func (s syncingStore) DeleteLink(ctx context.Context, code string) error {
	s.p.sync()
	return s.Store.DeleteLink(ctx, code)
}

// syncingSink does the same for the click recorder's batched writes.
type syncingSink struct{ p *persister }

func (s syncingSink) InsertClicks(ctx context.Context, clicks []analytics.Click) (int, error) {
	s.p.sync()
	return s.p.st.InsertClicks(ctx, clicks)
}

// registerMetrics mirrors the server's metrics (internal/app) so /metrics
// looks the same in the demo.
func registerMetrics(reg *metrics.Registry, rec *analytics.Recorder, lim *ratelimit.Limiter) {
	reg.NewCounterFunc("linkly_clicks_recorded_total", "Clicks written to the database.",
		func() float64 { return float64(rec.Recorded()) })
	reg.NewCounterFunc("linkly_clicks_dropped_total", "Clicks discarded because the queue was full.",
		func() float64 { return float64(rec.Dropped()) })
	reg.NewCounterFunc("linkly_clicks_failed_total", "Clicks lost to database write errors.",
		func() float64 { return float64(rec.Failed()) })
	reg.NewCounterFunc("linkly_clicks_orphaned_total", "Clicks discarded because their link was deleted before they were written.",
		func() float64 { return float64(rec.Orphaned()) })
	reg.NewGaugeFunc("linkly_click_queue_depth", "Clicks waiting to be written.",
		func() float64 { return float64(rec.Pending()) })
	reg.NewGaugeFunc("linkly_rate_limit_buckets", "Clients currently tracked by the rate limiter.",
		func() float64 { return float64(lim.Len()) })
	reg.NewGaugeFunc("go_goroutines", "Number of goroutines.",
		func() float64 { return float64(runtime.NumGoroutine()) })
}
