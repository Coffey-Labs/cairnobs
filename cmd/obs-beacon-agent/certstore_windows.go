package main

import (
	"context"
	"log"
	"time"

	"github.com/cairnobs/cairnobs/internal/certfiles"
)

// watchCertStores reads the machine's certificate stores for the
// inventory (cert_stores; docs/windows.md), as the root helper reads
// certificate files on Linux: at start, then every certfiles.AskEvery.
// Nil when cert_stores is [].
func watchCertStores(ctx context.Context, stores *[]string) *certfiles.Watch {
	names := certfiles.StoreDefaults
	if stores != nil {
		names = *stores
	}
	if len(names) == 0 {
		return nil
	}
	w := &certfiles.Watch{}
	go func() {
		logged := ""
		for {
			list, err := certfiles.ReadStores(names)
			if err != nil && err.Error() != logged {
				logged = err.Error()
				log.Printf("certificate stores: %v", err)
			}
			w.Set(list) // what could be read, even when a store couldn't
			select {
			case <-ctx.Done():
				return
			case <-time.After(certfiles.AskEvery):
			}
		}
	}()
	return w
}
