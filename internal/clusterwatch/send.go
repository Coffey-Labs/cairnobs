package clusterwatch

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"log"
	"time"

	"github.com/cairnobs/cairnobs/internal/report"
)

// Sender sends one batch to obs-beacond.
type Sender func(ctx context.Context, b report.Workloads) error

// Config is how the cluster agent paces itself; zero values take the
// defaults in docs/cluster-watcher.md.
type Config struct {
	Name      string        // the agent's name, which is the cluster's
	Agent     string        // this build's version
	Batch     time.Duration // between batches of changes: 5s
	Heartbeat time.Duration // a batch at least this often, empty or not: 30s
	Relist    time.Duration // between full listings: 30m
	Page      int           // records in one request: 500
	SyncWait  time.Duration // how long to wait for the first lists before sending what there is: 1m
	// NoCertScan stops the handshakes on TLS Ingress and Route hosts
	// (docs/cert-inventory.md, step 4).
	NoCertScan bool
	// CertRenew: the agent runs with -cert-renew, so it may ask
	// cert-manager to renew a Certificate (docs/cert-rotation.md, step 3).
	CertRenew bool
}

func (c *Config) defaults() {
	if c.Batch == 0 {
		c.Batch = 5 * time.Second
	}
	if c.Heartbeat == 0 {
		c.Heartbeat = 30 * time.Second
	}
	if c.Relist == 0 {
		c.Relist = 30 * time.Minute
	}
	if c.Page == 0 {
		c.Page = 500
	}
	if c.SyncWait == 0 {
		c.SyncWait = time.Minute
	}
}

// outage is how long sending may fail before the next success starts with
// a full listing: obs-beacond may have lost or dropped something meanwhile.
const outage = 2 * time.Minute

