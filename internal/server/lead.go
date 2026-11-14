package server

// One evaluator at a time (docs/scale-and-availability.md). When copies
// share a PostgreSQL database, the copy holding the "evaluator" lease is
// the one that judges alerts and sends notices, and runs the jobs that
// write history or send mail; every other copy serves the interface and
// takes reports, reading the evaluator's incidents from the database.
//
// The holder renews every leaseRenew; the lease lapses after leaseTTL, and
// another copy takes it at the next epoch, carrying on from the incidents
// as stored. Incident saves are fenced by the epoch (state.Fence), and a
// round's notices go out only once its saves are in, so a copy that was
// paused and wakes after losing the lease tells nobody. On the way out the
// holder releases the lease and says so, so another takes over at once.
//
// Alone (SQLite, or no database), a copy is always the evaluator.

import (
	"context"
	"log"
	"sync"
	"time"

	"github.com/cairnobs/cairnobs/internal/state"
)

const (
	leaseEvaluator = "evaluator"
	leaseTTL       = 15 * time.Second
	leaseRenew     = 5 * time.Second
)

type leadership struct {
	mu      sync.Mutex
	fence   *state.Fence // nil: not the evaluator
	renewed time.Time
	cancel  context.CancelFunc // ends the evaluator's jobs
	poke    chan struct{}      // the lease was released: try for it now
}

// leading says whether this copy is the evaluator. A holder that couldn't
// renew in time isn't, whatever it last heard: by then another copy may
// have taken over.
func (srv *Server) leading() bool {
	if !srv.shared() {
		return true
	}
	l := &srv.lead
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.fence != nil && time.Since(l.renewed) < leaseTTL
}

// runLead runs the evaluator's jobs for as long as this copy is the
// evaluator, until ctx ends.
func (srv *Server) runLead(ctx context.Context, jobs func(context.Context)) {
	if !srv.shared() {
		jobs(ctx)
		return
	}
	srv.alerts.onFenced = func() { srv.stepDown("its save was refused") }
	t := time.NewTicker(leaseRenew)
	defer t.Stop()
	for {
		srv.tryLead(ctx, jobs)
		select {
		case <-ctx.Done():
			srv.stepDown("")
			return
		case <-t.C:
		case <-srv.lead.poke:
		}
	}
}

func (srv *Server) tryLead(ctx context.Context, jobs func(context.Context)) {
	now := time.Now()
	f, ok, err := srv.db.Acquire(leaseEvaluator, srv.copyID, leaseTTL, now)
	l := &srv.lead
	l.mu.Lock()
	cur, lapsed := l.fence, time.Since(l.renewed) >= leaseTTL
	switch {
	case err != nil:
		l.mu.Unlock()
		log.Printf("evaluator: can't reach the lease: %v", err)
		if cur != nil && lapsed {
			srv.stepDown("it couldn't renew the lease")
			srv.alerts.lead(nil)
		}
	case ok && cur != nil && cur.Epoch == f.Epoch:
		l.renewed = now
		l.mu.Unlock()
	case ok:
		if l.cancel != nil {
			l.cancel()
		}
		l.mu.Unlock()
		// The alerter takes the fence before this copy says it's leading,
		// so no round runs unfenced.
		srv.alerts.lead(&f)
		lctx, cancel := context.WithCancel(ctx)
		l.mu.Lock()
		l.fence, l.renewed, l.cancel = &f, now, cancel
		l.mu.Unlock()
		log.Printf("evaluator: this copy (%s) is the evaluator now, epoch %d", srv.copyID, f.Epoch)
		go jobs(lctx)
	default:
		l.mu.Unlock()
		if cur != nil {
			srv.stepDown("another copy holds the lease")
			srv.alerts.lead(nil)
		}
	}
}

// released hears that the evaluator gave the lease up.
func (srv *Server) released() {
	select {
	case srv.lead.poke <- struct{}{}:
	default:
	}
}

// stepDown stops being the evaluator; why "" is on the way out, and gives
// the lease up so another copy needn't wait for it to lapse. The release
// happens under the lock, so a second caller on the way out (runLead and
// ListenAndServe both are) returns only once it's done, not while the
// process is about to exit under it. It doesn't touch the alerter, which
// may be the one calling.
func (srv *Server) stepDown(why string) {
	l := &srv.lead
	l.mu.Lock()
	defer l.mu.Unlock()
	f := l.fence
	l.fence = nil
	if l.cancel != nil {
		l.cancel()
		l.cancel = nil
	}
	if f == nil {
		return
	}
	if why == "" {
		if err := srv.db.Release(*f); err != nil {
			log.Printf("evaluator: can't release the lease: %v", err)
			return
		}
		log.Printf("evaluator: this copy gave the lease up on the way out")
		srv.publish(noticeLease, "")
		return
	}
	log.Printf("evaluator: this copy isn't the evaluator any more: %s", why)
}
