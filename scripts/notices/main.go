// notices writes THIRD_PARTY_NOTICES.md: every third-party component that
// ships inside Cairn OBS's binaries, with its license text, as MIT, BSD and
// Apache require for binary distribution. The packages install the file next
// to LICENSE.
//
//	go run ./scripts/notices          rewrite THIRD_PARTY_NOTICES.md
//	go run ./scripts/notices -check   fail if it's out of date (make check)
//
// It reads what's already on disk: the Go module cache (`go list`) and
// web/node_modules (after npm ci). A license it can't name stops it.
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
)

const out = "THIRD_PARTY_NOTICES.md"

// binaries are the commands Cairn OBS ships.
var binaries = []string{"obs-beacond", "obs-beacon-agent"}

// webPackages are the npm packages whose code ends up in the web UI's bundle
// (everything else in web/package.json is a build tool). Found by building
// with --sourcemap and reading the client maps' sources; look again when a
// runtime dependency is added.
var webPackages = []string{"svelte", "@sveltejs/kit"}

type component struct {
	name, version, license, usedIn string
	files                          []licenseFile
}

type licenseFile struct{ name, text string }

// licenseNames are the files a component's notices are read from.
var licenseNames = regexp.MustCompile(`(?i)^(licen[cs]e|copying|notice|patents)([-._].*)?$`)

func main() {
	check := flag.Bool("check", false, "fail if "+out+" is out of date instead of writing it")
	flag.Parse()
	comps, err := collect()
	if err != nil {
		fmt.Fprintln(os.Stderr, "notices:", err)
		os.Exit(1)
	}
	doc := render(comps)
	if *check {
		have, _ := os.ReadFile(out)
		if !bytes.Equal(have, doc) {
			fmt.Fprintf(os.Stderr, "notices: %s is out of date; run `go run ./scripts/notices` and commit it\n", out)
			os.Exit(1)
		}
		return
	}
	if err := os.WriteFile(out, doc, 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "notices:", err)
		os.Exit(1)
	}
}

func collect() ([]component, error) {
	used := map[string][]string{} // module path@version → binaries
	dirs := map[string]string{}
	for _, b := range binaries {
		cmd := exec.Command("go", "list", "-deps", "-f", `{{with .Module}}{{if not .Main}}{{.Path}}@{{.Version}} {{.Dir}}{{end}}{{end}}`, "./cmd/"+b)
		cmd.Stderr = os.Stderr
		o, err := cmd.Output()
		if err != nil {
			return nil, fmt.Errorf("go list %s: %w", b, err)
		}
		for _, line := range strings.Split(strings.TrimSpace(string(o)), "\n") {
			mod, dir, ok := strings.Cut(line, " ")
			if !ok {
				continue
			}
			if !slices.Contains(used[mod], b) {
				used[mod] = append(used[mod], b)
			}
			dirs[mod] = dir
		}
	}
	var comps []component
	goroot, err := exec.Command("go", "env", "GOROOT").Output()
	if err != nil {
		return nil, err
	}
	std, err := readComponent("Go standard library", "", strings.TrimSpace(string(goroot)), strings.Join(binaries, ", "))
	if err != nil {
		return nil, err
	}
	comps = append(comps, std)
	mods := make([]string, 0, len(used))
	for m := range used {
		mods = append(mods, m)
	}
	sort.Strings(mods)
	for _, m := range mods {
		path, ver, _ := strings.Cut(m, "@")
		c, err := readComponent(path, ver, dirs[m], strings.Join(used[m], ", "))
		if err != nil {
			return nil, err
		}
		comps = append(comps, c)
	}
	for _, p := range webPackages {
		dir := filepath.Join("web", "node_modules", p)
		b, err := os.ReadFile(filepath.Join(dir, "package.json"))
		if err != nil {
			return nil, fmt.Errorf("%s: %w (run npm ci in web first)", p, err)
		}
		var pkg struct{ Version string }
		if err := json.Unmarshal(b, &pkg); err != nil {
			return nil, fmt.Errorf("%s: %w", p, err)
		}
		c, err := readComponent(p, pkg.Version, dir, "the web interface (inside obs-beacond)")
		if err != nil {
			return nil, err
		}
		comps = append(comps, c)
	}
	fonts, err := readFiles("web/static/fonts", "LICENSE.txt")
	if err != nil {
		return nil, err
	}
	comps = append(comps, component{name: "Overpass and Overpass Mono (fonts)", license: "OFL-1.1", usedIn: "the web interface (inside obs-beacond)", files: fonts})
	// The icons are Lucide's shapes, copied into web/src/lib/icons.ts rather
	// than installed, so their license is kept next to them.
	icons, err := readFiles("web/src/lib", "icons.LICENSE.txt")
	if err != nil {
		return nil, err
	}
	if lic := identify(icons[0].text); lic != "ISC AND MIT" && lic != "MIT AND ISC" {
		return nil, fmt.Errorf("web/src/lib/icons.LICENSE.txt: expected Lucide's ISC + Feather's MIT, found %q", lic)
	}
	comps = append(comps, component{name: "Lucide (icon shapes)", license: "ISC AND MIT", usedIn: "the web interface (inside obs-beacond)", files: icons})
	return comps, nil
}

