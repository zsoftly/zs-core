package brand

import (
	"errors"
	"net"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
)

// CanonicalWebhookURL validates the restricted webhook endpoint form and
// returns its identity. Queries, fragments and credentials are intentionally
// unsupported because a delivery identity must be stable and auditable.
func CanonicalWebhookURL(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Opaque != "" || u.Host == "" || u.User != nil || u.Fragment != "" || strings.Contains(raw, "#") || u.RawQuery != "" || u.ForceQuery {
		return "", errors.New("webhook URL must be an absolute HTTPS URL without credentials, query, or fragment")
	}
	port := u.Port()
	if strings.HasSuffix(u.Host, ":") || (port != "" && !validWebhookPort(port)) {
		return "", errors.New("webhook URL port must be between 1 and 65535")
	}
	if port != "" {
		n, _ := strconv.ParseUint(port, 10, 16)
		port = strconv.FormatUint(n, 10)
	}
	host := strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
	if host == "" {
		return "", errors.New("webhook URL host is required")
	}
	if strings.Contains(host, "%") {
		return "", errors.New("webhook URL host must not contain an IPv6 zone")
	}
	if ip, err := netip.ParseAddr(host); err == nil {
		host = ip.Unmap().String()
	}
	if port != "" && port != "443" {
		host = net.JoinHostPort(host, port)
	} else if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	path := u.EscapedPath()
	if path == "" {
		path = "/"
	}
	path = canonicalWebhookPath(path)
	return "https://" + host + path, nil
}

func validWebhookPort(port string) bool {
	n, err := strconv.ParseUint(port, 10, 16)
	return err == nil && n > 0
}

func canonicalWebhookPath(path string) string {
	const hex = "0123456789ABCDEF"
	var b strings.Builder
	b.Grow(len(path))
	for i := 0; i < len(path); i++ {
		if path[i] != '%' || i+2 >= len(path) {
			b.WriteByte(path[i])
			continue
		}
		value := fromHex(path[i+1])<<4 | fromHex(path[i+2])
		if isUnreserved(value) {
			b.WriteByte(value)
		} else {
			b.WriteByte('%')
			b.WriteByte(hex[value>>4])
			b.WriteByte(hex[value&0x0f])
		}
		i += 2
	}
	return b.String()
}

func fromHex(b byte) byte {
	if b >= '0' && b <= '9' {
		return b - '0'
	}
	if b >= 'a' && b <= 'f' {
		return b - 'a' + 10
	}
	return b - 'A' + 10
}

func isUnreserved(b byte) bool {
	return b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9' || b == '-' || b == '.' || b == '_' || b == '~'
}
