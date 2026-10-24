package notifications

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"testing"
)

// Every supplied IANA registry prefix is represented here. The policy denies
// special-purpose space even where IANA labels an assignment globally reachable.
var registryPrefixes = mustPrefixes(
	"0.0.0.0/8", "0.0.0.0/32", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8", "169.254.0.0/16", "172.16.0.0/12", "192.0.0.0/24", "192.0.0.0/29", "192.0.0.8/32", "192.0.0.9/32", "192.0.0.10/32", "192.0.0.170/32", "192.0.0.171/32", "192.0.2.0/24", "192.31.196.0/24", "192.52.193.0/24", "192.88.99.0/24", "192.88.99.2/32", "192.168.0.0/16", "192.175.48.0/24", "198.18.0.0/15", "198.51.100.0/24", "203.0.113.0/24", "240.0.0.0/4", "255.255.255.255/32",
	"::1/128", "::/128", "::ffff:0:0/96", "64:ff9b::/96", "64:ff9b:1::/48", "100::/64", "100:0:0:1::/64", "2001::/23", "2001::/32", "2001:1::1/128", "2001:1::2/128", "2001:1::3/128", "2001:2::/48", "2001:3::/32", "2001:4:112::/48", "2001:10::/28", "2001:20::/28", "2001:30::/28", "2001:db8::/32", "2002::/16", "2620:4f:8000::/48", "3fff::/20", "5f00::/16", "fc00::/7", "fe80::/10",
)

func TestPublicAddressDeniesEveryIANARegistryPrefixBoundary(t *testing.T) {
	if len(registryPrefixes) != 51 {
		t.Fatalf("registry prefix count = %d, want 51", len(registryPrefixes))
	}
	for _, prefix := range registryPrefixes {
		for _, address := range prefixBoundaryAddresses(prefix) {
			want := expectedPublicAddress(address)
			if got := publicAddress(address); got != want {
				t.Errorf("publicAddress(%s), for %s = %t, want %t", address, prefix, got, want)
			}
		}
	}
}

func TestPublicAddressPolicy(t *testing.T) {
	for _, test := range []struct {
		address string
		want    bool
	}{
		{"8.8.8.8", true},
		{"2606:4700:4700::1111", true},
		{"224.0.0.1", false},
		{"::ffff:127.0.0.1", false},
		{"::ffff:8.8.8.8", true},
		{"fe80::1%en0", false},
		{"invalid", false},
	} {
		address, err := netip.ParseAddr(test.address)
		if err != nil {
			if test.address == "invalid" && publicAddress(netip.Addr{}) == test.want {
				continue
			}
			t.Fatal(err)
		}
		if got := publicAddress(address); got != test.want {
			t.Errorf("publicAddress(%q) = %t, want %t", test.address, got, test.want)
		}
	}
}

func TestPublicDialContextPinsResolvedAddressAndRejectsRebinding(t *testing.T) {
	var dialed []string
	dial := func(_ context.Context, _, address string) (net.Conn, error) {
		dialed = append(dialed, address)
		if address == "1.1.1.1:443" {
			connection, peer := net.Pipe()
			peer.Close()
			return connection, nil
		}
		return nil, errors.New("dialled")
	}
	lookup := func(_ context.Context, host string) ([]netip.Addr, error) {
		if host == "fallback.example" {
			return []netip.Addr{netip.MustParseAddr("8.8.8.8"), netip.MustParseAddr("1.1.1.1")}, nil
		}
		if host == "rebind.example" {
			return []netip.Addr{netip.MustParseAddr("8.8.8.8"), netip.MustParseAddr("127.0.0.1")}, nil
		}
		return []netip.Addr{netip.MustParseAddr("8.8.8.8")}, nil
	}
	dialContext := publicDialContext(lookup, dial)
	connection, err := dialContext(context.Background(), "tcp", "fallback.example:443")
	if err != nil || strings.Join(dialed, ",") != "8.8.8.8:443,1.1.1.1:443" {
		t.Fatalf("fallback dials = %q, %v", dialed, err)
	}
	connection.Close()
	dialed = nil
	if _, err := dialContext(context.Background(), "tcp", "rebind.example:443"); err == nil || len(dialed) != 0 {
		t.Fatalf("rebinding was accepted: dials = %q, %v", dialed, err)
	}
	if _, err := dialContext(context.Background(), "tcp", "[2606:4700:4700::1111%en0]:443"); err == nil || len(dialed) != 0 {
		t.Fatalf("zoned address was accepted: dials = %q, %v", dialed, err)
	}
	if _, err := dialContext(context.Background(), "tcp", "[::ffff:127.0.0.1]:443"); err == nil {
		t.Fatal("mapped loopback literal was accepted")
	}
	dialed = nil
	if _, err := dialContext(context.Background(), "tcp", "[::ffff:8.8.8.8]:443"); err == nil || strings.Join(dialed, ",") != "8.8.8.8:443" {
		t.Fatalf("mapped public literal = %q, %v", dialed, err)
	}
}

