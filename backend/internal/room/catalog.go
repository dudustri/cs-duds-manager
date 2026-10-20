package room

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

var nameRe = regexp.MustCompile(`^[a-z0-9_]+$`)

// Map and Mode can only be minted by a Catalog, so every name that reaches hlds argv or an
// RCON line is a catalog entry. The fields are unexported: room.Map("x; quit") does not compile.
type Map struct{ name string }

// Mode also carries the cvar that proves the mode cfg was applied (its `// probe` line) and the settings a room
// creator may change (modes/<mode>/options.json, optional).
type Mode struct {
	name, probeCvar, probeVal string
	options                   []Option
}

// Option is one room setting: a server cvar and the only values it may take.
type Option struct {
	Cvar    string      `json:"cvar"`
	Label   string      `json:"label"`
	Default string      `json:"default"`
	Values  [][2]string `json:"values"` // [value, label]
}

// Settings are validated option values, ready for the room's argv; only Mode.Settings mints them.
type Settings struct{ s string }

var (
	cvarRe  = regexp.MustCompile(`^[a-z_]{1,32}$`)
	valueRe = regexp.MustCompile(`^[0-9a-z._-]{1,16}$`)
)

func (m Mode) Options() []Option { return m.options }

// Settings checks the requested values against the mode's options (missing ones take their default) and
// returns them as "cvar/value/cvar/value..." in option order (the engine's tokenizer splits on ',', ':' and '=', not
// on '/'). An unknown option or value is an error.
func (m Mode) Settings(req map[string]string) (Settings, error) {
	parts := make([]string, 0, len(m.options))
	for _, o := range m.options {
		v, ok := req[o.Cvar]
		if !ok {
			v = o.Default
		}
		if !slices.ContainsFunc(o.Values, func(p [2]string) bool { return p[0] == v }) {
			return Settings{}, fmt.Errorf("%s: %q is not one of its values", o.Cvar, v)
		}
		parts = append(parts, o.Cvar+"/"+v)
	}
	for k := range req {
		if !slices.ContainsFunc(m.options, func(o Option) bool { return o.Cvar == k }) {
			return Settings{}, fmt.Errorf("mode %s has no option %q", m.name, k)
		}
	}
	return Settings{strings.Join(parts, "/")}, nil
}

func loadOptions(path string) ([]Option, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	var opts []Option
	if err := json.Unmarshal(b, &opts); err != nil {
		return nil, err
	}
	for _, o := range opts {
		ok := cvarRe.MatchString(o.Cvar) && o.Label != "" && len(o.Values) > 0
		for _, v := range o.Values {
			ok = ok && valueRe.MatchString(v[0]) && v[1] != ""
		}
		if !ok || !slices.ContainsFunc(o.Values, func(p [2]string) bool { return p[0] == o.Default }) {
			return nil, fmt.Errorf("bad option %+v", o)
		}
	}
	return opts, nil
}

func (m Map) String() string  { return m.name }
func (m Mode) String() string { return m.name }

type Catalog struct {
	maps  map[string]Map
	modes map[string]Mode
}

// LoadCatalog reads map names from manifest.json and modes from modesDir/*/mode.cfg. Each
// mode.cfg must have exactly one line ending in `// probe`, e.g. `mp_forcerespawn 1 // probe`, and each mode
// needs its plugin list (see pluginList).
func LoadCatalog(manifestPath, modesDir string) (*Catalog, error) {
	b, err := os.ReadFile(manifestPath)
	if err != nil {
		return nil, err
	}
	var mf struct {
		Maps map[string]json.RawMessage `json:"maps"`
	}
	if err := json.Unmarshal(b, &mf); err != nil {
		return nil, fmt.Errorf("manifest: %w", err)
	}
	c := &Catalog{maps: map[string]Map{}, modes: map[string]Mode{}}
	for n := range mf.Maps {
		if !nameRe.MatchString(n) {
			return nil, fmt.Errorf("manifest: bad map name %q", n)
		}
		c.maps[n] = Map{n}
	}
	dirs, err := os.ReadDir(modesDir)
	if err != nil {
		return nil, err
	}
	for _, d := range dirs {
		if !d.IsDir() {
			continue
		}
		if !nameRe.MatchString(d.Name()) {
			return nil, fmt.Errorf("bad mode name %q", d.Name())
		}
		cfg, err := os.ReadFile(filepath.Join(modesDir, d.Name(), "mode.cfg"))
		if err != nil {
			return nil, fmt.Errorf("mode %s: %w", d.Name(), err)
		}
		// the build generates it; without it hlds would start with the wrong plugins and fail only at runtime
		if _, err := os.Stat(filepath.Join(modesDir, "..", pluginList(d.Name()))); err != nil {
			return nil, fmt.Errorf("mode %s: plugin list: %w", d.Name(), err)
		}
		cvar, val, err := parseProbe(string(cfg))
		if err != nil {
			return nil, fmt.Errorf("mode %s: %w", d.Name(), err)
		}
		opts, err := loadOptions(filepath.Join(modesDir, d.Name(), "options.json"))
		if err != nil {
			return nil, fmt.Errorf("mode %s: options.json: %w", d.Name(), err)
		}
		c.modes[d.Name()] = Mode{d.Name(), cvar, val, opts}
	}
	if len(c.maps) == 0 || len(c.modes) == 0 {
		return nil, fmt.Errorf("catalog: %d maps, %d modes", len(c.maps), len(c.modes))
	}
	return c, nil
}

// pluginList is the mode's AMXX plugin list, relative to the game dir (cstrike/, the parent of modesDir).
func pluginList(mode string) string { return "addons/amxmodx/configs/modes/" + mode + ".ini" }

func parseProbe(cfg string) (cvar, val string, err error) {
	n := 0
	for _, line := range strings.Split(cfg, "\n") {
		code, comment, ok := strings.Cut(line, "//")
		if !ok || strings.TrimSpace(comment) != "probe" {
			continue
		}
		n++
		f := strings.Fields(code)
		if len(f) < 2 || !nameRe.MatchString(f[0]) {
			return "", "", fmt.Errorf("malformed probe line %q", line)
		}
		cvar, val = f[0], strings.Trim(strings.Join(f[1:], " "), `"`)
	}
	if n != 1 {
		return "", "", fmt.Errorf("%d probe lines, want exactly 1", n)
	}
	return cvar, val, nil
}

func (c *Catalog) Map(name string) (Map, bool)   { m, ok := c.maps[name]; return m, ok }
func (c *Catalog) Mode(name string) (Mode, bool) { m, ok := c.modes[name]; return m, ok }

func sortedKeys[V any](m map[string]V) []string {
	return slices.Sorted(func(yield func(string) bool) {
		for k := range m {
			if !yield(k) {
				return
			}
		}
	})
}

func (c *Catalog) Maps() []string  { return sortedKeys(c.maps) }
func (c *Catalog) Modes() []string { return sortedKeys(c.modes) }
