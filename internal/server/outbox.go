// SPDX-FileCopyrightText: 2026 Coffey Labs LLC
// SPDX-License-Identifier: AGPL-3.0-or-later

package server

import (
	"context"
	"encoding/json"
	"log"
	"time"

	"github.com/cairnobs/cairnobs/internal/notify"
	"github.com/cairnobs/cairnobs/internal/state"
)

// outboxItems turns a round's notices into outbox rows, one per channel.
func outboxItems(jobs []job, now time.Time) []state.OutboxItem {
	var out []state.OutboxItem
	for _, j := range jobs {
		b, _ := json.Marshal(j.n)
		for _, ch := range j.chans {
			out = append(out, state.OutboxItem{Made: now, Channel: ch.ID(), Notice: string(b)})
		}
	}
	return out
}

// poke wakes the outbox's sender.
func (a *alerter) poke() {
	select {
	case a.wake <- struct{}{}:
	default:
	}
}

// outboxCheck is how often the sender looks without being woken: for
// retries coming due, and notices a copy that lost the evaluator role left.
const outboxCheck = 2 * time.Second

// sendOutbox sends what's in the outbox until ctx ends. Only the evaluator
// runs it (one of its jobs, lead.go), so a notice goes from one copy.
func (a *alerter) sendOutbox(ctx context.Context) {
	t := time.NewTicker(outboxCheck)
	defer t.Stop()
	for {
		for ctx.Err() == nil && a.sendDue(ctx) {
		}
		select {
		case <-ctx.Done():
			return
		case <-a.wake:
		case <-t.C:
		}
	}
}

// sendDue sends the notices that are due; true if there may be more.
func (a *alerter) sendDue(ctx context.Context) bool {
	const batch = 50
	now := time.Now()
	due, err := a.db.DueNotices(now, batch)
	if err != nil {
		log.Printf("alerts: can't read the outbox: %v", err)
		return false
	}
	chans := map[string]notify.Channel{}
	for _, ch := range a.chans() {
		chans[ch.ID()] = ch
	}
	for _, it := range due {
		if ctx.Err() != nil {
			return false
		}
		var n notify.Notice
		ch, ok := chans[it.Channel]
		if err := json.Unmarshal([]byte(it.Notice), &n); err != nil || !ok {
			log.Printf("alerts: dropping a notice for %s: the channel is gone or the notice unreadable", it.Channel)
			_ = a.db.NoticeDone(it.ID)
			continue
		}
		sctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		err := ch.Send(sctx, n)
		cancel()
		switch {
		case err == nil:
			log.Printf("alerts: sent %s %s (%s) to %s", n.Kind, n.ID, n.Severity, ch.ID())
			if err := a.db.NoticeDone(it.ID); err != nil {
				log.Printf("alerts: sent %s %s to %s, but can't take it off the outbox: it may go again: %v", n.Kind, n.ID, ch.ID(), err)
			}
		case ctx.Err() != nil:
			return false // stopping: it stays for whoever is next
		case it.Attempts >= len(a.retry):
			log.Printf("alerts: gave up on %s for %s %s: %v", ch.ID(), n.Kind, n.ID, err)
			_ = a.db.NoticeDone(it.ID)
		default:
			wait := a.retry[it.Attempts]
			log.Printf("alerts: %v; trying again in %s", err, wait)
			_ = a.db.NoticeLater(it.ID, it.Attempts+1, time.Now().Add(wait))
		}
	}
	return len(due) == batch
}
