package service

import (
	"crypto/rand"
	"fmt"
	"io"
	"net/url"
	"strings"
)

const (
	// CodeLength is the length of generated codes: 62^7 ≈ 3.5e12 values.
	CodeLength = 7
	// MaxURLLength bounds stored targets. 2048 is the practical limit most
	// browsers, proxies and CDNs agree on.
	MaxURLLength = 2048
	// MinAliasLength and MaxAliasLength bound custom aliases.
	MinAliasLength = 3
	MaxAliasLength = 32
)

const base62 = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

// reserved holds paths the server owns or may own later. Aliases are
// compared case-insensitively so "API" cannot shadow "/api" in a
// case-insensitive proxy or confuse readers.
var reserved = map[string]bool{
	"api": true, "healthz": true, "metrics": true, "static": true,
	"admin": true, "login": true, "logout": true, "openapi": true,
	"docs": true, "stats": true, "www": true, "linkly": true,
}

// IsReserved reports whether s collides with a reserved path.
func IsReserved(s string) bool { return reserved[strings.ToLower(s)] }

// NewCode returns a random base62 code of length n read from src
// (normally crypto/rand.Reader). It uses rejection sampling so every
// character is equally likely: bytes >= 248 (the largest multiple of 62
// below 256) are discarded rather than folded in with a biased modulo.
func NewCode(src io.Reader, n int) (string, error) {
	if src == nil {
		src = rand.Reader
	}
	out := make([]byte, 0, n)
	buf := make([]byte, n+n/4+1)
	for len(out) < n {
		if _, err := io.ReadFull(src, buf); err != nil {
			return "", fmt.Errorf("reading randomness: %w", err)
		}
		for _, b := range buf {
			if b >= 248 {
				continue
			}
			out = append(out, base62[b%62])
			if len(out) == n {
				break
			}
		}
	}
	return string(out), nil
}

// ValidCodeSyntax reports whether s could be a code or alias at all. The
// redirect handler uses it to answer obvious garbage without a query.
func ValidCodeSyntax(s string) bool {
	if len(s) == 0 || len(s) > MaxAliasLength {
		return false
	}
	for i := 0; i < len(s); i++ {
		if !isAliasByte(s[i]) {
			return false
		}
	}
	return true
}

func isAliasByte(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c == '-' || c == '_'
}

// ValidateAlias checks a custom alias: 3–32 characters from [A-Za-z0-9_-],
// starting and ending with a letter or digit, and not a reserved word.
func ValidateAlias(alias string) error {
	if n := len(alias); n < MinAliasLength || n > MaxAliasLength {
		return &ValidationError{"alias", fmt.Sprintf("must be %d to %d characters", MinAliasLength, MaxAliasLength)}
	}
	for i := 0; i < len(alias); i++ {
		if !isAliasByte(alias[i]) {
			return &ValidationError{"alias", "may only contain letters, digits, '-' and '_'"}
		}
	}
	if first, last := alias[0], alias[len(alias)-1]; first == '-' || first == '_' || last == '-' || last == '_' {
		return &ValidationError{"alias", "must start and end with a letter or digit"}
	}
	if IsReserved(alias) {
		return &ValidationError{"alias", fmt.Sprintf("%q is reserved", alias)}
	}
	return nil
}

// ValidateURL checks a link target and returns it with surrounding
// whitespace removed.
// Only absolute http(s) URLs with a host are accepted; credentials in the
// URL are refused because "https://bank.com@evil.example" is a classic
// phishing disguise; and links back to selfHost are refused so a short
// link can never redirect to another short link (or itself) in a loop.
func ValidateURL(raw, selfHost string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", &ValidationError{"url", "is required"}
	}
	if len(raw) > MaxURLLength {
		return "", &ValidationError{"url", fmt.Sprintf("must be at most %d bytes", MaxURLLength)}
	}
	if strings.ContainsAny(raw, " \t\r\n") {
		return "", &ValidationError{"url", "must not contain whitespace"}
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", &ValidationError{"url", "is not a valid URL"}
	}
	if scheme := strings.ToLower(u.Scheme); scheme != "http" && scheme != "https" {
		return "", &ValidationError{"url", "must use http or https"}
	}
	if u.User != nil {
		return "", &ValidationError{"url", "must not contain credentials"}
	}
	host := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	if host == "" {
		return "", &ValidationError{"url", "must include a host"}
	}
	if selfHost != "" && host == strings.ToLower(selfHost) {
		return "", &ValidationError{"url", "must not point at this shortener"}
	}
	// Return the input itself (trimmed) rather than u.String(): re-encoding
	// would percent-escape internationalised hosts and could subtly change
	// what the user asked for.
	return raw, nil
}
