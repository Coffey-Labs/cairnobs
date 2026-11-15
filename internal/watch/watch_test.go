package watch

import (
	"context"

	"errors"
	"github.com/cairnobs/cairnobs/internal/maint"
	"strings"
	"testing"
	"time"

	"github.com/cairnobs/cairnobs/internal/notify"
)

type spy struct{ got []notify.Notice }

func (s *spy) ID() string   { return "spy" }
func (s *spy) Type() string { return "spy" }
func (s *spy) Min() string  { return "warning" }
func (s *spy) Send(_ context.Context, n notify.Notice) error {
	s.got = append(s.got, n)
	return nil
}

func watch(t0 time.Time, publicUp *bool) (*Watch, *spy) {
	s := &spy{}
	w := &Watch{cfg: Config{After: 3 * time.Minute, PublicURL: "https://beacon.test/healthz"}, host: "node1", chans: []notify.Channel{s}, lastOK: t0}
	w.probe = func(context.Context) error {
		if *publicUp {
			return nil
		}
		return errors.New("connection refused")
	}
	return w, s
}

func TestBeaconDownAndBack(t *testing.T) {
	ctx := context.Background()
	t0 := time.Now()
	up := false
	w, s := watch(t0, &up)
	w.Failed(ctx, t0.Add(2*time.Minute))
	if len(s.got) != 0 {
		t.Fatal("alerted before `after`")
	}
	w.Failed(ctx, t0.Add(4*time.Minute))
	w.Failed(ctx, t0.Add(5*time.Minute)) // once, not every report
	if len(s.got) != 1 || s.got[0].Kind != "firing" || s.got[0].Severity != "critical" || !strings.Contains(s.got[0].Detail, "node1 hasn't reached obs-beacond for 4 min, and https://beacon.test/healthz doesn't answer either (connection refused)") {
		t.Fatalf("down: %+v", s.got)
	}
	w.Reported(ctx, t0.Add(20*time.Minute))
	w.Reported(ctx, t0.Add(21*time.Minute))
	if len(s.got) != 2 || s.got[1].Kind != "resolved" || !strings.Contains(s.got[1].Detail, "out for about 20 min") {
		t.Fatalf("back: %+v", s.got)
	}
}

func TestOnlyThisHostsLinkIsQuiet(t *testing.T) {
	ctx := context.Background()
	t0 := time.Now()
	up := true // the public page answers: obs-beacond is fine, this host's path isn't
	w, s := watch(t0, &up)
	w.Failed(ctx, t0.Add(10*time.Minute))
	if len(s.got) != 0 {
		t.Fatalf("alerted though obs-beacond is up: %+v", s.got)
	}
}

func TestNeedsURLAndChannel(t *testing.T) {
	if _, err := New(Config{}, "node1", time.Now()); err == nil {
		t.Fatal("no public_url accepted")
	}
	if _, err := New(Config{PublicURL: "https://beacon.test/healthz"}, "node1", time.Now()); err == nil {
		t.Fatal("no channels accepted")
	}
}

func TestQuietDuringBeaconHostsReboot(t *testing.T) {
	ctx := context.Background()
	up := false
	win := maint.Window{Name: "node3 reboot", Daily: "05:30", Timezone: "Europe/Berlin", Duration: 20 * time.Minute}
	if err := win.Check(); err != nil {
		t.Fatal(err)
	}
	berlin, _ := time.LoadLocation("Europe/Berlin")
	t0 := time.Date(2026, 10, 16, 5, 29, 0, 0, berlin)
	w, s := watch(t0, &up)
	w.cfg.Quiet = []maint.Window{win}
	w.Failed(ctx, t0.Add(10*time.Minute)) // 05:39, rebooting: quiet
	if len(s.got) != 0 {
		t.Fatalf("alerted in the window: %+v", s.got)
	}
	w.Failed(ctx, t0.Add(22*time.Minute)) // 05:51, window closed, still down: alert
	if len(s.got) != 1 {
		t.Fatalf("after the window: %+v", s.got)
	}
}
