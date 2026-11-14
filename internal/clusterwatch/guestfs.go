package clusterwatch

import (
	"context"
	"net/http"
	"sync"
	"time"

	"github.com/cairnobs/cairnobs/internal/kube"
)

// Guest file systems aren't an object to watch: KubeVirt asks each VM's
// guest agent when someone reads the filesystemlist subresource. The agent
// reads it for every running VM whose guest agent answers, every
// guestFSEvery, a few at a time, and keeps the answers under "guestfs" by
// namespace/name, beside the objects, so build stays a pure function.

// guestFSEvery is how often each VM's file systems are read. A var for the
// tests.
var guestFSEvery = 5 * time.Minute

const (
	guestFSAtOnce  = 4
	guestFSTimeout = 10 * time.Second
	guestFSRes     = "guest file systems"
)

func watchGuestFS(ctx context.Context, api API, s *store) {
	for ctx.Err() == nil && !s.allSynced() {
		sleep(ctx, time.Second) // the VMIs first
	}
	for ctx.Err() == nil {
		readGuestFS(ctx, api, s)
		sleep(ctx, guestFSEvery)
	}
}

// readGuestFS reads every running VM's file systems once.
func readGuestFS(ctx context.Context, api API, s *store) {
	var want []*virtualMachineInstance
	for _, i := range each[virtualMachineInstance](s.snapshot(), "virtualmachineinstances") {
		if i.Status.Phase == "Running" && i.agentConnected() {
			want = append(want, i)
		}
	}
	got := map[string]any{}
	var mu sync.Mutex
	var denied error
	sem := make(chan struct{}, guestFSAtOnce)
	var wg sync.WaitGroup
	for _, i := range want {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer func() { <-sem; wg.Done() }()
			c, cancel := context.WithTimeout(ctx, guestFSTimeout)
			defer cancel()
			fs := new(guestFS)
			err := api.Get(c, "/apis/subresources.kubevirt.io/v1/namespaces/"+i.Metadata.Namespace+"/virtualmachineinstances/"+i.Metadata.Name+"/filesystemlist", fs)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				got[nsName(i.Metadata)] = fs
			case kube.Code(err) == http.StatusForbidden:
				denied = err
			}
			// Anything else (the guest agent went away, the VM is
			// stopping) leaves the VM without file systems this time.
		}()
	}
	wg.Wait()
	if ctx.Err() != nil {
		return
	}
	if len(want) > 0 {
		s.setProblem(guestFSRes, denied)
	}
	s.mu.Lock()
	s.objs["guestfs"] = got
	s.mu.Unlock()
	s.changed()
}
