package service

import (
	"bytes"
	"context"
	"errors"
	"math"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SAMARTHJAISWAL/linkly/internal/store"
)

func TestNewCode(t *testing.T) {
	tests := []struct {
		name string
		src  []byte
		n    int
		want string
	}{
		{"maps bytes mod 62", []byte{0, 1, 61, 62, 63}, 5, "01z01"},
		{"rejects bytes >= 248", []byte{248, 255, 250, 10, 247, 0, 0, 0, 0, 0}, 2, "Az"},
		{"keeps reading until n accepted", bytes.Repeat([]byte{255}, 9), 3, "000"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := NewCode(bytes.NewReader(append(tt.src, make([]byte, 64)...)), tt.n)
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Fatalf("NewCode = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestNewCodeShortRead(t *testing.T) {
	if _, err := NewCode(bytes.NewReader(nil), 7); err == nil {
		t.Fatal("expected an error from an empty reader")
	}
}

func TestNewCodeCryptoRand(t *testing.T) {
	seen := map[string]bool{}
	counts := map[rune]int{}
	const samples = 20_000
	for range samples {
		c, err := NewCode(nil, CodeLength)
		if err != nil {
			t.Fatal(err)
		}
		if len(c) != CodeLength || !ValidCodeSyntax(c) {
			t.Fatalf("bad code %q", c)
		}
		if seen[c] {
			t.Fatalf("duplicate code %q in %d samples", c, samples)
		}
		seen[c] = true
		for _, r := range c {
			counts[r]++
		}
	}
	if len(counts) != 62 {
		t.Fatalf("saw %d distinct characters, want 62", len(counts))
	}
	// Each character is expected samples*7/62 ≈ 2258 times; a biased
	// generator (modulo without rejection) would skew the first 8 symbols
	// by ~3%. Allow ±10% so the test is never flaky.
	expected := float64(samples*CodeLength) / 62
	for r, n := range counts {
		if math.Abs(float64(n)-expected) > expected*0.10 {
			t.Errorf("character %q appeared %d times, expected ~%.0f", r, n, expected)
		}
	}
}

func TestValidateAlias(t *testing.T) {
	tests := []struct {
		alias   string
		wantErr string // substring; empty means valid
	}{
		{"abc", ""},
		{"My-Launch_2026", ""},
		{strings.Repeat("a", 32), ""},
		{"ab", "3 to 32"},
		{strings.Repeat("a", 33), "3 to 32"},
		{"has space", "letters, digits"},
		{"emoji😀", "letters, digits"},
		{"dot.dot", "letters, digits"},
		{"slash/x", "letters, digits"},
		{"-leading", "start and end"},
		{"trailing_", "start and end"},
		{"api", "reserved"},
		{"API", "reserved"},
		{"healthz", "reserved"},
		{"Metrics", "reserved"},
		{"static", "reserved"},
	}
	for _, tt := range tests {
		t.Run(tt.alias, func(t *testing.T) {
			err := ValidateAlias(tt.alias)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			var ve *ValidationError
			if !errors.As(err, &ve) || ve.Field != "alias" || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("err = %v, want alias error containing %q", err, tt.wantErr)
			}
		})
	}
}

func TestValidateURL(t *testing.T) {
	tests := []struct {
		in      string
		want    string // returned value when valid
		wantErr string
	}{
		{"https://example.com", "https://example.com", ""},
		{"  https://example.com/a?b=c#d  ", "https://example.com/a?b=c#d", ""},
		{"HTTP://Example.COM/Path", "HTTP://Example.COM/Path", ""},
		{"http://93.184.215.14:8080/x", "http://93.184.215.14:8080/x", ""},
		{"https://[2001:db8::1]/", "https://[2001:db8::1]/", ""},
		{"https://xn--r8jz45g.jp/", "https://xn--r8jz45g.jp/", ""},
		{"https://my_host.example.com/", "https://my_host.example.com/", ""},
		{"https://example.com./", "https://example.com./", ""},
		// Non-ASCII hosts are refused (punycode is the accepted spelling),
		// which also stops junk such as a no-break space reaching Location.
		{"https://例え.jp/", "", "ASCII"},
		{"https://exa\u00a0mple.com/", "", "ASCII"},
		{"https://exa%C2%A0mple.com/", "", "ASCII"},
		{"https://ex\u0430mple.com/", "", "ASCII"}, // Cyrillic a
		// IP literals that point back at this machine or a private network.
		{"http://127.0.0.1:8080/x", "", "local or private"},
		{"http://127.9.9.9/x", "", "local or private"},
		{"http://[::1]/x", "", "local or private"},
		{"http://[::ffff:127.0.0.1]/x", "", "local or private"},
		{"http://[::]/x", "", "local or private"},
		{"http://0.0.0.0/x", "", "local or private"},
		{"http://0.1.2.3/x", "", "local or private"},
		{"http://10.1.2.3/x", "", "local or private"},
		{"http://172.16.0.1/x", "", "local or private"},
		{"http://192.168.1.1/x", "", "local or private"},
		{"http://100.64.0.1/x", "", "local or private"},
		{"http://169.254.169.254/latest/meta-data", "", "local or private"},
		{"http://[fe80::1]/x", "", "local or private"},
		{"http://[fe80::1%25en0]/x", "", "local or private"},
		{"http://[fc00::1]/x", "", "local or private"},
		{"http://224.0.0.1/x", "", "local or private"},
		{"http://255.255.255.255/x", "", "local or private"},
		// Numeric hosts that browsers parse as IPv4 addresses.
		{"http://2130706433/x", "", "non-standard"},
		{"http://0x7f000001/x", "", "non-standard"},
		{"http://0x7f.0.0.1/x", "", "non-standard"},
		{"http://0177.0.0.1/x", "", "non-standard"},
		{"http://127.1/x", "", "non-standard"},
		{"http://1.2.3.4.5/x", "", "non-standard"},
		{"http://example.0x10/x", "", "non-standard"},
		{"http://localhost:8080/x", "", "local or private"},
		{"http://LOCALHOST./x", "", "local or private"},
		{"http://app.localhost/x", "", "local or private"},
		{"http://a..b.com/x", "", "invalid host"},
		{"http://-/x", "", "invalid host"},
		{"http://" + strings.Repeat("a", 64) + ".com/x", "", "invalid host"},
		{"", "", "required"},
		{"example.com", "", "http or https"},
		{"ftp://example.com/f", "", "http or https"},
		{"javascript:alert(1)", "", "http or https"},
		{"data:text/html,hi", "", "http or https"},
		{"https://", "", "host"},
		{"https:///path", "", "host"},
		{"https://user:pw@example.com", "", "credentials"},
		{"https://bank.com@evil.example", "", "credentials"},
		{"https://exa mple.com", "", "whitespace"},
		{"https://example.com/\nx", "", "whitespace"},
		{"http://%zz", "", "not a valid URL"},
		{"https://sho.rt/abc", "", "this shortener"},
		{"https://SHO.RT./abc", "", "this shortener"},
		{"https://sho.rt:8443/abc", "", "this shortener"},
		{"https://sub.sho.rt/abc", "https://sub.sho.rt/abc", ""},
		{"https://other.name/abc", "", "this shortener"}, // the request's Host
		{"https://example.com/" + strings.Repeat("a", MaxURLLength), "", "at most"},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got, err := ValidateURL(tt.in, "sho.rt", "other.name")
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if got != tt.want {
					t.Fatalf("ValidateURL = %q, want %q", got, tt.want)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("err = %v, want error containing %q", err, tt.wantErr)
			}
		})
	}
}

