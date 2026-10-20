// Command package-assets builds the pk3 bundles, manifest.json and mapcycle.txt from an HLDS tree.
package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"

	"cs-duds/backend/internal/assets"
)

func main() {
	content := flag.String("content", "", "HLDS tree (holds valve/ and cstrike/)")
	mapsFile := flag.String("maps", "", "maps.txt: one map name per line")
	out := flag.String("out", "", "output directory")
	flag.Parse()
	if *content == "" || *mapsFile == "" || *out == "" {
		flag.Usage()
		os.Exit(2)
	}
	raw, err := os.ReadFile(*mapsFile)
	if err != nil {
		fail(err)
	}
	var maps []string
	for _, l := range strings.Split(string(raw), "\n") {
		if l = strings.TrimSpace(l); l != "" && !strings.HasPrefix(l, "#") && !strings.HasPrefix(l, "//") {
			maps = append(maps, l)
		}
	}
	if _, err := assets.Package(*content, maps, *out); err != nil {
		var me *assets.MissingError
		if errors.As(err, &me) {
			fmt.Fprint(os.Stderr, me.Error())
			os.Exit(1)
		}
		fail(err)
	}
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "package-assets:", err)
	os.Exit(1)
}
