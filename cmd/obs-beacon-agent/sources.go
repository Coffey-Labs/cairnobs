package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"regexp"
	"time"

	"github.com/cairnobs/cairnobs/internal/clusterwatch"
	"github.com/cairnobs/cairnobs/internal/flasharray"
	"github.com/cairnobs/cairnobs/internal/flashblade"
	"github.com/cairnobs/cairnobs/internal/nutanix"
	"github.com/cairnobs/cairnobs/internal/proxmox"
	"github.com/cairnobs/cairnobs/internal/purerest"
	"github.com/cairnobs/cairnobs/internal/report"
	"github.com/cairnobs/cairnobs/internal/version"
	"github.com/cairnobs/cairnobs/internal/vsphere"
)

// A host agent can read whole platforms beside its own host: a Proxmox VE
// cluster it's a node of, a vCenter or Prism Central it can reach
// (docs/cluster-watcher.md, steps 7 to 9), and any number of Pure
// FlashArrays (docs/pure.md). Each is sent as a cluster agent sends its
// cluster, as a source of its own named in every batch (docs/pure.md,
// decision 1): "mgmt1:vcsa-01". One of each platform; arrays as listed.

// sourceName is what a source may be called: a host name.
var sourceName = regexp.MustCompile(`^[a-z0-9]([a-z0-9.-]{0,61}[a-z0-9])?$`)

// namedSetup is one platform to read, by its source name.
type namedSetup struct {
	name  string
	setup func() (*platform, error)
}

// platformSetups is what cfg says to read. A name that isn't a host name,
// or one two platforms share, is an error, and nothing is read.
func platformSetups(cfg *config) ([]namedSetup, error) {
	var out []namedSetup
	if c := cfg.Proxmox; c != nil {
		c.Defaults()
		out = append(out, namedSetup{c.Name, func() (*platform, error) { return proxmoxPlatform(*c) }})
	}
	if c := cfg.VSphere; c != nil {
		c.Defaults()
		out = append(out, namedSetup{c.Name, func() (*platform, error) { return vspherePlatform(*c) }})
	}
	if c := cfg.Nutanix; c != nil {
		c.Defaults()
		out = append(out, namedSetup{c.Name, func() (*platform, error) { return nutanixPlatform(*c) }})
	}
	for i := range cfg.Pure {
		c := &cfg.Pure[i]
		c.Defaults()
		if c.Type == purerest.FlashBlade {
			out = append(out, namedSetup{c.Name, func() (*platform, error) { return flashbladePlatform(*c) }})
		} else {
			out = append(out, namedSetup{c.Name, func() (*platform, error) { return flasharrayPlatform(*c) }})
		}
	}
	seen := map[string]bool{}
	for _, n := range out {
		if !sourceName.MatchString(n.name) {
			return nil, fmt.Errorf("a platform's name %q isn't a host name (a-z, 0-9, dots and dashes): set name: in its section", n.name)
		}
		if seen[n.name] {
			return nil, fmt.Errorf("two platforms are both called %q: set name: in one of them", n.name)
		}
		seen[n.name] = true
	}
	return out, nil
}

// sourceNames are the names of what this agent reads, for its reports:
// never nil, so obs-beacond can tell "none" from an agent too old to say.
func sourceNames(ns []namedSetup) []string {
	out := make([]string, 0, len(ns))
	for _, n := range ns {
		out = append(out, n.name)
	}
	return out
}

// platform reads one platform: its records and usage, or an error when its
// API doesn't answer at all.
type platform struct {
	name  string // for the log
	every time.Duration
	read  func(ctx context.Context, now time.Time) ([]report.Workload, []report.Usage, error)
	// down is the last records with the platform's own record saying its
	// API doesn't answer.
	down func(last []report.Workload, err error) []report.Workload
}

func proxmoxPlatform(cfg proxmox.Config) (*platform, error) {
	cfg.Defaults()
	c, err := proxmox.New(cfg)
	if err != nil {
		return nil, err
	}
	rd := proxmox.NewReader(c)
	return &platform{name: "proxmox " + cfg.URL, every: cfg.Every, down: proxmox.Unreachable,
		read: func(ctx context.Context, now time.Time) ([]report.Workload, []report.Usage, error) {
			s, err := rd.Read(ctx, now)
			if err != nil {
				return nil, nil, err
			}
			recs, use := proxmox.Records(s)
			return recs, use, nil
		}}, nil
}

