package state

import (
	"database/sql"
	"errors"
	"time"
)

// Agent certificates: every one obs-beacond's agent CA has signed. An agent
// has at most one live certificate; enrolling again or renewing revokes the
// one before it.
const agentsSchema = `
CREATE TABLE IF NOT EXISTS agent_certs (
	serial TEXT PRIMARY KEY,
	agent TEXT NOT NULL,
	issued INTEGER NOT NULL,
	not_after INTEGER NOT NULL,
	revoked INTEGER
);
CREATE INDEX IF NOT EXISTS agent_certs_agent ON agent_certs (agent);
`

// metaAgentCA holds the agent CA (certificate and key, PEM), sealed.
const metaAgentCA = "agent_ca"

// AgentCA returns the stored agent CA, unsealed; nil when there is none yet.
func (d *DB) AgentCA() ([]byte, error) {
	v := d.Meta(metaAgentCA)
	if v == "" {
		return nil, nil
	}
	b, err := d.Unseal(v)
	if err != nil {
		return nil, errors.New("the agent CA can't be unsealed: is state_key_file the key this database was made with? " +
			"If that key is lost, see docs/formats.md (State) to start a new agent CA")
	}
	return b, nil
}

// SetAgentCA stores the agent CA, sealed.
func (d *DB) SetAgentCA(pem []byte) error { return d.SetMeta(metaAgentCA, d.Seal(pem)) }

// AgentCert is one certificate the CA signed for an agent.
type AgentCert struct {
	Serial   string
	Agent    string
	Issued   time.Time
	NotAfter time.Time
	Revoked  time.Time // zero while live
}

// IssueAgentCert records a new certificate for an agent and revokes every
// other live certificate it has, in one transaction.
func (d *DB) IssueAgentCert(c AgentCert) error {
	tx, err := d.begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`UPDATE agent_certs SET revoked = ? WHERE agent = ? AND revoked IS NULL`, c.Issued.Unix(), c.Agent); err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO agent_certs (serial, agent, issued, not_after) VALUES (?, ?, ?, ?)`,
		c.Serial, c.Agent, c.Issued.Unix(), c.NotAfter.Unix()); err != nil {
		return err
	}
	return tx.Commit()
}

// AgentCertLive says whether serial is the agent's live certificate.
func (d *DB) AgentCertLive(serial, agent string) (bool, error) {
	var revoked sql.NullInt64
	err := d.queryRow(`SELECT revoked FROM agent_certs WHERE serial = ? AND agent = ?`, serial, agent).Scan(&revoked)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return !revoked.Valid, nil
}

// LiveAgentCerts lists every agent's live certificate.
func (d *DB) LiveAgentCerts() ([]AgentCert, error) {
	rows, err := d.query(`SELECT serial, agent, issued, not_after FROM agent_certs WHERE revoked IS NULL ORDER BY agent`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AgentCert{}
	for rows.Next() {
		var c AgentCert
		var issued, notAfter int64
		if err := rows.Scan(&c.Serial, &c.Agent, &issued, &notAfter); err != nil {
			return nil, err
		}
		c.Issued, c.NotAfter = time.Unix(issued, 0), time.Unix(notAfter, 0)
		out = append(out, c)
	}
	return out, rows.Err()
}
