package notifystore

import (
	"fmt"
	"net"
	"net/url"
)

// ValidateWebhookURL rejects a notification target URL that resolves to
// an internal, loopback, link-local, or cloud-metadata address --
// closes an SSRF path a security audit found: without this, any user
// who could create a notification target could point
// delivery.Worker.attempt's outbound POST at
// http://169.254.169.254/... or an internal service address, and read
// back what happened via delivery_log's recorded status code -- a
// semi-blind SSRF oracle. Applies to all three Kind values (webhook,
// slack, pagerduty), since all three deliver through the same
// WebhookURL-addressed POST -- see webhook.go's package doc comment.
//
// Callers should invoke this both at target-creation time
// (httpapi.handleCreateTarget) and again immediately before every
// delivery attempt (delivery.Worker.attempt): a hostname that resolved
// to a public IP at creation time can be repointed at an internal one
// later (DNS rebinding), so checking only once would leave that gap
// open.
func ValidateWebhookURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("invalid URL: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("URL scheme must be http or https, got %q", u.Scheme)
	}
	host := u.Hostname()
	if host == "" {
		return fmt.Errorf("URL must have a host")
	}

	ips, err := net.LookupIP(host)
	if err != nil {
		return fmt.Errorf("resolving host %q: %w", host, err)
	}
	if len(ips) == 0 {
		return fmt.Errorf("host %q did not resolve to any address", host)
	}
	for _, ip := range ips {
		if isDisallowedWebhookTarget(ip) {
			return fmt.Errorf("host %q resolves to %s, a disallowed address -- internal, loopback, link-local, and cloud-metadata addresses are not allowed as notification target URLs", host, ip)
		}
	}
	return nil
}

// isDisallowedWebhookTarget covers RFC1918/RFC4193 private ranges,
// loopback, link-local (which also covers 169.254.169.254, the AWS/GCP/
// Azure instance-metadata address), unspecified, and multicast.
func isDisallowedWebhookTarget(ip net.IP) bool {
	return ip.IsLoopback() ||
		ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() ||
		ip.IsPrivate() ||
		ip.IsUnspecified() ||
		ip.IsMulticast()
}
