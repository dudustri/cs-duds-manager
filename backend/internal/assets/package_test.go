package assets_test

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"cs-duds/backend/internal/assets"
)

// ---- fixtures: real BSP v30 and WAD3 bytes, synthesized ----

type tex struct {
	name     string
	external bool // all four mip offsets 0: the pixels live in a WAD
}

func le32(b []byte, v int) []byte { return binary.LittleEndian.AppendUint32(b, uint32(v)) }

func miptex(t tex) []byte {
	b := make([]byte, 16)
	copy(b, t.name)
	b = le32(b, 16)
	b = le32(b, 16)
	if t.external {
		return append(b, make([]byte, 16)...)
	}
	for _, o := range []int{40, 296, 360, 376} {
		b = le32(b, o)
	}
	return append(b, make([]byte, 340)...)
}

// bsp builds a GoldSrc BSP v30: header (version + 15 lumps), lump 0 entities, lump 2 miptex.
func bsp(entities string, texs ...tex) []byte {
	ent := append([]byte(entities), 0)
	tl := le32(nil, len(texs))
	var mips []byte
	off := 4 + 4*len(texs)
	for _, x := range texs {
		tl = le32(tl, off+len(mips))
		mips = append(mips, miptex(x)...)
	}
	tl = append(tl, mips...)
	const hdr = 4 + 15*8
	b := le32(nil, 30)
	for i := 0; i < 15; i++ {
		switch i {
		case 0:
			b = le32(le32(b, hdr), len(ent))
		case 2:
			b = le32(le32(b, hdr+len(ent)), len(tl))
		default:
			b = le32(le32(b, 0), 0)
		}
	}
	return append(append(b, ent...), tl...)
}

// wad builds a WAD3 holding the named textures.
func wad(names ...string) []byte {
	var data []byte
	var dir []byte
	pos := 12
	for _, n := range names {
		m := miptex(tex{name: n})
		e := le32(le32(le32(nil, pos+len(data)), len(m)), len(m))
		e = append(e, 0x43, 0, 0, 0)
		nm := make([]byte, 16)
		copy(nm, n)
		dir = append(dir, append(e, nm...)...)
		data = append(data, m...)
	}
	h := le32(le32([]byte("WAD3"), len(names)), 12+len(data))
	return append(append(h, data...), dir...)
}

func ents(world string, others ...string) string {
	s := "{\n\"classname\" \"worldspawn\"\n" + world + "}\n"
	for _, o := range others {
		s += "{\n" + o + "}\n"
	}
	return s
}

func kv(k, v string) string { return fmt.Sprintf("%q %q\n", k, v) }

