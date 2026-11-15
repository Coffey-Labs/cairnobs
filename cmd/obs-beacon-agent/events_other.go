//go:build !windows

package main

import (
	"context"

	"github.com/cairnobs/cairnobs/internal/winevent"
)

// watchEvents: the event log is Windows'.
func watchEvents(context.Context) *winevent.Watch { return nil }