// memStore is an in-memory Store for service tests.
type memStore struct {
	mu        sync.Mutex
	links     map[string]store.Link
	nextID    int64
	creates   int
	lastSince time.Time
}

func newMemStore() *memStore { return &memStore{links: map[string]store.Link{}} }

func (m *memStore) CreateLink(_ context.Context, l store.Link) (store.Link, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.creates++
	if _, ok := m.links[l.Code]; ok {
		return store.Link{}, store.ErrConflict
	}
	m.nextID++
	l.ID = m.nextID
	m.links[l.Code] = l
	return l, nil
}

func (m *memStore) GetLink(_ context.Context, code string) (store.Link, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	l, ok := m.links[code]
	if !ok {
		return store.Link{}, store.ErrNotFound
	}
	return l, nil
}

func (m *memStore) DeleteLink(_ context.Context, code string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.links[code]; !ok {
		return store.ErrNotFound
	}
	delete(m.links, code)
	return nil
}

func (m *memStore) Stats(_ context.Context, _ int64, since time.Time, _ int) (store.Stats, error) {
	m.mu.Lock()
	m.lastSince = since
	m.mu.Unlock()
	return store.Stats{
		Total:     5,
		Daily:     []store.DayCount{{Day: "2026-03-01", Clicks: 2}, {Day: "2026-03-30", Clicks: 3}},
		Referrers: []store.ReferrerCount{{Host: "t.co", Clicks: 5}},
	}, nil
}