func put(t *testing.T, root, rel string, data []byte) {
	t.Helper()
	p := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func filler(s string) []byte { return []byte(strings.Repeat(s+" ", 400)) }

var sky = []string{"up", "dn", "lf", "rt", "ft", "bk"}

// baseTree is an HLDS-shaped tree: valve/ underneath, cstrike/ overriding it.
func baseTree(t *testing.T) string {
	root := t.TempDir()
	put(t, root, "valve/halflife.wad", wad("wall1", "floor1"))
	put(t, root, "valve/sound/common.wav", filler("valve"))
	put(t, root, "valve/sound/valve_only.wav", filler("valve-only"))
	put(t, root, "valve/liblist.gam", filler("valve-liblist"))
	put(t, root, "cstrike/cstrike.wad", wad("cs_tex"))
	put(t, root, "cstrike/sound/common.wav", filler("cstrike"))
	put(t, root, "cstrike/sound/ambience/wind.wav", filler("wind"))
	put(t, root, "cstrike/models/player.mdl", filler("player"))
	put(t, root, "cstrike/Sprites/Fire.SPR", filler("fire")) // mixed case on disk
	put(t, root, "cstrike/liblist.gam", filler("cs-liblist"))
	for _, f := range sky {
		put(t, root, "cstrike/gfx/env/desert"+f+".tga", filler("sky"+f))
	}
	for _, drop := range []string{"addons/amxmodx/x.amxx", "dlls/mp.so", "cl_dlls/client.so", "maps/graph.nav", "logs/a.log", "reslists/r.lst"} {
		put(t, root, "cstrike/"+drop, filler("dropped"))
	}
	return root
}

func goodMap(t *testing.T, root, name string) {
	world := kv("wad", `\cstrike\cstrike.wad;\valve\halflife.wad;\valve\absent.wad`) + kv("skyname", "desert")
	e := ents(world,
		kv("classname", "ambient_generic")+kv("message", "ambience/wind.wav"),
		kv("classname", "func_wall")+kv("model", "*1"),
		kv("classname", "item")+kv("model", "models/player.mdl"),
		kv("classname", "env_sprite")+kv("model", "sprites/fire.spr"))
	put(t, root, "cstrike/maps/"+name+".bsp", bsp(e, tex{"wall1", true}, tex{"cs_tex", true}, tex{"inner", false}))
	put(t, root, "cstrike/maps/"+name+".txt", filler(name+"-txt"))
	put(t, root, "cstrike/overviews/"+name+".bmp", filler(name+"-overview"))
}

func zipNames(t *testing.T, path string) map[string]*zip.File {
	t.Helper()
	zr, err := zip.OpenReader(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	t.Cleanup(func() { zr.Close() })
	m := map[string]*zip.File{}
	for _, f := range zr.File {
		m[f.Name] = f
	}
	return m
}

func readZip(t *testing.T, f *zip.File) string {
	r, err := f.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	var b bytes.Buffer
	b.ReadFrom(r)
	return b.String()
}

func TestRejectsMissingReferences(t *testing.T) {
	root := baseTree(t)
	// de_bad: sky face "bk" absent, models/missing.mdl absent, texture mystery_tex in no listed wad
	for _, f := range sky[:5] {
		put(t, root, "cstrike/gfx/env/desertx"+f+".tga", filler("x"))
	}
	world := kv("wad", `\cstrike\cstrike.wad;\valve\halflife.wad`) + kv("skyname", "desertx")
	e := ents(world,
		kv("classname", "item")+kv("model", "models/missing.mdl"),
		kv("classname", "item")+kv("model", "models/player.mdl"),
		kv("classname", "func_wall")+kv("model", "*2"),
		kv("classname", "ambient_generic")+kv("message", "ambience/wind.wav"))
	put(t, root, "cstrike/maps/de_bad.bsp", bsp(e, tex{"wall1", true}, tex{"mystery_tex", true}, tex{"inner", false}))
	out := t.TempDir()

	_, err := assets.Package(root, []string{"de_bad"}, out)
	var me *assets.MissingError
	if !errors.As(err, &me) {
		t.Fatalf("err = %v (%T), want *assets.MissingError", err, err)
	}
	if len(me.Missing) != 3 {
		t.Fatalf("missing %+v, want exactly 3 entries", me.Missing)
	}
	for _, want := range []string{"missing.mdl", "desertxbk", "mystery_tex"} {
		found := false
		for _, m := range me.Missing {
			if m.Map == "de_bad" && strings.Contains(strings.ToLower(m.Path), want) {
				found = true
			}
		}
		if !found {
			t.Errorf("no Missing entry for %q in %+v", want, me.Missing)
		}
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Error() does not mention %q: %s", want, err)
		}
	}
	if ents, _ := os.ReadDir(out); len(ents) != 0 {
		t.Errorf("out dir was touched: %v", ents)
	}
}

func TestSplitAndManifest(t *testing.T) {
	root := baseTree(t)
	goodMap(t, root, "de_a")
	goodMap(t, root, "de_b")
	out := t.TempDir()
	m, err := assets.Package(root, []string{"de_b", "de_a"}, out)
	if err != nil {
		t.Fatalf("Package: %v", err)
	}

	pk3 := regexp.MustCompile(`^assets/([0-9a-f]{64})\.pk3$`)
	check := func(label string, b assets.Bundle) string {
		sm := pk3.FindStringSubmatch(b.File)
		if sm == nil || sm[1] != b.SHA256 {
			t.Fatalf("%s: file %q sha256 %q", label, b.File, b.SHA256)
		}
		raw, err := os.ReadFile(filepath.Join(out, b.File))
		if err != nil {
			t.Fatalf("%s: %v", label, err)
		}
		sum := sha256.Sum256(raw)
		if hex.EncodeToString(sum[:]) != b.SHA256 || int64(len(raw)) != b.Size {
			t.Errorf("%s: manifest says sha256 %s size %d, file is %x size %d", label, b.SHA256, b.Size, sum, len(raw))
		}
		return filepath.Join(out, b.File)
	}
	basePath := check("base", m.Base)
	if len(m.Maps) != 2 {
		t.Fatalf("maps %v", m.Maps)
	}
	base := zipNames(t, basePath)
	for _, want := range []string{"cstrike.wad", "halflife.wad", "sound/common.wav", "sound/valve_only.wav", "models/player.mdl",
		"sprites/fire.spr", "gfx/env/desertup.tga", "sound/ambience/wind.wav"} {
		if base[want] == nil {
			t.Errorf("base.pk3 lacks %q (names are lowercase, cstrike over valve)", want)
		}
	}
	if f := base["sound/common.wav"]; f != nil && !strings.HasPrefix(readZip(t, f), "cstrike") {
		t.Error("sound/common.wav must be cstrike's copy, not valve's")
	}
	if base["liblist.gam"] != nil { // the server's liblist makes the browser engine look for dlls/mp_emscripten_wasm32.wasm
		t.Error("base.pk3 must not ship the server's liblist.gam")
	}
	for name := range base {
		for _, bad := range []string{"maps/", "overviews/", "addons/", "dlls/", "cl_dlls/", "logs/", "reslists/"} {
			if strings.HasPrefix(name, bad) {
				t.Errorf("base.pk3 contains %q", name)
			}
		}
		for _, ext := range []string{".so", ".dll", ".dylib", ".exe", ".nav"} {
			if strings.HasSuffix(name, ext) {
				t.Errorf("base.pk3 contains %q", name)
			}
		}
	}
	for _, n := range []string{"de_a", "de_b"} {
		mp := zipNames(t, check(n, m.Maps[n]))
		var got []string
		for name := range mp {
			got = append(got, name)
		}
		sort.Strings(got)
		if want := []string{"maps/" + n + ".bsp", "maps/" + n + ".txt", "overviews/" + n + ".bmp"}; !reflect.DeepEqual(got, want) {
			t.Errorf("%s bundle holds %v, want %v", n, got, want)
		}
	}

	var onDisk assets.Manifest
	raw, err := os.ReadFile(filepath.Join(out, "manifest.json"))
	if err != nil || json.Unmarshal(raw, &onDisk) != nil || !reflect.DeepEqual(onDisk, m) {
		t.Errorf("manifest.json (%v) does not equal the returned manifest", err)
	}
	cycle, _ := os.ReadFile(filepath.Join(out, "mapcycle.txt"))
	if got := strings.Fields(string(cycle)); !reflect.DeepEqual(got, []string{"de_b", "de_a"}) {
		t.Errorf("mapcycle.txt = %q, want the maps in input order", cycle)
	}
	if _, err := assets.Package(root, []string{"de_a", "de_a"}, t.TempDir()); err == nil {
		t.Error("duplicate map names must be an error")
	}
}

func TestReproducibleAndStoredWads(t *testing.T) {
	root := baseTree(t)
	goodMap(t, root, "de_a")
	run := func() (string, assets.Manifest) {
		out := t.TempDir()
		m, err := assets.Package(root, []string{"de_a"}, out)
		if err != nil {
			t.Fatalf("Package: %v", err)
		}
		return out, m
	}
	out1, m1 := run()
	future := time.Now().Add(48 * time.Hour)
	filepath.Walk(root, func(p string, _ os.FileInfo, _ error) error { return os.Chtimes(p, future, future) })
	out2, m2 := run()
	if !reflect.DeepEqual(m1, m2) {
		t.Fatalf("manifests differ between runs:\n%+v\n%+v", m1, m2)
	}
	for _, b := range []assets.Bundle{m1.Base, m1.Maps["de_a"]} {
		a, _ := os.ReadFile(filepath.Join(out1, b.File))
		c, _ := os.ReadFile(filepath.Join(out2, b.File))
		if len(a) == 0 || !bytes.Equal(a, c) {
			t.Errorf("%s is not byte-identical across runs (mtimes must not leak in)", b.File)
		}
	}
	names := zipNames(t, filepath.Join(out1, m1.Base.File))
	var order []string
	zr, _ := zip.OpenReader(filepath.Join(out1, m1.Base.File))
	for _, f := range zr.File {
		order = append(order, f.Name)
	}
	zr.Close()
	if !sort.StringsAreSorted(order) {
		t.Errorf("entries are not sorted: %v", order)
	}
	for name, f := range names {
		want := zip.Deflate
		if strings.HasSuffix(name, ".wad") {
			want = zip.Store // FWGS seek-reads wads inside a pack
		}
		if f.Method != want {
			t.Errorf("%s: method %d, want %d", name, f.Method, want)
		}
	}
	if names["cstrike.wad"] == nil || names["halflife.wad"] == nil {
		t.Error("base.pk3 lacks the wads")
	}
}

// Extra maps: their own wads, sky and sounds ride in the map's pk3 (never in base.pk3); a map that references a
// file nobody has is skipped, not fatal; a stock file of the same name beats the extra copy.
func TestExtraMaps(t *testing.T) {
	root := baseTree(t)
	goodMap(t, root, "de_stock")
	extra := t.TempDir()
	world := kv("wad", `\cstrike\pool.wad`) + kv("skyname", "pool")
	put(t, extra, "maps/fy_pool.bsp", bsp(ents(world, kv("classname", "ambient_generic")+kv("message", "pool/splash.wav")), tex{"tiles", true}))
	put(t, extra, "pool.wad", wad("tiles"))
	for _, f := range sky {
		put(t, extra, "gfx/env/pool"+f+".tga", filler("poolsky"+f))
	}
	put(t, extra, "sound/pool/splash.wav", filler("splash"))
	put(t, extra, "sound/common.wav", filler("extra-overrides-stock")) // must not replace the stock file
	put(t, extra, "maps/aim_broken.bsp", bsp(ents("", kv("classname", "item")+kv("model", "models/nowhere.mdl"))))
	// missing only cosmetic files (a .res extra, the sky, a sound): still offered, without them
	put(t, extra, "maps/fy_quiet.bsp", bsp(ents(kv("skyname", "gone"), kv("classname", "ambient_generic")+kv("message", "gone/x.wav"))))
	put(t, extra, "maps/fy_quiet.res", []byte("sound/misc/gone.wav\n"))

	out := t.TempDir()
	man, skipped, err := assets.PackageAll(root, []string{"de_stock"}, extra, []string{"fy_pool", "aim_broken", "fy_quiet"}, out)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := man.Maps["fy_pool"]; !ok || len(man.Maps) != 3 || man.Maps["fy_quiet"].File == "" {
		t.Fatalf("maps %v", man.Maps)
	}
	if len(skipped) != 1 || skipped[0].Map != "aim_broken" || skipped[0].Path != "models/nowhere.mdl" {
		t.Fatalf("skipped %+v", skipped)
	}
	pool := zipNames(t, filepath.Join(out, man.Maps["fy_pool"].File))
	for _, want := range []string{"maps/fy_pool.bsp", "pool.wad", "gfx/env/poolup.tga", "sound/pool/splash.wav"} {
		if pool[want] == nil {
			t.Errorf("fy_pool.pk3 lacks %s", want)
		}
	}
	base := zipNames(t, filepath.Join(out, man.Base.File))
	for _, never := range []string{"pool.wad", "sound/pool/splash.wav", "maps/fy_pool.bsp"} {
		if base[never] != nil {
			t.Errorf("base.pk3 holds extra file %s", never)
		}
	}
	if got := readZip(t, base["sound/common.wav"]); !strings.HasPrefix(got, "cstrike ") {
		t.Errorf("stock sound/common.wav was replaced: %q", got[:20])
	}
	cycle, _ := os.ReadFile(filepath.Join(out, "mapcycle.txt"))
	if string(cycle) != "de_stock\nfy_pool\nfy_quiet\n" {
		t.Errorf("mapcycle %q", cycle)
	}
}
