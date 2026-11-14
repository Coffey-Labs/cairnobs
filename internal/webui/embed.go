// Package webui holds the built web interface, embedded into obs-beacond.
// `make web` builds web/ and copies the result into dist/; until then dist/
// holds only .keep and obs-beacond serves a "not built" page.
package webui

import "embed"

//go:embed all:dist
var Files embed.FS
