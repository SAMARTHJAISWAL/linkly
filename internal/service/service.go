// Package service holds linkly's business rules: validating new links,
// allocating codes, enforcing expiry and shaping statistics. It knows
// nothing about HTTP.
package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/SAMARTHJAISWAL/linkly/internal/store"
)

var (
	// ErrNotFound means no link has the code.
	ErrNotFound = errors.New("link not found")
	// ErrExpired means the link exists but its expiry has passed.
	ErrExpired = errors.New("link expired")
	// ErrAliasTaken means a custom alias is already in use.
	ErrAliasTaken = errors.New("alias already in use")
)

// ValidationError describes invalid input in one field.
type ValidationError struct {
	Field string
	Msg   string
}

func (e *ValidationError) Error() string { return e.Field + " " + e.Msg }

// Store is the persistence the service needs; *store.Store implements it.
type Store interface {
	CreateLink(ctx context.Context, l store.Link) (store.Link, error)
	GetLink(ctx context.Context, code string) (store.Link, error)
	DeleteLink(ctx context.Context, code string) error
	Stats(ctx context.Context, linkID int64, since time.Time, topReferrers int) (store.Stats, error)
}

const (
	// StatsDays is the length of the per-day click histogram.
	StatsDays = 30
	// TopReferrers is how many referring hosts stats report.
	TopReferrers = 10
	// MaxExpiry is how far in the future an expiry may be set.
	MaxExpiry = 10 * 365 * 24 * time.Hour
	// codeAttempts bounds retries on a generated-code collision. With 62^7
	// codes a single collision is already vanishingly rare; needing more
	// than a few retries means something is wrong (e.g. a broken RNG).
	codeAttempts = 5
)

// Service implements link operations. It is safe for concurrent use.
type Service struct {
	store    Store
	selfHost string
	now      func() time.Time
	rand     io.Reader // nil means crypto/rand
}

// Options configures a Service.
type Options struct {
	SelfHost string           // host of the public base URL; links to it are refused
	Now      func() time.Time // defaults to time.Now
	Rand     io.Reader        // defaults to crypto/rand.Reader
}

// New returns a Service backed by st.
func New(st Store, opts Options) *Service {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &Service{store: st, selfHost: opts.SelfHost, now: opts.Now, rand: opts.Rand}
}

// CreateRequest is the input for Create. A zero ExpiresAt means never.
type CreateRequest struct {
	URL       string
	Alias     string
	ExpiresAt time.Time
}

// Create validates req and stores a new link, generating a code unless an
// alias was given.
func (s *Service) Create(ctx context.Context, req CreateRequest) (store.Link, error) {
	target, err := ValidateURL(req.URL, s.selfHost)
	if err != nil {
		return store.Link{}, err
	}
	// Timestamps are stored with second precision; truncate before
	// comparing so what is validated is what is stored.
	now := s.now().UTC().Truncate(time.Second)
	link := store.Link{URL: target, CreatedAt: now}
	if !req.ExpiresAt.IsZero() {
		exp := req.ExpiresAt.UTC().Truncate(time.Second)
		if !exp.After(now) {
			return store.Link{}, &ValidationError{"expires_at", "must be in the future"}
		}
		if exp.Sub(now) > MaxExpiry {
			return store.Link{}, &ValidationError{"expires_at", "must be within 10 years"}
		}
		link.ExpiresAt = exp
	}

	if req.Alias != "" {
		if err := ValidateAlias(req.Alias); err != nil {
			return store.Link{}, err
		}
		link.Code = req.Alias
		created, err := s.store.CreateLink(ctx, link)
		if errors.Is(err, store.ErrConflict) {
			return store.Link{}, ErrAliasTaken
		}
		return created, err
	}

	for range codeAttempts {
		code, err := NewCode(s.rand, CodeLength)
		if err != nil {
			return store.Link{}, err
		}
		if IsReserved(code) {
			continue
		}
		link.Code = code
		created, err := s.store.CreateLink(ctx, link)
		if errors.Is(err, store.ErrConflict) {
			continue
		}
		return created, err
	}
	return store.Link{}, fmt.Errorf("no free code after %d attempts", codeAttempts)
}

// Get returns a link whether or not it has expired.
func (s *Service) Get(ctx context.Context, code string) (store.Link, error) {
	if !ValidCodeSyntax(code) {
		return store.Link{}, ErrNotFound
	}
	l, err := s.store.GetLink(ctx, code)
	if errors.Is(err, store.ErrNotFound) {
		return store.Link{}, ErrNotFound
	}
	return l, err
}

// Resolve returns the link to redirect to, or ErrNotFound / ErrExpired.
func (s *Service) Resolve(ctx context.Context, code string) (store.Link, error) {
	l, err := s.Get(ctx, code)
	if err != nil {
		return store.Link{}, err
	}
	if s.Expired(l) {
		return l, ErrExpired
	}
	return l, nil
}

// Expired reports whether l's expiry has passed.
func (s *Service) Expired(l store.Link) bool {
	return !l.ExpiresAt.IsZero() && !s.now().Before(l.ExpiresAt)
}

// Delete removes a link and its clicks.
func (s *Service) Delete(ctx context.Context, code string) error {
	if !ValidCodeSyntax(code) {
		return ErrNotFound
	}
	err := s.store.DeleteLink(ctx, code)
	if errors.Is(err, store.ErrNotFound) {
		return ErrNotFound
	}
	return err
}

// LinkStats is a link with its click statistics. Daily always has
// StatsDays entries, oldest first, with zero-click days filled in.
type LinkStats struct {
	Link      store.Link
	Total     int64
	Daily     []store.DayCount
	Referrers []store.ReferrerCount
}

// Stats returns a link's statistics. Expired links still have stats.
func (s *Service) Stats(ctx context.Context, code string) (LinkStats, error) {
	l, err := s.Get(ctx, code)
	if err != nil {
		return LinkStats{}, err
	}
	today := s.now().UTC().Truncate(24 * time.Hour)
	first := today.AddDate(0, 0, -(StatsDays - 1))
	st, err := s.store.Stats(ctx, l.ID, first, TopReferrers)
	if err != nil {
		return LinkStats{}, err
	}
	return LinkStats{
		Link:      l,
		Total:     st.Total,
		Daily:     fillDays(st.Daily, first, StatsDays),
		Referrers: st.Referrers,
	}, nil
}

// fillDays expands sparse per-day counts into n consecutive days from first.
func fillDays(sparse []store.DayCount, first time.Time, n int) []store.DayCount {
	byDay := make(map[string]int64, len(sparse))
	for _, d := range sparse {
		byDay[d.Day] = d.Clicks
	}
	out := make([]store.DayCount, n)
	for i := range out {
		day := first.AddDate(0, 0, i).Format(time.DateOnly)
		out[i] = store.DayCount{Day: day, Clicks: byDay[day]}
	}
	return out
}
