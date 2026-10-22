package notifications

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"time"

	"github.com/zsoftly/zs-core/pkg/brand"
)

const (
	DefaultMaxResponseBytes int64 = 64 << 10
	DefaultRequestTimeout         = 10 * time.Second
)

var specialPurposePrefixes = mustPrefixes(
	// IANA IPv4 Special-Purpose Address Space, updated 2025-10-09.
	"0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8",
	"169.254.0.0/16", "172.16.0.0/12", "192.0.0.0/24", "192.0.2.0/24",
	"192.31.196.0/24", "192.52.193.0/24", "192.88.99.0/24",
	"192.168.0.0/16", "192.175.48.0/24", "198.18.0.0/15",
	"198.51.100.0/24", "203.0.113.0/24", "240.0.0.0/4",
	"255.255.255.255/32",
	// IANA IPv6 Special-Purpose Address Space, updated 2025-10-09.
	"::/128", "::1/128", "::ffff:0:0/96", "64:ff9b::/96",
	"64:ff9b:1::/48", "100::/64", "100:0:0:1::/64", "2001::/23",
	"2001::/32", "2001:1::1/128", "2001:1::2/128", "2001:1::3/128",
	"2001:2::/48", "2001:3::/32", "2001:4:112::/48", "2001:10::/28",
	"2001:20::/28", "2001:30::/28", "2001:db8::/32", "2002::/16",
	"2620:4f:8000::/48", "3fff::/20", "5f00::/16", "fc00::/7", "fe80::/10",
)

var publicIPv6 = netip.MustParsePrefix("2000::/3")

func mustPrefixes(values ...string) []netip.Prefix {
	prefixes := make([]netip.Prefix, 0, len(values))
	for _, value := range values {
		prefixes = append(prefixes, netip.MustParsePrefix(value))
	}
	return prefixes
}

// CanonicalURL returns the stable endpoint identity accepted by a webhook
// manifest. URL validation belongs to brand because manifests use it too.
func CanonicalURL(raw string) (string, error) { return brand.CanonicalWebhookURL(raw) }

// HTTPClient returns the transport used for outbound webhook delivery. It does
// not inherit http.DefaultTransport because callers may have changed its proxy,
// TLS, or dial hooks. Redirects are returned to the caller as a non-successful
// response and are never followed.
func HTTPClient(resolver *net.Resolver) *http.Client {
	transport := &http.Transport{
		Proxy:                 nil,
		DialContext:           PublicDialContext(resolver, &net.Dialer{Timeout: DefaultRequestTimeout}),
		ForceAttemptHTTP2:     false,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: time.Second,
		TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12},
	}
	return &http.Client{
		Transport: httpsOnlyTransport{transport: transport},
		Timeout:   DefaultRequestTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

type httpsOnlyTransport struct {
	transport http.RoundTripper
}

func (t httpsOnlyTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if request.URL.Scheme != "https" {
		return nil, errors.New("webhook requests must use HTTPS")
	}
	return t.transport.RoundTrip(request)
}

// PublicDialContext resolves a name once, rejects any non-public
// answer, and dials the approved address literally. This prevents a second
// DNS resolution from changing the address after the policy check.
func PublicDialContext(resolver *net.Resolver, dialer *net.Dialer) func(context.Context, string, string) (net.Conn, error) {
	if resolver == nil {
		resolver = net.DefaultResolver
	}
	if dialer == nil {
		dialer = &net.Dialer{}
	}
	return publicDialContext(func(ctx context.Context, host string) ([]netip.Addr, error) {
		return resolver.LookupNetIP(ctx, "ip", host)
	}, dialer.DialContext)
}

type dialContext func(context.Context, string, string) (net.Conn, error)
type lookupNetIP func(context.Context, string) ([]netip.Addr, error)

func publicDialContext(lookup lookupNetIP, dial dialContext) dialContext {
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, fmt.Errorf("split webhook address: %w", err)
		}
		portNumber, err := strconv.ParseUint(port, 10, 16)
		if err != nil || portNumber == 0 {
			return nil, errors.New("webhook address has an invalid port")
		}
		if strings.Contains(host, "%") {
			return nil, errors.New("webhook address must not contain an IPv6 zone")
		}
		if literal, err := netip.ParseAddr(host); err == nil {
			if !publicAddress(literal) {
				return nil, errors.New("webhook address is not public")
			}
			return dial(ctx, network, net.JoinHostPort(literal.Unmap().String(), port))
		}
		addresses, err := lookup(ctx, host)
		if err != nil {
			return nil, fmt.Errorf("resolve webhook host: %w", err)
		}
		if len(addresses) == 0 {
			return nil, errors.New("webhook host did not resolve")
		}
		for _, ip := range addresses {
			if !publicAddress(ip) {
				return nil, errors.New("webhook host resolved to a non-public address")
			}
		}
		var lastErr error
		for _, ip := range addresses {
			connection, err := dial(ctx, network, net.JoinHostPort(ip.Unmap().String(), port))
			if err == nil {
				return connection, nil
			}
			lastErr = err
		}
		return nil, lastErr
	}
}

func publicAddress(addr netip.Addr) bool {
	if !addr.IsValid() || addr.Zone() != "" {
		return false
	}
	addr = addr.Unmap()
	for _, prefix := range specialPurposePrefixes {
		if prefix.Contains(addr) {
			return false
		}
	}
	if addr.Is4() {
		return !addr.IsMulticast() && !addr.IsPrivate() && !addr.IsLoopback() && !addr.IsLinkLocalUnicast() && !addr.IsUnspecified()
	}
	return publicIPv6.Contains(addr)
}