func readFiles(dir string, names ...string) ([]licenseFile, error) {
	var files []licenseFile
	for _, n := range names {
		b, err := os.ReadFile(filepath.Join(dir, n))
		if err != nil {
			return nil, err
		}
		files = append(files, licenseFile{n, strings.TrimSpace(strings.ReplaceAll(string(b), "\r\n", "\n"))})
	}
	return files, nil
}

func readComponent(name, version, dir, usedIn string) (component, error) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return component{}, fmt.Errorf("%s: %w", name, err)
	}
	var names []string
	for _, e := range ents {
		if !e.IsDir() && licenseNames.MatchString(e.Name()) {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	files, err := readFiles(dir, names...)
	if err != nil {
		return component{}, err
	}
	var main string
	for _, f := range files {
		if strings.HasPrefix(strings.ToUpper(f.name), "LICEN") || strings.HasPrefix(strings.ToUpper(f.name), "COPYING") {
			main = f.text
			break
		}
	}
	lic := identify(main)
	if main == "" || lic == "" {
		return component{}, fmt.Errorf("%s %s: can't tell its license from %v; look and add it to identify()", name, version, names)
	}
	return component{name: name, version: version, license: lic, usedIn: usedIn, files: files}, nil
}

// identify names a license from its text. Only the ones that are fine to
// ship inside an AGPL-3.0 program are known; anything else stops the build
// so a person looks.
func identify(t string) string {
	has := func(s string) bool { return strings.Contains(t, s) }
	var found []string
	if has("Apache License") && has("Version 2.0") {
		found = append(found, "Apache-2.0")
	}
	if has("Permission is hereby granted, free of charge") {
		found = append(found, "MIT")
	}
	if has("Redistribution and use in source and binary forms") {
		if has("Neither the name") || has("may be used to endorse or promote") {
			found = append(found, "BSD-3-Clause")
		} else {
			found = append(found, "BSD-2-Clause")
		}
	}
	if has("Permission to use, copy, modify, and/or distribute this software") {
		found = append(found, "ISC")
	}
	return strings.Join(found, " AND ")
}

func render(comps []component) []byte {
	var b bytes.Buffer
	b.WriteString("# Third-party notices\n\n")
	b.WriteString("Cairn OBS is licensed under the GNU Affero General Public License v3.0\n")
	b.WriteString("(LICENSE). Its binaries also contain the components below, each under\n")
	b.WriteString("its own license, reproduced in full. Generated by `go run ./scripts/notices`;\n")
	b.WriteString("don't edit by hand.\n\n")
	b.WriteString("| Component | Version | License | In |\n|---|---|---|---|\n")
	for _, c := range comps {
		fmt.Fprintf(&b, "| %s | %s | %s | %s |\n", c.name, orDash(c.version), c.license, c.usedIn)
	}
	for _, c := range comps {
		fmt.Fprintf(&b, "\n## %s", c.name)
		if c.version != "" {
			fmt.Fprintf(&b, " %s", c.version)
		}
		b.WriteString("\n")
		for _, f := range c.files {
			fmt.Fprintf(&b, "\n%s:\n\n~~~~text\n%s\n~~~~\n", f.name, f.text)
		}
	}
	return b.Bytes()
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
