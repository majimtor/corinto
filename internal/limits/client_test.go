package limits_test

import (
	"net/http"
	"net/http/httptest"
	"net/netip"
	"slices"
	"strings"
	"testing"

	"github.com/majimtor/corinto/internal/limits"
)

func TestTheAddressOfAClient(t *testing.T) {
	proxies, err := limits.ParseProxies("198.51.100.0/24, 2001:db8:ffff::/48")
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name      string
		remote    string
		forwarded []string
		trusted   []netip.Prefix
		want      string
	}{
		{"the connection, when no proxy is trusted", "203.0.113.7:40000", []string{"192.0.2.1"}, nil, "203.0.113.7"},
		{"the connection, when it is not a proxy", "203.0.113.7:40000", []string{"192.0.2.1"}, proxies, "203.0.113.7"},
		{"what the proxy saw", "198.51.100.5:40000", []string{"192.0.2.1"}, proxies, "192.0.2.1"},
		{"the rightmost address that is not a proxy", "198.51.100.5:40000", []string{"192.0.2.77, 192.0.2.1, 198.51.100.9"}, proxies, "192.0.2.1"},
		{"across several headers", "198.51.100.5:40000", []string{"192.0.2.77", "192.0.2.1"}, proxies, "192.0.2.1"},
		{"the proxy itself, when it forwards nothing", "198.51.100.5:40000", nil, proxies, "198.51.100.5"},
		{"the last proxy, when every hop is one", "198.51.100.5:40000", []string{"198.51.100.9"}, proxies, "198.51.100.9"},
		{"the proxy, when the header is not an address", "198.51.100.5:40000", []string{"unknown"}, proxies, "198.51.100.5"},
		{"what can be read to the right of what cannot", "198.51.100.5:40000", []string{"192.0.2.1, unknown, 198.51.100.9"}, proxies, "198.51.100.9"},
		{"an IPv4 address written as IPv6", "[::ffff:203.0.113.7]:40000", nil, nil, "203.0.113.7"},
		{"an IPv6 client behind an IPv6 proxy", "[2001:db8:ffff::5]:40000", []string{"2001:db8:1:2::9"}, proxies, "2001:db8:1:2::9"},
		{"no address at all", "", nil, nil, "invalid IP"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			request := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/v1/key", nil)
			request.RemoteAddr = c.remote
			for _, value := range c.forwarded {
				request.Header.Add("X-Forwarded-For", value)
			}
			if got := limits.ClientAddress(request, c.trusted).String(); got != c.want {
				t.Errorf("ClientAddress = %s, want %s", got, c.want)
			}
		})
	}
}

func TestTheListOfTrustedProxies(t *testing.T) {
	proxies, err := limits.ParseProxies(" 10.0.0.0/8 ,192.0.2.10,, 2001:db8::/32,::ffff:192.0.2.11 ")
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, proxy := range proxies {
		got = append(got, proxy.String())
	}
	if want := []string{"10.0.0.0/8", "192.0.2.10/32", "2001:db8::/32", "192.0.2.11/32"}; !slices.Equal(got, want) {
		t.Errorf("ParseProxies = %v, want %v", got, want)
	}
	if none, err := limits.ParseProxies(""); err != nil || len(none) != 0 {
		t.Errorf("an empty list = %v, %v; want no proxy and no error", none, err)
	}
	for _, list := range []string{"proxy.example", "10.0.0.0/33", "10.0.0.1, nothing"} {
		if _, err := limits.ParseProxies(list); err == nil {
			t.Errorf("limits.ParseProxies(%q) gave no error", list)
		}
	}
}

// What is kept of an address is a hash that only this process can compute.
func TestAnAddressIsKeptOnlyAsAKeyedHash(t *testing.T) {
	first, err := limits.NewAddresses()
	if err != nil {
		t.Fatal(err)
	}
	second, err := limits.NewAddresses()
	if err != nil {
		t.Fatal(err)
	}
	address := netip.MustParseAddr("203.0.113.7")
	key := first.Key(address)
	if key != first.Key(address) {
		t.Error("the same address gives two keys")
	}
	if key == first.Key(netip.MustParseAddr("203.0.113.8")) {
		t.Error("two addresses give the same key")
	}
	if key == second.Key(address) {
		t.Error("two processes give the same key to an address: the hash has no secret")
	}
	if len(key) != 32 || strings.Contains(key, string(address.AsSlice())) || strings.Contains(key, address.String()) {
		t.Error("the key is not a hash, or carries the address")
	}
	if first.Key(netip.MustParseAddr("2001:db8:1:2::1")) != first.Key(netip.MustParseAddr("2001:db8:1:2:ffff::9")) {
		t.Error("two addresses of one IPv6 network give different keys")
	}
	if first.Key(netip.MustParseAddr("2001:db8:1:2::1")) == first.Key(netip.MustParseAddr("2001:db8:1:3::1")) {
		t.Error("two IPv6 networks give the same key")
	}
}
