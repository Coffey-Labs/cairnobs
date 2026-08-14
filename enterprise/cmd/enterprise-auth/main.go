// Command enterprise-auth is Sentry's SSO/tenant-provisioning/RBAC
// service (commercial license, not AGPL) -- see
// /docs/phase-4-isolation-design.md and /docs/phase-4-rbac-design.md.
//
// Wires session issuance/validation (internal/session), the
// POST /internal/authorize endpoint api/authz.HTTPAuthorizer calls (the
// piece that turns on RBAC enforcement in /api), and -- since
// internal/loginhandler -- the real GET /auth/oidc/login and
// GET /auth/oidc/callback handlers that issue a *human* session after
// an actual IdP round trip, resolving tenant/role via internal/rbacstore.
// Still deliberately missing: SAML's equivalent (ACS endpoint) -- same
// shape, not yet built, following internal/loginhandler's OIDC pattern
// once it is. What's fully wired: -mint-service-token (the RoleService
// credential /alerting presents) and, when OIDC_ISSUER_URL is
// configured, a real human login flow.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sentry/sentry/enterprise/internal/authhandler"
	"github.com/sentry/sentry/enterprise/internal/config"
	"github.com/sentry/sentry/enterprise/internal/loginhandler"
	"github.com/sentry/sentry/enterprise/internal/oidc"
	"github.com/sentry/sentry/enterprise/internal/rbacstore"
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

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	features := authhandler.Features{
		OIDCEnabled: cfg.OIDC.IssuerURL != "",
		SAMLEnabled: cfg.SAML.IDPMetadataURL != "",
	}
	authhandler.New(logger, sessionManager, features).RegisterRoutes(mux)
	loginhandler.New(logger, oidcProvider, sessionManager, rbac, cfg.PostLoginRedirectURL).RegisterRoutes(mux)

	srv := &http.Server{Addr: cfg.HTTPListenAddr, Handler: mux}

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
