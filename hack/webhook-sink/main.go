// Command webhook-sink is a minimal HTTP receiver for manually verifying
// alert delivery against a live stack (see /docs/phase-3-runbook.md) when
// a real Slack workspace/PagerDuty account isn't available -- logs every
// received POST body to stdout and returns 200. Not part of any
// docker-compose service by default; run it standalone when you need it.
package main

import (
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"time"
)

func main() {
	addr := flag.String("addr", ":9099", "listen address")
	flag.Parse()

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "reading body: "+err.Error(), http.StatusBadRequest)
			return
		}
		fmt.Printf("[%s] %s %s\n%s\n\n", time.Now().UTC().Format(time.RFC3339), r.Method, r.URL.Path, body)
		w.WriteHeader(http.StatusOK)
	})

	log.Printf("webhook-sink listening on %s", *addr)
	log.Fatal(http.ListenAndServe(*addr, nil))
}
