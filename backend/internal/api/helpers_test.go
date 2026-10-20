package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	"cs-duds/backend/internal/api"
	"cs-duds/backend/internal/relay"
	"cs-duds/backend/internal/room"
	"cs-duds/backend/internal/testpeer"
)

const adminPassword = "test-admin-pw"

var fakeBin string

// TestMain builds the fakehlds test double once.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "fakehlds")
	if err != nil {
		panic(err)
	}
	fakeBin = filepath.Join(dir, "fakehlds")
	goBin := filepath.Join(runtime.GOROOT(), "bin", "go")
	if p, err := exec.LookPath("go"); err == nil {
		goBin = p
	}
	cmd := exec.Command(goBin, "build", "-o", fakeBin, "cs-duds/backend/internal/room/fakehlds")
	cmd.Dir = "../.."
	if out, err := cmd.CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "build fakehlds: %v\n%s", err, out)
		os.Exit(2)
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

func freePorts(t testing.TB, n int) []int {
	t.Helper()
	var conns []*net.UDPConn
	var ports []int
	for i := 0; i < n; i++ {
		c, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
		if err != nil {
			t.Fatal(err)
		}
		conns = append(conns, c)
		ports = append(ports, c.LocalAddr().(*net.UDPAddr).Port)
	}
	for _, c := range conns {
		c.Close()
	}
	// ascending, so ports[0] is the lowest (rooms take the lowest free port)
	for i := range ports {
		for j := i + 1; j < len(ports); j++ {
			if ports[j] < ports[i] {
				ports[i], ports[j] = ports[j], ports[i]
			}
		}
	}
	return ports
}

type spec struct {
	cfg  api.Config
	maps []string // manifest maps; nil -> testdata/manifest.json
}

type harness struct {
	url                          string
	ports                        []int
	argvLog, rconLog, sigtermLog string
	hldsDir                      string
	srv                          *api.Server
	admin                        string // cookie value after login()
	client                       *http.Client
}

// start runs api.New in-process with fakehlds as the hlds binary, wrapped in httptest.
// Defaults: 3 room ports, 1-minute idle timeout, 100 ms status polls, generous limits.
func start(t *testing.T, mods ...func(*spec)) *harness {
	t.Helper()
	hldsDir := t.TempDir()
	for _, m := range []string{"classic", "deathmatch", "gungame"} {
		b, err := os.ReadFile(filepath.Join("testdata", m, "mode.cfg"))
		if err != nil {
			t.Fatal(err)
		}
		dst := filepath.Join(hldsDir, "cstrike", "modes", m)
		os.MkdirAll(dst, 0o755)
		os.WriteFile(filepath.Join(dst, "mode.cfg"), b, 0o644)
	}
	hash, _ := bcrypt.GenerateFromPassword([]byte(adminPassword), bcrypt.MinCost)
	h := &harness{ports: freePorts(t, 3), client: &http.Client{Timeout: 40 * time.Second}, hldsDir: hldsDir}
	logs := t.TempDir()
	h.argvLog, h.rconLog, h.sigtermLog = filepath.Join(logs, "argv"), filepath.Join(logs, "rcon"), filepath.Join(logs, "sigterms")
	t.Setenv("FAKEHLDS_ARGV", h.argvLog)
	t.Setenv("FAKEHLDS_LOG", h.rconLog)
	t.Setenv("FAKEHLDS_SIGTERMS", h.sigtermLog)
	sp := &spec{cfg: api.Config{
		Rooms: room.Config{HLDSDir: hldsDir, HLDSBin: fakeBin, Ports: h.ports, IdleTimeout: time.Minute,
			StatusEvery: 100 * time.Millisecond, ReadyTimeout: 10 * time.Second},
		ModesDir:          filepath.Join(hldsDir, "cstrike", "modes"),
		ManifestPath:      filepath.Join("testdata", "manifest.json"),
		Relay:             relay.Config{ICEPort: freePorts(t, 1)[0], PublicIP: netip.MustParseAddr("127.0.0.1"), IdleTimeout: 30 * time.Second, SessionLimitPerIP: 16},
		AdminPasswordHash: hash,
		CreateLimitBurst:  1000, CreateLimitEvery: time.Millisecond,
		LoginLimitBurst: 1000, LoginLimitEvery: time.Millisecond,
	}}
	for _, m := range mods {
		m(sp)
	}
	if sp.maps != nil {
		mf := map[string]any{"base": map[string]any{}, "maps": map[string]any{}}
		for _, m := range sp.maps {
			mf["maps"].(map[string]any)[m] = map[string]any{"file": "assets/x.pk3", "sha256": strings.Repeat("0", 64), "size": 1}
		}
		b, _ := json.Marshal(mf)
		sp.cfg.ManifestPath = filepath.Join(logs, "manifest.json")
		os.WriteFile(sp.cfg.ManifestPath, b, 0o644)
	}
	h.ports = sp.cfg.Rooms.Ports
	srv, err := api.New(sp.cfg)
	if err != nil {
		t.Fatalf("api.New: %v", err)
	}
	h.srv = srv
	ts := httptest.NewServer(srv)
	h.url = ts.URL
	t.Cleanup(func() { ts.Close(); srv.Close() })
	return h
}

