package main

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/cairnobs/cairnobs/api/authz"
	"github.com/cairnobs/cairnobs/api/localauth"
)

type fakeSeedStore struct {
	usernames map[string]bool
	created   []createdUser
}

type createdUser struct {
	username string
	role     authz.Role
}

func newFakeSeedStore(existing ...string) *fakeSeedStore {
	f := &fakeSeedStore{usernames: map[string]bool{}}
	for _, u := range existing {
		f.usernames[u] = true
	}
	return f
}

func (f *fakeSeedStore) UsernameExists(_ context.Context, username string) (bool, error) {
	return f.usernames[username], nil
}

func (f *fakeSeedStore) CreateUser(_ context.Context, username, _ string, role authz.Role) (*localauth.User, error) {
	f.usernames[username] = true
	f.created = append(f.created, createdUser{username: username, role: role})
	return &localauth.User{ID: "id-" + username, Username: username, Role: role}, nil
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestSeedAdminCreatesTheAdminOnAFreshDeployment(t *testing.T) {
	fs := newFakeSeedStore()
	var out bytes.Buffer

	if code := runSeedAdmin(context.Background(), discardLogger(), &out, fs); code != 0 {
		t.Fatalf("runSeedAdmin: exit code = %d, want 0", code)
	}
	if len(fs.created) != 1 || fs.created[0].username != seedAdminUsername {
		t.Fatalf("created = %+v, want one %q", fs.created, seedAdminUsername)
	}
	if fs.created[0].role != authz.RoleOwner {
		t.Fatalf("created role = %q, want %q", fs.created[0].role, authz.RoleOwner)
	}
	if !strings.Contains(out.String(), "password:") {
		t.Fatalf("output does not print the generated password: %q", out.String())
	}
}

func TestSeedAdminSkipsWhenTheAdminAlreadyExists(t *testing.T) {
	fs := newFakeSeedStore(seedAdminUsername)
	var out bytes.Buffer

	if code := runSeedAdmin(context.Background(), discardLogger(), &out, fs); code != 0 {
		t.Fatalf("runSeedAdmin: exit code = %d, want 0", code)
	}
	if len(fs.created) != 0 {
		t.Fatalf("created = %+v, want none -- a second admin must never be minted", fs.created)
	}
	if !strings.Contains(out.String(), "skipping") {
		t.Fatalf("output does not say it skipped: %q", out.String())
	}
}

// The recovery case this command exists for. It used to refuse here,
// because it asked whether the deployment had *any* local user rather
// than whether the admin account it creates was missing -- so an
// operator whose administrator account was gone, but whose deployment
// still held other accounts, was told "already provisioned" and left
// with no supported way back in.
func TestSeedAdminStillSeedsWhenOtherUsersExist(t *testing.T) {
	fs := newFakeSeedStore("someone-else")
	var out bytes.Buffer

	if code := runSeedAdmin(context.Background(), discardLogger(), &out, fs); code != 0 {
		t.Fatalf("runSeedAdmin: exit code = %d, want 0", code)
	}
	if len(fs.created) != 1 || fs.created[0].username != seedAdminUsername {
		t.Fatalf("created = %+v, want one %q", fs.created, seedAdminUsername)
	}
}
