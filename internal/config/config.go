// Package config loads linkly's runtime configuration from environment
// variables. Every setting has a default that is safe for local development.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Config is the fully parsed and validated runtime configuration.
type Config struct {
	Addr    string // LINKLY_ADDR: listen address.
	DBPath  string // LINKLY_DB: path to the SQLite database file.
	BaseURL string // LINKLY_BASE_URL: public origin used to build short URLs, without a trailing slash.

	// AdminToken (LINKLY_ADMIN_TOKEN) protects DELETE. Empty disables deletion.
	AdminToken string

	// TrustProxy (LINKLY_TRUST_PROXY) makes the client IP come from
	// X-Forwarded-For. Only enable it behind a proxy that sets that header.
	TrustProxy bool

	RateLimit   float64       // LINKLY_RATE_LIMIT: link creations per second per client. 0 disables.
	RateBurst   int           // LINKLY_RATE_BURST: bucket size.
	RateIdleTTL time.Duration // LINKLY_RATE_IDLE_TTL: idle buckets older than this are evicted.

	ClickBuffer        int           // LINKLY_CLICK_BUFFER: queued clicks before new ones are dropped.
	ClickBatch         int           // LINKLY_CLICK_BATCH: max clicks per INSERT transaction.
	ClickFlushInterval time.Duration // LINKLY_CLICK_FLUSH_INTERVAL: max time a click waits in the queue.

	ShutdownTimeout time.Duration // LINKLY_SHUTDOWN_TIMEOUT: budget for draining requests and clicks.
	LogLevel        slog.Level    // LINKLY_LOG_LEVEL: debug, info, warn or error.
}

// Default returns the configuration used when no variables are set.
func Default() Config {
	return Config{
		Addr:               ":8080",
		DBPath:             "linkly.db",
		BaseURL:            "http://localhost:8080",
		RateLimit:          1,
		RateBurst:          10,
		RateIdleTTL:        10 * time.Minute,
		ClickBuffer:        10_000,
		ClickBatch:         500,
		ClickFlushInterval: time.Second,
		ShutdownTimeout:    10 * time.Second,
		LogLevel:           slog.LevelInfo,
	}
}

// Load reads configuration through getenv (normally os.Getenv). It reports
// every invalid variable at once rather than stopping at the first.
func Load(getenv func(string) string) (Config, error) {
	c := Default()
	p := parser{getenv: getenv}

	p.str("LINKLY_ADDR", &c.Addr)
	p.str("LINKLY_DB", &c.DBPath)
	p.str("LINKLY_BASE_URL", &c.BaseURL)
	p.str("LINKLY_ADMIN_TOKEN", &c.AdminToken)
	p.boolean("LINKLY_TRUST_PROXY", &c.TrustProxy)
	p.float("LINKLY_RATE_LIMIT", &c.RateLimit)
	p.integer("LINKLY_RATE_BURST", &c.RateBurst)
	p.duration("LINKLY_RATE_IDLE_TTL", &c.RateIdleTTL)
	p.integer("LINKLY_CLICK_BUFFER", &c.ClickBuffer)
	p.integer("LINKLY_CLICK_BATCH", &c.ClickBatch)
	p.duration("LINKLY_CLICK_FLUSH_INTERVAL", &c.ClickFlushInterval)
	p.duration("LINKLY_SHUTDOWN_TIMEOUT", &c.ShutdownTimeout)
	if v := getenv("LINKLY_LOG_LEVEL"); v != "" {
		if err := c.LogLevel.UnmarshalText([]byte(v)); err != nil {
			p.errs = append(p.errs, fmt.Errorf("LINKLY_LOG_LEVEL: %w", err))
		}
	}

	c.BaseURL = strings.TrimRight(c.BaseURL, "/")
	p.errs = append(p.errs, c.validate()...)
	if err := errors.Join(p.errs...); err != nil {
		return Config{}, err
	}
	return c, nil
}

