package notifystore

import "testing"

// Uses literal IP addresses throughout, not real hostnames -- net.LookupIP
// resolves a literal IP without a network round trip, so these tests stay
// deterministic and fast in any environment, including one with no DNS/
// network access.
func TestValidateWebhookURLRejectsInternalAndMetadataAddresses(t *testing.T) {
	disallowed := []string{
		"http://169.254.169.254/latest/meta-data/",  // cloud instance metadata
		"http://127.0.0.1:8123/",                     // loopback -- e.g. ClickHouse
		"http://10.0.0.5:5432/",                       // RFC1918
		"http://172.17.0.2:9092/",                     // RFC1918 (default docker bridge range)
		"http://192.168.1.1/",                         // RFC1918
		"http://[::1]/",                                // IPv6 loopback
		"http://[fe80::1]/",                            // IPv6 link-local
		"http://[fc00::1]/",                            // IPv6 unique local (RFC4193)
		"http://0.0.0.0/",                              // unspecified
	}
	for _, u := range disallowed {
		if err := ValidateWebhookURL(u); err == nil {
			t.Errorf("ValidateWebhookURL(%q): want error, got nil", u)
		}
	}
}

func TestValidateWebhookURLAllowsPublicAddress(t *testing.T) {
	// A real-looking public IP literal, not a hostname needing DNS.
	if err := ValidateWebhookURL("https://8.8.8.8/webhook"); err != nil {
		t.Errorf("ValidateWebhookURL on a public IP: err = %v, want nil", err)
	}
}

func TestValidateWebhookURLRejectsBadScheme(t *testing.T) {
	cases := []string{"ftp://8.8.8.8/", "file:///etc/passwd", "not-a-url"}
	for _, u := range cases {
		if err := ValidateWebhookURL(u); err == nil {
			t.Errorf("ValidateWebhookURL(%q): want error, got nil", u)
		}
	}
}

func TestValidateWebhookURLRejectsEmptyHost(t *testing.T) {
	if err := ValidateWebhookURL("http:///path"); err == nil {
		t.Error("ValidateWebhookURL with no host: want error, got nil")
	}
}
