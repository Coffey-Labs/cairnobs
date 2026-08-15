// Command enterprise-auth is Sentry's SSO/tenant-provisioning/RBAC
// service (commercial license, not AGPL) -- see
// /docs/phase-4-isolation-design.md and /docs/phase-4-rbac-design.md.
//
// Wires session issuance/validation (internal/session), the
// POST /internal/authorize endpoint api/authz.HTTPAuthorizer calls (the
// piece that turns on RBAC enforcement in /api), and --
// internal/loginhandler -- the real GET /auth/{oidc,saml}/login and
// GET /auth/oidc/callback + POST /auth/saml/acs handlers that issue a
// *human* session after an actual IdP round trip, resolving tenant/role
// via internal/rbacstore. Both protocols are now fully wired -- OIDC via
// discovery, SAML via fetching+parsing SAML_IDP_METADATA_URL at startup
// (crewjam/saml's samlsp.FetchMetadata; a trusted operator-supplied URL,
// same trust level as OIDC_ISSUER_URL's discovery fetch, not
// end-user-controlled input). Also fully wired: -mint-service-token
// (the RoleService credential /alerting presents), -create-tenant/
// -grant-membership-* -- the operator actions that replace
// phase-4-runbook.md's old "log in once so UpsertUserBySSO creates a
// users row, then hand-write a psql INSERT into tenant_memberships"
// bootstrap dance with a real command -- and their counterparts
// -revoke-membership-*/-list-memberships-tenant.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/crewjam/saml/samlsp"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sentry/sentry/api/httpserver"
	"github.com/sentry/sentry/enterprise/internal/authhandler"
	"github.com/sentry/sentry/enterprise/internal/config"
	"github.com/sentry/sentry/enterprise/internal/loginhandler"
	"github.com/sentry/sentry/enterprise/internal/oidc"
	"github.com/sentry/sentry/enterprise/internal/rbacstore"
	samlpkg "github.com/sentry/sentry/enterprise/internal/saml"
	"github.com/sentry/sentry/enterprise/internal/session"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	cfg, err := config.Load()
	if err != nil {
		logger.Error("loading config", "error", err)
		os.Exit(1)
	}

	// -mint-service-token issues a RoleService credential and prints it
	// to stdout, then exits -- an operator bootstrap step (run once,
	// paste the output into /alerting's API_SERVICE_TOKEN), not an HTTP
	// endpoint. Minting a service token has no session/cookie to check
	// like a human login flow would, so this is deliberately an offline
	// operator action gated by access to enterprise-auth's own
	// environment/secrets, not a network-reachable endpoint.
	mintServiceToken := flag.String("mint-service-token", "", "mint a RoleService credential for the named caller (e.g. \"alerting\") and exit")
	// -create-tenant/-grant-membership-* are offline operator actions,
	// same "not a network-reachable endpoint" shape as
	// -mint-service-token and enterprise-api's -provision-tenant --
	// deliberately not an authenticated HTTP admin API, which would
	// have to solve "who's allowed to create the very first tenant/
	// membership" (a real bootstrap problem an offline flag sidesteps
	// entirely: access to enterprise-auth's own environment/secrets is
	// the trust boundary, same as every other operator flag here).
	createTenant := flag.String("create-tenant", "", "create a tenant row (id) in 'provisioning' status and exit -- pair with -display-name; use enterprise-api -provision-tenant separately for ClickHouse/Tantivy provisioning")
	createTenantDisplayName := flag.String("display-name", "", "display name for -create-tenant (defaults to the tenant id if unset)")
	grantTenant := flag.String("grant-membership-tenant", "", "tenant id to grant a membership in -- all three -grant-membership-* flags are required together")
	grantUserEmail := flag.String("grant-membership-user-email", "", "email of an existing user to grant a tenant_memberships row to -- the user must have attempted an SSO login at least once already (UpsertUserBySSO creates the users row on first login, even one that then fails with \"no tenant membership\")")
	grantRole := flag.String("grant-membership-role", "", "role to grant: viewer, editor, admin, or owner")
	revokeTenant := flag.String("revoke-membership-tenant", "", "tenant id to revoke a membership from -- both -revoke-membership-* flags are required together")
	revokeUserEmail := flag.String("revoke-membership-user-email", "", "email of the user whose tenant_memberships row to delete")
	listMembershipsTenant := flag.String("list-memberships-tenant", "", "print every user with a membership in this tenant (id, email, display name, role) and exit")
	transferOwnerTenant := flag.String("transfer-owner-tenant", "", "tenant id to transfer ownership in -- both -transfer-owner-* flags are required together")
	transferOwnerUserEmail := flag.String("transfer-owner-user-email", "", "email of the existing member to make the new Owner -- the current owner is downgraded to admin, not removed; see rbacstore.TransferOwner")
	createIngestCredentialTenant := flag.String("create-ingest-credential-tenant", "", "mint a new ingest bearer token for this tenant, print it once, and exit -- see ingest/internal/grpcserver.TenantResolver")
	listIngestCredentialsTenant := flag.String("list-ingest-credentials-tenant", "", "print every ingest credential's id/created_at for this tenant (never the token itself -- only its hash is stored) and exit")
	revokeIngestCredential := flag.String("revoke-ingest-credential", "", "delete an ingest credential by id (see -list-ingest-credentials-tenant) and exit")
	// -healthcheck: same self-check mode as api/-healthcheck (see that
	// binary's doc comment) -- enterprise-auth's image is distroless too.
	healthcheck := flag.Bool("healthcheck", false, "self-check mode for Docker's HEALTHCHECK")
	flag.Parse()

	if *healthcheck {
		os.Exit(runHealthcheck(cfg.HTTPListenAddr))
	}

	sessionManager, err := session.NewManager(cfg.SessionSigningKey)
	if err != nil {
		logger.Error("constructing session manager", "error", err)
		os.Exit(1)
	}

	if *mintServiceToken != "" {
		token, err := sessionManager.IssueServiceToken(*mintServiceToken)
		if err != nil {
			logger.Error("minting service token", "error", err)
			os.Exit(1)
		}
		fmt.Println(token)
		return
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	pgDSN := fmt.Sprintf("postgres://%s:%s@%s/%s", cfg.Postgres.Username, cfg.Postgres.Password, cfg.Postgres.Addr, cfg.Postgres.Database)
	pgPool, err := pgxpool.New(ctx, pgDSN)
	if err != nil {
		logger.Error("opening postgres pool", "error", err)
		os.Exit(1)
	}
	defer pgPool.Close()
	if err := pgPool.Ping(ctx); err != nil {
		logger.Error("pinging postgres", "error", err)
		os.Exit(1)
	}
	rbac := rbacstore.NewStore(pgPool)

	if *createTenant != "" {
		os.Exit(runCreateTenant(ctx, logger, rbac, *createTenant, *createTenantDisplayName))
	}
	if *grantTenant != "" || *grantUserEmail != "" || *grantRole != "" {
		os.Exit(runGrantMembership(ctx, logger, rbac, *grantTenant, *grantUserEmail, *grantRole))
	}
	if *revokeTenant != "" || *revokeUserEmail != "" {
		os.Exit(runRevokeMembership(ctx, logger, rbac, *revokeTenant, *revokeUserEmail))
	}
	if *listMembershipsTenant != "" {
		os.Exit(runListMemberships(ctx, logger, rbac, *listMembershipsTenant))
	}
	if *transferOwnerTenant != "" || *transferOwnerUserEmail != "" {
		os.Exit(runTransferOwner(ctx, logger, rbac, *transferOwnerTenant, *transferOwnerUserEmail))
	}
	if *createIngestCredentialTenant != "" {
		os.Exit(runCreateIngestCredential(ctx, logger, rbac, *createIngestCredentialTenant))
	}
	if *listIngestCredentialsTenant != "" {
		os.Exit(runListIngestCredentials(ctx, logger, rbac, *listIngestCredentialsTenant))
	}
	if *revokeIngestCredential != "" {
		os.Exit(runRevokeIngestCredential(ctx, logger, rbac, *revokeIngestCredential))
	}

	// oidcProvider stays nil (loginhandler.RegisterRoutes then registers
	// nothing) unless OIDC is actually configured -- matches every other
	// optional-config path in this codebase.
	var oidcProvider *oidc.Provider
	if cfg.OIDC.IssuerURL != "" {
		oidcProvider, err = oidc.New(ctx, oidc.Config{
			IssuerURL: cfg.OIDC.IssuerURL, ClientID: cfg.OIDC.ClientID,
			ClientSecret: cfg.OIDC.ClientSecret, RedirectURL: cfg.OIDC.RedirectURL,
			Scopes: []string{"email", "profile"},
		})
		if err != nil {
			logger.Error("discovering OIDC issuer", "error", err)
			os.Exit(1)
		}
		logger.Info("OIDC provider configured", "issuer", cfg.OIDC.IssuerURL)
	} else {
		logger.Info("OIDC not configured (OIDC_ISSUER_URL unset) -- skipping discovery, /auth/oidc/* routes disabled")
	}

	// samlProvider stays nil (loginhandler.RegisterRoutes then registers
	// nothing) unless SAML is actually configured -- same shape as OIDC
	// above.
	var samlProvider *samlpkg.ServiceProvider
	if cfg.SAML.IDPMetadataURL != "" {
		metadataURL, err := url.Parse(cfg.SAML.IDPMetadataURL)
		if err != nil {
			logger.Error("parsing SAML_IDP_METADATA_URL", "error", err)
			os.Exit(1)
		}
		idpMetadata, err := samlsp.FetchMetadata(ctx, http.DefaultClient, *metadataURL)
		if err != nil {
			logger.Error("fetching SAML IdP metadata", "error", err)
			os.Exit(1)
		}
		samlProvider, err = samlpkg.New(samlpkg.Config{
			EntityID: cfg.SAML.EntityID, ACSURL: cfg.SAML.ACSURL, IDPMetadata: idpMetadata,
		})
		if err != nil {
			logger.Error("constructing SAML service provider", "error", err)
			os.Exit(1)
		}
		logger.Info("SAML provider configured", "idp_metadata_url", cfg.SAML.IDPMetadataURL)
	} else {
		logger.Info("SAML not configured (SAML_IDP_METADATA_URL unset) -- /auth/saml/* routes disabled")
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	features := authhandler.Features{
		OIDCEnabled: cfg.OIDC.IssuerURL != "",
		SAMLEnabled: cfg.SAML.IDPMetadataURL != "",
	}
	authhandler.New(logger, sessionManager, features, rbac).RegisterRoutes(mux)
	loginhandler.New(logger, oidcProvider, samlProvider, sessionManager, rbac, cfg.PostLoginRedirectURL, cfg.SelectTenantRedirectURL).RegisterRoutes(mux)

	// Credentialed, not plain, CORS: GET /auth/memberships and POST
	// /auth/select-tenant are called from web's tenant-picker page via
	// `fetch(..., {credentials: 'include'})` so the pending-login/session
	// cookies actually go along -- see httpserver.WithCredentialedCORS's
	// doc comment for why that rules out the wildcard origin every other
	// service's WithCORS defaults to.
	handler := httpserver.WithCredentialedCORS(mux, cfg.CORSAllowedOrigin)
	srv := &http.Server{Addr: cfg.HTTPListenAddr, Handler: handler}

	errCh := make(chan error, 1)
	go func() {
		logger.Info("enterprise-auth listening", "addr", cfg.HTTPListenAddr)
		errCh <- srv.ListenAndServe()
	}()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			logger.Error("graceful shutdown failed", "error", err)
		}
	case err := <-errCh:
		if err != nil && err != http.ErrServerClosed {
			logger.Error("server exited with error", "error", err)
			os.Exit(1)
		}
	}
}

