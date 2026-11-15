package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/cairnobs/cairnobs/internal/agentlink"
	"github.com/cairnobs/cairnobs/internal/jointoken"
)

// serviceUser runs the agent; join hands it the state it writes.
const serviceUser = "obs-beacon"

// runJoin is `obs-beacon-agent join [-force] <token>`: it joins this host to
// the obs-beacond the token came from (Hosts, Add host) and writes
// agent.yaml. install.sh runs it; it can be run by hand after installing
// the package.
func runJoin(args []string) error {
	fs := flag.NewFlagSet("join", flag.ExitOnError)
	cfgPath := fs.String("config", defaultConfig, "the config file to write")
	stateDir := fs.String("state-dir", defaultStateDir, "where the certificate and key go")
	force := fs.Bool("force", false, "replace an existing config file")
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "usage: obs-beacon-agent join [-force] <token>")
		fs.PrintDefaults()
	}
	_ = fs.Parse(args)
	if fs.NArg() != 1 {
		fs.Usage()
		os.Exit(2)
	}
	tok, err := jointoken.Parse(fs.Arg(0))
	if err != nil {
		return err
	}
	if _, err := os.Stat(*cfgPath); err == nil && !*force {
		return fmt.Errorf("%s exists: this host is set up already (-force replaces it)", *cfgPath)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	name, err := agentlink.Join(ctx, tok.Server, tok.CAFingerprint, tok.Secret, *stateDir)
	if err != nil {
		return err
	}
	uid, gid, owned := serviceIDs()
	if owned {
		if err := chownTree(*stateDir, uid, gid); err != nil {
			return err
		}
	}
	if err := writeJoinedConfig(*cfgPath, tok, name, *stateDir, gid, owned); err != nil {
		return err
	}
	fmt.Printf("joined as %s; wrote %s\n", name, *cfgPath)
	return nil
}

// serviceIDs is the service user's uid and gid, when running as root and the
// user exists (the package makes it).
func serviceIDs() (uid, gid int, ok bool) {
	if os.Geteuid() != 0 {
		return 0, 0, false
	}
	u, err := user.Lookup(serviceUser)
	if err != nil {
		return 0, 0, false
	}
	uid, err1 := strconv.Atoi(u.Uid)
	gid, err2 := strconv.Atoi(u.Gid)
	return uid, gid, err1 == nil && err2 == nil
}

func chownTree(dir string, uid, gid int) error {
	return filepath.WalkDir(dir, func(p string, _ os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		return os.Lchown(p, uid, gid)
	})
}

// writeJoinedConfig writes agent.yaml for a joined host: root:obs-beacon 0640,
// like a config written by hand.
func writeJoinedConfig(p string, tok jointoken.Token, name, stateDir string, gid int, owned bool) error {
	var b strings.Builder
	fmt.Fprintf(&b, "# Written by `obs-beacon-agent join` on %s. See the example in\n", time.Now().UTC().Format(time.DateOnly))
	fmt.Fprintf(&b, "# %s for everything else it can say.\n\n", examplesDir)
	fmt.Fprintf(&b, "server: %q\n", tok.Server)
	fmt.Fprintf(&b, "ca_fingerprint: %q\n", tok.CAFingerprint)
	fmt.Fprintf(&b, "host: %s\n", name)
	if stateDir != defaultStateDir {
		fmt.Fprintf(&b, "state_dir: %q\n", stateDir)
	}
	b.WriteString("interval: 15s\n")
	if hasHelper {
		// The helper runs with or without Docker: it also takes the map sample.
		b.WriteString("helper_socket: /run/obs-beacon/helper.sock  # the root helper: containers and the map sample\n")
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(p), "."+filepath.Base(p)+".*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.WriteString(b.String()); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o640); err != nil {
		tmp.Close()
		return err
	}
	if owned {
		if err := tmp.Chown(0, gid); err != nil {
			tmp.Close()
			return err
		}
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), p); err != nil {
		return errors.Join(fmt.Errorf("writing %s", p), err)
	}
	return nil
}
