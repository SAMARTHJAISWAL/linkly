// Package memstore is an in-memory implementation of linkly's persistence
// interfaces. It exists for the WebAssembly demo, where SQLite is not
// available: the real service, HTTP handler and click recorder run on top
// of it unchanged. It mirrors the SQLite store's semantics (unique codes,
// cascading deletes, clicks for unknown codes skipped, second-precision
// timestamps, stats ordering) and can be snapshotted to JSON so the browser
// can keep its state in localStorage.
package memstore

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/SAMARTHJAISWAL/linkly/internal/analytics"
	"github.com/SAMARTHJAISWAL/linkly/internal/store"
)

// Store is safe for concurrent use. The zero value is not usable; use New.
type Store struct {
	mu       sync.RWMutex
	nextID   int64
	links    map[string]store.Link // by code
	clicks   []click
	onChange func()
}

// click mirrors a row of the clicks table: Unix seconds, like SQLite.
type click struct {
	LinkID  int64  `json:"l"`
	At      int64  `json:"t"`
	RefHost string `json:"r,omitempty"`
}

// New returns an empty store.
func New() *Store {
	return &Store{nextID: 1, links: make(map[string]store.Link)}
}

// OnChange registers fn to be called after every successful mutation,
// outside the store's lock. The demo uses it to persist snapshots.
func (s *Store) OnChange(fn func()) {
	s.mu.Lock()
	s.onChange = fn
	s.mu.Unlock()
}

func (s *Store) changed() {
	s.mu.RLock()
	fn := s.onChange
	s.mu.RUnlock()
	if fn != nil {
		fn()
	}
}

// Ping always succeeds; it implements httpapi.Pinger.
func (s *Store) Ping(context.Context) error { return nil }

// CreateLink stores l with a fresh ID, or returns store.ErrConflict if the
// code is taken.
func (s *Store) CreateLink(_ context.Context, l store.Link) (store.Link, error) {
	s.mu.Lock()
	if _, taken := s.links[l.Code]; taken {
		s.mu.Unlock()
		return store.Link{}, store.ErrConflict
	}
	l.ID = s.nextID
	s.nextID++
	l.CreatedAt = truncate(l.CreatedAt)
	l.ExpiresAt = truncate(l.ExpiresAt)
	s.links[l.Code] = l
	s.mu.Unlock()
	s.changed()
	return l, nil
}

// GetLink returns the link with the given code or store.ErrNotFound.
func (s *Store) GetLink(_ context.Context, code string) (store.Link, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	l, ok := s.links[code]
	if !ok {
		return store.Link{}, store.ErrNotFound
	}
	return l, nil
}

// DeleteLink removes a link and its clicks.
func (s *Store) DeleteLink(_ context.Context, code string) error {
	s.mu.Lock()
	l, ok := s.links[code]
	if !ok {
		s.mu.Unlock()
		return store.ErrNotFound
	}
	delete(s.links, code)
	s.clicks = slices.DeleteFunc(s.clicks, func(c click) bool { return c.LinkID == l.ID })
	s.mu.Unlock()
	s.changed()
	return nil
}

// InsertClicks stores a batch of clicks, skipping codes that no longer
// exist, and reports how many were stored. It implements analytics.Sink.
func (s *Store) InsertClicks(_ context.Context, clicks []analytics.Click) (int, error) {
	s.mu.Lock()
	n := 0
	for _, c := range clicks {
		l, ok := s.links[c.Code]
		if !ok {
			continue
		}
		s.clicks = append(s.clicks, click{LinkID: l.ID, At: c.At.Unix(), RefHost: c.RefHost})
		n++
	}
	s.mu.Unlock()
	if n > 0 {
		s.changed()
	}
	return n, nil
}

