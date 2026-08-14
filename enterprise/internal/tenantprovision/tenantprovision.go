// Package tenantprovision does the ClickHouse-side half of tenant
// provisioning /docs/phase-4-isolation-design.md describes: one
// dedicated database + one narrowly-granted user per tenant, ordered
// and idempotent (CREATE DATABASE -> CREATE USER -> GRANT). This is the
// piece that was missing before -- deploy/operator's Tenant controller
// only manages the K8s-side credential Secret; nothing called
// ClickHouse's DDL to make that Secret's credentials actually work
// until this package.
//
// What this package does NOT do: Tantivy index provisioning (still
// unbuilt -- see /docs/security/threat-model.md), and it does not
// itself decide when a tenant becomes 'active' in rbacstore.tenants --
// the caller (enterprise-api's -provision-tenant flag) does that only
// after ProvisionClickHouse returns success, matching the ordered gate
// /docs/phase-4-isolation-design.md specifies: CREATE USER -> GRANT ->
// only then mark active.
package tenantprovision

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"regexp"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

// tenantIdentifierPattern is deliberately strict: tenant IDs become
// literal ClickHouse database/user names, interpolated directly into
// DDL statements below (ClickHouse's driver has no parameterized-query
// support for identifiers, only values -- this is the real reason
// rbacstore.Tenant.ID and every K8s Tenant CRD name are constrained to
// look like a DNS-safe slug already; this regexp is the enforcement
// point specific to this package's SQL construction, not a general
// tenant-ID validator).
var tenantIdentifierPattern = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,62}$`)

// Credentials is what ProvisionClickHouse hands back for the caller to
// persist (rbacstore.Store.SetDataSourceClickHouseCredentials) --
// Password is returned exactly once; ClickHouse itself doesn't store it
// recoverably, so losing this return value means re-provisioning
// (dropping and recreating the user) is the only recovery path.
type Credentials struct {
	Username string
	Password string
}

// Provisioner wraps an admin ClickHouse connection -- one with
// access_management enabled, the same credential docker-compose.yml's
// CLICKHOUSE_PASSWORD/the Helm chart's clickhouse Secret already is.
// Never the per-tenant connections enterprise/internal/chrunner opens.
type Provisioner struct {
	admin driver.Conn
}

func New(admin driver.Conn) *Provisioner {
	return &Provisioner{admin: admin}
}

// ProvisionClickHouse creates tenantID's database and a fresh user for
// it, granted SELECT on exactly that database and nothing else.
// Database creation is idempotent (CREATE DATABASE IF NOT EXISTS --
// harmless to repeat). User creation deliberately is NOT idempotent
// (plain CREATE USER, no IF NOT EXISTS): ClickHouse has no way to read
// back an existing user's password, so silently succeeding on a second
// call would either mean returning stale/wrong credentials or silently
// rotating a live tenant's password out from under it -- the same
// "never rotate a live credential without coordinating the consumer
// side" reasoning as deploy/operator/internal/controller/
// tenant_controller.go's reconcileSecret. A second call for an
// already-provisioned tenant fails loudly instead, which is the correct
// outcome: the caller (enterprise-api's -provision-tenant flag) must
// check rbacstore for existing credentials before ever calling this,
// not rely on this function to be safely re-callable.
//
// system.* access: intentionally not explicitly granted anywhere here,
// relying on ClickHouse RBAC's default-deny for a freshly created user
// once access_management is enabled on the admin connection (required
// for CREATE USER/GRANT to work at all). This is exactly the assumption
// /docs/phase-4-isolation-design.md's task 2 finding says must be
// verified live per ClickHouse version, not trusted from documentation
// -- see /docs/security/threat-model.md's "system.query_log metadata
// leakage" section; that verification has not happened yet.
func (p *Provisioner) ProvisionClickHouse(ctx context.Context, tenantID string) (Credentials, error) {
	if !tenantIdentifierPattern.MatchString(tenantID) {
		return Credentials{}, fmt.Errorf("tenantprovision: tenant id %q is not a safe ClickHouse identifier", tenantID)
	}

	database := tenantID
	username := "tenant_" + tenantID

	if err := p.admin.Exec(ctx, fmt.Sprintf("CREATE DATABASE IF NOT EXISTS `%s`", database)); err != nil {
		return Credentials{}, fmt.Errorf("tenantprovision: creating database: %w", err)
	}

	password, err := generatePassword()
	if err != nil {
		return Credentials{}, err
	}

	// No IF NOT EXISTS -- see this function's doc comment for why a
	// second call must fail, not silently succeed.
	if err := p.admin.Exec(ctx, fmt.Sprintf(
		"CREATE USER `%s` IDENTIFIED WITH plaintext_password BY '%s'",
		username, escapeSingleQuotes(password),
	)); err != nil {
		return Credentials{}, fmt.Errorf("tenantprovision: creating user (already provisioned? this call is not safe to retry): %w", err)
	}

	if err := p.admin.Exec(ctx, fmt.Sprintf("GRANT SELECT ON `%s`.* TO `%s`", database, username)); err != nil {
		return Credentials{}, fmt.Errorf("tenantprovision: granting select: %w", err)
	}

	return Credentials{Username: username, Password: password}, nil
}

func generatePassword() (string, error) {
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("tenantprovision: generating password: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// escapeSingleQuotes guards against a generated password (base64
// RawURLEncoding, so alphanumeric plus '-'/'_' only, never a literal
// quote) accidentally breaking out of the SQL string literal -- belt
// and suspenders given generatePassword's actual alphabet can't produce
// one, since this function's output gets interpolated directly into DDL
// (see tenantIdentifierPattern's doc comment on why: ClickHouse's driver
// has no parameterized identifiers/literals for DDL).
func escapeSingleQuotes(s string) string {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		if s[i] == '\'' {
			out = append(out, '\'', '\'')
			continue
		}
		out = append(out, s[i])
	}
	return string(out)
}