// Run watches the cluster and sends what it sees until ctx ends.
func Run(ctx context.Context, api API, cfg Config, send Sender) {
	cfg.defaults()
	s := newStore()
	for _, r := range resources {
		go watchResource(ctx, api, s, r)
	}
	go watchVersion(ctx, api, s)
	go watchGuestFS(ctx, api, s)
	go watchMetrics(ctx, api, s)
	if !cfg.NoCertScan {
		go scanHosts(ctx, cfg.Name, s)
	}
	start := time.Now()
	for !s.allSynced() && time.Since(start) < cfg.SyncWait {
		sleep(ctx, 200*time.Millisecond)
		if ctx.Err() != nil {
			return
		}
	}
	log.Printf("cluster: listed the cluster in %s; sending to obs-beacond every %s", time.Since(start).Round(time.Millisecond), cfg.Batch)
	sn := newSender(cfg, send)
	t := time.NewTicker(cfg.Batch)
	defer t.Stop()
	for {
		sn.tick(ctx, s, time.Now())
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

type tracked struct {
	state string
	since time.Time
}

type sender struct {
	cfg  Config
	send Sender
	gen  string

	track    map[string]tracked // every record's state, and since when
	sent     map[string][]byte  // what obs-beacond has, as sent
	needFull bool
	lastFull time.Time
	lastOK   time.Time // the last batch taken
	lastTry  time.Time // the last batch sent, taken or not
	failing  string    // the last error logged, so it's logged once

	usage     []report.Usage // the latest reading's sums, waiting to be sent
	usageAt   time.Time      // when that reading was taken
	usageSent time.Time      // the reading obs-beacond has
}

func newSender(cfg Config, send Sender) *sender {
	cfg.defaults()
	return &sender{cfg: cfg, send: send, track: map[string]tracked{}, sent: map[string][]byte{}, needFull: true}
}

func newGeneration() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// records is every record now, with Since kept from when the state began.
func (sn *sender) records(s *store, now time.Time) []report.Workload {
	v := s.snapshot()
	recs := build(v)
	s.mu.Lock()
	cl := clusterRecord(sn.cfg.Name, s.version, s.problem, s.apiDown)
	at := s.usageAt
	s.mu.Unlock()
	if sn.cfg.CertRenew {
		// obs-beacond offers "Renew now" only where this says it may.
		if cl.Facts == nil {
			cl.Facts = map[string]any{}
		}
		cl.Facts["cert_renew"] = true
	}
	// A new reading's sums go with the next batch; each reading once.
	if sn.newReading(at) {
		sn.usage, sn.usageAt = usage(v, recs), at
	}
	return sn.stamp(append(recs, cl), now)
}

func (sn *sender) newReading(at time.Time) bool {
	return !at.IsZero() && at.After(sn.usageSent) && !at.Equal(sn.usageAt)
}

// stamp keeps each record's Since from when its state began.
func (sn *sender) stamp(recs []report.Workload, now time.Time) []report.Workload {
	next := make(map[string]tracked, len(recs))
	for i := range recs {
		r := &recs[i]
		if p, ok := sn.track[r.ID]; ok {
			switch {
			case p.state == r.State && !p.since.IsZero():
				r.Since = p.since
			case p.state == r.State:
			case r.Since.IsZero() || !r.Since.After(p.since):
				// Changed since the last look, and Kubernetes doesn't say
				// when: now is close enough.
				r.Since = now.Truncate(time.Second)
			}
		}
		next[r.ID] = tracked{r.State, r.Since}
	}
	sn.track = next
	return recs
}

func (sn *sender) tick(ctx context.Context, s *store, now time.Time) {
	sn.batch(ctx, sn.records(s, now), now)
}

// Batcher sends any source's records to obs-beacond as a cluster agent
// does: a full listing now and then, changes in between, a heartbeat, and
// each usage reading once. A Proxmox node's agent uses it.
type Batcher struct{ sn *sender }

// NewBatcher paces itself by cfg, as Run does.
func NewBatcher(cfg Config, send Sender) *Batcher { return &Batcher{newSender(cfg, send)} }

// Tick sends what changed since the last tick, if anything did or a
// heartbeat is due. use is the reading taken at, when new.
func (b *Batcher) Tick(ctx context.Context, recs []report.Workload, use []report.Usage, at, now time.Time) {
	if b.sn.newReading(at) {
		b.sn.usage, b.sn.usageAt = use, at
	}
	b.sn.batch(ctx, b.sn.stamp(recs, now), now)
}

// Every is how often to call Tick.
func (b *Batcher) Every() time.Duration { return b.sn.cfg.Batch }

func (sn *sender) batch(ctx context.Context, recs []report.Workload, now time.Time) {
	if sn.needFull || now.Sub(sn.lastFull) >= sn.cfg.Relist {
		sn.full(ctx, recs, now)
		return
	}
	var changed []report.Workload
	cur := make(map[string]bool, len(recs))
	enc := map[string][]byte{}
	for _, r := range recs {
		cur[r.ID] = true
		b, _ := json.Marshal(r)
		if !bytes.Equal(sn.sent[r.ID], b) {
			changed = append(changed, r)
			enc[r.ID] = b
		}
	}
	var removed []string
	for id := range sn.sent {
		if !cur[id] {
			removed = append(removed, id)
		}
	}
	newUsage := sn.usageAt.After(sn.usageSent)
	if len(changed) == 0 && len(removed) == 0 && !newUsage && now.Sub(sn.lastTry) < sn.cfg.Heartbeat {
		return
	}
	for i := 0; i < len(changed) || i == 0; i += sn.cfg.Page {
		chunk := changed[i:min(i+sn.cfg.Page, len(changed))]
		b := report.Workloads{V: report.Version, Agent: sn.cfg.Agent, Generation: sn.gen, Records: chunk}
		last := i+sn.cfg.Page >= len(changed)
		if last {
			b.Removed = removed
			if newUsage {
				b.Usage, b.UsageAt = sn.usage, sn.usageAt
			}
		}
		if !sn.try(ctx, b, now) {
			return
		}
		if last && newUsage {
			sn.usageSent = sn.usageAt
		}
		for _, r := range chunk {
			sn.sent[r.ID] = enc[r.ID]
		}
		if last {
			for _, id := range removed {
				delete(sn.sent, id)
			}
		}
	}
}

// full sends everything: a begin, pages, an end. obs-beacond drops what
// the listing didn't hold when the end arrives.
func (sn *sender) full(ctx context.Context, recs []report.Workload, now time.Time) {
	gen := newGeneration()
	sent := make(map[string][]byte, len(recs))
	for i := 0; i < len(recs) || i == 0; i += sn.cfg.Page {
		chunk := recs[i:min(i+sn.cfg.Page, len(recs))]
		part := report.FullPage
		if i == 0 {
			part = report.FullBegin
		}
		if !sn.try(ctx, report.Workloads{V: report.Version, Agent: sn.cfg.Agent, Generation: gen, Full: part, Records: chunk}, now) {
			return // the next tick starts again, with a new generation
		}
		for _, r := range chunk {
			b, _ := json.Marshal(r)
			sent[r.ID] = b
		}
	}
	end := report.Workloads{V: report.Version, Agent: sn.cfg.Agent, Generation: gen, Full: report.FullEnd}
	if sn.usageAt.After(sn.usageSent) {
		end.Usage, end.UsageAt = sn.usage, sn.usageAt
	}
	if !sn.try(ctx, end, now) {
		return
	}
	if end.Usage != nil {
		sn.usageSent = sn.usageAt
	}
	sn.gen, sn.sent, sn.needFull, sn.lastFull = gen, sent, false, now
}

// try sends one batch, and notes how it went.
func (sn *sender) try(ctx context.Context, b report.Workloads, now time.Time) bool {
	if b.Records == nil {
		b.Records = []report.Workload{}
	}
	sn.lastTry = now
	err := sn.send(ctx, b)
	if err != nil {
		if msg := err.Error(); msg != sn.failing {
			sn.failing = msg
			log.Printf("cluster: sending to obs-beacond: %v", err)
		}
		return false
	}
	if sn.failing != "" {
		log.Printf("cluster: obs-beacond takes batches again")
		sn.failing = ""
	}
	if !sn.lastOK.IsZero() && now.Sub(sn.lastOK) > outage && b.Full == "" {
		// After a long gap, list everything once more.
		sn.needFull = true
	}
	sn.lastOK = now
	return true
}
