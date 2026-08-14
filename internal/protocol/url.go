package protocol

import (
	"fmt"
	"net"
	"net/url"
	"strings"
)

// NormalizeReturnURL canonicalizes HTTPS (or loopback HTTP) return URLs.
func NormalizeReturnURL(raw string) (string, error) {
	if raw == "" || strings.Contains(raw, " ") || strings.Contains(raw, "#") {
		return "", ErrInvalidURL
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrInvalidURL, err)
	}
	if u.IsAbs() == false || u.Host == "" || u.Opaque != "" || u.User != nil || u.Fragment != "" {
		return "", ErrInvalidURL
	}
	if u.RawFragment != "" {
		return "", ErrInvalidURL
	}
	scheme := strings.ToLower(u.Scheme)
	host, port, err := splitHostPort(u.Host)
	if err != nil {
		return "", ErrInvalidURL
	}
	host = strings.ToLower(host)
	switch scheme {
	case "https":
		if port == "443" {
			port = ""
		}
	case "http":
		if !isLoopbackHost(host) {
			return "", ErrInvalidURL
		}
		if port == "80" {
			port = ""
		}
	default:
		return "", ErrInvalidURL
	}
	if host == "" {
		return "", ErrInvalidURL
	}
	path := u.EscapedPath()
	if path == "" {
		path = "/"
	}
	if !strings.HasPrefix(path, "/") {
		return "", ErrInvalidURL
	}
	authority := host
	if strings.Contains(host, ":") && !strings.HasPrefix(host, "[") {
		authority = "[" + host + "]"
	}
	if port != "" {
		authority += ":" + port
	}
	out := scheme + "://" + authority + path
	if u.RawQuery != "" {
		out += "?" + u.RawQuery
	}
	return out, nil
}

// splitHostPort separates host and port, including bracketed IPv6.
func splitHostPort(hostport string) (string, string, error) {
	if hostport == "" {
		return "", "", ErrInvalidURL
	}
	if strings.HasPrefix(hostport, "[") {
		host, port, err := net.SplitHostPort(hostport)
		if err != nil {
			if strings.HasSuffix(hostport, "]") {
				return strings.TrimPrefix(strings.TrimSuffix(hostport, "]"), "["), "", nil
			}
			return "", "", ErrInvalidURL
		}
		return host, port, nil
	}
	if strings.Count(hostport, ":") == 1 {
		host, port, err := net.SplitHostPort(hostport)
		if err != nil {
			return "", "", ErrInvalidURL
		}
		return host, port, nil
	}
	if strings.Contains(hostport, ":") {
		return "", "", ErrInvalidURL
	}
	return hostport, "", nil
}

// isLoopbackHost reports whether host is localhost or a loopback address.
func isLoopbackHost(host string) bool {
	switch host {
	case "localhost", "127.0.0.1", "::1":
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// IsHTTPSPublicURL requires HTTPS except for explicit loopback development URLs.
func IsHTTPSPublicURL(raw string) error {
	normalized, err := NormalizeReturnURL(raw)
	if err != nil {
		return err
	}
	u, err := url.Parse(normalized)
	if err != nil {
		return ErrInvalidURL
	}
	if u.Scheme != "https" && !isLoopbackHost(u.Hostname()) {
		return ErrInvalidURL
	}
	return nil
}