// runCreateTenant creates a tenant row in rbacstore, refusing if one
// already exists (idempotency-refusal, same reasoning as
// enterprise-api's runProvisionTenant refusing an already-active
// tenant: a second run for the same id is far more likely to be an
// operator mistake than an intentional retry, and CreateTenant's
// generated columns -- id is the only stable identity here, there's no
// credential to accidentally rotate -- so this guards against duplicate
// setup steps, not a security property). Only touches rbacstore -- data-
// plane provisioning (ClickHouse/Tantivy) is enterprise-api
// -provision-tenant's separate job, per
// /docs/phase-4-isolation-design.md's ordered provisioning gate; running
// this alone leaves the tenant able to log users in but not yet able to
// serve their queries, same "two separate operator actions" reality
// enterprise/README.md already discloses.
func runCreateTenant(ctx context.Context, logger *slog.Logger, rbac *rbacstore.Store, tenantID, displayName string) int {
	if _, err := rbac.GetTenant(ctx, tenantID); err == nil {
		logger.Error("tenant already exists -- refusing to create it again", "tenant_id", tenantID)
		return 1
	} else if err != rbacstore.ErrNotFound {
		logger.Error("checking for existing tenant", "error", err)
		return 1
	}
	name := displayName
	if name == "" {
		name = tenantID
	}
	if _, err := rbac.CreateTenant(ctx, tenantID, name); err != nil {
		logger.Error("creating tenant", "error", err)
		return 1
	}
	logger.Info("created tenant", "tenant_id", tenantID, "display_name", name, "status", "provisioning")
	return 0
}

