package store

import (
	"errors"
	"time"
)

// The types and errors below carry no SQLite dependency, so they live in a
// file without a build constraint: packages that only need the data model
// (service, httpapi, and the in-memory store used by the WebAssembly demo)
// can build for js/wasm, where the SQLite driver cannot.

var (
	// ErrNotFound means no link has the requested code.
	ErrNotFound = errors.New("store: link not found")
	// ErrConflict means the code is already taken.
	ErrConflict = errors.New("store: code already exists")
)

// Link is a stored short link. A zero ExpiresAt means it never expires.
type Link struct {
	ID        int64
	Code      string
	URL       string
	CreatedAt time.Time
	ExpiresAt time.Time
}

// DayCount is the number of clicks on one UTC day ("2006-01-02").
type DayCount struct {
	Day    string
	Clicks int64
}

// ReferrerCount is the number of clicks from one referring host ("" is direct).
type ReferrerCount struct {
	Host   string
	Clicks int64
}

// Stats summarises a link's clicks. Daily is sparse: days without clicks
// are omitted.
type Stats struct {
	Total     int64
	Daily     []DayCount
	Referrers []ReferrerCount
}
