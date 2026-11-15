//go:build !windows

package units

import (
	"context"
	"fmt"
	"os/exec"

	"github.com/cairnobs/cairnobs/internal/report"
)

// Read asks systemd about the named units, in one call.
func Read(ctx context.Context, names []string) ([]report.Unit, error) {
	if len(names) == 0 {
		return nil, nil
	}
	args := []string{"show", "--timestamp=unix", "-p", "Id,LoadState,ActiveState,SubState,NRestarts,ActiveEnterTimestamp"}
	for _, n := range names {
		args = append(args, Name(n))
	}
	out, err := exec.CommandContext(ctx, "systemctl", args...).Output()
	if err != nil {
		return nil, fmt.Errorf("systemctl show: %w", err)
	}
	return parse(out), nil
}

// Running lists the services running now, by name, leaving out the
// per-user and per-session instances that come and go with logins.
func Running(ctx context.Context) ([]string, error) {
	out, err := exec.CommandContext(ctx, "systemctl", "list-units", "--type=service", "--state=running", "--no-legend", "--plain", "--no-pager").Output()
	if err != nil {
		return nil, fmt.Errorf("systemctl list-units: %w", err)
	}
	return parseRunning(out), nil
}
