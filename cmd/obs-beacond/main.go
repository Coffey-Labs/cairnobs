// obs-beacond is the Cairn OBS server: it takes reports from the agents, judges
// them, and serves the API and the web interface.
//
//	obs-beacond -config /etc/obs-beacon/obs-beacond.yaml   run
//	obs-beacond token                              make a token for a new agent
//	obs-beacond ca [-config ...]                   the agent CA's fingerprint, for agents' ca_fingerprint
//	obs-beacond store copy -from <file> [-config ...]  move a SQLite install onto PostgreSQL, once
package main

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/cairnobs/cairnobs/internal/pki"
	"github.com/cairnobs/cairnobs/internal/server"
	"github.com/cairnobs/cairnobs/internal/state"
	"github.com/cairnobs/cairnobs/internal/version"
)

func main() {
	log.SetFlags(0)
	if len(os.Args) > 1 && os.Args[1] == "token" {
		tok := server.NewAgentToken()
		fmt.Printf("token (goes in the agent's token_file):\n  %s\n", tok)
		fmt.Printf("token_sha256 (goes in obs-beacond's agents list):\n  %s\n", server.HashToken(tok))
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "ca" {
		if err := printCA(os.Args[2:]); err != nil {
			log.Fatalf("obs-beacond ca: %v", err)
		}
		return
	}
	if len(os.Args) > 2 && os.Args[1] == "store" && os.Args[2] == "copy" {
		if err := storeCopy(os.Args[3:]); err != nil {
			log.Fatalf("obs-beacond store copy: %v", err)
		}
		return
	}
	cfgPath := flag.String("config", "/etc/obs-beacon/obs-beacond.yaml", "config file")
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Parse()
	if *showVersion {
		fmt.Println(version.Version)
		return
	}
	cfg, err := server.LoadConfig(*cfgPath)
	if err != nil {
		log.Fatalf("obs-beacond: %v", err)
	}
	if cfg.DevSignin {
		log.Printf("obs-beacond: development sign-in is ON (loopback only)")
	}
	srv, err := server.New(cfg)
	if err != nil {
		log.Fatalf("obs-beacond: %v", err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	err = srv.ListenAndServe(ctx)
	_ = srv.Close()
	if err != nil {
		log.Fatalf("obs-beacond: %v", err)
	}
}

// printCA prints the agent CA's fingerprint from the copy obs-beacond writes
// next to its database. It never opens the database, so running it as root
// can't leave root-owned files behind for the service to trip over.
func printCA(args []string) error {
	fs := flag.NewFlagSet("ca", flag.ExitOnError)
	cfgPath := fs.String("config", "/etc/obs-beacon/obs-beacond.yaml", "config file")
	_ = fs.Parse(args)
	cfg, err := server.LoadConfig(*cfgPath)
	if err != nil {
		return err
	}
	p := server.AgentCAFile(cfg)
	if p == "" || cfg.AgentListen == "" || cfg.AgentTLS.Off {
		return fmt.Errorf("%s has no TLS agent listener (it needs agent_listen and database, without agent_tls.off)", *cfgPath)
	}
	b, err := os.ReadFile(p)
	if os.IsNotExist(err) {
		return fmt.Errorf("%s doesn't exist yet: obs-beacond makes the agent CA when it first starts", p)
	}
	if err != nil {
		return err
	}
	blk, _ := pem.Decode(b)
	if blk == nil {
		return fmt.Errorf("%s: no certificate", p)
	}
	c, err := x509.ParseCertificate(blk.Bytes)
	if err != nil {
		return fmt.Errorf("%s: %w", p, err)
	}
	fmt.Printf("ca_fingerprint (goes in every agent's agent.yaml):\n  %s\n", pki.Fingerprint(c))
	fmt.Printf("certificate: %s, valid until %s\n", p, c.NotAfter.UTC().Format("2006-01-02"))
	return nil
}

// storeCopy moves a SQLite install onto PostgreSQL: run once, with
// obs-beacond stopped, into an empty database. The PostgreSQL side comes
// from the config (its database and database_password_file), so the
// password is never on the command line.
func storeCopy(args []string) error {
	fs := flag.NewFlagSet("store copy", flag.ExitOnError)
	from := fs.String("from", "", "the SQLite file to copy, e.g. /var/lib/obs-beacon/beacon.db")
	cfgPath := fs.String("config", "/etc/obs-beacon/obs-beacond.yaml", "the config that names the PostgreSQL database")
	_ = fs.Parse(args)
	if *from == "" {
		return fmt.Errorf("-from: the SQLite file to copy")
	}
	cfg, err := server.LoadConfig(*cfgPath)
	if err != nil {
		return err
	}
	if !state.IsPostgres(cfg.Database) {
		return fmt.Errorf("%s: database is %q; set it to the postgres:// URL to copy into", *cfgPath, cfg.Database)
	}
	copied, err := state.Copy(*from, cfg.Database, state.Options{PasswordFile: cfg.DatabasePasswordFile})
	if err != nil {
		return err
	}
	var total int64
	for _, c := range copied {
		fmt.Printf("  %-16s %8d rows\n", c.Table, c.Rows)
		total += c.Rows
	}
	fmt.Printf("Copied %d rows; the counts match. Start obs-beacond with this config and the same state_key_file.\n", total)
	return nil
}
