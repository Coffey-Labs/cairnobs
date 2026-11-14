package state

import (
	"database/sql"
	"errors"
	"time"
)

// The local directory: accounts Cairn OBS keeps itself, signed
// in with passkeys, or with a password and a TOTP code together where
// passkeys can't work. A passkey is a public key, setup links are stored as
// their SHA-256, passwords as Argon2id hashes; TOTP secrets are sealed with
// the state key.

const usersSchema = `
CREATE TABLE IF NOT EXISTS users (
	id TEXT PRIMARY KEY, name TEXT NOT NULL, email TEXT NOT NULL, role TEXT NOT NULL,
	created INTEGER NOT NULL, disabled INTEGER NOT NULL DEFAULT 0, last_sign_in INTEGER,
	emergency INTEGER NOT NULL DEFAULT 0
);
CREATE UNIQUE INDEX IF NOT EXISTS users_email ON users (email);
CREATE TABLE IF NOT EXISTS passkeys (
	id TEXT PRIMARY KEY, user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	name TEXT NOT NULL, credential TEXT NOT NULL, created INTEGER NOT NULL, last_used INTEGER
);
CREATE INDEX IF NOT EXISTS passkeys_user ON passkeys (user_id);
CREATE TABLE IF NOT EXISTS setup_links (
	token_sha256 TEXT PRIMARY KEY, user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	expires INTEGER NOT NULL
);
`

// User is a local account.
type User struct {
	ID         string    `json:"id"` // random; also the passkeys' user handle
	Name       string    `json:"name"`
	Email      string    `json:"email"`
	Role       string    `json:"role"` // admin, operator or viewer
	Created    time.Time `json:"created"`
	Disabled   bool      `json:"disabled"`
	LastSignIn time.Time `json:"last_sign_in,omitzero"`
	Emergency  bool      `json:"emergency"` // signs in only through the emergency door
	Password   bool      `json:"password"`  // has a password and TOTP (read only: SetPassword sets it)
}

// Passkey is one registered credential. Credential is the WebAuthn
// library's record, as JSON.
type Passkey struct {
	ID         string    `json:"id"` // the credential id, base64url
	UserID     string    `json:"user_id"`
	Name       string    `json:"name"`
	Credential string    `json:"-"`
	Created    time.Time `json:"created"`
	LastUsed   time.Time `json:"last_used,omitzero"`
}

// ErrNotFound is a lookup that found nothing.
var ErrNotFound = errors.New("not found")

func scanUser(row interface{ Scan(...any) error }) (User, error) {
	var u User
	var created int64
	var last sql.NullInt64
	err := row.Scan(&u.ID, &u.Name, &u.Email, &u.Role, &created, &u.Disabled, &last, &u.Emergency, &u.Password)
	if errors.Is(err, sql.ErrNoRows) {
		return u, ErrNotFound
	}
	u.Created, u.LastSignIn = time.Unix(created, 0), fromUnix(last)
	return u, err
}

const userCols = `id, name, email, role, created, disabled, last_sign_in, emergency`

// userSelect is userCols and whether the account has a password.
const userSelect = userCols + `, password IS NOT NULL`

