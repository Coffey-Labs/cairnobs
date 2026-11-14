package check

import (
	"context"
	"math/rand/v2"
	"sort"
	"sync"
	"time"

	"github.com/cairnobs/cairnobs/internal/report"
)

// parallel is how many checks an agent runs at once.
const parallel = 8

// Runner runs a set of checks, each on its own schedule, and keeps the
// latest result of each.
type Runner struct {
	ctx     context.Context
	sem     chan struct{}
	mu      sync.Mutex
	running map[string]running // by check id
	results map[string]report.CheckResult
}

type running struct {
	spec Spec
	stop context.CancelFunc
}

// NewRunner runs checks until ctx is done.
func NewRunner(ctx context.Context) *Runner {
	return &Runner{ctx: ctx, sem: make(chan struct{}, parallel), running: map[string]running{}, results: map[string]report.CheckResult{}}
}

// Set replaces the checks: new ones start (spread over their interval, so
// a fresh list doesn't fire all at once), changed ones restart, removed
// ones stop and their results go.
func (r *Runner) Set(specs []Spec) {
	r.mu.Lock()
	defer r.mu.Unlock()
	want := map[string]Spec{}
	for _, s := range specs {
		want[s.ID] = s
	}
	for id, cur := range r.running {
		if s, ok := want[id]; !ok || !same(s, cur.spec) {
			cur.stop()
			delete(r.running, id)
			delete(r.results, id)
		}
	}
	for id, s := range want {
		if _, ok := r.running[id]; ok {
			continue
		}
		ctx, stop := context.WithCancel(r.ctx)
		r.running[id] = running{spec: s, stop: stop}
		go r.loop(ctx, s)
	}
}

func same(a, b Spec) bool {
	return a.HTTP == b.HTTP && a.TCP == b.TCP && a.DNS == b.DNS && a.Ping == b.Ping && a.Status == b.Status &&
		a.Contains == b.Contains && a.TLS == b.TLS && a.Type == b.Type && a.Server == b.Server &&
		a.Every == b.Every && a.Timeout == b.Timeout && a.Name == b.Name
}

func (r *Runner) loop(ctx context.Context, s Spec) {
	// The first run is spread over a minute (or the interval, if shorter) so
	// a new list doesn't fire all at once, but a 30-minute check doesn't
	// sit unanswered for half an hour either.
	t := time.NewTimer(time.Duration(rand.Int64N(int64(min(s.Every, time.Minute)))))
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		select {
		case r.sem <- struct{}{}:
		case <-ctx.Done():
			return
		}
		res := Run(ctx, s)
		<-r.sem
		r.mu.Lock()
		if _, ok := r.running[s.ID]; ok && ctx.Err() == nil {
			r.results[s.ID] = res
		}
		r.mu.Unlock()
		t.Reset(s.Every)
	}
}

// Results returns the latest result of every check that has run, by id.
func (r *Runner) Results() []report.CheckResult {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]report.CheckResult, 0, len(r.results))
	for _, res := range r.results {
		out = append(out, res)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}
