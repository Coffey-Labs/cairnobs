package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// roleCookie signs someone in with the role, as a sign-in provider would
// (through dev sign-in, so no provider is asked again).
func roleCookie(t *testing.T, srv *Server, role string) *http.Cookie {
	t.Helper()
	rec := httptest.NewRecorder()
	srv.sessions.put(rec, httptest.NewRequest("GET", "/", nil), session{User: role + "-person", Role: role, Dev: true}) // dev: no provider to re-ask
	return rec.Result().Cookies()[0]
}

// Admins change things; operators see everything and handle alerts;
// viewers see what's watched.
func TestRolesDecideWhatsAllowed(t *testing.T) {
	srv := stateServer(t, t.TempDir(), true)
	h := srv.Handler()
	admin, operator, viewer := roleCookie(t, srv, RoleAdmin), roleCookie(t, srv, RoleOperator), roleCookie(t, srv, RoleViewer)

	for _, tc := range []struct {
		method, path, body      string
		admin, operator, viewer int
	}{
		{"GET", "/api/v1/overview", "", 200, 200, 200},
		{"GET", "/api/v1/settings", "", 200, 200, 403},
		{"GET", "/api/v1/settings/checks", "", 200, 200, 403},
		{"GET", "/api/v1/settings/channels", "", 200, 200, 403},
		{"GET", "/api/v1/settings/checks/export", "", 200, 200, 403},
		{"GET", "/api/v1/agents", "", 200, 200, 403},
		{"GET", "/api/v1/audit", "", 200, 200, 403},
		// Past the role check, an unknown incident is simply not found.
		{"POST", "/api/v1/incidents/nope/ack", "", 404, 404, 403},
		{"PUT", "/api/v1/settings/checks/site", `{"name":"Site","kind":"http","target":"https://example.org/"}`, 200, 403, 403},
		{"POST", "/api/v1/agents/join", `{"name":"web9"}`, 0, 403, 403},
		{"POST", "/api/v1/alerts/test", "", 200, 403, 403},
	} {
		for _, who := range []struct {
			c    *http.Cookie
			want int
			role string
		}{{admin, tc.admin, "admin"}, {operator, tc.operator, "operator"}, {viewer, tc.viewer, "viewer"}} {
			if who.want == 0 {
				continue // depends on setup this test doesn't make
			}
			if rec := sendCookie(h, tc.method, tc.path, who.c, tc.body); rec.Code != who.want {
				t.Errorf("%s %s as %s: %d, want %d (%s)", tc.method, tc.path, who.role, rec.Code, who.want, rec.Body)
			}
		}
	}

	rows := []ContainerRow{{}}
	rows[0].Logs = []string{"a line"}
	for role, want := range map[string]bool{RoleAdmin: true, RoleOperator: true, RoleViewer: false} {
		req := httptest.NewRequest("GET", "/", nil)
		req.AddCookie(roleCookie(t, srv, role))
		if got := srv.logsFor(req, rows)[0].Logs != nil; got != want {
			t.Errorf("%s sees log lines: %v", role, got)
		}
	}
}
