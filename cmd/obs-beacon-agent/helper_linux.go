package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"os/user"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/cairnobs/cairnobs/internal/certfiles"
	"github.com/cairnobs/cairnobs/internal/certreload"
	"github.com/cairnobs/cairnobs/internal/docker"
	"github.com/cairnobs/cairnobs/internal/libvirt"
	"github.com/cairnobs/cairnobs/internal/netmap"
	"github.com/cairnobs/cairnobs/internal/report"
	"github.com/cairnobs/cairnobs/internal/version"
)

// runHelper serves the read-only Docker gate on a unix socket. It runs as
// root (it needs the Docker socket); the agent reaches it through the
// socket's group.
func runHelper(args []string) error {
	fs := flag.NewFlagSet("helper", flag.ExitOnError)
	listen := fs.String("listen", "/run/obs-beacon/helper.sock", "unix socket to serve on")
	dockerSock := fs.String("docker", "/var/run/docker.sock", "Docker's socket")
	group := fs.String("group", "obs-beacon", "group allowed to use the socket (empty: owner only)")
	config := fs.String("config", "/etc/obs-beacon/agent.yaml", "the agent's config, read for cert_reload (the agent's word isn't taken)")
	_ = fs.Parse(args)

	_ = os.Remove(*listen)
	ln, err := net.Listen("unix", *listen)
	if err != nil {
		return err
	}
	mode := os.FileMode(0o600)
	if *group != "" {
		g, err := user.LookupGroup(*group)
		if err != nil {
			return err
		}
		gid, _ := strconv.Atoi(g.Gid)
		if err := os.Chown(*listen, -1, gid); err != nil {
			return err
		}
		mode = 0o660
	}
	if err := os.Chmod(*listen, mode); err != nil {
		return err
	}
	mux := http.NewServeMux()
	mux.Handle("/", docker.Gate(*dockerSock))
	var sampling sync.Mutex
	sampler := &netmap.Sampler{}
	mux.HandleFunc("GET "+docker.MapPath, func(w http.ResponseWriter, r *http.Request) {
		sampling.Lock() // one sample at a time
		m := sampler.Sample()
		sampling.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(m)
	})
	// Only root may read it; it says nothing a guest can't see.
	mux.HandleFunc("GET "+docker.MachinePath, func(w http.ResponseWriter, r *http.Request) {
		b, _ := os.ReadFile("/sys/class/dmi/id/product_uuid")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"uuid": report.MachineUUID(string(b))})
	})
	// libvirt's read-only socket: domains and their numbers, nothing more.
	var reading sync.Mutex
	mux.HandleFunc("GET "+docker.VMsPath, func(w http.ResponseWriter, r *http.Request) {
		reading.Lock()
		doms, err := libvirt.Read(libvirt.Sockets, "qemu:///system", 10*time.Second)
		reading.Unlock()
		out := map[string]any{"domains": doms}
		switch {
		case errors.Is(err, libvirt.ErrAbsent):
			out = map[string]any{"absent": true}
		case err != nil:
			out = map[string]any{"error": err.Error()}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(out)
	})
	// Certificate files: the certificates parsed out of the files the
	// agent's globs match, never a key, and nothing for a file without a
	// certificate in it.
	files := &certfiles.Reader{}
	mux.HandleFunc("GET "+docker.CertFilesPath, func(w http.ResponseWriter, r *http.Request) {
		globs := r.URL.Query()["glob"]
		if len(globs) > 64 {
			globs = globs[:64]
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"files": files.Read(globs)})
	})
	// The one thing it changes: reloading a unit for a renewed certificate,
	// only as agent.yaml's cert_reload allows, read here on each request
	// (docs/cert-rotation.md, step 2). One at a time.
	var reloading sync.Mutex
	mux.HandleFunc("POST "+docker.ReloadPath, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var req certreload.Request
		if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&req); err != nil {
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "bad request: " + err.Error()})
			return
		}
		reloading.Lock()
		defer reloading.Unlock()
		rules, err := certreload.ReadRules(*config)
		if err != nil {
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "can't read cert_reload: " + err.Error()})
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
		defer cancel()
		res, err := certreload.Do(ctx, rules, req, certreload.Systemd{}, time.Now())
		out := map[string]any{"result": res}
		if err != nil {
			out["error"] = err.Error()
			log.Printf("reload %s refused: %v", req.Unit, err)
		} else {
			log.Printf("reloaded %s for %s (%s)", req.Unit, res.File, res.Fingerprint)
		}
		_ = json.NewEncoder(w).Encode(out)
	})
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()
	log.Printf("obs-beacon-agent helper %s: read-only Docker gate, map sample, libvirt domains, certificate files and cert_reload on %s", version.Version, *listen)
	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