// PutUser adds or updates an account. It never touches the password.
func (d *DB) PutUser(u User) error {
	_, err := d.exec(`INSERT INTO users (`+userCols+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (id) DO UPDATE SET name=excluded.name, email=excluded.email, role=excluded.role,
			disabled=excluded.disabled, last_sign_in=excluded.last_sign_in, emergency=excluded.emergency`,
		u.ID, u.Name, u.Email, u.Role, u.Created.Unix(), u.Disabled, unix(u.LastSignIn), u.Emergency)
	return err
}

// GetUser finds an account by id.
func (d *DB) GetUser(id string) (User, error) {
	return scanUser(d.queryRow(`SELECT `+userSelect+` FROM users WHERE id = ?`, id))
}

// UserByEmail finds an account by its email address (stored lower-case).
func (d *DB) UserByEmail(email string) (User, error) {
	return scanUser(d.queryRow(`SELECT `+userSelect+` FROM users WHERE email = ?`, email))
}

// UsableAdmins counts enabled admins who can sign in: with a passkey or a
// password.
func (d *DB) UsableAdmins() (int, error) {
	var n int
	err := d.queryRow(`SELECT count(*) FROM users u WHERE role = 'admin' AND disabled = 0
		AND (password IS NOT NULL OR EXISTS (SELECT 1 FROM passkeys p WHERE p.user_id = u.id))`).Scan(&n)
	return n, err
}

// PasswordLogin is what password sign-in checks: the Argon2id hash, the
// sealed TOTP secret and the last TOTP step used.
type PasswordLogin struct {
	Hash     string
	TOTP     string // sealed
	LastStep int64
}

// SetPassword gives an account a password and TOTP secret (sealed by the
// caller), replacing any before. step is the step of the code that proved
// the secret, so that code can't be used again.
func (d *DB) SetPassword(userID string, l PasswordLogin) error {
	_, err := d.exec(`UPDATE users SET password = ?, totp = ?, totp_step = ? WHERE id = ?`, l.Hash, l.TOTP, l.LastStep, userID)
	return err
}

// ClearPassword removes an account's password and TOTP.
func (d *DB) ClearPassword(userID string) error {
	_, err := d.exec(`UPDATE users SET password = NULL, totp = NULL, totp_step = 0 WHERE id = ?`, userID)
	return err
}

// Password returns an account's password login; ErrNotFound when it has none.
func (d *DB) Password(userID string) (PasswordLogin, error) {
	var l PasswordLogin
	var hash, totp sql.NullString
	err := d.queryRow(`SELECT password, totp, totp_step FROM users WHERE id = ?`, userID).Scan(&hash, &totp, &l.LastStep)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && !hash.Valid) {
		return l, ErrNotFound
	}
	l.Hash, l.TOTP = hash.String, totp.String
	return l, err
}

// UseTOTPStep records that a TOTP step was used. It fails (false) when that
// step or a later one already was, so two sign-ins racing with one code
// can't both get in.
func (d *DB) UseTOTPStep(userID string, step int64) (bool, error) {
	res, err := d.exec(`UPDATE users SET totp_step = ? WHERE id = ? AND totp_step < ?`, step, userID, step)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// Users lists every account, by name.
func (d *DB) Users() ([]User, error) {
	rows, err := d.query(`SELECT ` + userSelect + ` FROM users ORDER BY lower(name), name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []User{}
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// DeleteUser removes an account with its passkeys and setup links.
func (d *DB) DeleteUser(id string) error {
	_, err := d.exec(`DELETE FROM users WHERE id = ?`, id)
	return err
}

// PutPasskey adds or updates a passkey.
func (d *DB) PutPasskey(p Passkey) error {
	_, err := d.exec(`INSERT INTO passkeys (id, user_id, name, credential, created, last_used) VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT (id) DO UPDATE SET name=excluded.name, credential=excluded.credential, last_used=excluded.last_used`,
		p.ID, p.UserID, p.Name, p.Credential, p.Created.Unix(), unix(p.LastUsed))
	return err
}

// Passkeys lists a user's passkeys, oldest first.
func (d *DB) Passkeys(userID string) ([]Passkey, error) {
	rows, err := d.query(`SELECT id, user_id, name, credential, created, last_used FROM passkeys WHERE user_id = ? ORDER BY created`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Passkey{}
	for rows.Next() {
		var p Passkey
		var created int64
		var last sql.NullInt64
		if err := rows.Scan(&p.ID, &p.UserID, &p.Name, &p.Credential, &created, &last); err != nil {
			return nil, err
		}
		p.Created, p.LastUsed = time.Unix(created, 0), fromUnix(last)
		out = append(out, p)
	}
	return out, rows.Err()
}

// DeletePasskey removes one of a user's passkeys.
func (d *DB) DeletePasskey(userID, id string) error {
	res, err := d.exec(`DELETE FROM passkeys WHERE id = ? AND user_id = ?`, id, userID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// PutSetupLink stores a setup link (by its SHA-256) for a user; a user has
// at most one, so a new one replaces the old.
func (d *DB) PutSetupLink(tokenSHA256, userID string, expires time.Time) error {
	tx, err := d.begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM setup_links WHERE user_id = ?`, userID); err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO setup_links (token_sha256, user_id, expires) VALUES (?, ?, ?)`, tokenSHA256, userID, expires.Unix()); err != nil {
		return err
	}
	return tx.Commit()
}

// SetupLink returns the user a live setup link belongs to.
func (d *DB) SetupLink(tokenSHA256 string, now time.Time) (string, error) {
	var uid string
	err := d.queryRow(`SELECT user_id FROM setup_links WHERE token_sha256 = ? AND expires > ?`, tokenSHA256, now.Unix()).Scan(&uid)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return uid, err
}

// UseSetupLink deletes a setup link once it has done its job.
func (d *DB) UseSetupLink(tokenSHA256 string) error {
	_, err := d.exec(`DELETE FROM setup_links WHERE token_sha256 = ?`, tokenSHA256)
	return err
}

// PendingSetup says whether a user has a live setup link.
func (d *DB) PendingSetup(userID string, now time.Time) bool {
	var n int
	_ = d.queryRow(`SELECT count(*) FROM setup_links WHERE user_id = ? AND expires > ?`, userID, now.Unix()).Scan(&n)
	return n > 0
}