var fixedNow = time.Date(2026, 3, 30, 12, 0, 0, 0, time.UTC)

func newTestService(st Store, rnd []byte) *Service {
	opts := Options{SelfHost: "sho.rt", Now: func() time.Time { return fixedNow }}
	if rnd != nil {
		opts.Rand = bytes.NewReader(rnd)
	}
	return New(st, opts)
}

func TestCreateGeneratesCode(t *testing.T) {
	st := newMemStore()
	svc := newTestService(st, nil)
	l, err := svc.Create(context.Background(), CreateRequest{URL: "https://example.com"})
	if err != nil {
		t.Fatal(err)
	}
	if len(l.Code) != CodeLength || l.URL != "https://example.com" || !l.CreatedAt.Equal(fixedNow) || !l.ExpiresAt.IsZero() {
		t.Fatalf("unexpected link %+v", l)
	}
}

func TestCreateRetriesOnCollision(t *testing.T) {
	st := newMemStore()
	st.links["0000000"] = store.Link{Code: "0000000"}
	// The first draw yields "0000000" (taken); the second yields "1111111".
	rnd := append(bytes.Repeat([]byte{0}, 9), bytes.Repeat([]byte{1}, 9)...)
	svc := newTestService(st, rnd)

	l, err := svc.Create(context.Background(), CreateRequest{URL: "https://example.com"})
	if err != nil {
		t.Fatal(err)
	}
	if l.Code != "1111111" || st.creates != 2 {
		t.Fatalf("code=%q after %d creates, want 1111111 after 2", l.Code, st.creates)
	}
}

func TestCreateGivesUpAfterRepeatedCollisions(t *testing.T) {
	st := newMemStore()
	st.links["0000000"] = store.Link{Code: "0000000"}
	svc := newTestService(st, make([]byte, 1000)) // always "0000000"
	_, err := svc.Create(context.Background(), CreateRequest{URL: "https://example.com"})
	if err == nil || !strings.Contains(err.Error(), "no free code") {
		t.Fatalf("err = %v, want exhaustion error", err)
	}
	if st.creates != codeAttempts {
		t.Fatalf("made %d attempts, want %d", st.creates, codeAttempts)
	}
}

