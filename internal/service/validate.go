package service

import (
	"crypto/rand"
	"fmt"
	"io"
	"net"
	"net/netip"
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
//
// Only absolute http(s) URLs with a host are accepted. Credentials in the
// URL are refused because "https://bank.com@evil.example" is a classic
// phishing disguise. The host must be plain ASCII (see validateHost) and
// must not be a loopback, private, link-local or unspecified address, or a
// numeric spelling of one, because such a link points at the shortener's
// own machine or network rather than somewhere a visitor can reach. Links
// to any of selfHosts (compared after the same normalisation) are refused
// so a short link can never redirect to another short link, or itself, in
// a loop.
func ValidateURL(raw string, selfHosts ...string) (string, error) {
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
	host := NormalizeHost(u.Hostname())
	if host == "" {
		return "", &ValidationError{"url", "must include a host"}
	}
	if msg := validateHost(host); msg != "" {
		return "", &ValidationError{"url", msg}
	}
	for _, self := range selfHosts {
		if self != "" && host == NormalizeHost(self) {
			return "", &ValidationError{"url", "must not point at this shortener"}
		}
	}
	// Return the input itself (trimmed) rather than u.String(), which could
	// subtly change what the user asked for. The host is plain ASCII by now,
	// so nothing unexpected can reach a Location header.
	return raw, nil
}

// NormalizeHost returns the form of a host name used for comparisons:
// lower-cased, without a port, and without the trailing dot of a fully
// qualified name ("Sho.RT.:443" and "sho.rt" are the same host). IPv6
// literals lose their brackets.
func NormalizeHost(host string) string {
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	} else if strings.HasPrefix(host, "[") && strings.HasSuffix(host, "]") {
		host = host[1 : len(host)-1]
	}
	return strings.TrimSuffix(strings.ToLower(host), ".")
}

// localPrefixes are address ranges, beyond what netip classifies as
// loopback, private, link-local, multicast or unspecified, that reach the
// local machine or carrier-internal networks.
var localPrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),     // "this network"; Linux routes it to localhost
	netip.MustParsePrefix("100.64.0.0/10"), // carrier-grade NAT shared space
	netip.MustParsePrefix("::/96"),         // IPv4-compatible IPv6 (deprecated)
}

// validateHost checks a normalised target host and returns a message
// describing the problem, or "".
//
// Non-ASCII hosts are refused outright rather than converted with IDNA:
// it needs no dependency, it rules out look-alike (homograph) hosts and
// invisible characters such as a no-break space, and an internationalised
// domain can still be shortened by writing it in its punycode (xn--) form,
// which is what browsers send anyway.
//
// Numeric hosts are checked the way browsers parse them (WHATWG URL): a
// host whose last label is a decimal or 0x-hex number is an IPv4 address,
// so "2130706433", "0x7f000001", "0177.0.0.1" and "127.1" all mean
// 127.0.0.1. Only the canonical dotted-quad spelling is accepted, and then
// only for a public unicast address.
func validateHost(host string) string {
	for i := 0; i < len(host); i++ {
		if host[i] >= 0x80 {
			return "host must be ASCII; write internationalised domain names in punycode (xn--...)"
		}
	}
	const localMsg = "must not point at a local or private address"
	if ip, err := netip.ParseAddr(host); err == nil {
		ip = ip.Unmap()
		if ip.Zone() != "" || !ip.IsGlobalUnicast() || ip.IsPrivate() {
			return localMsg
		}
		for _, p := range localPrefixes {
			if p.Contains(ip) {
				return localMsg
			}
		}
		return ""
	}
	if len(host) > 253 {
		return "has an invalid host"
	}
	labels := strings.Split(host, ".")
	for _, l := range labels {
		if l == "" || len(l) > 63 || l[0] == '-' || l[len(l)-1] == '-' {
			return "has an invalid host"
		}
		for i := 0; i < len(l); i++ {
			if c := l[i]; !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
				return "has an invalid host"
			}
		}
	}
	if isNumericLabel(labels[len(labels)-1]) {
		return "host must not be an IPv4 address in non-standard form"
	}
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return localMsg // RFC 6761: always the loopback interface
	}
	return ""
}

// isNumericLabel reports whether a label is a number in the sense of the
// WHATWG IPv4 parser: decimal digits, or 0x followed by hex digits.
func isNumericLabel(l string) bool {
	if rest, ok := strings.CutPrefix(l, "0x"); ok {
		return strings.Trim(rest, "0123456789abcdef") == ""
	}
	return strings.Trim(l, "0123456789") == ""
}
