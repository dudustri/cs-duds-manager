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
			if dropDirs[strings.SplitN(key, "/", 2)[0]] || dropExts[path.Ext(key)] || dropFiles[key] {
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
	seen := map[string]bool{}
	for _, m := range maps {
		if m == "" || strings.ContainsAny(m, `/\`) || seen[m] {
			return Manifest{}, fmt.Errorf("bad or duplicate map name %q", m)
		}
		seen[m] = true
	}
	t, err := scanTree(contentDir)
	if err != nil {
		return Manifest{}, err
	}
	wadCache := map[string]map[string]struct{}{}
	var missing []Missing
	for _, m := range maps {
		mm, err := checkMap(t, m, wadCache)
		if err != nil {
			return Manifest{}, fmt.Errorf("%s: %w", m, err)
		}
		missing = append(missing, mm...)
	}
	if len(missing) > 0 {
		return Manifest{}, &MissingError{missing}
	}

	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return Manifest{}, err
	}
	tmp, err := os.MkdirTemp(outDir, ".pkg-")
	if err != nil {
		return Manifest{}, err
	}
	defer os.RemoveAll(tmp)

	man := Manifest{Maps: map[string]Bundle{}}
	var baseKeys []string
	for k := range t {
		if !strings.HasPrefix(k, "maps/") && !strings.HasPrefix(k, "overviews/") {
			baseKeys = append(baseKeys, k)
		}
	}
	if man.Base, err = writePK3(tmp, t, baseKeys); err != nil {
		return Manifest{}, err
	}
	for _, m := range maps {
		if man.Maps[m], err = writePK3(tmp, t, ownFiles(t, m)); err != nil {
			return Manifest{}, err
		}
	}
	js, _ := json.MarshalIndent(man, "", "  ")
	if err := os.WriteFile(filepath.Join(tmp, "manifest.json"), append(js, '\n'), 0o644); err != nil {
		return Manifest{}, err
	}
	cycle := strings.Join(maps, "\n") + "\n"
	if err := os.WriteFile(filepath.Join(tmp, "mapcycle.txt"), []byte(cycle), 0o644); err != nil {
		return Manifest{}, err
	}

	// Publish: pk3s first, manifest last, so a reader never sees a manifest without its files.
	if err := os.MkdirAll(filepath.Join(outDir, "assets"), 0o755); err != nil {
		return Manifest{}, err
	}
	pubs, _ := filepath.Glob(filepath.Join(tmp, "assets", "*.pk3"))
	for _, p := range pubs {
		if err := os.Rename(p, filepath.Join(outDir, "assets", filepath.Base(p))); err != nil {
			return Manifest{}, err
		}
	}
	for _, f := range []string{"mapcycle.txt", "manifest.json"} {
		if err := os.Rename(filepath.Join(tmp, f), filepath.Join(outDir, f)); err != nil {
			return Manifest{}, err
		}
	}
	return man, nil
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

func checkMap(t map[string]string, m string, wadCache map[string]map[string]struct{}) ([]Missing, error) {
	bspPath, ok := t["maps/"+m+".bsp"]
	if !ok {
		return []Missing{{m, "maps/" + m + ".bsp", "map file"}}, nil
	}
	raw, err := os.ReadFile(bspPath)
	if err != nil {
		return nil, err
	}
	ents, external, err := parseBSP(raw)
	if err != nil {
		return nil, err
	}

	var out []Missing
	dedup := map[string]bool{}
	ref := func(p, why string) {
		p = strings.ToLower(strings.ReplaceAll(p, `\`, "/"))
		if _, ok := t[p]; !ok && !dedup[p] {
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
		return out, nil
	}
	lumps := map[string]struct{}{}
	for _, w := range wads {
		wp, ok := t[w]
		if !ok { // a listed-but-absent wad is fine when every external texture still resolves
			continue
		}
		names, ok := wadCache[w]
		if !ok {
			b, err := os.ReadFile(wp)
			if err != nil {
				return nil, err
			}
			if names, err = wadLumps(b); err != nil {
				return nil, fmt.Errorf("%s: %w", w, err)
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
	return out, nil
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