func TestHTTPClientDisablesProxyAndRedirects(t *testing.T) {
	client := HTTPClient(nil)
	httpsTransport, ok := client.Transport.(httpsOnlyTransport)
	if !ok {
		t.Fatalf("transport type = %T", client.Transport)
	}
	transport, ok := httpsTransport.transport.(*http.Transport)
	if !ok || transport.Proxy != nil || transport.DialTLSContext != nil || transport.TLSClientConfig == nil || transport.TLSClientConfig.InsecureSkipVerify {
		t.Fatalf("unsafe transport: %#v", transport)
	}
	redirects := 0
	requests := 0
	httpsTransport.transport = roundTripper(func(request *http.Request) (*http.Response, error) {
		requests++
		if request.URL.Host == "first.example" {
			return &http.Response{StatusCode: http.StatusFound, Header: http.Header{"Location": []string{"https://second.example/"}}, Body: http.NoBody, Request: request}, nil
		}
		redirects++
		return nil, errors.New("redirect target dialled")
	})
	client.Transport = httpsTransport
	request, err := http.NewRequest(http.MethodGet, "https://first.example/", nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.Do(request)
	if err != nil || response.StatusCode != http.StatusFound || redirects != 0 || requests != 1 {
		t.Fatalf("redirect response = %#v, %v; target calls = %d", response, err, redirects)
	}
	request, err = http.NewRequest(http.MethodGet, "http://first.example/", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Do(request); err == nil || requests != 1 {
		t.Fatal("HTTP request was accepted")
	}
}

type roundTripper func(*http.Request) (*http.Response, error)

func (f roundTripper) RoundTrip(request *http.Request) (*http.Response, error) { return f(request) }

func overlapsRegistry(address netip.Addr) bool {
	address = address.Unmap()
	for _, prefix := range registryPrefixes {
		if prefix.Contains(address) {
			return true
		}
	}
	return false
}

func expectedPublicAddress(address netip.Addr) bool {
	if !address.IsValid() || address.Zone() != "" {
		return false
	}
	address = address.Unmap()
	if overlapsRegistry(address) {
		return false
	}
	if address.Is4() {
		return !address.IsMulticast() && !address.IsPrivate() && !address.IsLoopback() && !address.IsLinkLocalUnicast() && !address.IsUnspecified()
	}
	return netip.MustParsePrefix("2000::/3").Contains(address)
}
func prefixBoundaryAddresses(prefix netip.Prefix) []netip.Addr {
	first := prefix.Masked().Addr()
	last := lastAddress(prefix)
	addresses := []netip.Addr{first, last}
	if before, ok := decrement(first); ok {
		addresses = append(addresses, before)
	}
	if after, ok := increment(last); ok {
		addresses = append(addresses, after)
	}
	return addresses
}

func lastAddress(prefix netip.Prefix) netip.Addr {
	address := prefix.Masked().Addr()
	bits := prefix.Bits()
	if address.Is4() {
		bytes := address.As4()
		for bit := bits; bit < 32; bit++ {
			bytes[bit/8] |= 1 << (7 - bit%8)
		}
		return netip.AddrFrom4(bytes)
	}
	bytes := address.As16()
	for bit := bits; bit < 128; bit++ {
		bytes[bit/8] |= 1 << (7 - bit%8)
	}
	return netip.AddrFrom16(bytes)
}

func increment(address netip.Addr) (netip.Addr, bool) {
	if address.Is4() {
		bytes := address.As4()
		for i := len(bytes) - 1; i >= 0; i-- {
			bytes[i]++
			if bytes[i] != 0 {
				return netip.AddrFrom4(bytes), true
			}
		}
		return netip.Addr{}, false
	}
	bytes := address.As16()
	for i := len(bytes) - 1; i >= 0; i-- {
		bytes[i]++
		if bytes[i] != 0 {
			return netip.AddrFrom16(bytes), true
		}
	}
	return netip.Addr{}, false
}

func decrement(address netip.Addr) (netip.Addr, bool) {
	if address.Is4() {
		bytes := address.As4()
		for i := len(bytes) - 1; i >= 0; i-- {
			if bytes[i] != 0 {
				bytes[i]--
				return netip.AddrFrom4(bytes), true
			}
			bytes[i] = 255
		}
		return netip.Addr{}, false
	}
	bytes := address.As16()
	for i := len(bytes) - 1; i >= 0; i-- {
		if bytes[i] != 0 {
			bytes[i]--
			return netip.AddrFrom16(bytes), true
		}
		bytes[i] = 255
	}
	return netip.Addr{}, false
}