func (c Config) validate() []error {
	var errs []error
	if c.Addr == "" {
		errs = append(errs, errors.New("LINKLY_ADDR must not be empty"))
	}
	if c.DBPath == "" {
		errs = append(errs, errors.New("LINKLY_DB must not be empty"))
	}
	u, err := url.Parse(c.BaseURL)
	switch {
	case err != nil:
		errs = append(errs, fmt.Errorf("LINKLY_BASE_URL: %w", err))
	case u.Scheme != "http" && u.Scheme != "https":
		errs = append(errs, errors.New("LINKLY_BASE_URL must use http or https"))
	case u.Host == "":
		errs = append(errs, errors.New("LINKLY_BASE_URL must include a host"))
	case u.Path != "" || u.RawQuery != "" || u.Fragment != "":
		errs = append(errs, errors.New("LINKLY_BASE_URL must be an origin with no path, query or fragment"))
	case u.User != nil:
		errs = append(errs, errors.New("LINKLY_BASE_URL must not contain credentials"))
	case strings.HasSuffix(u.Hostname(), "."):
		// Short URLs are built from BaseURL verbatim, and the self-link check
		// compares normalised hosts; one spelling per host keeps both simple.
		errs = append(errs, errors.New("LINKLY_BASE_URL host must not end with a trailing dot"))
	case !isASCII(u.Hostname()):
		errs = append(errs, errors.New("LINKLY_BASE_URL host must be ASCII; use the punycode (xn--) form"))
	}
	switch {
	case math.IsNaN(c.RateLimit) || math.IsInf(c.RateLimit, 0):
		// NaN compares false with everything, so it would pass ">= 0" and
		// silently disable limiting.
		errs = append(errs, errors.New("LINKLY_RATE_LIMIT must be a finite number"))
	case c.RateLimit < 0:
		errs = append(errs, errors.New("LINKLY_RATE_LIMIT must be >= 0"))
	}
	if c.RateLimit > 0 && c.RateBurst < 1 {
		errs = append(errs, errors.New("LINKLY_RATE_BURST must be >= 1"))
	}
	if c.RateIdleTTL <= 0 {
		errs = append(errs, errors.New("LINKLY_RATE_IDLE_TTL must be > 0"))
	}
	if c.ClickBuffer < 1 {
		errs = append(errs, errors.New("LINKLY_CLICK_BUFFER must be >= 1"))
	}
	if c.ClickBatch < 1 {
		errs = append(errs, errors.New("LINKLY_CLICK_BATCH must be >= 1"))
	}
	if c.ClickFlushInterval <= 0 {
		errs = append(errs, errors.New("LINKLY_CLICK_FLUSH_INTERVAL must be > 0"))
	}
	if c.ShutdownTimeout <= 0 {
		errs = append(errs, errors.New("LINKLY_SHUTDOWN_TIMEOUT must be > 0"))
	}
	return errs
}

// BaseHost returns the host (without port) of BaseURL, lower-cased.
func (c Config) BaseHost() string {
	u, err := url.Parse(c.BaseURL)
	if err != nil {
		return ""
	}
	return strings.ToLower(u.Hostname())
}

func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			return false
		}
	}
	return true
}

type parser struct {
	getenv func(string) string
	errs   []error
}

func (p *parser) str(key string, dst *string) {
	if v := p.getenv(key); v != "" {
		*dst = v
	}
}

func (p *parser) boolean(key string, dst *bool) {
	p.parse(key, func(v string) (err error) { *dst, err = strconv.ParseBool(v); return })
}

func (p *parser) float(key string, dst *float64) {
	p.parse(key, func(v string) (err error) { *dst, err = strconv.ParseFloat(v, 64); return })
}

func (p *parser) integer(key string, dst *int) {
	p.parse(key, func(v string) (err error) { *dst, err = strconv.Atoi(v); return })
}

func (p *parser) duration(key string, dst *time.Duration) {
	p.parse(key, func(v string) (err error) { *dst, err = time.ParseDuration(v); return })
}

func (p *parser) parse(key string, fn func(string) error) {
	v := p.getenv(key)
	if v == "" {
		return
	}
	if err := fn(v); err != nil {
		p.errs = append(p.errs, fmt.Errorf("%s=%q: %w", key, v, err))
	}
}
