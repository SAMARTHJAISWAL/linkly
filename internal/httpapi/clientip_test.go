package httpapi

import (
	"net/http/httptest"
	"testing"
)

func TestClientKey(t *testing.T) {
	tests := []struct {
		name   string
		remote string
		xff    []string
		trust  bool
		want   string
	}{
		{"ipv4 peer", "192.0.2.7:5555", nil, false, "192.0.2.7"},
		{"xff ignored when untrusted", "192.0.2.7:5555", []string{"203.0.113.9"}, false, "192.0.2.7"},
		{"trusted single", "10.0.0.1:1", []string{"203.0.113.9"}, true, "203.0.113.9"},
		{"trusted takes right-most", "10.0.0.1:1", []string{"6.6.6.6, 203.0.113.9"}, true, "203.0.113.9"},
		{"trusted across repeated headers", "10.0.0.1:1", []string{"6.6.6.6", "198.51.100.4"}, true, "198.51.100.4"},
		{"trusted but garbage falls back to peer", "10.0.0.1:1", []string{"not-an-ip"}, true, "10.0.0.1"},
		{"trusted but absent", "10.0.0.1:1", nil, true, "10.0.0.1"},
		{"ipv6 keyed by /64", "[2001:db8:1:2:aaaa::1]:443", nil, false, "2001:db8:1:2::/64"},
		{"same /64 shares a key", "[2001:db8:1:2:bbbb::9]:443", nil, false, "2001:db8:1:2::/64"},
		{"ipv4-mapped ipv6", "[::ffff:192.0.2.1]:80", nil, false, "192.0.2.1"},
		{"zone stripped", "[fe80::1%en0]:80", nil, false, "fe80::/64"},
		{"no port", "192.0.2.7", nil, false, "192.0.2.7"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", "/", nil)
			r.RemoteAddr = tt.remote
			for _, v := range tt.xff {
				r.Header.Add("X-Forwarded-For", v)
			}
			if got := clientKey(r, tt.trust); got != tt.want {
				t.Fatalf("clientKey = %q, want %q", got, tt.want)
			}
		})
	}
}
