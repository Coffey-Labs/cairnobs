package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCmdUsersMissingSubcommand(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := cmdUsers(nil, strings.NewReader(""), &stdout, &stderr)
	if code != 1 {
		t.Fatalf("code = %d, want 1", code)
	}
}

func TestCmdUsersLoginPrintsOnlyTheToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/auth/login" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		var body loginRequestBody
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.Username != "admin" || body.Password != "s3cret!!" {
			t.Errorf("unexpected credentials: %+v", body)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"token":"abc123","user_id":"u1","username":"admin","role":"owner"}`))
	}))
	defer srv.Close()

	var stdout, stderr bytes.Buffer
	code := cmdUsers([]string{"login", "admin", "--api", srv.URL}, strings.NewReader("s3cret!!\n"), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code = %d, want 0; stderr=%s", code, stderr.String())
	}
	if got := strings.TrimSpace(stdout.String()); got != "abc123" {
		t.Fatalf("stdout = %q, want exactly the raw token (pipeable into CAIRNOBSCTL_TOKEN)", got)
	}
}

func TestCmdUsersLoginFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":"invalid username or password"}`))
	}))
	defer srv.Close()

	var stdout, stderr bytes.Buffer
	code := cmdUsers([]string{"login", "admin", "--api", srv.URL}, strings.NewReader("wrong\n"), &stdout, &stderr)
	if code != 1 {
		t.Fatalf("code = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "invalid username or password") {
		t.Fatalf("stderr = %q, want it to surface the server's error message", stderr.String())
	}
	if stdout.String() != "" {
		t.Fatalf("stdout = %q, want empty on failure (nothing pipeable into CAIRNOBSCTL_TOKEN)", stdout.String())
	}
}

func TestCmdUsersCreateSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/auth/users" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		var body struct {
			Username string `json:"username"`
			Password string `json:"password"`
			Role     string `json:"role"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.Role != "viewer" {
			t.Errorf("role = %q, want viewer (from --role)", body.Role)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte(`{"id":"u2","username":"bob","role":"viewer"}`))
	}))
	defer srv.Close()

	var stdout, stderr bytes.Buffer
	code := cmdUsers([]string{"create", "bob", "--role", "viewer", "--api", srv.URL}, strings.NewReader("bobspassword\n"), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code = %d, want 0; stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "bob") {
		t.Fatalf("stdout = %q, want it to contain the created user", stdout.String())
	}
}

func TestCmdUsersCreateDefaultsRoleToEditor(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Role string `json:"role"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.Role != "editor" {
			t.Errorf("role = %q, want editor (the default when --role is omitted)", body.Role)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte(`{"id":"u2","username":"bob","role":"editor"}`))
	}))
	defer srv.Close()

	var stdout, stderr bytes.Buffer
	code := cmdUsers([]string{"create", "bob", "--api", srv.URL}, strings.NewReader("bobspassword\n"), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code = %d, want 0; stderr=%s", code, stderr.String())
	}
}

func TestCmdUsersDeleteMissingID(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := cmdUsers([]string{"delete"}, strings.NewReader(""), &stdout, &stderr)
	if code != 1 {
		t.Fatalf("code = %d, want 1", code)
	}
}

func TestCmdUsersDeleteSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete || r.URL.Path != "/auth/users/u2" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	var stdout, stderr bytes.Buffer
	code := cmdUsers([]string{"delete", "u2", "--api", srv.URL}, strings.NewReader(""), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code = %d, want 0; stderr=%s", code, stderr.String())
	}
}

func TestCmdUsersResetPasswordWithGeneratedPassword(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/auth/users/u2/reset-password" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		body, _ := io.ReadAll(r.Body)
		if strings.TrimSpace(string(body)) != "{}" {
			t.Errorf("body = %q, want {} (no --password-stdin supplied)", body)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"password":"generated-abc"}`))
	}))
	defer srv.Close()

	var stdout, stderr bytes.Buffer
	code := cmdUsers([]string{"reset-password", "u2", "--api", srv.URL}, strings.NewReader(""), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code = %d, want 0; stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "generated-abc") {
		t.Fatalf("stdout = %q, want it to contain the generated password", stdout.String())
	}
}

// TestCmdUsersResetPasswordWithStdinPassword is the regression test for
// the security-audit finding that this CLI accepted a plaintext
// --password <value> flag (visible via `ps`/shell history). Setting a
// specific password must go through --password-stdin plus piped input
// instead, never a bare argument.
func TestCmdUsersResetPasswordWithStdinPassword(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Password string `json:"password"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.Password != "a-specific-password" {
			t.Errorf("password = %q, want the value piped via stdin", body.Password)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	var stdout, stderr bytes.Buffer
	code := cmdUsers([]string{"reset-password", "u2", "--password-stdin", "--api", srv.URL}, strings.NewReader("a-specific-password\n"), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code = %d, want 0; stderr=%s", code, stderr.String())
	}
}