func vspherePlatform(cfg vsphere.Config) (*platform, error) {
	cfg.Defaults()
	c, err := vsphere.New(cfg)
	if err != nil {
		return nil, err
	}
	return &platform{name: "vsphere " + c.Name(), every: cfg.Every,
		down: func(last []report.Workload, err error) []report.Workload {
			return vsphere.Unreachable(c.Name(), last, err)
		},
		read: func(ctx context.Context, now time.Time) ([]report.Workload, []report.Usage, error) {
			s, err := c.Read(ctx, now)
			if err != nil {
				return nil, nil, err
			}
			recs, use := vsphere.Records(s)
			return recs, use, nil
		}}, nil
}

func nutanixPlatform(cfg nutanix.Config) (*platform, error) {
	cfg.Defaults()
	c, err := nutanix.New(cfg)
	if err != nil {
		return nil, err
	}
	return &platform{name: "nutanix " + c.Name(), every: cfg.Every,
		down: func(last []report.Workload, err error) []report.Workload {
			return nutanix.Unreachable(c.Name(), last, err)
		},
		read: func(ctx context.Context, now time.Time) ([]report.Workload, []report.Usage, error) {
			s, err := c.Read(ctx, now)
			if err != nil {
				return nil, nil, err
			}
			return nutanix.Records(s), nil, nil
		}}, nil
}

func flasharrayPlatform(cfg flasharray.Config) (*platform, error) {
	c, err := flasharray.New(cfg)
	if err != nil {
		return nil, err
	}
	return &platform{name: "flasharray " + c.Name(), every: cfg.Every,
		down: func(last []report.Workload, err error) []report.Workload {
			return flasharray.Unreachable(c.Name(), last, err)
		},
		read: func(ctx context.Context, now time.Time) ([]report.Workload, []report.Usage, error) {
			s, err := c.Read(ctx, now)
			if err != nil {
				return nil, nil, err
			}
			return flasharray.Records(s), flasharray.Usage(s), nil
		}}, nil
}

func flashbladePlatform(cfg flashblade.Config) (*platform, error) {
	c, err := flashblade.New(cfg)
	if err != nil {
		return nil, err
	}
	return &platform{name: "flashblade " + c.Name(), every: cfg.Every,
		down: func(last []report.Workload, err error) []report.Workload {
			return flashblade.Unreachable(c.Name(), last, err)
		},
		read: func(ctx context.Context, now time.Time) ([]report.Workload, []report.Usage, error) {
			s, err := c.Read(ctx, now)
			if err != nil {
				return nil, nil, err
			}
			return flashblade.Records(s), flashblade.Usage(s), nil
		}}, nil
}

// runPlatform sets the platform up (trying again every 5 minutes while its
// token or password isn't there yet), then reads it every so often and
// sends what changed.
func (a *agent) runPlatform(ctx context.Context, ns namedSetup) {
	var p *platform
	for {
		var err error
		if p, err = ns.setup(); err == nil {
			break
		}
		log.Printf("%v; trying again in 5m", err)
		select {
		case <-ctx.Done():
			return
		case <-time.After(5 * time.Minute):
		}
	}
	send := func(ctx context.Context, b report.Workloads) error {
		if !a.link.Ready() {
			return errors.New("not enrolled yet") // the report loop enrolls
		}
		b.Source = ns.name
		_, err := postWorkloads(ctx, a.link, a.cfg.Server, b) // actions come with its reports
		return err
	}
	bt := clusterwatch.NewBatcher(clusterwatch.Config{Name: a.cfg.Host, Agent: version.Version}, send)
	var recs []report.Workload
	var use []report.Usage
	var at, readAt time.Time
	failing := ""
	t := time.NewTicker(bt.Every())
	defer t.Stop()
	log.Printf("%s: reading every %s", p.name, p.every)
	for {
		now := time.Now()
		if now.Sub(readAt) >= p.every {
			readAt = now
			rctx, cancel := context.WithTimeout(ctx, min(p.every, 50*time.Second))
			r, u, err := p.read(rctx, now)
			cancel()
			switch {
			case ctx.Err() != nil:
				return
			case err != nil:
				if err.Error() != failing {
					failing = err.Error()
					log.Printf("%s: %v", p.name, err)
				}
				recs = p.down(recs, err)
			default:
				if failing != "" {
					log.Printf("%s: answers again", p.name)
					failing = ""
				}
				recs, use, at = r, u, now.Truncate(time.Second)
			}
		}
		if recs != nil {
			bt.Tick(ctx, recs, use, at, now)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
