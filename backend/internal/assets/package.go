// Package assets builds the client content bundles (pk3) from an HLDS tree.
package assets

import (
	"archive/zip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"
)

type Bundle struct {
	File   string `json:"file"` // "assets/<sha256>.pk3"
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

type Manifest struct {
	Base Bundle            `json:"base"`
	Maps map[string]Bundle `json:"maps"`
}

type Missing struct{ Map, Path, Why string }

type MissingError struct{ Missing []Missing }

func (e *MissingError) Error() string {
	var b strings.Builder
	for _, m := range e.Missing {
		fmt.Fprintf(&b, "%s: missing %s (%s)\n", m.Map, m.Path, m.Why)
	}
	return b.String()
}

var (
	dropDirs = map[string]bool{"addons": true, "dlls": true, "cl_dlls": true, "logs": true, "reslists": true}
	dropExts = map[string]bool{".so": true, ".dll": true, ".dylib": true, ".exe": true, ".nav": true}
	// the server's liblist.gam names dlls/mp.dll; the browser engine writes its own pointing at its wasm server lib
	dropFiles = map[string]bool{"liblist.gam": true}
	// Half-Life content CS never plays: the soundtrack and video, and the single-player voices
	dropPrefixes = []string{"media/", "sound/scientist/", "sound/barney/", "sound/hgrunt/", "sound/gman/", "sound/holo/",
		"sound/tride/", "sound/vox/", "sound/fvox/"}
	// textures most maps share stay in base.pk3; every other wad, and every sky, travels with the maps that use it
	coreWads = map[string]bool{"cstrike.wad": true, "halflife.wad": true, "decals.wad": true, "fonts.wad": true,
		"gfx.wad": true, "liquids.wad": true, "spraypaint.wad": true, "cached.wad": true}
	modelRef  = regexp.MustCompile(`(?i)\.(mdl|spr)$`)
	soundRef  = regexp.MustCompile(`(?i)\.wav$`)
	skyFaces  = []string{"up", "dn", "lf", "rt", "ft", "bk"}
	fixedTime = time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
)

// scanTree flattens valve/ then cstrike/ (which overrides) into lowercase game-relative keys.
func scanTree(contentDir string) (map[string]string, error) {
	t := map[string]string{}
	for _, game := range []string{"valve", "cstrike"} {
		root := filepath.Join(contentDir, game)
		if _, err := os.Stat(root); os.IsNotExist(err) {
			continue
		}
		err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			rel, _ := filepath.Rel(root, p)
			key := strings.ToLower(filepath.ToSlash(rel))
			if dropDirs[strings.SplitN(key, "/", 2)[0]] || dropExts[path.Ext(key)] || dropFiles[key] || dropped(key) {
				return nil
			}
			t[key] = p
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	if len(t) == 0 {
		return nil, fmt.Errorf("no files under %s/{valve,cstrike}", contentDir)
	}
	return t, nil
}

// Package builds base.pk3 + one pk3 per map + manifest.json + mapcycle.txt in outDir.
// It fails with *MissingError, writing nothing, if any map references a missing file.
func Package(contentDir string, maps []string, outDir string) (Manifest, error) {
	man, _, err := PackageAll(contentDir, maps, "", nil, outDir)
	return man, err
}

// PackageAll is Package plus extra maps from extraDir (a cstrike/-shaped tree of community maps). The base stays
// the stock tree only: an extra map's pk3 carries its own files plus every extra file it references (wads, sky,
// models, sounds; stock wins a name clash). Stock maps must resolve completely; an extra map that references a
// missing file is skipped and returned in skipped instead of failing the build.
func PackageAll(contentDir string, maps []string, extraDir string, extraMaps []string, outDir string) (Manifest, []Missing, error) {
	seen := map[string]bool{}
	for _, m := range append(append([]string{}, maps...), extraMaps...) {
		if m == "" || strings.ContainsAny(m, `/\`) || seen[m] {
			return Manifest{}, nil, fmt.Errorf("bad or duplicate map name %q", m)
		}
		seen[m] = true
	}
	t, err := scanTree(contentDir)
	if err != nil {
		return Manifest{}, nil, err
	}
	wadCache := map[string]map[string]struct{}{}
	var missing []Missing
	stockDeps := map[string][]string{}
	for _, m := range maps {
		mm, deps, err := checkMap(t, m, wadCache)
		stockDeps[m] = deps
		if err != nil {
			return Manifest{}, nil, fmt.Errorf("%s: %w", m, err)
		}
		missing = append(missing, mm...)
	}
	if len(missing) > 0 {
		return Manifest{}, nil, &MissingError{missing}
	}

	// extra maps: resolved against stock + extra; files only extra has travel with the map
	all := t
	extraFiles := map[string][]string{} // map -> its pk3 keys (own files + referenced extra files)
	var skipped []Missing
	var accepted []string
	if extraDir != "" && len(extraMaps) > 0 {
		x, err := scanExtra(extraDir)
		if err != nil {
			return Manifest{}, nil, err
		}
		all = make(map[string]string, len(t)+len(x))
		for k, v := range x {
			all[k] = v
		}
		for k, v := range t {
			all[k] = v
		}
		for _, m := range extraMaps {
			mm, deps, err := checkMap(all, m, wadCache)
			if err != nil {
				skipped = append(skipped, Missing{m, "maps/" + m + ".bsp", err.Error()})
				continue
			}
			if hard := slices.DeleteFunc(mm, cosmetic); len(hard) > 0 {
				skipped = append(skipped, hard...)
				continue
			}
			keys := ownFiles(all, m)
			for _, d := range deps {
				if _, stock := t[d]; !stock || mapScoped(d) {
					keys = append(keys, d)
				}
			}
			extraFiles[m] = keys
			accepted = append(accepted, m)
		}
	}

	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return Manifest{}, nil, err
	}
	tmp, err := os.MkdirTemp(outDir, ".pkg-")
	if err != nil {
		return Manifest{}, nil, err
	}
	defer os.RemoveAll(tmp)

	man := Manifest{Maps: map[string]Bundle{}}
	var baseKeys []string
	for k := range t {
		if !strings.HasPrefix(k, "maps/") && !strings.HasPrefix(k, "overviews/") && !mapScoped(k) {
			baseKeys = append(baseKeys, k)
		}
	}
	if man.Base, err = writePK3(tmp, t, baseKeys); err != nil {
		return Manifest{}, nil, err
	}
	for _, m := range maps {
		keys := ownFiles(t, m)
		for _, d := range stockDeps[m] {
			if mapScoped(d) {
				keys = append(keys, d)
			}
		}
		if man.Maps[m], err = writePK3(tmp, t, dedupe(keys)); err != nil {
			return Manifest{}, nil, err
		}
	}
	for _, m := range accepted {
		if man.Maps[m], err = writePK3(tmp, all, dedupe(extraFiles[m])); err != nil {
			return Manifest{}, nil, err
		}
	}
	maps = append(append([]string{}, maps...), accepted...)
	js, _ := json.MarshalIndent(man, "", "  ")
	if err := os.WriteFile(filepath.Join(tmp, "manifest.json"), append(js, '\n'), 0o644); err != nil {
		return Manifest{}, nil, err
	}
	cycle := strings.Join(maps, "\n") + "\n"
	if err := os.WriteFile(filepath.Join(tmp, "mapcycle.txt"), []byte(cycle), 0o644); err != nil {
		return Manifest{}, nil, err
	}

	// Publish: pk3s first, manifest last, so a reader never sees a manifest without its files.
	if err := os.MkdirAll(filepath.Join(outDir, "assets"), 0o755); err != nil {
		return Manifest{}, nil, err
	}
	pubs, _ := filepath.Glob(filepath.Join(tmp, "assets", "*.pk3"))
	for _, p := range pubs {
		if err := os.Rename(p, filepath.Join(outDir, "assets", filepath.Base(p))); err != nil {
			return Manifest{}, nil, err
		}
	}
	for _, f := range []string{"mapcycle.txt", "manifest.json"} {
		if err := os.Rename(filepath.Join(tmp, f), filepath.Join(outDir, f)); err != nil {
			return Manifest{}, nil, err
		}
	}
	return man, skipped, nil
}

func dropped(key string) bool {
	return slices.ContainsFunc(dropPrefixes, func(p string) bool { return strings.HasPrefix(key, p) })
}

// mapScoped: files that belong in the bundles of the maps that use them rather than in base.pk3.
func mapScoped(key string) bool {
	return strings.HasPrefix(key, "gfx/env/") || (strings.HasSuffix(key, ".wad") && !coreWads[key])
}

// cosmetic: what a community map can do without and still load: entries of its .res download list, its sky
// (the engine warns and draws none) and sounds (silent). Models and textures are never cosmetic.
func cosmetic(m Missing) bool {
	return strings.HasSuffix(m.Why, ".res") || strings.HasPrefix(m.Path, "gfx/env/") || strings.HasPrefix(m.Path, "sound/")
}

func dedupe(keys []string) []string {
	slices.Sort(keys)
	return slices.Compact(keys)
}

// scanExtra reads a cstrike/-shaped tree of extra content with the same filters as the stock scan.
func scanExtra(dir string) (map[string]string, error) {
	x := map[string]string{}
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		key := strings.ToLower(filepath.ToSlash(rel))
		if dropDirs[strings.SplitN(key, "/", 2)[0]] || dropExts[path.Ext(key)] || dropFiles[key] {
			return nil
		}
		x[key] = p
		return nil
	})
	return x, err
}

func ownFiles(t map[string]string, m string) (keys []string) {
	for k := range t {
		for _, dir := range []string{"maps/", "overviews/"} {
			if rest, ok := strings.CutPrefix(k, dir+m+"."); ok && !strings.Contains(rest, "/") {
				keys = append(keys, k)
			}
		}
	}
	return keys
}

// checkMap returns what map m references but t lacks, and the tree keys it references (deps).
func checkMap(t map[string]string, m string, wadCache map[string]map[string]struct{}) (out []Missing, deps []string, err error) {
	bspPath, ok := t["maps/"+m+".bsp"]
	if !ok {
		return []Missing{{m, "maps/" + m + ".bsp", "map file"}}, nil, nil
	}
	raw, err := os.ReadFile(bspPath)
	if err != nil {
		return nil, nil, err
	}
	ents, external, err := parseBSP(raw)
	if err != nil {
		return nil, nil, err
	}

	dedup := map[string]bool{}
	ref := func(p, why string) {
		p = strings.ToLower(strings.ReplaceAll(p, `\`, "/"))
		if _, ok := t[p]; ok {
			deps = append(deps, p)
		} else if !dedup[p] {
			dedup[p] = true
			out = append(out, Missing{m, p, why})
		}
	}
	var wads []string
	for i, e := range ents {
		keys := make([]string, 0, len(e))
		for k := range e {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			v := e[k]
			why := fmt.Sprintf("entity %d %q", i, k)
			switch {
			case e["classname"] == "worldspawn" && k == "skyname" && v != "":
				for _, f := range skyFaces {
					ref("gfx/env/"+v+f+".tga", why)
				}
			case e["classname"] == "worldspawn" && k == "wad":
				for _, w := range strings.Split(v, ";") {
					w = strings.ToLower(w[strings.LastIndexAny(w, `\/`)+1:])
					if w != "" {
						wads = append(wads, w)
					}
				}
			case strings.HasPrefix(v, "*"): // brush model reference
			case modelRef.MatchString(v):
				ref(v, why)
			case soundRef.MatchString(v):
				ref("sound/"+v, why)
			}
		}
	}
	if res, err := os.ReadFile(t["maps/"+m+".res"]); err == nil {
		for _, l := range strings.Split(string(res), "\n") {
			if l = strings.TrimSpace(l); l != "" && !strings.HasPrefix(l, "//") {
				ref(l, "maps/"+m+".res")
			}
		}
	}

	if len(external) == 0 {
		return out, deps, nil
	}
	lumps := map[string]struct{}{}
	for _, w := range wads {
		wp, ok := t[w]
		if !ok { // a listed-but-absent wad is fine when every external texture still resolves
			continue
		}
		deps = append(deps, w)
		names, ok := wadCache[w]
		if !ok {
			b, err := os.ReadFile(wp)
			if err != nil {
				return nil, nil, err
			}
			if names, err = wadLumps(b); err != nil {
				return nil, nil, fmt.Errorf("%s: %w", w, err)
			}
			wadCache[w] = names
		}
		for n := range names {
			lumps[n] = struct{}{}
		}
	}
	for _, x := range external {
		if _, ok := lumps[x]; !ok && !dedup["texture "+x] {
			dedup["texture "+x] = true
			out = append(out, Missing{m, "texture " + x, "in none of: " + strings.Join(wads, ", ")})
		}
	}
	return out, deps, nil
}

// writePK3 writes the named tree files as a reproducible zip into tmp/assets/<sha256>.pk3.
func writePK3(tmp string, t map[string]string, keys []string) (Bundle, error) {
	sort.Strings(keys)
	if err := os.MkdirAll(filepath.Join(tmp, "assets"), 0o755); err != nil {
		return Bundle{}, err
	}
	f, err := os.CreateTemp(tmp, "bundle-*")
	if err != nil {
		return Bundle{}, err
	}
	defer f.Close()
	h := sha256.New()
	zw := zip.NewWriter(io.MultiWriter(f, h))
	for _, k := range keys {
		method := zip.Deflate
		if strings.HasSuffix(k, ".wad") {
			method = zip.Store // FWGS seek-reads wads inside a pack
		}
		w, err := zw.CreateHeader(&zip.FileHeader{Name: k, Method: method, Modified: fixedTime})
		if err != nil {
			return Bundle{}, err
		}
		if err := copyFile(w, t[k]); err != nil {
			return Bundle{}, err
		}
	}
	if err := zw.Close(); err != nil {
		return Bundle{}, err
	}
	st, err := f.Stat()
	if err != nil {
		return Bundle{}, err
	}
	sum := hex.EncodeToString(h.Sum(nil))
	if err := os.Rename(f.Name(), filepath.Join(tmp, "assets", sum+".pk3")); err != nil {
		return Bundle{}, err
	}
	return Bundle{File: "assets/" + sum + ".pk3", SHA256: sum, Size: st.Size()}, nil
}

func copyFile(w io.Writer, p string) error {
	r, err := os.Open(p)
	if err != nil {
		return err
	}
	defer r.Close()
	_, err = io.Copy(w, r)
	return err
}
