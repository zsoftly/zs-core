package brand

import (
	"errors"
	"net/url"
	"strings"
)

// loopbackHosts may use plain http in redirect URIs and web origins. Any
// port is allowed, as RFC 8252 section 7.3 describes for native apps.
var loopbackHosts = map[string]bool{"127.0.0.1": true, "localhost": true, "::1": true}

// checkRedirectURI enforces the v1 rules for a client redirect URI: https,
// or http on a loopback host; no wildcard in the host or port; at most one
// wildcard, as a trailing "/*", and not after a query or fragment, where it
// would match more than a path; no custom scheme, user information or
// fragment.
func checkRedirectURI(raw string) error {
	if strings.Count(raw, "*") > 1 || (strings.Contains(raw, "*") && !strings.HasSuffix(raw, "/*")) {
		return errors.New("the only wildcard allowed is a single trailing /*")
	}
	if strings.HasSuffix(raw, "/*") && strings.ContainsAny(raw, "?#") {
		return errors.New("the trailing /* wildcard must not follow a query or fragment")
	}
	u, err := checkOrigin(strings.TrimSuffix(raw, "*"))
	if err != nil {
		return err
	}
	if u.Fragment != "" || strings.Contains(raw, "#") {
		return errors.New("must not have a fragment")
	}
	return nil
}

// checkWebOrigin enforces the v1 rules for a web origin: "+" (the origins of
// the redirect URIs) or an https origin, or an http origin on a loopback
// host. A bare "*" is refused.
func checkWebOrigin(raw string) error {
	if raw == "+" {
		return nil
	}
	if raw == "*" {
		return errors.New(`"*" allows every origin and is refused; use "+" or list the origins`)
	}
	u, err := checkOrigin(raw)
	if err != nil {
		return err
	}
	if u.Path != "" || u.RawQuery != "" || u.Fragment != "" || strings.ContainsAny(raw, "?#") {
		return errors.New("must be an origin, scheme://host[:port], without a path")
	}
	return nil
}

func checkOrigin(raw string) (*url.URL, error) {
	if strings.ContainsAny(raw, " \t\r\n\\") {
		return nil, errors.New("must not contain spaces or backslashes")
	}
	u, err := url.Parse(raw)
	if err != nil || u.Opaque != "" {
		return nil, errors.New("must be an absolute https URL, or http on 127.0.0.1, localhost or [::1]")
	}
	switch u.Scheme {
	case "https":
	case "http":
		if !loopbackHosts[u.Hostname()] {
			return nil, errors.New("plain http is allowed only on 127.0.0.1, localhost and [::1]")
		}
	default:
		return nil, errors.New("must use https, or http on a loopback host; custom schemes are not supported in v1")
	}
	host := u.Hostname()
	if host == "" || strings.Contains(u.Host, "*") {
		return nil, errors.New("must name a host without a wildcard")
	}
	if u.User != nil {
		return nil, errors.New("must not contain user information")
	}
	return u, nil
}
