// Command enterprise-auth is Sentry's SSO/tenant-provisioning/RBAC
// service (commercial license, not AGPL) -- see
// /docs/phase-4-isolation-design.md and /docs/phase-4-rbac-design.md.
//
// Phase 4 task 5 adds session issuance/validation (internal/session) and
// the POST /internal/authorize endpoint api/internal/authz.HTTPAuthorizer
// calls -- the piece that actually turns on RBAC enforcement in /api.
// Still deliberately missing: the OIDC/SAML login/callback HTTP handlers
// that would issue a *human* session after a real IdP round trip, and
// internal/rbacstore (the org/tenant/user/role Postgres storage those
// handlers need to look up a role from). Both depend on RBAC storage
// that wasn't built in task 3's scope and are called out as deferred
// rather than half-built -- see the task 5 summary. What IS wired end to
// end: minting and validating the RoleService credential /alerting
// presents, via -mint-service-token below.
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

	"github.com/sentry/sentry/enterprise/internal/authhandler"
	"github.com/sentry/sentry/enterprise/internal/config"
	"github.com/sentry/sentry/enterprise/internal/oidc"
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

	if cfg.OIDC.IssuerURL != "" {
		if _, err := oidc.New(ctx, oidc.Config{
			IssuerURL: cfg.OIDC.IssuerURL, ClientID: cfg.OIDC.ClientID,
			ClientSecret: cfg.OIDC.ClientSecret, RedirectURL: cfg.OIDC.RedirectURL,
			Scopes: []string{"email", "profile"},
		}); err != nil {
			logger.Error("discovering OIDC issuer", "error", err)
			os.Exit(1)
		}
		logger.Info("OIDC provider configured", "issuer", cfg.OIDC.IssuerURL)
	} else {
		logger.Info("OIDC not configured (OIDC_ISSUER_URL unset) -- skipping discovery")
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
