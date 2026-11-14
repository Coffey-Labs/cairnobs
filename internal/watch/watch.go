// Package watch is "who watches Cairn OBS": an agent on another host notices
// when obs-beacond itself is gone and says so through its own alert channels,
// since obs-beacond can't report its own death.
package watch

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/cairnobs/cairnobs/internal/maint"
	"github.com/cairnobs/cairnobs/internal/notify"
	"github.com/cairnobs/cairnobs/internal/version"
)

// Config is the agent's `watch_server` section.
type Config struct {
	After     time.Duration   `yaml:"after"`      // reports failing this long before looking closer; default 3m
	PublicURL string          `yaml:"public_url"` // obs-beacond's health page by its public name, e.g. https://beacon.example.org/healthz
	Channels  []notify.Config `yaml:"channels"`   // same format as obs-beacond's alerts.channels
	Quiet     []maint.Window  `yaml:"quiet"`      // obs-beacond's host's own maintenance (its nightly reboot)
}

// Watch decides when obs-beacond is down. Reports failing alone isn't enough:
// that may be this host's link. obs-beacond is down when this host can't
// report AND its public health page doesn't answer either; otherwise
// obs-beacond is up and alerts about this host going quiet by itself.
type Watch struct {
	cfg      Config
	host     string
	chans    []notify.Channel
	probe    func(ctx context.Context) error
	lastOK   time.Time
	down     time.Time // when the outage was announced; zero when all is well
	lastDown time.Time // the last report that got through before it
}

// New makes a watch for this host, started now.
func New(cfg Config, host string, now time.Time) (*Watch, error) {
	if cfg.After == 0 {
		cfg.After = 3 * time.Minute
	}
	if cfg.PublicURL == "" {
		return nil, errors.New("watch_server: public_url is required")
	}
	if len(cfg.Channels) == 0 {
		return nil, errors.New("watch_server: at least one channel is required")
	}
	for i := range cfg.Quiet {
		if err := cfg.Quiet[i].Check(); err != nil {
			return nil, fmt.Errorf("watch_server: %w", err)
		}
	}
	w := &Watch{cfg: cfg, host: host, lastOK: now}
	for _, c := range cfg.Channels {
		ch, err := notify.New(c)
		if err != nil {
			return nil, fmt.Errorf("watch_server: %w", err)
		}
		w.chans = append(w.chans, ch)
	}
	client := &http.Client{Timeout: 10 * time.Second}
	w.probe = func(ctx context.Context) error {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, cfg.PublicURL, nil)
		if err != nil {
			return err
		}
		req.Header.Set("User-Agent", "obs-beacon-agent/"+version.Version)
		resp, err := client.Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		if resp.StatusCode != http.StatusOK || !strings.HasPrefix(string(b), "ok") {
			return fmt.Errorf("HTTP %d", resp.StatusCode)
		}
		return nil
	}
	return w, nil
}

// Reported is called after every report that reached obs-beacond.
func (w *Watch) Reported(ctx context.Context, now time.Time) {
	w.lastOK = now
	if !w.down.IsZero() {
		w.send(ctx, notify.Notice{Kind: "resolved", Severity: "critical", Title: "Cairn OBS is down",
			Detail: fmt.Sprintf("%s reaches obs-beacond again; it was out for about %s.", w.host, human(now.Sub(w.lastDown))),
			Host:   w.host, Since: w.down})
		w.down = time.Time{}
	}
}

// Failed is called after a report that didn't reach obs-beacond.
func (w *Watch) Failed(ctx context.Context, now time.Time) {
	if !w.down.IsZero() || now.Sub(w.lastOK) < w.cfg.After || maint.AnyActive(w.cfg.Quiet, now) {
		return // still down after the window closes: it alerts then
	}
	pctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	err := w.probe(pctx)
	cancel()
	if err == nil {
		return // obs-beacond is up; it will say this host went quiet
	}
	if w.send(ctx, notify.Notice{Kind: "firing", Severity: "critical", Title: "Cairn OBS is down",
		Detail: fmt.Sprintf("%s hasn't reached obs-beacond for %s, and %s doesn't answer either (%s). Nothing else will alert until it's back.",
			w.host, human(now.Sub(w.lastOK)), w.cfg.PublicURL, plain(err)),
		Host: w.host, Since: w.lastOK}) {
		w.down, w.lastDown = now, w.lastOK
	}
}

// send tells every channel that takes the severity; true if at least one
// took it (otherwise the next report tries again).
func (w *Watch) send(ctx context.Context, n notify.Notice) bool {
	ok := false
	for _, ch := range w.chans {
		if notify.Rank(n.Severity) < notify.Rank(ch.Min()) {
			continue
		}
		sctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		err := ch.Send(sctx, n)
		cancel()
		if err != nil {
			log.Printf("watch: %s to %s failed: %v", n.Kind, ch.ID(), err)
			continue
		}
		log.Printf("watch: sent %s %q to %s", n.Kind, n.Title, ch.ID())
		ok = true
	}
	return ok
}

// human is a duration as a person says it: 45 s, 3 min, 2 h 5 min.
func human(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%d s", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%d min", int(d.Minutes()))
	}
	return fmt.Sprintf("%d h %d min", int(d.Hours()), int(d.Minutes())%60)
}

// plain keeps the part of a network error a person needs.
func plain(err error) string {
	var ue *url.Error
	if errors.As(err, &ue) {
		err = ue.Err
	}
	var oe *net.OpError
	if errors.As(err, &oe) && oe.Err != nil {
		err = oe.Err
		var se *os.SyscallError
		if errors.As(err, &se) {
			err = se.Err
		}
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "timed out"
	}
	return err.Error()
}
