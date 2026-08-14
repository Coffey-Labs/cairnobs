module github.com/sentry/sentry/enterprise

go 1.25.0

// enterprise/ importing api/ (core) is the allowed direction of the
// module boundary hack/check-tenant-boundary.sh enforces -- see
// enterprise/internal/chrunner's doc comment: it implements api's
// executor.SQLRunner interface, which structurally requires importing
// the package that defines it.
replace github.com/sentry/sentry/api => ../api

// api's own go.mod replace directive for proto/ is module-local and
// doesn't propagate here -- enterprise/ needs its own, or `go build`
// tries to fetch github.com/sentry/sentry/proto from a real (nonexistent)
// remote, since api/searchclient (now transitively imported) depends on
// the generated search gRPC stubs.
replace github.com/sentry/sentry/proto => ../proto

require github.com/sentry/sentry/api v0.0.0-00010101000000-000000000000

require (
	github.com/ClickHouse/clickhouse-go/v2 v2.48.0
	github.com/coreos/go-oidc/v3 v3.20.0
	github.com/crewjam/saml v0.5.1
	github.com/go-jose/go-jose/v4 v4.1.4
	github.com/google/uuid v1.6.0
	github.com/jackc/pgx/v5 v5.10.0
	github.com/sentry/sentry/proto v0.0.0-00010101000000-000000000000
	golang.org/x/oauth2 v0.36.0
	google.golang.org/grpc v1.83.0
)

require (
	github.com/ClickHouse/ch-go v0.74.0 // indirect
	github.com/andybalholm/brotli v1.2.2 // indirect
	github.com/beevik/etree v1.5.0 // indirect
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/go-faster/city v1.0.1 // indirect
	github.com/go-faster/errors v0.7.1 // indirect
	github.com/golang-jwt/jwt/v4 v4.5.2 // indirect
	github.com/jackc/pgpassfile v1.0.0 // indirect
	github.com/jackc/pgservicefile v0.0.0-20240606120523-5a60cdf6a761 // indirect
	github.com/jackc/puddle/v2 v2.2.2 // indirect
	github.com/jonboulle/clockwork v0.2.2 // indirect
	github.com/klauspost/compress v1.19.1 // indirect
	github.com/mattermost/xml-roundtrip-validator v0.1.0 // indirect
	github.com/paulmach/orb v0.13.0 // indirect
	github.com/pierrec/lz4/v4 v4.1.27 // indirect
	github.com/russellhaering/goxmldsig v1.4.0 // indirect
	github.com/segmentio/asm v1.2.1 // indirect
	github.com/shopspring/decimal v1.4.0 // indirect
	go.opentelemetry.io/otel v1.44.0 // indirect
	go.opentelemetry.io/otel/trace v1.44.0 // indirect
	golang.org/x/crypto v0.54.0 // indirect
	golang.org/x/net v0.57.0 // indirect
	golang.org/x/sync v0.22.0 // indirect
	golang.org/x/sys v0.47.0 // indirect
	golang.org/x/text v0.40.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20260526163538-3dc84a4a5aaa // indirect
	google.golang.org/protobuf v1.36.12 // indirect
)