// Stats returns the total click count for a link, its clicks per UTC day
// at or after since (sparse, oldest first) and its top referrers (most
// clicks first, ties broken by host).
func (s *Store) Stats(_ context.Context, linkID int64, since time.Time, topReferrers int) (store.Stats, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	st := store.Stats{Daily: []store.DayCount{}, Referrers: []store.ReferrerCount{}}
	days := map[string]int64{}
	refs := map[string]int64{}
	cutoff := since.Unix()
	for _, c := range s.clicks {
		if c.LinkID != linkID {
			continue
		}
		st.Total++
		refs[c.RefHost]++
		if c.At >= cutoff {
			days[time.Unix(c.At, 0).UTC().Format(time.DateOnly)]++
		}
	}
	for day, n := range days {
		st.Daily = append(st.Daily, store.DayCount{Day: day, Clicks: n})
	}
	slices.SortFunc(st.Daily, func(a, b store.DayCount) int { return cmp.Compare(a.Day, b.Day) })
	for host, n := range refs {
		st.Referrers = append(st.Referrers, store.ReferrerCount{Host: host, Clicks: n})
	}
	slices.SortFunc(st.Referrers, func(a, b store.ReferrerCount) int {
		return cmp.Or(cmp.Compare(b.Clicks, a.Clicks), cmp.Compare(a.Host, b.Host))
	})
	if topReferrers >= 0 && len(st.Referrers) > topReferrers {
		st.Referrers = st.Referrers[:topReferrers]
	}
	return st, nil
}

// snapshotVersion is bumped whenever the snapshot format changes; Restore
// refuses formats it does not know rather than guessing.
const snapshotVersion = 1

type snapshot struct {
	Version int        `json:"v"`
	NextID  int64      `json:"next_id"`
	Links   []linkJSON `json:"links"`
	Clicks  []click    `json:"clicks"`
}

type linkJSON struct {
	ID        int64  `json:"id"`
	Code      string `json:"code"`
	URL       string `json:"url"`
	CreatedAt int64  `json:"created"`
	ExpiresAt int64  `json:"expires,omitempty"` // 0 means never
}

// Snapshot serialises the whole store to JSON. Links are ordered by ID so
// the output is deterministic.
func (s *Store) Snapshot() ([]byte, error) {
	s.mu.RLock()
	snap := snapshot{Version: snapshotVersion, NextID: s.nextID, Clicks: s.clicks}
	snap.Links = make([]linkJSON, 0, len(s.links))
	for _, l := range s.links {
		lj := linkJSON{ID: l.ID, Code: l.Code, URL: l.URL, CreatedAt: l.CreatedAt.Unix()}
		if !l.ExpiresAt.IsZero() {
			lj.ExpiresAt = l.ExpiresAt.Unix()
		}
		snap.Links = append(snap.Links, lj)
	}
	slices.SortFunc(snap.Links, func(a, b linkJSON) int { return cmp.Compare(a.ID, b.ID) })
	if snap.Clicks == nil {
		snap.Clicks = []click{}
	}
	b, err := json.Marshal(snap)
	s.mu.RUnlock()
	if err != nil {
		return nil, err
	}
	return b, nil
}

// Restore replaces the store's contents with a snapshot. On error the
// store is left unchanged. It does not call the OnChange hook: restoring
// is how external state comes in, not a local mutation.
func (s *Store) Restore(data []byte) error {
	var snap snapshot
	if err := json.Unmarshal(data, &snap); err != nil {
		return fmt.Errorf("memstore: decoding snapshot: %w", err)
	}
	if snap.Version != snapshotVersion {
		return fmt.Errorf("memstore: unsupported snapshot version %d", snap.Version)
	}
	links := make(map[string]store.Link, len(snap.Links))
	ids := make(map[int64]bool, len(snap.Links))
	next := snap.NextID
	for _, lj := range snap.Links {
		if lj.ID <= 0 || lj.Code == "" || ids[lj.ID] {
			return fmt.Errorf("memstore: invalid link %q in snapshot", lj.Code)
		}
		if _, dup := links[lj.Code]; dup {
			return fmt.Errorf("memstore: duplicate code %q in snapshot", lj.Code)
		}
		l := store.Link{ID: lj.ID, Code: lj.Code, URL: lj.URL, CreatedAt: time.Unix(lj.CreatedAt, 0).UTC()}
		if lj.ExpiresAt != 0 {
			l.ExpiresAt = time.Unix(lj.ExpiresAt, 0).UTC()
		}
		links[l.Code] = l
		ids[l.ID] = true
		next = max(next, l.ID+1)
	}
	// Drop clicks whose link is gone, as ON DELETE CASCADE would have.
	clicks := slices.DeleteFunc(slices.Clone(snap.Clicks), func(c click) bool { return !ids[c.LinkID] })

	s.mu.Lock()
	s.nextID = max(next, 1)
	s.links = links
	s.clicks = clicks
	s.mu.Unlock()
	return nil
}

// Len reports how many links are stored.
func (s *Store) Len() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.links)
}

func truncate(t time.Time) time.Time {
	if t.IsZero() {
		return t
	}
	return time.Unix(t.Unix(), 0).UTC()
}
