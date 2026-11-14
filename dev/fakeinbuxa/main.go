// fakeinbuxa runs the stand-in inbuxa sign-in for local development:
// `make dev` starts it on 127.0.0.1:8151. Never deploy it.
package main

import (
	"flag"
	"log"
	"net/http"
	"time"

	"github.com/cairnobs/cairnobs/internal/fakeinbuxa"
)

func main() {
	listen := flag.String("listen", "127.0.0.1:8151", "address (loopback only)")
	redirect := flag.String("redirect", "http://localhost:8150/auth/inbuxa/callback", "obs-beacond's registered callback")
	flag.Parse()
	base := "http://" + *listen
	f := fakeinbuxa.New("cairn-obs", *redirect)
	log.Printf("fake inbuxa on %s (development only)", base)
	srv := &http.Server{Addr: *listen, Handler: f.Handler(base), ReadHeaderTimeout: 5 * time.Second}
	log.Fatal(srv.ListenAndServe())
}
