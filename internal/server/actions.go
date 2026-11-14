package server

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/cairnobs/cairnobs/internal/report"
	"github.com/cairnobs/cairnobs/internal/state"
)

// Actions (docs/cert-rotation.md): what an admin asks an agent to do. An
// agent is never called; like its checks, a report's 204 carries
// X-Beacon-Actions when it has some waiting, it fetches them, decides for
// itself whether its config allows each, and the next report says what
// came of it. Everything goes through the database, so any copy can hand
// an action over, once.

// actionWindow is how long an action waits to be taken; after that it's
// never handed over (the reason for it may be gone).
const actionWindow = 10 * time.Minute

var errNoDatabase = errors.New("actions need a database")

// queueAction stores an action for an agent and audits it.
func (srv *Server) queueAction(agent, kind string, args any, who, what string) (string, error) {
	if srv.db == nil {
		return "", errNoDatabase
	}
	b, err := json.Marshal(args)
	if err != nil {
		return "", err
	}
	var id [8]byte
	_, _ = rand.Read(id[:])
	a := state.Action{ID: hex.EncodeToString(id[:]), Agent: agent, Kind: kind, Args: string(b), By: who, Created: time.Now()}
	if err := srv.db.AddAction(a); err != nil {
		return "", err
	}
	srv.audit(who, "asked "+agent+" to act", fmt.Sprintf("%s (action %s)", what, a.ID))
	return a.ID, nil
}

// actionsAnswer handles a report's action results and says, in its
// answer, whether more are waiting.
func (srv *Server) actionsAnswer(w http.ResponseWriter, agent string, results []report.ActionResult) {
	if srv.db == nil {
		return
	}
	now := time.Now()
	for _, r := range results {
		ok, err := srv.db.FinishAction(r.ID, agent, r.OK, truncate(r.Detail, 2000), now)
		switch {
		case err != nil:
			log.Printf("action %s from %s: %v", r.ID, agent, err)
		case ok && r.OK:
			srv.audit(agent, "did action "+r.ID, r.Detail)
		case ok:
			srv.audit(agent, "didn't do action "+r.ID, r.Detail)
		}
	}
	if waiting, err := srv.db.ActionsWaiting(agent, now.Add(-actionWindow)); err == nil && waiting {
		w.Header().Set("X-Beacon-Actions", "1")
	}
}

// handleAgentActions hands an agent its waiting actions.
func (srv *Server) handleAgentActions(w http.ResponseWriter, r *http.Request) {
	agent, ok := srv.agentFor(r)
	if !ok {
		http.Error(w, srv.agentRefusal(), http.StatusUnauthorized)
		return
	}
	out := []report.Action{}
	if srv.db != nil {
		now := time.Now()
		as, err := srv.db.TakeActions(agent, now.Add(-actionWindow), now)
		if err != nil {
			http.Error(w, "can't read actions", http.StatusServiceUnavailable)
			return
		}
		for _, a := range as {
			out = append(out, report.Action{ID: a.ID, Kind: a.Kind, Args: json.RawMessage(a.Args)})
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"actions": out})
}

// ActionView is one action as the UI shows it.
type ActionView struct {
	ID      string          `json:"id"`
	Agent   string          `json:"agent"`
	Kind    string          `json:"kind"`
	Args    json.RawMessage `json:"args"`
	By      string          `json:"by"`
	Created time.Time       `json:"created"`
	State   string          `json:"state"` // waiting, taken, done, failed, expired
	Detail  string          `json:"detail,omitempty"`
	At      *time.Time      `json:"at,omitempty"` // when it was taken or finished
}

func actionView(a state.Action, now time.Time) ActionView {
	v := ActionView{ID: a.ID, Agent: a.Agent, Kind: a.Kind, Args: json.RawMessage(a.Args), By: a.By, Created: a.Created, Detail: a.Detail}
	switch {
	case !a.Done.IsZero():
		v.State, v.At = "done", &a.Done
		if !a.OK {
			v.State = "failed"
		}
	case !a.Taken.IsZero():
		v.State, v.At = "taken", &a.Taken
	case now.Sub(a.Created) > actionWindow:
		v.State = "expired"
	default:
		v.State = "waiting"
	}
	return v
}

// handleActions lists recent actions, newest first.
func (srv *Server) handleActions(w http.ResponseWriter, r *http.Request) {
	out := []ActionView{}
	if srv.db != nil {
		as, err := srv.db.Actions(200)
		if err != nil {
			http.Error(w, "can't read actions", http.StatusServiceUnavailable)
			return
		}
		now := time.Now()
		for _, a := range as {
			out = append(out, actionView(a, now))
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"actions": out, "available": srv.db != nil})
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
