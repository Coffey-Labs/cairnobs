// SPDX-FileCopyrightText: 2026 Coffey Labs LLC
// SPDX-License-Identifier: AGPL-3.0-or-later

package server

// Readings: Cloudflare, Patroni and the mail round trip, which obs-beacond
// polls for itself. Only the evaluator polls; after each round it stores
// the reading (state.PutReading) and announces it, and the other copies
// read it back. So each outside API is asked once however many copies run,
// the mail round trip sends one test message per round, and every copy's
// pages show the same thing.
//
// A copy that becomes the evaluator, or a single obs-beacond that restarts,
// starts from the stored reading and polls when the next round is due, not
// at once: an upgrade sends no extra test message, and what a poller keeps
// between rounds (since when Cloudflare has failed, the round trip's day of
// history) carries over.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/cairnobs/cairnobs/internal/cloudflare"
	"github.com/cairnobs/cairnobs/internal/mailmon"
	"github.com/cairnobs/cairnobs/internal/state"
)

const (
	readingCloudflare = "cloudflare"
	readingAkamai     = "akamai"
	readingPatroni    = "patroni"
	readingMail       = "mail"

	patroniEvery = 15 * time.Second
)

// envelope is a reading as stored: Config is a digest of the poller's
// settings, so a reading taken with other settings is polled again at once
// rather than shown until the next round.
type envelope struct {
	Config string          `json:"config"`
	Body   json.RawMessage `json:"body"`
}

// cfReading is Cloudflare's snapshot with each zone's DNS records, which
// the snapshot's own JSON leaves out.
type cfReading struct {
	Snapshot cloudflare.Snapshot            `json:"snapshot"`
	Records  map[string][]cloudflare.Record `json:"records"`
}

// patroniShared is one member's read, as stored.
type patroniShared struct {
	URL     string          `json:"url"`
	At      time.Time       `json:"at"`
	Err     string          `json:"error,omitempty"`
	Members []patroniMember `json:"members,omitempty"`
	Node    *patroniNode    `json:"node,omitempty"`
}

// readingsSeen is when each reading this copy shows was taken.
type readingsSeen struct {
	mu sync.Mutex
	at map[string]time.Time
}

func (r *readingsSeen) get(name string) time.Time {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.at[name]
}

// newer records at for name and says whether it's newer than what this
// copy had.
func (r *readingsSeen) newer(name string, at time.Time) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.at == nil {
		r.at = map[string]time.Time{}
	}
	if !at.After(r.at[name]) {
		return false
	}
	r.at[name] = at
	return true
}

func digest(v any) string {
	h := sha256.Sum256(fmt.Appendf(nil, "%+v", v))
	return hex.EncodeToString(h[:8])
}

// readingConfig is the digest of a poller's settings.
func (srv *Server) readingConfig(name string) string {
	switch name {
	case readingCloudflare:
		return digest(srv.cf.Config())
	case readingAkamai:
		return digest(srv.ak.Config())
	case readingPatroni:
		return digest(srv.cfg.DataLayer.Patroni)
	case readingMail:
		return digest(srv.mail.Config())
	}
	return ""
}

// takeReading is what this copy has for name, as JSON.
func (srv *Server) takeReading(name string) ([]byte, error) {
	switch name {
	case readingCloudflare:
		r := cfReading{Snapshot: srv.cf.Snapshot(), Records: map[string][]cloudflare.Record{}}
		for _, z := range r.Snapshot.Zones {
			r.Records[z.Name] = z.Records
		}
		return json.Marshal(r)
	case readingAkamai:
		return json.Marshal(srv.ak.Snapshot())
	case readingPatroni:
		srv.patroni.mu.Lock()
		out := make([]patroniShared, 0, len(srv.patroni.reads))
		for u, r := range srv.patroni.reads {
			p := patroniShared{URL: u, At: r.at, Members: r.members, Node: r.node}
			if r.err != nil {
				p.Err = plainNetErr(r.err)
			}
			out = append(out, p)
		}
		srv.patroni.mu.Unlock()
		return json.Marshal(out)
	case readingMail:
		return json.Marshal(srv.mail.Snapshot())
	}
	return nil, fmt.Errorf("no reading %q", name)
}

