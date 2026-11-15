//go:build !windows

package main

import (
	"context"

	"github.com/cairnobs/cairnobs/internal/certfiles"
)

// watchCertStores: certificate stores are Windows'; elsewhere the root
// helper reads certificate files.
func watchCertStores(context.Context, *[]string) *certfiles.Watch { return nil }
