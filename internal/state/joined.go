package state

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Joined agents and join tokens. An agent can be
// listed in obs-beacond.yaml (`agents`, with a token) or join with a
// one-time token made in the UI; joined agents live here. A join token is
// kept only as its SHA-256, is good once, and runs out.
const joinedSchema = `
CREATE TABLE IF NOT EXISTS agents (
	name TEXT PRIMARY KEY,
	workstation INTEGER NOT NULL DEFAULT 0,
	added INTEGER NOT NULL,
	added_by TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS join_tokens (
	token_sha256 TEXT PRIMARY KEY,
	agent TEXT NOT NULL,
	workstation INTEGER NOT NULL DEFAULT 0,
	created INTEGER NOT NULL,
	created_by TEXT NOT NULL,
	expires INTEGER NOT NULL,
	used INTEGER
);
`

// addClusterColumns (migration 10) marks joined agents and join tokens
// that are for a cluster agent (docs/cluster-watcher.md).
func addClusterColumns(t txn) error {
	for _, table := range []string{"agents", "join_tokens"} {
		if err := addColumnIfMissing(t, table, "is_cluster", "INTEGER NOT NULL DEFAULT 0"); err != nil {
			return err
		}
	}
	return nil
}

// JoinedAgent is an agent that joined with a token.
type JoinedAgent struct {
	Name        string
	Workstation bool
	Cluster     bool // a cluster agent (docs/cluster-watcher.md), not a host
	Added       time.Time
	AddedBy     string
}

// JoinToken is a join token as stored: never the token itself.
type JoinToken struct {
	TokenSHA256 string
	Agent       string // the name the agent gets
	Workstation bool
	Cluster     bool
	Created     time.Time
	CreatedBy   string
	Expires     time.Time
}

// ErrJoinToken is a join token that doesn't exist, was used, or ran out.
var ErrJoinToken = errors.New("unknown, used or expired join token")

// JoinedAgents lists the agents that joined, by name.
func (d *DB) JoinedAgents() ([]JoinedAgent, error) {
	rows, err := d.query(`SELECT name, workstation, is_cluster, added, added_by FROM agents ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []JoinedAgent{}
	for rows.Next() {
		var a JoinedAgent
		var added int64
		if err := rows.Scan(&a.Name, &a.Workstation, &a.Cluster, &added, &a.AddedBy); err != nil {
			return nil, err
		}
		a.Added = time.Unix(added, 0)
		out = append(out, a)
	}
	return out, rows.Err()
}

// AddJoinToken stores a new join token. The caller has checked the name is
// free; UseJoinToken checks again.
func (d *DB) AddJoinToken(t JoinToken) error {
	_, err := d.exec(`INSERT INTO join_tokens (token_sha256, agent, workstation, is_cluster, created, created_by, expires) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		t.TokenSHA256, t.Agent, t.Workstation, t.Cluster, t.Created.Unix(), t.CreatedBy, t.Expires.Unix())
	return err
}

// PendingJoinTokens lists the tokens not yet used and not run out, oldest
// first, and forgets those that ran out over a day ago.
func (d *DB) PendingJoinTokens(now time.Time) ([]JoinToken, error) {
	if _, err := d.exec(`DELETE FROM join_tokens WHERE expires < ?`, now.Add(-24*time.Hour).Unix()); err != nil {
		return nil, err
	}
	rows, err := d.query(`SELECT token_sha256, agent, workstation, is_cluster, created, created_by, expires FROM join_tokens
		WHERE used IS NULL AND expires > ? ORDER BY created`, now.Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []JoinToken{}
	for rows.Next() {
		var t JoinToken
		var created, expires int64
		if err := rows.Scan(&t.TokenSHA256, &t.Agent, &t.Workstation, &t.Cluster, &created, &t.CreatedBy, &expires); err != nil {
			return nil, err
		}
		t.Created, t.Expires = time.Unix(created, 0), time.Unix(expires, 0)
		out = append(out, t)
	}
	return out, rows.Err()
}

// CancelJoinToken drops a pending token, found by the start of its hash
// (the id the UI shows). It reports whether there was one.
func (d *DB) CancelJoinToken(idPrefix string) (bool, error) {
	if len(idPrefix) < 8 {
		return false, nil
	}
	res, err := d.exec(`DELETE FROM join_tokens WHERE substr(token_sha256, 1, ?) = ? AND used IS NULL`, len(idPrefix), idPrefix)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// UseJoinToken spends a join token and adds its agent, in one transaction.
// taken says whether a name is already an agent in the config file; the
// agents table guards against two joins of the same name.
func (d *DB) UseJoinToken(tokenSHA256 string, now time.Time, taken func(string) bool) (JoinedAgent, error) {
	tx, err := d.begin()
	if err != nil {
		return JoinedAgent{}, err
	}
	defer func() { _ = tx.Rollback() }()
	var t JoinToken
	var created, expires int64
	err = tx.QueryRow(`SELECT agent, workstation, is_cluster, created, created_by, expires FROM join_tokens WHERE token_sha256 = ? AND used IS NULL`,
		tokenSHA256).Scan(&t.Agent, &t.Workstation, &t.Cluster, &created, &t.CreatedBy, &expires)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && now.Unix() >= expires) {
		return JoinedAgent{}, ErrJoinToken
	}
	if err != nil {
		return JoinedAgent{}, err
	}
	if _, err := tx.Exec(`UPDATE join_tokens SET used = ? WHERE token_sha256 = ?`, now.Unix(), tokenSHA256); err != nil {
		return JoinedAgent{}, err
	}
	var n int
	if err := tx.QueryRow(`SELECT count(*) FROM agents WHERE name = ?`, t.Agent).Scan(&n); err != nil {
		return JoinedAgent{}, err
	}
	if n > 0 || taken(t.Agent) {
		// Spend the token anyway: it can never succeed.
		_ = tx.Commit()
		return JoinedAgent{}, fmt.Errorf("there is already an agent called %s", t.Agent)
	}
	a := JoinedAgent{Name: t.Agent, Workstation: t.Workstation, Cluster: t.Cluster, Added: now, AddedBy: t.CreatedBy}
	if _, err := tx.Exec(`INSERT INTO agents (name, workstation, is_cluster, added, added_by) VALUES (?, ?, ?, ?, ?)`,
		a.Name, a.Workstation, a.Cluster, a.Added.Unix(), a.AddedBy); err != nil {
		return JoinedAgent{}, err
	}
	return a, tx.Commit()
}

// RemoveJoinedAgent removes a joined agent and revokes its certificates.
// It reports whether there was one.
func (d *DB) RemoveJoinedAgent(name string, now time.Time) (bool, error) {
	tx, err := d.begin()
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()
	res, err := tx.Exec(`DELETE FROM agents WHERE name = ?`, name)
	if err != nil {
		return false, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return false, nil
	}
	if _, err := tx.Exec(`UPDATE agent_certs SET revoked = ? WHERE agent = ? AND revoked IS NULL`, now.Unix(), name); err != nil {
		return false, err
	}
	return true, tx.Commit()
}
