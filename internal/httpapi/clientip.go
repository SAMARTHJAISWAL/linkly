package httpapi

import (
	"net"
	"net/http"
	"net/netip"
	"strings"
)

// clientKey returns the rate-limiting key for a request.
//
// Without trustProxy the key is the TCP peer address, which a client cannot
// forge. With trustProxy it is the right-most X-Forwarded-For entry: the
// address our own proxy saw and appended. Entries further left were
// supplied by the client and are trivially spoofed, so they are ignored.
// This assumes exactly one trusted proxy hop in front of linkly.
//
// IPv6 clients are keyed by their /64, since one host routinely controls
// a whole /64 and could otherwise rotate addresses to dodge the limit.
func clientKey(r *http.Request, trustProxy bool) string {
	if trustProxy {
		if vals := r.Header.Values("X-Forwarded-For"); len(vals) > 0 {
			last := vals[len(vals)-1]
			if i := strings.LastIndexByte(last, ','); i >= 0 {
				last = last[i+1:]
			}
			if addr, err := netip.ParseAddr(strings.TrimSpace(last)); err == nil {
				return keyFor(addr)
			}
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	if addr, err := netip.ParseAddr(host); err == nil {
		return keyFor(addr)
	}
	return host
}

func keyFor(addr netip.Addr) string {
	addr = addr.Unmap() // ::ffff:1.2.3.4 is just 1.2.3.4
	if addr.Is6() {
		if p, err := addr.WithZone("").Prefix(64); err == nil {
			return p.String()
		}
	}
	return addr.String()
}