// useReading puts a reading in place of what this copy has.
func (srv *Server) useReading(name string, body []byte) error {
	switch name {
	case readingCloudflare:
		var r cfReading
		if err := json.Unmarshal(body, &r); err != nil {
			return err
		}
		for i, z := range r.Snapshot.Zones {
			r.Snapshot.Zones[i].Records = r.Records[z.Name]
		}
		srv.cf.Set(r.Snapshot)
	case readingAkamai:
		var s akamaiReading
		if err := json.Unmarshal(body, &s); err != nil {
			return err
		}
		srv.ak.Set(s)
	case readingPatroni:
		var list []patroniShared
		if err := json.Unmarshal(body, &list); err != nil {
			return err
		}
		reads := make(map[string]patroniRead, len(list))
		for _, p := range list {
			r := patroniRead{at: p.At, members: p.Members, node: p.Node}
			if p.Err != "" {
				r.err = errors.New(p.Err)
			}
			reads[p.URL] = r
		}
		srv.patroni.mu.Lock()
		srv.patroni.reads = reads
		srv.patroni.mu.Unlock()
	case readingMail:
		var s mailmon.Snapshot
		if err := json.Unmarshal(body, &s); err != nil {
			return err
		}
		srv.mail.Set(s)
	default:
		return fmt.Errorf("no reading %q", name)
	}
	return nil
}

// readingOn says whether this install runs the poller.
func (srv *Server) readingOn(name string) bool {
	switch name {
	case readingCloudflare:
		return srv.cf != nil
	case readingAkamai:
		return srv.ak != nil
	case readingPatroni:
		return srv.cfg.DataLayer != nil && len(srv.cfg.DataLayer.Patroni) > 0
	case readingMail:
		return srv.mail != nil
	}
	return false
}

// loadReading reads the stored reading into this copy, when it's newer than
// the one it shows. It returns when the stored one was taken, and whether
// it was taken with the settings in force (zero and false when there's
// none, or no database).
func (srv *Server) loadReading(name string) (time.Time, bool) {
	if srv.db == nil || !srv.readingOn(name) {
		return time.Time{}, false
	}
	b, at, err := srv.db.Reading(name)
	if err != nil {
		if !errors.Is(err, state.ErrNotFound) {
			log.Printf("readings: %s: %v", name, err)
		}
		return time.Time{}, false
	}
	var e envelope
	if err := json.Unmarshal(b, &e); err != nil {
		log.Printf("readings: %s: %v", name, err)
		return time.Time{}, false
	}
	if srv.seen.newer(name, at) {
		if err := srv.useReading(name, e.Body); err != nil {
			log.Printf("readings: %s: %v", name, err)
			return time.Time{}, false
		}
	}
	return at, e.Config == srv.readingConfig(name)
}

// saveReading stores this copy's reading and tells the other copies.
func (srv *Server) saveReading(name string, at time.Time) {
	if srv.db == nil {
		return
	}
	body, err := srv.takeReading(name)
	if err == nil {
		body, err = json.Marshal(envelope{Config: srv.readingConfig(name), Body: body})
	}
	if err == nil {
		err = srv.db.PutReading(name, body, at)
	}
	if err != nil {
		log.Printf("readings: can't store %s: %v", name, err)
		return
	}
	srv.seen.newer(name, at)
	srv.publish(noticeReading, name)
}

// poll runs round every every, for as long as ctx lasts: on the evaluator.
// The first round is when the stored reading is due. A round cut short
// because this copy stopped being the evaluator is undone, so the page
// doesn't show "context canceled" until the new evaluator's reading comes.
func (srv *Server) poll(ctx context.Context, name string, every time.Duration, round func(context.Context)) {
	last, same := srv.loadReading(name)
	wait := time.Duration(0)
	if same {
		wait = max(time.Until(last.Add(every)), 0)
	}
	t := time.NewTimer(wait)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		before, _ := srv.takeReading(name)
		had := srv.seen.get(name)
		start := time.Now()
		round(ctx)
		if ctx.Err() != nil {
			// Unless the new evaluator's reading has come meanwhile.
			if before != nil && srv.seen.get(name).Equal(had) {
				_ = srv.useReading(name, before)
			}
			return
		}
		srv.saveReading(name, time.Now())
		t.Reset(max(every-time.Since(start), time.Second))
	}
}

// refreshReading reads a reading another copy stored. The evaluator has
// its own.
func (srv *Server) refreshReading(name string) {
	if !srv.leading() {
		srv.loadReading(name)
	}
}

// resyncReadings reads every reading newer than this copy's.
func (srv *Server) resyncReadings() {
	if srv.leading() {
		return
	}
	times, err := srv.db.ReadingTimes()
	if err != nil {
		log.Printf("shared: readings: %v", err)
		return
	}
	for name, at := range times {
		if at.After(srv.seen.get(name)) {
			srv.loadReading(name)
		}
	}
}