func TestCreateWithAlias(t *testing.T) {
	st := newMemStore()
	svc := newTestService(st, nil)
	ctx := context.Background()

	l, err := svc.Create(ctx, CreateRequest{URL: "https://example.com", Alias: "launch"})
	if err != nil || l.Code != "launch" {
		t.Fatalf("Create = %+v, %v", l, err)
	}
	if _, err := svc.Create(ctx, CreateRequest{URL: "https://example.org", Alias: "launch"}); !errors.Is(err, ErrAliasTaken) {
		t.Fatalf("err = %v, want ErrAliasTaken", err)
	}
	var ve *ValidationError
	if _, err := svc.Create(ctx, CreateRequest{URL: "https://example.org", Alias: "api"}); !errors.As(err, &ve) {
		t.Fatalf("err = %v, want ValidationError", err)
	}
}

func TestCreateExpiryValidation(t *testing.T) {
	tests := []struct {
		name    string
		expires time.Time
		wantErr bool
	}{
		{"never", time.Time{}, false},
		{"tomorrow", fixedNow.Add(24 * time.Hour), false},
		{"sub-second is truncated", fixedNow.Add(1500 * time.Millisecond), false},
		{"now", fixedNow, true},
		{"truncates to now", fixedNow.Add(500 * time.Millisecond), true},
		{"past", fixedNow.Add(-time.Hour), true},
		{"too far", fixedNow.Add(MaxExpiry + time.Hour), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc := newTestService(newMemStore(), nil)
			l, err := svc.Create(context.Background(), CreateRequest{URL: "https://example.com", ExpiresAt: tt.expires})
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if err == nil && !tt.expires.IsZero() && l.ExpiresAt.Nanosecond() != 0 {
				t.Fatalf("ExpiresAt %v not truncated to seconds", l.ExpiresAt)
			}
		})
	}
}

func TestCreateRejectsBadURL(t *testing.T) {
	svc := newTestService(newMemStore(), nil)
	var ve *ValidationError
	_, err := svc.Create(context.Background(), CreateRequest{URL: "https://sho.rt/loop"})
	if !errors.As(err, &ve) || ve.Field != "url" {
		t.Fatalf("err = %v, want url ValidationError", err)
	}
}

func TestResolve(t *testing.T) {
	st := newMemStore()
	st.links["live"] = store.Link{Code: "live", URL: "https://a"}
	st.links["future"] = store.Link{Code: "future", URL: "https://b", ExpiresAt: fixedNow.Add(time.Second)}
	st.links["exact"] = store.Link{Code: "exact", URL: "https://c", ExpiresAt: fixedNow}
	st.links["old"] = store.Link{Code: "old", URL: "https://d", ExpiresAt: fixedNow.Add(-time.Second)}
	svc := newTestService(st, nil)

	tests := []struct {
		code string
		want error
	}{
		{"live", nil},
		{"future", nil},
		{"exact", ErrExpired},
		{"old", ErrExpired},
		{"missing", ErrNotFound},
		{"bad.syntax", ErrNotFound},
		{"", ErrNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.code, func(t *testing.T) {
			_, err := svc.Resolve(context.Background(), tt.code)
			if !errors.Is(err, tt.want) {
				t.Fatalf("Resolve(%q) err = %v, want %v", tt.code, err, tt.want)
			}
		})
	}
}

func TestDelete(t *testing.T) {
	st := newMemStore()
	st.links["x1x"] = store.Link{Code: "x1x"}
	svc := newTestService(st, nil)
	ctx := context.Background()
	if err := svc.Delete(ctx, "x1x"); err != nil {
		t.Fatal(err)
	}
	if err := svc.Delete(ctx, "x1x"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second delete err = %v", err)
	}
	if err := svc.Delete(ctx, "../etc"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("bad syntax err = %v", err)
	}
}

