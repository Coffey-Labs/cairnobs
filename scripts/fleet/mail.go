package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/cairnobs/cairnobs/internal/mailmon/mailfake"
)

// The demo's mail server (fleet run -mail DIR): a stand-in on loopback for
// the Mail page's round trip, served by fleet-0048. Healthy on purpose,
// with deliveries taking a few seconds and a small outgoing queue, so the
// page shows what normal looks like.
const mailPort = 58440 // SMTP; IMAP and JMAP on the next two

func serveMail(ctx context.Context, dir string) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		log.Fatal(err)
	}
	const pw = "fleet-demo-mail-password"
	f, err := mailfake.Start("monitor@mail.example", pw,
		fmt.Sprintf("127.0.0.1:%d", mailPort), fmt.Sprintf("127.0.0.1:%d", mailPort+1), fmt.Sprintf("127.0.0.1:%d", mailPort+2))
	if err != nil {
		log.Fatal(err)
	}
	go func() { <-ctx.Done(); f.Close() }()
	// Deliveries take 1.5-6 s, and the queue holds a few messages a
	// receiving server asked to try again later.
	go func() {
		t := time.NewTicker(30 * time.Second)
		defer t.Stop()
		for k := 0; ; k++ {
			f.SetDelay(1500*time.Millisecond + time.Duration(k*7919%4500)*time.Millisecond)
			now := time.Now()
			var q []time.Time
			for i := range 3 + k%6 {
				q = append(q, now.Add(-time.Duration(2+i*4)*time.Minute))
			}
			f.SetQueue(q)
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
		}
	}()
	pf := filepath.Join(dir, "password")
	if err := os.WriteFile(pf, []byte(pw+"\n"), 0o600); err != nil {
		log.Fatal(err)
	}
	yaml := fmt.Sprintf(`mail:
  account: %s
  password_file: %s
  server: mail.example
  smtp: %s
  imap: %s
  url: %s
  client_id: cairn-obs
  redirect_url: https://obs.example/never-visited
  host: %s
  every: 1m
  wait: 45s
  stand_in: true
`, f.User, pf, f.SMTPAddr, f.IMAPAddr, f.URL, hostName(48))
	p := filepath.Join(dir, "mail.yaml")
	if err := os.WriteFile(p, []byte(yaml), 0o600); err != nil {
		log.Fatal(err)
	}
	log.Printf("fleet: mail server on 127.0.0.1:%d-%d; %s has the mail: section for obs-beacond.yaml", mailPort, mailPort+2, p)
}
