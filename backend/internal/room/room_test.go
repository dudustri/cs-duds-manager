package room

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseStatus(t *testing.T) {
	in := "hostname:  x\nmap     :  de_dust2 at: 0 x, 0 y, 0 z\nplayers :  3 active (16 max)\n\n" +
		"#      name userid uniqueid frag time ping loss adr\n" +
		"# 1 \"a\";quit;\"\" 7 STEAM_0:1:5 -2 01:02:03 41 0 127.10.0.1:27005\n" +
		"# 2 \"x y #3\" 3 STEAM_0:1:6 4 00:09 12 1 127.10.0.2:27005\n" +
		"# 3 \"Bot\" 9 BOT 0 00:09 0 0\n" +
		"# 4 \"proxy\" 10 HLTV 0 00:09 0 0 127.0.0.1:1\n"
	st, err := ParseStatus(in)
	if err != nil {
		t.Fatal(err)
	}
	if st.Map != "de_dust2" || len(st.Players) != 3 || st.Humans() != 2 {
		t.Fatalf("%+v", st)
	}
	p := st.Players[0]
	if p.UserID != 7 || p.Name != `a";quit;"` || p.Frags != -2 || p.Ping != 41 {
		t.Fatalf("%+v", p)
	}
	if b := st.Players[2]; !b.Bot {
		t.Fatalf("%+v", b)
	}
	if _, err := ParseStatus("garbage"); err == nil {
		t.Fatal("garbage parsed")
	}
}

func TestLoadCatalogProbe(t *testing.T) {
	dir := t.TempDir()
	mode := func(name, cfg string) {
		os.MkdirAll(filepath.Join(dir, "modes", name), 0o755)
		os.WriteFile(filepath.Join(dir, "modes", name, "mode.cfg"), []byte(cfg), 0o644)
		os.MkdirAll(filepath.Join(dir, "addons/amxmodx/configs/modes"), 0o755)
		os.WriteFile(filepath.Join(dir, pluginList(name)), nil, 0o644)
	}
	manifest := filepath.Join(dir, "manifest.json")
	os.WriteFile(manifest, []byte(`{"maps":{"de_dust2":{}}}`), 0o644)
	mode("dm", "// dm\nmp_forcerespawn 1 // probe\nmp_freeforall 1\n")
	c, err := LoadCatalog(manifest, filepath.Join(dir, "modes"))
	if err != nil {
		t.Fatal(err)
	}
	if m, _ := c.Mode("dm"); m.probeCvar != "mp_forcerespawn" || m.probeVal != "1" {
		t.Fatalf("%+v", m)
	}
	for name, cfg := range map[string]string{"none": "a 1\n", "two": "a 1 // probe\nb 2 // probe\n", "bad": "// probe\n"} {
		mode("bad", cfg)
		if _, err := LoadCatalog(manifest, filepath.Join(dir, "modes")); err == nil {
			t.Errorf("%s: LoadCatalog accepted %q", name, cfg)
		}
	}
	mode("bad", "a 1 // probe\n")
	os.Remove(filepath.Join(dir, pluginList("bad")))
	if _, err := LoadCatalog(manifest, filepath.Join(dir, "modes")); err == nil {
		t.Error("LoadCatalog accepted a mode without its plugin list")
	}
	os.Remove(filepath.Join(dir, "modes", "bad", "mode.cfg"))
	if _, err := LoadCatalog(manifest, filepath.Join(dir, "modes")); err == nil {
		t.Error("LoadCatalog accepted a mode directory without mode.cfg")
	}
}
