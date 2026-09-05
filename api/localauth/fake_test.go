package localauth

import (
	"context"
	"strconv"
	"time"

	"github.com/cairnobs/cairnobs/api/authz"
)

// fakeStore implements both store (handler.go) and sessionStore
// (authorizer.go) -- a real *Store satisfies both too, this is just the
// in-memory test double, same "fake enforces the same invariants the
// real pgx-backed Store does" posture dashboards/handler_test.go's
// fakeStore documents.
type fakeStore struct {
	users      map[string]*User // by id
	hashes     map[string]string
	byUsername map[string]string // username -> id
	sessions   map[string]Session
	nextID     int
	createErr  error
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		users:      map[string]*User{},
		hashes:     map[string]string{},
		byUsername: map[string]string{},
		sessions:   map[string]Session{},
	}
}

func (f *fakeStore) CreateUser(_ context.Context, username, passwordHash string, role authz.Role) (*User, error) {
	if f.createErr != nil {
		return nil, f.createErr
	}
	if _, ok := f.byUsername[username]; ok {
		return nil, ErrUsernameTaken
	}
	f.nextID++
	id := "user-" + strconv.Itoa(f.nextID)
	// DisplayTimezone mirrors the schema's NOT NULL DEFAULT 'UTC' (see
	// migration 0042) -- a fake that left it empty would let a handler
	// bug that drops the default pass unnoticed.
	u := &User{ID: id, Username: username, Role: role, DisplayTimezone: "UTC", CreatedAt: time.Now()}
	f.users[id] = u
	f.hashes[id] = passwordHash
	f.byUsername[username] = id
	return u, nil
}

func (f *fakeStore) ListUsers(_ context.Context) ([]User, error) {
	var out []User
	for _, u := range f.users {
		out = append(out, *u)
	}
	return out, nil
}

func (f *fakeStore) GetUserForLogin(_ context.Context, username string) (*User, string, error) {
	id, ok := f.byUsername[username]
	if !ok {
		return nil, "", ErrNotFound
	}
	return f.users[id], f.hashes[id], nil
}

func (f *fakeStore) GetUserByID(_ context.Context, id string) (*User, error) {
	u, ok := f.users[id]
	if !ok {
		return nil, ErrNotFound
	}
	return u, nil
}

func (f *fakeStore) DeleteUser(_ context.Context, id string) error {
	u, ok := f.users[id]
	if !ok {
		return ErrNotFound
	}
	delete(f.users, id)
	delete(f.hashes, id)
	delete(f.byUsername, u.Username)
	for hash, sess := range f.sessions {
		if sess.UserID == id {
			delete(f.sessions, hash)
		}
	}
	return nil
}

func (f *fakeStore) SetPasswordHash(_ context.Context, userID, hash string) error {
	if _, ok := f.users[userID]; !ok {
		return ErrNotFound
	}
	f.hashes[userID] = hash
	for h, sess := range f.sessions {
		if sess.UserID == userID {
			delete(f.sessions, h)
		}
	}
	return nil
}

func (f *fakeStore) SetRole(_ context.Context, userID string, role authz.Role) error {
	u, ok := f.users[userID]
	if !ok {
		return ErrNotFound
	}
	u.Role = role
	for h, sess := range f.sessions {
		if sess.UserID == userID {
			delete(f.sessions, h)
		}
	}
	return nil
}

// SetDisplayTimezone deliberately does not touch f.sessions -- unlike
// SetRole/SetPasswordHash above, changing a rendering preference is not
// a reason to sign anyone out, and the test for that asserts it.
func (f *fakeStore) SetDisplayTimezone(_ context.Context, userID, tz string) error {
	u, ok := f.users[userID]
	if !ok {
		return ErrNotFound
	}
	u.DisplayTimezone = tz
	return nil
}

func (f *fakeStore) CountUsersWithRole(_ context.Context, role authz.Role) (int, error) {
	n := 0
	for _, u := range f.users {
		if u.Role == role {
			n++
		}
	}
	return n, nil
}

func (f *fakeStore) GetPasswordHashByID(_ context.Context, id string) (string, error) {
	if _, ok := f.users[id]; !ok {
		return "", ErrNotFound
	}
	return f.hashes[id], nil
}

func (f *fakeStore) CreateSession(_ context.Context, userID, tenantID string, role authz.Role, ttl time.Duration) (string, error) {
	raw, hash, err := newOpaqueToken()
	if err != nil {
		return "", err
	}
	f.sessions[hash] = Session{UserID: userID, TenantID: tenantID, Role: role, ExpiresAt: time.Now().Add(ttl)}
	return raw, nil
}

func (f *fakeStore) GetSession(_ context.Context, tokenHash string) (*Session, error) {
	sess, ok := f.sessions[tokenHash]
	if !ok || sess.ExpiresAt.Before(time.Now()) {
		return nil, ErrNotFound
	}
	return &sess, nil
}

func (f *fakeStore) DeleteSessionByHash(_ context.Context, tokenHash string) error {
	delete(f.sessions, tokenHash)
	return nil
}