func (h *harness) do(method, path string, body any, hdr map[string]string) (*http.Response, []byte) {
	var rd io.Reader
	switch b := body.(type) {
	case nil:
	case string:
		rd = strings.NewReader(b)
	default:
		j, _ := json.Marshal(b)
		rd = bytes.NewReader(j)
	}
	req, _ := http.NewRequest(method, h.url+path, rd)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := h.client.Do(req)
	if err != nil {
		return &http.Response{StatusCode: -1, Header: http.Header{}}, []byte(err.Error())
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp, raw
}

// expect asserts the status and, when code != "", the machine code of the JSON error body.
func (h *harness) expect(t *testing.T, method, path string, body any, hdr map[string]string, status int, code string) []byte {
	t.Helper()
	resp, raw := h.do(method, path, body, hdr)
	if resp.StatusCode != status {
		t.Fatalf("%s %s: status %d, want %d (body %s)", method, path, resp.StatusCode, status, raw)
	}
	if code != "" {
		var e struct{ Error, Code string }
		if json.Unmarshal(raw, &e) != nil || e.Code != code || e.Error == "" {
			t.Fatalf("%s %s: body %s, want {\"error\":..., \"code\":%q}", method, path, raw, code)
		}
	}
	return raw
}

type roomJSON struct {
	ID         string    `json:"id"`
	Map        string    `json:"map"`
	Mode       string    `json:"mode"`
	Players    int       `json:"players"`
	MaxPlayers int       `json:"maxPlayers"`
	CreatedAt  time.Time `json:"createdAt"`
	HostToken  string    `json:"hostToken"`
}

func (r roomJSON) bearer() map[string]string {
	return map[string]string{"Authorization": "Bearer " + r.HostToken}
}

func (h *harness) create(t *testing.T, mp, mode string, hdr ...map[string]string) roomJSON {
	t.Helper()
	var hh map[string]string
	if len(hdr) > 0 {
		hh = hdr[0]
	}
	raw := h.expect(t, "POST", "/api/rooms", map[string]string{"map": mp, "mode": mode}, hh, 201, "")
	var r roomJSON
	if err := json.Unmarshal(raw, &r); err != nil || r.ID == "" || r.HostToken == "" {
		t.Fatalf("create body %s (%v)", raw, err)
	}
	return r
}

func (h *harness) login(t *testing.T) {
	t.Helper()
	resp, raw := h.do("POST", "/api/admin/login", map[string]string{"password": adminPassword}, nil)
	if resp.StatusCode != 204 {
		t.Fatalf("login: %d %s", resp.StatusCode, raw)
	}
	for _, c := range resp.Cookies() {
		if c.Name == "cs_admin" {
			h.admin = c.Value
		}
	}
	if h.admin == "" {
		t.Fatal("login set no cs_admin cookie")
	}
}

func (h *harness) adminHdr() map[string]string {
	return map[string]string{"Cookie": "cs_admin=" + h.admin}
}

func (h *harness) list(t *testing.T) []roomJSON {
	t.Helper()
	var rs []roomJSON
	if err := json.Unmarshal(h.expect(t, "GET", "/api/rooms", nil, nil, 200, ""), &rs); err != nil {
		t.Fatal(err)
	}
	return rs
}

func (h *harness) has(t *testing.T, id string) (roomJSON, bool) {
	for _, r := range h.list(t) {
		if r.ID == id {
			return r, true
		}
	}
	return roomJSON{}, false
}

type playerJSON struct {
	Userid  int    `json:"userid"`
	Name    string `json:"name"`
	Steamid string `json:"steamid"`
	Bot     bool   `json:"bot"`
}

func (h *harness) players(t *testing.T, r roomJSON) []playerJSON {
	t.Helper()
	var ps []playerJSON
	raw := h.expect(t, "GET", "/api/rooms/"+r.ID+"/players", nil, r.bearer(), 200, "")
	if err := json.Unmarshal(raw, &ps); err != nil {
		t.Fatalf("players body %s: %v", raw, err)
	}
	return ps
}

func waitFor(t *testing.T, d time.Duration, what string, cond func() bool) {
	t.Helper()
	for end := time.Now().Add(d); time.Now().Before(end); time.Sleep(50 * time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatalf("timed out after %v waiting for %s", d, what)
}

func readLines(path string) []string {
	b, _ := os.ReadFile(path)
	if len(b) == 0 {
		return nil
	}
	return strings.Split(strings.TrimRight(string(b), "\n"), "\n")
}

func linesWithPrefix(path, prefix string) []string {
	var out []string
	for _, l := range readLines(path) {
		if strings.HasPrefix(l, prefix) {
			out = append(out, l)
		}
	}
	return out
}

func oobMsg(s string) []byte { return append([]byte{0xff, 0xff, 0xff, 0xff}, s...) }

func ctx15(t *testing.T) context.Context {
	c, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)
	return c
}

func (h *harness) dial(t *testing.T, r roomJSON, opts ...testpeer.Option) *testpeer.Peer {
	t.Helper()
	p, resp, err := testpeer.Dial(ctx15(t), h.url, r.ID, opts...)
	if err != nil || resp.StatusCode != 201 {
		t.Fatalf("dial: %v (%v)", err, resp)
	}
	t.Cleanup(func() { p.Close() })
	return p
}

// recv returns the next datagram minus the OOB header, or ok=false after d.
func recv(p *testpeer.Peer, d time.Duration) (string, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	b, err := p.Recv(ctx)
	if err != nil {
		return "", false
	}
	return string(bytes.TrimPrefix(b, []byte{0xff, 0xff, 0xff, 0xff})), true
}

func whoami(t *testing.T, p *testpeer.Peer) netip.AddrPort {
	t.Helper()
	if err := p.Send(oobMsg("whoami")); err != nil {
		t.Fatal(err)
	}
	s, ok := recv(p, 5*time.Second)
	if !ok || !strings.HasPrefix(s, "seen ") {
		t.Fatalf("whoami reply %q ok=%v", s, ok)
	}
	ap, err := netip.ParseAddrPort(strings.TrimPrefix(s, "seen "))
	if err != nil {
		t.Fatalf("whoami reply %q: %v", s, err)
	}
	return ap
}

// join makes a fakehlds human whose adr is the peer's session address; returns its userid.
func join(t *testing.T, p *testpeer.Peer, name string) int {
	t.Helper()
	if err := p.Send(oobMsg("join " + name)); err != nil {
		t.Fatal(err)
	}
	s, ok := recv(p, 5*time.Second)
	var id int
	if _, err := fmt.Sscanf(s, "joined %d", &id); !ok || err != nil {
		t.Fatalf("join reply %q ok=%v", s, ok)
	}
	return id
}
