package totp

import (
	"encoding/base32"
	"strings"
	"testing"
	"time"
)

// RFC 6238 appendix B, SHA-1, with the last six digits.
func TestRFC6238(t *testing.T) {
	secret := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString([]byte("12345678901234567890"))
	for unix, want := range map[int64]string{59: "287082", 1111111109: "081804", 1111111111: "050471", 1234567890: "005924", 2000000000: "279037", 20000000000: "353130"} {
		got, err := Code(secret, unix/30)
		if err != nil || got != want {
			t.Errorf("at %d: %s %v, want %s", unix, got, err, want)
		}
	}
}

func TestVerify(t *testing.T) {
	s := NewSecret()
	now := time.Unix(1_800_000_015, 0)
	cur := Step(now)
	c, _ := Code(s, cur)
	step, ok := Verify(s, c[:3]+" "+c[3:], now, 0)
	if !ok || step != cur {
		t.Fatalf("the current code: %v %d", ok, step)
	}
	if _, ok := Verify(s, c, now, cur); ok {
		t.Fatal("a code worked twice")
	}
	prev, _ := Code(s, cur-1)
	if _, ok := Verify(s, prev, now, 0); !ok {
		t.Fatal("the code from one step ago was refused")
	}
	old, _ := Code(s, cur-2)
	if _, ok := Verify(s, old, now, 0); ok {
		t.Fatal("a code two steps old was taken")
	}
	for _, bad := range []string{"", "12345", "1234567", "abcdef"} {
		if _, ok := Verify(s, bad, now, 0); ok {
			t.Errorf("took %q", bad)
		}
	}
	if u := URI("Cairn OBS", "a@b.test", s); !strings.HasPrefix(u, "otpauth://totp/Cairn%20OBS:a@b.test?") || !strings.Contains(u, "secret="+s) {
		t.Fatalf("uri %s", u)
	}
}
