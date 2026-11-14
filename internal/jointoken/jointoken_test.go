package jointoken

import (
	"strings"
	"testing"
)

func TestRoundTrip(t *testing.T) {
	in := Token{Server: "https://monitor.example.org:8461", CAFingerprint: "SHA256:" + strings.Repeat("AB", 32), Secret: strings.Repeat("s", 43)}
	out, err := Parse("  " + in.Encode() + "\n")
	if err != nil {
		t.Fatal(err)
	}
	if out.Server != in.Server || out.Secret != in.Secret || out.CAFingerprint != "sha256:"+strings.Repeat("ab", 32) {
		t.Fatalf("got %+v", out)
	}
	for _, bad := range []string{
		"", "cobsj1_", "cobsj1_!!!", in.Encode()[:20], "x" + in.Encode(),
		Token{Server: "http://h:8461", CAFingerprint: in.CAFingerprint, Secret: in.Secret}.Encode(),
		Token{Server: in.Server, CAFingerprint: "sha256:12", Secret: in.Secret}.Encode(),
		Token{Server: in.Server, CAFingerprint: in.CAFingerprint, Secret: "short"}.Encode(),
	} {
		if _, err := Parse(bad); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}
