package main

import (
	"context"
	"log"
	"time"

	"github.com/cairnobs/cairnobs/internal/winevent"
)

// watchEvents reads the event log at start and then every
// winevent.Every (docs/windows.md, step 7).
func watchEvents(ctx context.Context) *winevent.Watch {
	w := &winevent.Watch{}
	go func() {
		logged := ""
		for {
			e := winevent.Read(time.Now())
			if e.Error != logged {
				logged = e.Error
				if e.Error != "" {
					log.Printf("event log: %s", e.Error)
				}
			}
			w.Set(e)
			select {
			case <-ctx.Done():
				return
			case <-time.After(winevent.Every):
			}
		}
	}()
	return w
}