// runGrantMembership replaces phase-4-runbook.md's old manual-SQL
// bootstrap: an operator who knows a user's email (the user must have
// attempted an SSO login at least once already, so UpsertUserBySSO's
// created the users row -- this flag never creates a user itself, since
// that identity has to come from a real IdP round trip, not an operator
// guess) can grant them a tenant_memberships row without touching SQL
// directly. role="owner" also calls SetOwner, since a tenant's Owner is
// a tenant-level column, not just the highest tenant_memberships role
// (see SetOwner's doc comment) -- a real Owner assignment via this flag
// needs both to agree, same as any other owner-assignment call site.
func runGrantMembership(ctx context.Context, logger *slog.Logger, rbac *rbacstore.Store, tenantID, userEmail, role string) int {
	if tenantID == "" || userEmail == "" || role == "" {
		logger.Error("-grant-membership-tenant, -grant-membership-user-email, and -grant-membership-role are all required together")
		return 1
	}
	rbacRole := rbacstore.Role(role)
	switch rbacRole {
	case rbacstore.RoleViewer, rbacstore.RoleEditor, rbacstore.RoleAdmin, rbacstore.RoleOwner:
	default:
		logger.Error("invalid -grant-membership-role -- must be viewer, editor, admin, or owner", "role", role)
		return 1
	}

	tenant, err := rbac.GetTenant(ctx, tenantID)
	if err != nil {
		logger.Error("looking up tenant", "tenant_id", tenantID, "error", err)
		return 1
	}
	user, err := rbac.GetUserByEmail(ctx, userEmail)
	if err != nil {
		if err == rbacstore.ErrNotFound {
			logger.Error("no user with this email exists yet -- they must attempt an SSO login at least once first (it will fail with \"no tenant membership\", but UpsertUserBySSO creates the users row before that check runs)", "email", userEmail)
		} else {
			logger.Error("looking up user by email", "email", userEmail, "error", err)
		}
		return 1
	}
	// role="owner" is only correct for a tenant's *first* owner
	// assignment (SetMembership+SetOwner below, mirroring
	// TransferOwner's doc comment on when each is appropriate) -- a
	// second use of this flag while a *different* owner already exists
	// would leave that owner's membership row stale at "owner" while
	// tenants.owner_user_id points elsewhere, exactly the inconsistency
	// TransferOwner exists to avoid. Refuse and point at the right flag
	// instead of silently producing that state.
	if rbacRole == rbacstore.RoleOwner && tenant.OwnerUserID != "" && tenant.OwnerUserID != user.ID {
		logger.Error("tenant already has a different owner -- use -transfer-owner-tenant/-transfer-owner-user-email instead, which also downgrades the current owner", "tenant_id", tenantID, "current_owner_user_id", tenant.OwnerUserID)
		return 1
	}

	if err := rbac.SetMembership(ctx, tenantID, user.ID, rbacRole); err != nil {
		logger.Error("setting membership", "error", err)
		return 1
	}
	if rbacRole == rbacstore.RoleOwner {
		if err := rbac.SetOwner(ctx, tenantID, user.ID); err != nil {
			logger.Error("setting tenant owner", "error", err)
			return 1
		}
	}
	logger.Info("granted membership", "tenant_id", tenantID, "user_id", user.ID, "email", userEmail, "role", role)
	return 0
}

