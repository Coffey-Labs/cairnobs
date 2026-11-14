package server

// The web interface across versions (docs/scale-and-availability.md,
// Version skew). During a rolling upgrade a page from one version may ask
// any copy for one of its hashed files; every copy stores its own in the
// database when it starts, and serves from there what it doesn't carry.

import (
	"bytes"
	"io/fs"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/cairnobs/cairnobs/internal/state"
	"github.com/cairnobs/cairnobs/internal/version"
)

const immutableDir = "_app/immutable"

// shareAssets stores this build's hashed files for the other versions'
// copies. Only when copies share the database: alone, nothing else serves.
func (srv *Server) shareAssets() {
	if !srv.shared() || srv.ui == nil {
		return
	}
	files := map[string][]byte{}
	err := fs.WalkDir(srv.ui, immutableDir, func(p string, e fs.DirEntry, err error) error {
		if err != nil || e.IsDir() {
			return err
		}
		b, err := fs.ReadFile(srv.ui, p)
		files[p] = b
		return err
	})
	if err == nil {
		err = srv.db.PutAssets(version.Version, files, time.Now())
	}
	if err != nil {
		log.Printf("ui: can't share this version's files with the other copies: %v", err)
	}
}

// otherVersionsFile serves a hashed file this build doesn't have from the
// ones other versions stored, and says whether it did.
func (srv *Server) otherVersionsFile(w http.ResponseWriter, r *http.Request, p string) bool {
	if !srv.shared() || !strings.HasPrefix(p, immutableDir+"/") {
		return false
	}
	b, err := srv.db.Asset(p)
	if err != nil {
		if err != state.ErrNotFound {
			log.Printf("ui: %s: %v", p, err)
		}
		return false
	}
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	http.ServeContent(w, r, p, time.Time{}, bytes.NewReader(b))
	return true
}
