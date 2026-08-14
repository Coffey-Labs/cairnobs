package tenant

import (
	"context"
	"testing"
)

func TestFromContextRoundTrip(t *testing.T) {
	id := TrustFromValidatedSession("acme-corp")
	ctx := WithContext(context.Background(), id)

	got, ok := FromContext(ctx)
	if !ok {
		t.Fatalf("expected FromContext to find a tenant")
	}
	if got.String() != "acme-corp" {
		t.Fatalf("got %q, want %q", got.String(), "acme-corp")
	}
}

func TestFromContextMissing(t *testing.T) {
	_, ok := FromContext(context.Background())
	if ok {
		t.Fatalf("expected no tenant in a bare context")
	}
}

func TestFromContextDoesNotMatchUnrelatedStringKey(t *testing.T) {
	// The unexported contextKey type is what closes the "collision" gap
	// the design doc calls out -- a context.WithValue using a plain
	// string key must not be found by FromContext.
	ctx := context.WithValue(context.Background(), "tenant_id", "spoofed") //nolint:staticcheck
	_, ok := FromContext(ctx)
	if ok {
		t.Fatalf("FromContext must not find a value set under an unrelated key type")
	}
}