// runRevokeMembership is -grant-membership's inverse -- looks the user
// up by email (same reasoning as runGrantMembership: an operator knows
// an email, not a generated UUID) and deletes their tenant_memberships
// row. RevokeMembership itself refuses a tenant's current Owner (see
// that method's doc comment); this function doesn't duplicate that
// check, it just surfaces whatever error comes back.
func runRevokeMembership(ctx context.Context, logger *slog.Logger, rbac *rbacstore.Store, tenantID, userEmail string) int {
	if tenantID == "" || userEmail == "" {
		logger.Error("-revoke-membership-tenant and -revoke-membership-user-email are both required together")
		return 1
	}
	user, err := rbac.GetUserByEmail(ctx, userEmail)
	if err != nil {
		if err == rbacstore.ErrNotFound {
			logger.Error("no user with this email exists", "email", userEmail)
		} else {
			logger.Error("looking up user by email", "email", userEmail, "error", err)
		}
		return 1
	}
	if err := rbac.RevokeMembership(ctx, tenantID, user.ID); err != nil {
		if err == rbacstore.ErrNotFound {
			logger.Error("user has no membership in this tenant", "tenant_id", tenantID, "email", userEmail)
		} else {
			logger.Error("revoking membership", "error", err)
		}
		return 1
	}
	logger.Info("revoked membership", "tenant_id", tenantID, "user_id", user.ID, "email", userEmail)
	return 0
}

