// SPDX-FileCopyrightText: 2026 Coffey Labs LLC
// SPDX-License-Identifier: AGPL-3.0-or-later

package state

// Actions: things an admin asked an agent to do (docs/cert-rotation.md),
// kept here so whichever copy of obs-beacond the agent reports to can hand
// them over, once. An action not taken within its time is never handed
// over; finished ones stay 30 days for the page and the audit trail.

import (
	"time"
)

const actionsSchema = `
CREATE TABLE IF NOT EXISTS agent_actions (
	id TEXT PRIMARY KEY, agent TEXT NOT NULL, kind TEXT NOT NULL, args TEXT NOT NULL,
	by_user TEXT NOT NULL, created BIGINT NOT NULL, taken BIGINT NOT NULL DEFAULT 0,
	done BIGINT NOT NULL DEFAULT 0, ok BIGINT NOT NULL DEFAULT 0, detail TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS agent_actions_agent ON agent_actions (agent, taken, created);
`

// Action is one thing an agent was asked to do.
type Action struct {
	ID      string
	Agent   string
	Kind    string // "reload", "cert-renew"
	Args    string // JSON
	By      string
	Created time.Time
	Taken   time.Time // zero: not handed over yet
	Done    time.Time // zero: no result yet
	OK      bool
	Detail  string
}

// actionsKept is how long finished (or never taken) actions are kept.
const actionsKept = 30 * 24 * time.Hour

// AddAction stores a new action; ones older than 30 days go at the same
// time.
func (d *DB) AddAction(a Action) error {
	if _, err := d.exec(`DELETE FROM agent_actions WHERE created < ?`, a.Created.Add(-actionsKept).Unix()); err != nil {
		return err
	}
	_, err := d.exec(`INSERT INTO agent_actions (id, agent, kind, args, by_user, created) VALUES (?, ?, ?, ?, ?, ?)`,
		a.ID, a.Agent, a.Kind, a.Args, a.By, a.Created.Unix())
	return err
}

// ActionsWaiting says whether the agent has actions to take, made since
// `since`.
func (d *DB) ActionsWaiting(agent string, since time.Time) (bool, error) {
	var n int
	err := d.queryRow(`SELECT count(*) FROM agent_actions WHERE agent = ? AND taken = 0 AND created >= ?`, agent, since.Unix()).Scan(&n)
	return n > 0, err
}

// TakeActions hands the agent its waiting actions made since `since`,
// marking them taken in the same step, so two copies asked at the same
// moment never hand one over twice.
func (d *DB) TakeActions(agent string, since, now time.Time) ([]Action, error) {
	rows, err := d.query(`UPDATE agent_actions SET taken = ? WHERE agent = ? AND taken = 0 AND created >= ?
		RETURNING id, kind, args, by_user, created`, now.Unix(), agent, since.Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Action
	for rows.Next() {
		a := Action{Agent: agent, Taken: now}
		var created int64
		if err := rows.Scan(&a.ID, &a.Kind, &a.Args, &a.By, &created); err != nil {
			return nil, err
		}
		a.Created = time.Unix(created, 0)
		out = append(out, a)
	}
	return out, rows.Err()
}

// FinishAction records what came of an action the agent took; false when
// it isn't that agent's, wasn't taken, or already has a result.
func (d *DB) FinishAction(id, agent string, ok bool, detail string, at time.Time) (bool, error) {
	okv := 0
	if ok {
		okv = 1
	}
	r, err := d.exec(`UPDATE agent_actions SET done = ?, ok = ?, detail = ? WHERE id = ? AND agent = ? AND taken > 0 AND done = 0`,
		at.Unix(), okv, detail, id, agent)
	if err != nil {
		return false, err
	}
	n, err := r.RowsAffected()
	return n == 1, err
}

// Actions lists the newest actions first, at most limit.
func (d *DB) Actions(limit int) ([]Action, error) {
	rows, err := d.query(`SELECT id, agent, kind, args, by_user, created, taken, done, ok, detail FROM agent_actions
		ORDER BY created DESC, id LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Action
	for rows.Next() {
		var a Action
		var created, taken, done, ok int64
		if err := rows.Scan(&a.ID, &a.Agent, &a.Kind, &a.Args, &a.By, &created, &taken, &done, &ok, &a.Detail); err != nil {
			return nil, err
		}
		a.Created, a.OK = time.Unix(created, 0), ok == 1
		if taken > 0 {
			a.Taken = time.Unix(taken, 0)
		}
		if done > 0 {
			a.Done = time.Unix(done, 0)
		}
		out = append(out, a)
	}
	return out, rows.Err()
}
