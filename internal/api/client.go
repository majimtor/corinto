package api

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/netip"
	"strings"
)

// ParseProxies reads the proxies whose forwarding header is believed: a
// list, separated by commas, of addresses and of networks written as an
// address, a slash and a length. An empty list trusts none.
func ParseProxies(list string) ([]netip.Prefix, error) {
	var proxies []netip.Prefix
	for entry := range strings.SplitSeq(list, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		if network, err := netip.ParsePrefix(entry); err == nil {
			proxies = append(proxies, network.Masked())
			continue
		}
		address, err := netip.ParseAddr(entry)
		if err != nil {
			return nil, fmt.Errorf("%q is neither an address nor a network", entry)
		}
		address = address.Unmap()
		proxies = append(proxies, netip.PrefixFrom(address, address.BitLen()))
	}
	return proxies, nil
}

// clientAddress returns the address a request comes from. It is the one of
// the connection. A forwarding header is believed only when the connection
// comes from a trusted proxy, and then only as far as the proxies reach:
// the header is read from the right, where each proxy appends what it saw,
// up to the first address that is not a proxy. Anything further left was
// written by the client and could be anything.
func clientAddress(r *http.Request, trusted []netip.Prefix) netip.Addr {
	remote, err := netip.ParseAddrPort(r.RemoteAddr)
	if err != nil {
		// No address to tell clients apart by: they all count as one.
		return netip.Addr{}
	}
	client := remote.Addr().Unmap().WithZone("")
	if !within(trusted, client) {
		return client
	}
	hops := strings.Split(strings.Join(r.Header.Values("X-Forwarded-For"), ","), ",")
	for i := len(hops) - 1; i >= 0; i-- {
		hop, err := netip.ParseAddr(strings.TrimSpace(hops[i]))
		if err != nil {
			// What cannot be read ends the chain at the last proxy.
			break
		}
		client = hop.Unmap().WithZone("")
		if !within(trusted, client) {
			break
		}
	}
	return client
}

func within(networks []netip.Prefix, address netip.Addr) bool {
	for _, network := range networks {
		if network.Contains(address) {
			return true
		}
	}
	return false
}

// addresses turns a client address into the key of a counter. The address
// itself is never kept: the key is a hash of it under a secret that exists
// only in this process, so that what is in memory cannot be turned back
// into an address, by reading it or by trying every address.
type addresses struct {
	secret []byte
}

func newAddresses() (*addresses, error) {
	secret := make([]byte, sha256.Size)
	if _, err := rand.Read(secret); err != nil {
		return nil, fmt.Errorf("generate the secret of the address hash: %w", err)
	}
	return &addresses{secret: secret}, nil
}

// key returns the key of the counter of an address. An IPv6 address counts
// with its whole /64, which is what one client is given: counting each of
// its addresses apart would let it start a new counter with every request.
func (a *addresses) key(address netip.Addr) string {
	if address.Is6() {
		if network, err := address.Prefix(64); err == nil {
			address = network.Addr()
		}
	}
	mac := hmac.New(sha256.New, a.secret)
	// A hash never fails to take what it is given.
	_, _ = mac.Write(address.AsSlice())
	return string(mac.Sum(nil))
}