// runListMemberships prints every member of a tenant to stdout (plain
// text, not JSON -- an operator convenience for deciding who to
// -grant-membership-role= or -revoke-membership-*, not a machine-
// readable API; enterprise-auth has no admin HTTP surface for this at
// all, per this binary's doc comment on why that's deliberate).
func runListMemberships(ctx context.Context, logger *slog.Logger, rbac *rbacstore.Store, tenantID string) int {
	if _, err := rbac.GetTenant(ctx, tenantID); err != nil {
		logger.Error("looking up tenant", "tenant_id", tenantID, "error", err)
		return 1
	}
	members, err := rbac.ListMembershipsForTenant(ctx, tenantID)
	if err != nil {
		logger.Error("listing memberships", "error", err)
		return 1
	}
	if len(members) == 0 {
		fmt.Println("(no members)")
		return 0
	}
	for _, m := range members {
		fmt.Printf("%s\t%s\t%s\t%s\n", m.UserID, m.Email, m.DisplayName, m.Role)
	}
	return 0
}

// runTransferOwner is the operator-facing side of the RBAC matrix's
// "Transfer tenant Owner -- Owner only" row -- rbacstore.TransferOwner
// does the actual atomic work (downgrade current owner, promote new
// owner, update tenants.owner_user_id); this just resolves the email to
// a user the same way runGrantMembership/runRevokeMembership do.
func runTransferOwner(ctx context.Context, logger *slog.Logger, rbac *rbacstore.Store, tenantID, userEmail string) int {
	if tenantID == "" || userEmail == "" {
		logger.Error("-transfer-owner-tenant and -transfer-owner-user-email are both required together")
		return 1
	}
	user, err := rbac.GetUserByEmail(ctx, userEmail)
	if err != nil {
		if err == rbacstore.ErrNotFound {
			logger.Error("no user with this email exists -- they must attempt an SSO login at least once first", "email", userEmail)
		} else {
			logger.Error("looking up user by email", "email", userEmail, "error", err)
		}
		return 1
	}
	if err := rbac.TransferOwner(ctx, tenantID, user.ID, rbacstore.RoleAdmin); err != nil {
		logger.Error("transferring tenant ownership", "error", err)
		return 1
	}
	logger.Info("transferred tenant ownership", "tenant_id", tenantID, "new_owner_user_id", user.ID, "email", userEmail, "previous_owner_downgraded_to", rbacstore.RoleAdmin)
	return 0
}

