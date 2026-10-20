// Command package-assets builds the pk3 bundles, manifest.json and mapcycle.txt from an HLDS tree.
package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"cs-duds/backend/internal/assets"
)

func main() {
	content := flag.String("content", "", "HLDS tree (holds valve/ and cstrike/)")
	mapsFile := flag.String("maps", "", "maps.txt: one map name per line")
	out := flag.String("out", "", "output directory")
	extra := flag.String("extra", "", "optional cstrike/-shaped tree of community maps: every maps/*.bsp in it is offered")
	exclude := flag.String("exclude", "", "optional file of extra map names to leave out, one per line")
	flag.Parse()
	if *content == "" || *mapsFile == "" || *out == "" {
		flag.Usage()
		os.Exit(2)
	}
	maps := readList(*mapsFile)
	var extraMaps []string
	if *extra != "" {
		skip := map[string]bool{}
		for _, m := range maps {
			skip[m] = true
		}
		if *exclude != "" {
			for _, m := range readList(*exclude) {
				skip[m] = true
			}
		}
		bsps, _ := filepath.Glob(filepath.Join(*extra, "maps", "*.bsp"))
		for _, b := range bsps {
			m := strings.ToLower(strings.TrimSuffix(filepath.Base(b), filepath.Ext(b)))
			if !skip[m] && validName.MatchString(m) {
				extraMaps = append(extraMaps, m)
			}
		}
		sort.Strings(extraMaps)
	}
	_, skipped, err := assets.PackageAll(*content, maps, *extra, extraMaps, *out)
	if len(skipped) > 0 {
		gone := map[string]bool{}
		for _, s := range skipped {
			gone[s.Map] = true
		}
		fmt.Fprintf(os.Stderr, "package-assets: %d of %d extra maps skipped (missing files):\n", len(gone), len(extraMaps))
		fmt.Fprint(os.Stderr, (&assets.MissingError{Missing: skipped}).Error())
	}
	if err != nil {
		var me *assets.MissingError
		if errors.As(err, &me) {
			fmt.Fprint(os.Stderr, me.Error())
			os.Exit(1)
		}
		fail(err)
	}
}

// validName: the room catalog only takes names like this (room.nameRe), so anything else is never offered.
var validName = regexp.MustCompile(`^[a-z0-9_]+$`)

func readList(file string) (out []string) {
	raw, err := os.ReadFile(file)
	if err != nil {
		fail(err)
	}
	for _, l := range strings.Split(string(raw), "\n") {
		if l = strings.TrimSpace(l); l != "" && !strings.HasPrefix(l, "#") && !strings.HasPrefix(l, "//") {
			out = append(out, l)
		}
	}
	return out
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "package-assets:", err)
	os.Exit(1)
}
