// Package clusterwatch is the cluster agent (docs/cluster-watcher.md): it
// lists and watches a cluster's nodes, pods, controllers, jobs and volumes
// through the API, keeps the few fields it needs, and sends obs-beacond
// short records: everything when it starts and every 30 minutes, the
// changes every 5 seconds in between.
package clusterwatch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math/rand/v2"
	"net/http"
	"sync"
	"time"

	"github.com/cairnobs/cairnobs/internal/kube"
)

// API is the part of the Kubernetes API the watcher uses (*kube.Client).
type API interface {
	Get(ctx context.Context, path string, v any) error
	List(ctx context.Context, path string, each func(raw json.RawMessage) error) (string, error)
	Watch(ctx context.Context, path, rv string, timeout time.Duration, fn func(typ string, raw json.RawMessage) error) (string, error)
	Version(ctx context.Context) (string, error)
}

// watchFor is how long the agent asks each watch to last; the API ends it
// then, and the agent watches on from where it was.
const watchFor = 5 * time.Minute

// notServedRetry is how often a kind the API doesn't serve is looked for
// again: OpenShift's on a plain Kubernetes cluster, or a CRD not installed
// yet. A var for the tests.
var notServedRetry = 10 * time.Minute

// store is what the watches keep.
type store struct {
	mu      sync.Mutex
	objs    view
	synced  map[string]bool   // listed at least once (or can't be: see problems)
	problem map[string]string // resource → why it can't be read now
	version string
	apiDown string    // why the API can't be reached, after a minute of trying
	failing time.Time // since when /version has failed; zero: it hasn't
	usageAt time.Time // when nodemetrics and podmetrics were read; zero: no usage
	changes chan struct{}
}

func newStore() *store {
	s := &store{objs: view{}, synced: map[string]bool{}, problem: map[string]string{}, changes: make(chan struct{}, 1)}
	for _, r := range resources {
		s.objs[r.name] = map[string]any{}
	}
	s.objs["guestfs"] = map[string]any{}
	s.objs[tlsSeenRes] = map[string]any{}
	return s
}

func (s *store) changed() {
	select {
	case s.changes <- struct{}{}:
	default:
	}
}

// snapshot is a shallow copy of every object: the objects themselves are
// never changed once decoded, only replaced.
func (s *store) snapshot() view {
	s.mu.Lock()
	defer s.mu.Unlock()
	v := make(view, len(s.objs))
	for k, m := range s.objs {
		c := make(map[string]any, len(m))
		for id, o := range m {
			c[id] = o
		}
		v[k] = c
	}
	return v
}

// allSynced says whether every resource has been listed (or can't be).
func (s *store) allSynced() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range resources {
		if !s.synced[r.name] {
			return false
		}
	}
	return true
}

func (s *store) setProblem(res string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	was := s.problem[res]
	if err == nil {
		delete(s.problem, res)
		if was != "" {
			log.Printf("cluster: reading %s again", res)
		}
		return
	}
	msg := err.Error()
	switch kube.Code(err) {
	case http.StatusForbidden:
		msg = "not allowed (the ClusterRole doesn't grant it)"
	case http.StatusNotFound:
		// Not served by this cluster (an old API version): nothing to read.
		s.synced[res] = true
		return
	}
	s.problem[res] = msg
	// Something that can't be read counts as listed, so the rest is sent;
	// the cluster's record says what's missing.
	s.synced[res] = true
	if msg != was {
		log.Printf("cluster: can't read %s: %v", res, err)
	}
}

// watchResource lists one resource, then watches it, listing again when
// the watch can't go on from where it was. It runs until ctx ends.
func watchResource(ctx context.Context, api API, s *store, r resource) {
	backoff := time.Second
	for ctx.Err() == nil {
		fresh := map[string]any{}
		rv, err := api.List(ctx, r.path, func(raw json.RawMessage) error {
			id, o, err := r.decode(raw)
			if err != nil {
				return fmt.Errorf("decoding %s: %w", r.name, err)
			}
			fresh[id] = o
			return nil
		})
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			s.setProblem(r.name, err)
			if kube.Code(err) == http.StatusNotFound {
				sleep(ctx, notServedRetry) // not this kind of cluster, or not yet
				continue
			}
			sleep(ctx, backoff)
			backoff = min(backoff*2, time.Minute)
			continue
		}
		s.mu.Lock()
		s.objs[r.name] = fresh
		s.synced[r.name] = true
		s.mu.Unlock()
		s.setProblem(r.name, nil)
		s.changed()
		backoff = time.Second
		for ctx.Err() == nil {
			// Spread the watches' ends so they don't all start again at once.
			d := watchFor + time.Duration(rand.Int64N(int64(time.Minute)))
			began := time.Now()
			rv, err = api.Watch(ctx, r.path, rv, d, func(typ string, raw json.RawMessage) error {
				id, o, err := r.decode(raw)
				if err != nil {
					return fmt.Errorf("decoding %s: %w", r.name, err)
				}
				s.mu.Lock()
				if typ == kube.Deleted {
					delete(s.objs[r.name], id)
				} else {
					s.objs[r.name][id] = o
				}
				s.mu.Unlock()
				s.changed()
				return nil
			})
			if errors.Is(err, kube.ErrGone) {
				break // list again
			}
			if err != nil && ctx.Err() == nil {
				s.setProblem(r.name, err)
				sleep(ctx, backoff)
				backoff = min(backoff*2, time.Minute)
				break // list again: changes may have been missed
			}
			backoff = time.Second
			if time.Since(began) < time.Second {
				sleep(ctx, time.Second) // ended at once: don't spin
			}
		}
	}
}

// watchVersion asks for the API's version every minute: it's the
// cluster's version, and when it fails for a minute the API is down.
func watchVersion(ctx context.Context, api API, s *store) {
	for ctx.Err() == nil {
		v, err := api.Version(ctx)
		now := time.Now()
		s.mu.Lock()
		if err == nil {
			if s.apiDown != "" {
				log.Printf("cluster: the API answers again")
			}
			s.version, s.apiDown, s.failing = v, "", time.Time{}
		} else if ctx.Err() == nil {
			if s.failing.IsZero() {
				s.failing = now
			}
			if now.Sub(s.failing) >= time.Minute && s.apiDown == "" {
				s.apiDown = err.Error()
				log.Printf("cluster: can't reach the API: %v", err)
			}
		}
		s.mu.Unlock()
		wait := time.Minute
		if err != nil {
			wait = 15 * time.Second
		}
		sleep(ctx, wait)
	}
}

func sleep(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}