// runCreateIngestCredential mints a new ingest bearer token for a
// tenant and prints it to stdout exactly once -- rbacstore only ever
// stores its hash (see ingest_credentials's doc comment), so this
// output is the only chance to capture the plaintext. An agent presents
// it as an `Authorization: Bearer <token>` gRPC metadata entry on every
// PushBatch call; ingest resolves it to a tenant via
// POST /internal/authorize-ingest.
func runCreateIngestCredential(ctx context.Context, logger *slog.Logger, rbac *rbacstore.Store, tenantID string) int {
	if _, err := rbac.GetTenant(ctx, tenantID); err != nil {
		logger.Error("looking up tenant", "tenant_id", tenantID, "error", err)
		return 1
	}
	token, err := rbac.CreateIngestCredential(ctx, tenantID)
	if err != nil {
		logger.Error("creating ingest credential", "error", err)
		return 1
	}
	fmt.Println(token)
	return 0
}

func runListIngestCredentials(ctx context.Context, logger *slog.Logger, rbac *rbacstore.Store, tenantID string) int {
	if _, err := rbac.GetTenant(ctx, tenantID); err != nil {
		logger.Error("looking up tenant", "tenant_id", tenantID, "error", err)
		return 1
	}
	creds, err := rbac.ListIngestCredentialsForTenant(ctx, tenantID)
	if err != nil {
		logger.Error("listing ingest credentials", "error", err)
		return 1
	}
	if len(creds) == 0 {
		fmt.Println("(no ingest credentials)")
		return 0
	}
	for _, c := range creds {
		fmt.Printf("%s\t%s\n", c.ID, c.CreatedAt.Format(time.RFC3339))
	}
	return 0
}

func runRevokeIngestCredential(ctx context.Context, logger *slog.Logger, rbac *rbacstore.Store, id string) int {
	if err := rbac.RevokeIngestCredential(ctx, id); err != nil {
		if err == rbacstore.ErrNotFound {
			logger.Error("no ingest credential with this id", "id", id)
		} else {
			logger.Error("revoking ingest credential", "error", err)
		}
		return 1
	}
	logger.Info("revoked ingest credential", "id", id)
	return 0
}

// runHealthcheck mirrors api/cmd/api/main.go's runHealthcheck exactly --
// see that function's doc comment for why this execs the binary against
// itself rather than using an external tool.
func runHealthcheck(listenAddr string) int {
	addr := listenAddr
	if strings.HasPrefix(addr, ":") {
		addr = "localhost" + addr
	}
	client := http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get("http://" + addr + "/healthz")
	if err != nil {
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 1
	}
	return 0
}
