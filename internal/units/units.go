// Package units reads systemd units' state with `systemctl show`, which
// any user may run: no D-Bus library, no privileges.
package units

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/cairnobs/cairnobs/internal/report"
)

// Name adds ".service" when a unit is named without a type.
func Name(n string) string {
	if strings.Contains(n, ".") {
		return n
	}
	return n + ".service"
}

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

func parseRunning(out []byte) []string {
	var names []string
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) == 0 || !strings.HasSuffix(f[0], ".service") {
			continue
		}
		if strings.HasPrefix(f[0], "user@") || strings.HasPrefix(f[0], "user-runtime-dir@") {
			continue
		}
		names = append(names, f[0])
	}
	return names
}

// parse reads systemctl show's blank-line-separated blocks of key=value.
func parse(out []byte) []report.Unit {
	var units []report.Unit
	var u report.Unit
	flush := func() {
		if u.Name != "" {
			units = append(units, u)
		}
		u = report.Unit{}
	}
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			flush()
			continue
		}
		k, v, _ := strings.Cut(line, "=")
		switch k {
		case "Id":
			u.Name = v
		case "LoadState":
			u.Load = v
		case "ActiveState":
			u.Active = v
		case "SubState":
			u.Sub = v
		case "NRestarts":
			u.Restarts, _ = strconv.Atoi(v)
		case "ActiveEnterTimestamp":
			if sec, err := strconv.ParseInt(strings.TrimPrefix(v, "@"), 10, 64); err == nil && sec > 0 {
				u.Since = time.Unix(sec, 0).UTC()
			}
		}
	}
	flush()
	return units
}
