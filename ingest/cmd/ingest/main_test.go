package main

import (
	"testing"

	"github.com/cairnobs/cairnobs/ingest/consumer"
	"github.com/cairnobs/cairnobs/ingest/internal/grpcserver"
)

// TestTenantIDHeaderKeyConstantsMatch guards against the literal drift
// grpcserver.TenantIDHeaderKey's doc comment warns about: the producer
// side (grpcserver) and the consumer side (consumer) each define their
// own copy of this Kafka header key name rather than importing across
// that producer/consumer boundary, so nothing else catches a typo in
// either one at compile time.
func TestTenantIDHeaderKeyConstantsMatch(t *testing.T) {
	if grpcserver.TenantIDHeaderKey != consumer.TenantIDHeaderKey {
		t.Fatalf("grpcserver.TenantIDHeaderKey = %q, consumer.TenantIDHeaderKey = %q -- these must match",
			grpcserver.TenantIDHeaderKey, consumer.TenantIDHeaderKey)
	}
}
