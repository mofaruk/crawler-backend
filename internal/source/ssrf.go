package source

import (
	"fmt"
	"net"
	"net/url"
	"strings"
)

// ValidateTargetURL rejects URLs that point at infrastructure rather than the
// public web.
//
// Both the URL source and every crawl target are supplied by users, and the
// crawler fetches them server-side. Without this check the API is a
// server-side request forgery primitive: a caller can point a "site" at
// http://169.254.169.254/latest/meta-data/ and have the crawler read cloud
// credentials, or sweep an internal network by watching status codes.
//
// The check resolves the host first, so a public name that maps to a private
// address (DNS rebinding, an internal split-horizon zone) is caught too. All
// resolved addresses must be public — one private answer rejects the URL.
//
// allowPrivate short-circuits the whole check for local development, where
// sources legitimately live on host.docker.internal.
func ValidateTargetURL(raw string, allowPrivate bool) error {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return fmt.Errorf("invalid URL: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("URL scheme must be http or https, got %q", u.Scheme)
	}

	host := u.Hostname()
	if host == "" {
		return fmt.Errorf("URL has no host")
	}

	// ALLOW_PRIVATE_TARGETS exists so local development can crawl fixtures on
	// host.docker.internal. It permits private *addresses* — it must not
	// disable scheme and host validation, or "javascript:alert(1)" is accepted
	// and stored as a site's base_url.
	if allowPrivate {
		return nil
	}

	ips, err := net.LookupIP(host)
	if err != nil {
		return fmt.Errorf("cannot resolve host %q: %w", host, err)
	}
	if len(ips) == 0 {
		return fmt.Errorf("host %q resolved to no addresses", host)
	}

	for _, ip := range ips {
		if !isPublicIP(ip) {
			return fmt.Errorf("host %q resolves to the non-public address %s; only public web addresses may be crawled", host, ip)
		}
	}
	return nil
}

// isPublicIP reports whether an address is routable on the public internet.
func isPublicIP(ip net.IP) bool {
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsUnspecified() ||
		ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsInterfaceLocalMulticast() || ip.IsMulticast() {
		return false
	}

	// Ranges net.IP's helpers do not cover.
	for _, cidr := range []string{
		"100.64.0.0/10",  // RFC 6598 carrier-grade NAT
		"192.0.0.0/24",   // RFC 6890 IETF protocol assignments
		"192.0.2.0/24",   // TEST-NET-1
		"198.18.0.0/15",  // benchmarking
		"198.51.100.0/24",// TEST-NET-2
		"203.0.113.0/24", // TEST-NET-3
		"240.0.0.0/4",    // reserved
		"::/128",         // unspecified
		"64:ff9b::/96",   // IPv4/IPv6 translation
		"100::/64",       // discard-only
		"2001:db8::/32",  // documentation
	} {
		if _, block, err := net.ParseCIDR(cidr); err == nil && block.Contains(ip) {
			return false
		}
	}
	return true
}
