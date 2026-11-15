// SPDX-FileCopyrightText: 2026 Coffey Labs LLC
// SPDX-License-Identifier: AGPL-3.0-or-later

// zipfiles writes a zip for the Windows release: the same bytes from the
// same files every time (fixed times, the order given), as the release's
// other files are.
//
//	go run ./scripts/zipfiles OUT.zip TIME name=path ...
//
// TIME is an RFC 3339 time for every entry (the tagged commit's).
package main

import (
	"archive/zip"
	"io"
	"log"
	"os"
	"strings"
	"time"
)

func main() {
	log.SetFlags(0)
	if len(os.Args) < 4 {
		log.Fatal("usage: zipfiles OUT.zip TIME name=path ...")
	}
	at, err := time.Parse(time.RFC3339, os.Args[2])
	if err != nil {
		log.Fatal(err)
	}
	f, err := os.Create(os.Args[1])
	if err != nil {
		log.Fatal(err)
	}
	z := zip.NewWriter(f)
	for _, a := range os.Args[3:] {
		name, path, ok := strings.Cut(a, "=")
		if !ok {
			log.Fatalf("%q: want name=path", a)
		}
		w, err := z.CreateHeader(&zip.FileHeader{Name: name, Method: zip.Deflate, Modified: at.UTC()})
		if err != nil {
			log.Fatal(err)
		}
		src, err := os.Open(path)
		if err != nil {
			log.Fatal(err)
		}
		if _, err := io.Copy(w, src); err != nil {
			log.Fatal(err)
		}
		src.Close()
	}
	if err := z.Close(); err != nil {
		log.Fatal(err)
	}
	if err := f.Close(); err != nil {
		log.Fatal(err)
	}
}