func TestStatsFillsThirtyDays(t *testing.T) {
	st := newMemStore()
	st.links["s1s"] = store.Link{ID: 1, Code: "s1s", ExpiresAt: fixedNow.Add(-time.Hour)} // expired links keep stats
	svc := newTestService(st, nil)

	ls, err := svc.Stats(context.Background(), "s1s")
	if err != nil {
		t.Fatal(err)
	}
	if len(ls.Daily) != StatsDays {
		t.Fatalf("len(Daily) = %d, want %d", len(ls.Daily), StatsDays)
	}
	if first, last := ls.Daily[0], ls.Daily[StatsDays-1]; first != (store.DayCount{Day: "2026-03-01", Clicks: 2}) ||
		last != (store.DayCount{Day: "2026-03-30", Clicks: 3}) {
		t.Fatalf("first=%v last=%v", first, last)
	}
	var sum int64
	for _, d := range ls.Daily {
		sum += d.Clicks
	}
	if sum != 5 || ls.Total != 5 || len(ls.Referrers) != 1 {
		t.Fatalf("sum=%d total=%d refs=%v", sum, ls.Total, ls.Referrers)
	}
	if want := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC); !st.lastSince.Equal(want) {
		t.Fatalf("queried since %v, want %v", st.lastSince, want)
	}
}

func TestValidCodeSyntax(t *testing.T) {
	tests := map[string]bool{
		"abc": true, "a": true, "A-b_9": true, strings.Repeat("z", 32): true,
		"": false, strings.Repeat("z", 33): false, "a.b": false, "a/b": false, "%41": false,
	}
	for in, want := range tests {
		if got := ValidCodeSyntax(in); got != want {
			t.Errorf("ValidCodeSyntax(%q) = %v, want %v", in, got, want)
		}
	}
}

// TestSelfHostIsNormalised: the configured host and the request's Host are
// normalised exactly like the target's, so case, a trailing dot or a port
// cannot be used to point a link back at the shortener.
func TestSelfHostIsNormalised(t *testing.T) {
	ctx := context.Background()
	svc := New(newMemStore(), Options{SelfHost: "SHO.RT.", Now: func() time.Time { return fixedNow }})
	for _, target := range []string{"https://sho.rt/x", "https://SHO.rt./x", "http://sho.rt:80/x"} {
		_, err := svc.Create(ctx, CreateRequest{URL: target})
		var ve *ValidationError
		if !errors.As(err, &ve) || !strings.Contains(ve.Msg, "this shortener") {
			t.Errorf("%s: err = %v, want a self-link error", target, err)
		}
	}
	for _, host := range []string{"Alias.Example:8080", "alias.example.", "[2001:db8::7]:80"} {
		target := "https://alias.example/x"
		if strings.HasPrefix(host, "[") {
			target = "https://[2001:db8::7]/x"
		}
		_, err := svc.Create(ctx, CreateRequest{URL: target, RequestHost: host})
		var ve *ValidationError
		if !errors.As(err, &ve) || !strings.Contains(ve.Msg, "this shortener") {
			t.Errorf("request host %s: err = %v, want a self-link error", host, err)
		}
	}
	if _, err := svc.Create(ctx, CreateRequest{URL: "https://example.com/x", RequestHost: "alias.example"}); err != nil {
		t.Fatalf("unrelated target refused: %v", err)
	}
}

func TestPointsAtSelf(t *testing.T) {
	svc := New(newMemStore(), Options{SelfHost: "sho.rt"})
	tests := []struct {
		target, reqHost string
		want            bool
	}{
		{"https://sho.rt/abc", "", true},
		{"https://SHO.RT./abc", "127.0.0.1:8080", true},
		{"http://127.0.0.1:8080/loopy", "127.0.0.1:8080", true},
		{"http://127.0.0.1:9999/loopy", "127.0.0.1:8080", true}, // same host, any port
		{"http://[::1]:8080/x", "[::1]:8080", true},
		{"http://Other.Name./x", "other.name", true},
		{"https://example.com/x", "127.0.0.1:8080", false},
		{"https://sub.sho.rt/x", "", false},
		{"::not a url", "", false},
	}
	for _, tt := range tests {
		if got := svc.PointsAtSelf(tt.target, tt.reqHost); got != tt.want {
			t.Errorf("PointsAtSelf(%q, %q) = %v, want %v", tt.target, tt.reqHost, got, tt.want)
		}
	}
}
