package api_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/fs"
	"math/rand"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"cs-duds/backend/internal/testpeer"
)

func tokenAfter(argv, flag string) string {
	f := strings.Fields(argv)
	for i := range f {
		if f[i] == flag && i+1 < len(f) {
			return f[i+1]
		}
	}
	return ""
}

func TestRoomLifecycle(t *testing.T) {
	h := start(t)
	h.login(t)

	var cat struct{ Maps, Modes []string }
	json.Unmarshal(h.expect(t, "GET", "/api/catalog", nil, nil, 200, ""), &cat)
	if !reflect.DeepEqual(cat.Maps, []string{"de_dust2", "de_inferno", "de_nuke"}) || !reflect.DeepEqual(cat.Modes, []string{"classic", "deathmatch", "gungame"}) {
		t.Fatalf("catalog %+v", cat)
	}

	raw := h.expect(t, "POST", "/api/rooms", map[string]string{"map": "de_dust2", "mode": "classic"}, nil, 201, "")
	var generic map[string]any
	json.Unmarshal(raw, &generic)
	for k := range generic {
		if strings.Contains(strings.ToLower(k), "port") || k == "addr" || k == "rcon" {
			t.Errorf("create response leaks %q: %s", k, raw)
		}
	}
	var r roomJSON
	json.Unmarshal(raw, &r)
	if r.ID == "" || len(r.HostToken) < 32 || r.Map != "de_dust2" || r.Mode != "classic" || r.MaxPlayers != 16 || r.Players != 0 || r.CreatedAt.IsZero() {
		t.Fatalf("CreatedRoom %s", raw)
	}
	if got, ok := h.has(t, r.ID); !ok || got.Players != 0 || got.HostToken != "" {
		t.Fatalf("listed room %+v ok=%v (hostToken must not be listed)", got, ok)
	}
	if body := strings.TrimSpace(string(h.expect(t, "GET", "/api/rooms/"+r.ID+"/players", nil, r.bearer(), 200, ""))); body != "[]" {
		t.Fatalf("players of an empty room: %s", body)
	}
	h.expect(t, "POST", "/api/rooms/"+r.ID+"/kick", map[string]int{"userid": 99}, r.bearer(), 204, "")
	raw = h.expect(t, "POST", "/api/rooms/"+r.ID+"/vote-map", map[string][]string{"maps": {"de_inferno", "de_nuke"}}, r.bearer(), 202, "")
	if strings.TrimSpace(string(raw)) != "{}" {
		t.Errorf("vote body %s", raw)
	}
	h.expect(t, "POST", "/api/rooms/"+r.ID+"/vote-map", map[string][]string{"maps": {"de_inferno"}}, r.bearer(), 409, "vote_running")

	h.expect(t, "DELETE", "/api/rooms/"+r.ID, nil, nil, 403, "not_admin")
	h.expect(t, "DELETE", "/api/rooms/"+r.ID, nil, h.adminHdr(), 204, "")
	if _, ok := h.has(t, r.ID); ok {
		t.Fatal("room still listed after DELETE")
	}
	h.expect(t, "DELETE", "/api/rooms/"+r.ID, nil, h.adminHdr(), 404, "room_not_found")

	r2 := h.create(t, "de_nuke", "deathmatch") // the freed lowest port is reused
	if r2.ID == r.ID {
		t.Error("room ids must not repeat")
	}
	argv := readLines(h.argvLog)
	if len(argv) != 2 {
		t.Fatalf("hlds started %d times, want 2: %q", len(argv), argv)
	}
	low := fmt.Sprint(h.ports[0])
	for i, want := range []struct{ mp, mode string }{{"de_dust2", "classic"}, {"de_nuke", "deathmatch"}} {
		a := argv[i]
		if tokenAfter(a, "-port") != low || tokenAfter(a, "-game") != "cstrike" || tokenAfter(a, "+ip") != "127.0.0.1" ||
			tokenAfter(a, "-maxplayers") != "16" || tokenAfter(a, "+map") != want.mp ||
			tokenAfter(a, "+mapchangecfgfile") != "modes/"+want.mode+"/mode.cfg" || tokenAfter(a, "+rcon_password") == "" {
			t.Errorf("argv[%d] = %q (want port %s, map %s, mode %s)", i, a, low, want.mp, want.mode)
		}
		if c, m := strings.Index(a, "+sv_rcon_condebug 0"), strings.Index(a, "+map"); c < 0 || c > m {
			t.Errorf("argv[%d]: +sv_rcon_condebug 0 must precede +map: %q", i, a)
		}
	}
}

func TestCapacityIsPortRange(t *testing.T) {
	h := start(t, func(s *spec) { s.cfg.Rooms.Ports = freePorts(t, 2) })
	h.login(t)
	a := h.create(t, "de_dust2", "classic")
	h.create(t, "de_dust2", "classic")
	h.expect(t, "POST", "/api/rooms", map[string]string{"map": "de_dust2", "mode": "classic"}, nil, 503, "rooms_full")
	h.expect(t, "DELETE", "/api/rooms/"+a.ID, nil, h.adminHdr(), 204, "")
	h.create(t, "de_dust2", "classic")
}

func TestCrashedRoomIsCleanedUp(t *testing.T) {
	h := start(t, func(s *spec) {
		s.maps = []string{"de_dust2", "crash"}
		s.cfg.Rooms.Ports = freePorts(t, 1)
	})
	r := h.create(t, "crash", "classic")
	waitFor(t, 10*time.Second, "the crashed room to leave the list", func() bool { _, ok := h.has(t, r.ID); return !ok })
	h.expect(t, "POST", "/api/rooms/"+r.ID+"/session", "v=0", map[string]string{"Content-Type": "application/sdp"}, 404, "room_not_found")
	h.create(t, "de_dust2", "classic") // the only port is reusable
}

func TestIdleReap(t *testing.T) {
	h := start(t, func(s *spec) { s.cfg.Rooms.IdleTimeout = 1500 * time.Millisecond })
	h.login(t)
	empty := h.create(t, "de_dust2", "classic")
	busy := h.create(t, "de_inferno", "classic")
	p := h.dial(t, busy)
	join(t, p, "bob")
	waitFor(t, 10*time.Second, "the empty room to be reaped", func() bool { _, ok := h.has(t, empty.ID); return !ok })
	waitFor(t, 5*time.Second, "players=1 in the list", func() bool { r, ok := h.has(t, busy.ID); return ok && r.Players == 1 })
	time.Sleep(2500 * time.Millisecond) // > idle timeout with a human inside
	if _, ok := h.has(t, busy.ID); !ok {
		t.Fatal("room with a human was reaped")
	}
	ps := h.players(t, busy)
	h.expect(t, "POST", "/api/rooms/"+busy.ID+"/kick", map[string]int{"userid": ps[0].Userid}, busy.bearer(), 204, "")
	waitFor(t, 10*time.Second, "the room to be reaped after its last human left", func() bool { _, ok := h.has(t, busy.ID); return !ok })
}

func TestStopRacesReap(t *testing.T) {
	h := start(t, func(s *spec) { s.cfg.Rooms.IdleTimeout = 400 * time.Millisecond })
	h.login(t)
	for i := 1; i <= 3; i++ {
		r := h.create(t, "de_dust2", "classic")
		time.Sleep(time.Duration(250+50*i) * time.Millisecond) // straddle the reaper's deadline
		var wg sync.WaitGroup
		codes := make([]int, 3)
		for j := range codes {
			wg.Add(1)
			go func() {
				defer wg.Done()
				resp, _ := h.do("DELETE", "/api/rooms/"+r.ID, nil, h.adminHdr())
				codes[j] = resp.StatusCode
			}()
		}
		wg.Wait()
		for _, c := range codes {
			if c != 204 && c != 404 {
				t.Fatalf("iteration %d: DELETE returned %d, want 204 or 404 (%v)", i, c, codes)
			}
		}
		waitFor(t, 5*time.Second, "room gone", func() bool { _, ok := h.has(t, r.ID); return !ok })
		if n := len(readLines(h.sigtermLog)); n != i {
			t.Fatalf("iteration %d: %d SIGTERMs in total, want exactly one per room (%d)", i, n, i)
		}
	}
}

func TestCreateValidation(t *testing.T) {
	h := start(t)
	post := func(body any, status int, code string) {
		t.Helper()
		h.expect(t, "POST", "/api/rooms", body, nil, status, code)
	}
	post(map[string]string{"map": "de_nope", "mode": "classic"}, 400, "unknown_map")
	post(map[string]string{"map": "de_dust2", "mode": "nope"}, 400, "unknown_mode")
	for _, bad := range []string{"de_dust2;quit", "de_dust2 +rcon_password x", "../../etc/passwd", "de_dust2\nquit", "DE_DUST2", ""} {
		post(map[string]string{"map": bad, "mode": "classic"}, 400, "unknown_map")
		post(map[string]string{"map": "de_dust2", "mode": bad}, 400, "unknown_mode")
	}
	post("{not json", 400, "bad_request")
	post("", 400, "bad_request")
	post(`{"map":1,"mode":"classic"}`, 400, "bad_request")
	if lines := readLines(h.argvLog); lines != nil {
		t.Fatalf("hlds was spawned for invalid input: %q", lines)
	}
	if n := len(h.list(t)); n != 0 {
		t.Fatalf("%d rooms listed", n)
	}
}

func TestModeProbe(t *testing.T) {
	h := start(t, func(s *spec) {
		s.cfg.Rooms.Ports = freePorts(t, 1)
		s.cfg.Rooms.ReadyTimeout = 2 * time.Second
	})
	h.login(t)
	modes := []string{"classic", "deathmatch", "gungame"}
	for _, m := range modes { // probe passes: 201 means joinable and mode-correct
		r := h.create(t, "de_dust2", m)
		h.expect(t, "DELETE", "/api/rooms/"+r.ID, nil, h.adminHdr(), 204, "")
	}
	t.Setenv("FAKEHLDS_BREAK_PROBE", "1") // fakehlds ignores the mode cfg: every probe cvar is unset
	before := len(readLines(h.sigtermLog))
	for _, m := range modes {
		h.expect(t, "POST", "/api/rooms", map[string]string{"map": "de_dust2", "mode": m}, nil, 504, "start_failed")
		if n := len(h.list(t)); n != 0 {
			t.Fatalf("mode %s: failed room is listed", m)
		}
	}
	if n := len(readLines(h.sigtermLog)) - before; n != len(modes) {
		t.Errorf("%d failed rooms were SIGTERMed, want %d (the process must be killed)", n, len(modes))
	}
	t.Setenv("FAKEHLDS_BREAK_PROBE", "0")
	h.create(t, "de_dust2", "classic") // the single port was freed each time
}

func TestHostAndAdminAuth(t *testing.T) {
	h := start(t, func(s *spec) { s.cfg.LoginLimitBurst, s.cfg.LoginLimitEvery = 5, time.Hour })
	r := h.create(t, "de_dust2", "classic")
	paths := []struct {
		method, path string
		body         any
	}{
		{"GET", "/api/rooms/" + r.ID + "/players", nil},
		{"POST", "/api/rooms/" + r.ID + "/kick", map[string]int{"userid": 5}},
		{"POST", "/api/rooms/" + r.ID + "/vote-map", map[string][]string{"maps": {"de_nuke"}}},
	}
	for _, p := range paths {
		h.expect(t, p.method, p.path, p.body, nil, 403, "not_host")
		h.expect(t, p.method, p.path, p.body, map[string]string{"Authorization": "Bearer nope"}, 403, "not_host")
	}
	h.expect(t, "GET", paths[0].path, nil, r.bearer(), 200, "")
	h.expect(t, "POST", paths[1].path, paths[1].body, r.bearer(), 204, "")
	h.expect(t, "GET", "/api/admin", nil, nil, 401, "")
	h.expect(t, "DELETE", "/api/rooms/"+r.ID, nil, nil, 403, "not_admin")
	h.expect(t, "DELETE", "/api/rooms/"+r.ID, nil, map[string]string{"Authorization": "Bearer " + r.HostToken}, 403, "not_admin")

	// login: wrong -> 401, right -> cookie; secure only behind https
	h.expect(t, "POST", "/api/admin/login", map[string]string{"password": "wrong"}, nil, 401, "bad_password")
	resp, _ := h.do("POST", "/api/admin/login", map[string]string{"password": adminPassword}, nil)
	var plain *http.Cookie
	for _, c := range resp.Cookies() {
		if c.Name == "cs_admin" {
			plain = c
		}
	}
	if resp.StatusCode != 204 || plain == nil || plain.Secure || !plain.HttpOnly || plain.Path != "/api" || plain.SameSite != http.SameSiteStrictMode || plain.MaxAge != 43200 {
		t.Fatalf("login over http: status %d cookie %+v", resp.StatusCode, plain)
	}
	resp, _ = h.do("POST", "/api/admin/login", map[string]string{"password": adminPassword}, map[string]string{"X-Forwarded-Proto": "https"})
	var sec *http.Cookie
	for _, c := range resp.Cookies() {
		if c.Name == "cs_admin" {
			sec = c
		}
	}
	if resp.StatusCode != 204 || sec == nil || !sec.Secure {
		t.Fatalf("login behind https: status %d cookie %+v", resp.StatusCode, sec)
	}
	cookie := func(v string) map[string]string { return map[string]string{"Cookie": "cs_admin=" + v} }
	h.expect(t, "GET", "/api/admin", nil, cookie(plain.Value), 204, "")
	// admin cookie works for host actions in any room
	h.expect(t, "GET", paths[0].path, nil, cookie(plain.Value), 200, "")
	h.expect(t, "POST", paths[2].path, paths[2].body, cookie(plain.Value), 202, "")
	// tampered: flipped mac, or a later expiry with the old mac
	parts := strings.Split(plain.Value, ".")
	if len(parts) != 3 || parts[0] != "v1" {
		t.Fatalf("cookie format %q, want v1.<exp>.<mac>", plain.Value)
	}
	last := parts[2][len(parts[2])-1]
	flip := map[byte]byte{'0': '1'}[last]
	if flip == 0 {
		flip = '0'
	}
	h.expect(t, "GET", "/api/admin", nil, cookie(parts[0]+"."+parts[1]+"."+parts[2][:len(parts[2])-1]+string(flip)), 401, "")
	h.expect(t, "GET", "/api/admin", nil, cookie(parts[0]+".99999999999."+parts[2]), 401, "")
	h.expect(t, "GET", "/api/admin", nil, cookie("garbage"), 401, "")
	// 5th attempt (the burst) passes, the 6th is rate limited
	h.expect(t, "POST", "/api/admin/login", map[string]string{"password": "wrong"}, nil, 401, "bad_password")
	h.expect(t, "POST", "/api/admin/login", map[string]string{"password": "wrong"}, nil, 401, "bad_password")
	h.expect(t, "POST", "/api/admin/login", map[string]string{"password": adminPassword}, nil, 429, "rate_limited")
	h.expect(t, "POST", "/api/admin/login", map[string]string{"password": adminPassword}, map[string]string{"X-Forwarded-For": "203.0.113.50"}, 204, "")
	h.expect(t, "DELETE", "/api/rooms/"+r.ID, nil, cookie(plain.Value), 204, "")

	off := start(t, func(s *spec) { s.cfg.AdminPasswordHash = nil })
	off.expect(t, "POST", "/api/admin/login", map[string]string{"password": adminPassword}, nil, 404, "admin_disabled")
}

func TestCreateRateLimitPerIP(t *testing.T) {
	h := start(t, func(s *spec) { s.cfg.CreateLimitBurst, s.cfg.CreateLimitEvery = 3, time.Hour })
	h.login(t)
	ip := func(a string) map[string]string { return map[string]string{"X-Forwarded-For": a} }
	for i := 0; i < 3; i++ {
		r := h.create(t, "de_dust2", "classic", ip("198.51.100.1"))
		h.expect(t, "DELETE", "/api/rooms/"+r.ID, nil, h.adminHdr(), 204, "")
	}
	h.expect(t, "POST", "/api/rooms", map[string]string{"map": "de_dust2", "mode": "classic"}, ip("198.51.100.1"), 429, "rate_limited")
	h.create(t, "de_dust2", "classic", ip("198.51.100.2"))
}

func TestKickByUserID(t *testing.T) {
	h := start(t)
	r := h.create(t, "de_dust2", "classic")
	hostile := `a";quit;"`
	names := []string{hostile, "bob", "x y #3"}
	ids := map[string]int{}
	for _, n := range names {
		ids[n] = join(t, h.dial(t, r), n)
	}
	got := h.players(t, r)
	var gotNames []string
	byName := map[string]playerJSON{}
	for _, p := range got {
		gotNames = append(gotNames, p.Name)
		byName[p.Name] = p
		if !strings.HasPrefix(p.Steamid, "STEAM_") || p.Bot {
			t.Errorf("player %+v", p)
		}
	}
	want := append([]string(nil), names...)
	sort.Strings(want)
	sort.Strings(gotNames)
	if !reflect.DeepEqual(gotNames, want) {
		t.Fatalf("players %q, want names listed intact %q", gotNames, want)
	}
	for n, id := range ids {
		if byName[n].Userid != id {
			t.Errorf("%q: userid %d, want %d", n, byName[n].Userid, id)
		}
	}
	// only a positive integer userid is accepted
	for _, bad := range []string{`{"userid":0}`, `{"userid":-1}`, `{"userid":"1;quit"}`, `{"userid":1.5}`, `{}`, `nope`} {
		h.expect(t, "POST", "/api/rooms/"+r.ID+"/kick", bad, r.bearer(), 400, "bad_request")
	}
	h.expect(t, "POST", "/api/rooms/"+r.ID+"/kick", map[string]int{"userid": byName[hostile].Userid}, r.bearer(), 204, "")
	if k := linesWithPrefix(h.rconLog, "kick"); !reflect.DeepEqual(k, []string{fmt.Sprintf("kick #%d", byName[hostile].Userid)}) {
		t.Fatalf("kick lines sent to the server: %q", k)
	}
	for _, l := range readLines(h.rconLog) {
		if strings.Contains(l, "quit") {
			t.Fatalf("hostile text reached RCON: %q", l)
		}
	}
	if ps := h.players(t, r); len(ps) != 2 {
		t.Fatalf("after kick: %+v", ps)
	}
	h.expect(t, "POST", "/api/rooms/"+r.ID+"/kick", map[string]int{"userid": 9999}, r.bearer(), 204, "") // idempotent
	if ps := h.players(t, r); len(ps) != 2 {
		t.Fatalf("after kicking an unknown userid: %+v", ps)
	}
}

func TestVoteMap(t *testing.T) {
	h := start(t)
	r := h.create(t, "de_dust2", "classic")
	vote := func(maps []string, status int, code string) {
		t.Helper()
		h.expect(t, "POST", "/api/rooms/"+r.ID+"/vote-map", map[string][]string{"maps": maps}, r.bearer(), status, code)
	}
	vote([]string{"cs_italy"}, 400, "unknown_map")
	vote([]string{"de_inferno;quit"}, 400, "unknown_map")
	vote([]string{"de_inferno", "de_inferno\nquit"}, 400, "unknown_map")
	vote(nil, 400, "bad_request")
	vote([]string{"de_inferno", "de_inferno"}, 400, "bad_request")
	vote([]string{"de_dust2", "de_inferno", "de_nuke", "de_dust2", "de_nuke"}, 400, "bad_request")
	if v := linesWithPrefix(h.rconLog, "amx_votemap"); v != nil {
		t.Fatalf("rejected votes reached the server: %q", v)
	}
	vote([]string{"de_inferno", "de_nuke"}, 202, "")
	vote([]string{"de_dust2"}, 409, "vote_running")
	// the 409 comes from the server, so the second vote reaches it too
	if v := linesWithPrefix(h.rconLog, "amx_votemap"); !reflect.DeepEqual(v, []string{"amx_votemap de_inferno de_nuke", "amx_votemap de_dust2"}) {
		t.Fatalf("vote lines %q", v)
	}
}

func TestUnknownRoomIs404(t *testing.T) {
	h := start(t)
	h.login(t)
	h.expect(t, "POST", "/api/rooms/nope/session", "v=0", map[string]string{"Content-Type": "application/sdp"}, 404, "room_not_found")
	h.expect(t, "DELETE", "/api/rooms/nope", nil, h.adminHdr(), 404, "room_not_found")
}

// ---- relay, black-box through the session endpoint and the data channel ----

func TestDatagramsRoundTrip(t *testing.T) {
	h := start(t)
	r := h.create(t, "de_dust2", "classic")
	p := h.dial(t, r)
	ap := whoami(t, p)
	if !netip.MustParsePrefix("127.10.0.0/16").Contains(ap.Addr()) {
		t.Fatalf("room saw %v, want a 127.10.x.y source", ap)
	}
	payload := oobMsg("echo")
	body := make([]byte, 1400-len(payload))
	rand.New(rand.NewSource(7)).Read(body)
	payload = append(payload, body...)
	if err := p.Send(payload); err != nil {
		t.Fatal(err)
	}
	ctx := ctx15(t)
	got, err := p.Recv(ctx)
	if err != nil || !bytes.Equal(got, payload) {
		t.Fatalf("1400-byte round trip: err=%v len=%d equal=%v", err, len(got), bytes.Equal(got, payload))
	}
}

func TestDistinctSourcePerSession(t *testing.T) {
	h := start(t)
	r := h.create(t, "de_dust2", "classic")
	seen := map[netip.Addr]bool{}
	for i := 0; i < 3; i++ {
		a := whoami(t, h.dial(t, r)).Addr()
		if a == netip.MustParseAddr("127.0.0.1") || !netip.MustParsePrefix("127.10.0.0/16").Contains(a) {
			t.Fatalf("session %d source %v", i, a)
		}
		seen[a] = true
	}
	if len(seen) != 3 {
		t.Fatalf("sources %v, want 3 distinct", seen)
	}
}

func TestOnlyTheRoomIsReachable(t *testing.T) {
	h := start(t)
	r := h.create(t, "de_dust2", "classic") // takes ports[0]
	canary := func(addr string) (*net.UDPConn, *atomic.Int64) {
		a, _ := net.ResolveUDPAddr("udp4", addr)
		c, err := net.ListenUDP("udp4", a)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { c.Close() })
		n := new(atomic.Int64)
		go func() {
			b := make([]byte, 65535)
			for {
				if _, _, err := c.ReadFromUDP(b); err != nil {
					return
				}
				n.Add(1)
			}
		}()
		return c, n
	}
	c1, n1 := canary("127.0.0.1:0")
	c2, n2 := canary(fmt.Sprintf("127.0.0.1:%d", h.ports[1])) // a free port inside the room range
	targets := []string{c1.LocalAddr().String(), c2.LocalAddr().String(), fmt.Sprint(h.ports[1]), fmt.Sprint(h.ports[0])}
	for pi := 0; pi < 2; pi++ {
		p := h.dial(t, r)
		for i := 0; i < 20; i++ {
			for _, tg := range targets {
				ua, _ := net.ResolveUDPAddr("udp4", c1.LocalAddr().String())
				bin := append(ua.IP.To4(), byte(ua.Port>>8), byte(ua.Port))
				for _, pl := range [][]byte{
					[]byte(tg), oobMsg("connect " + tg), []byte(`{"addr":"` + tg + `","dst":"` + tg + `"}`),
					append([]byte(tg+"\x00"), bin...), bin, []byte(r.ID + "@" + tg),
				} {
					p.Send(pl)
				}
			}
		}
		// the session still works, and a datagram from a non-room socket cannot reach it
		ap := whoami(t, p)
		if _, err := c1.WriteToUDP([]byte("evil"), net.UDPAddrFromAddrPort(ap)); err != nil {
			t.Fatal(err)
		}
		if s, ok := recv(p, 500*time.Millisecond); ok {
			t.Fatalf("a datagram from a non-room socket reached the peer: %q", s)
		}
	}
	time.Sleep(300 * time.Millisecond)
	if n1.Load() != 0 || n2.Load() != 0 {
		t.Fatalf("canaries received %d and %d datagrams, want 0", n1.Load(), n2.Load())
	}
	// the room id is the only destination: ports and addresses are not rooms
	sdp := map[string]string{"Content-Type": "application/sdp"}
	for _, id := range []string{fmt.Sprint(h.ports[0]), "127.0.0.1:" + fmt.Sprint(h.ports[0]), "..%2Fx", "127.0.0.1"} {
		h.expect(t, "POST", "/api/rooms/"+id+"/session", "v=0", sdp, 404, "room_not_found")
	}
}

func TestRejectsNonGameChannels(t *testing.T) {
	h := start(t)
	r := h.create(t, "de_dust2", "classic")
	good := h.dial(t, r)
	for name, opt := range map[string]testpeer.Option{
		"ordered":     testpeer.WithOrdered(),
		"retransmits": testpeer.WithRetransmits(3),
		"label":       testpeer.WithLabel("chat"),
		"second":      testpeer.WithExtraChannel(),
	} {
		p, resp, err := testpeer.Dial(ctx15(t), h.url, r.ID, opt)
		if err != nil || resp.StatusCode != 201 {
			t.Fatalf("%s: dial %v %v (the offer itself is accepted, the channel is judged later)", name, err, resp)
		}
		select {
		case <-p.Closed():
		case <-time.After(time.Second):
			t.Errorf("%s: peer connection still open 1s after Dial", name)
		}
		p.Close()
	}
	whoami(t, good) // the well-formed session was unaffected
}

func TestSessionLimits(t *testing.T) {
	h := start(t, func(s *spec) { s.cfg.Relay.SessionLimitPerIP = 2 })
	r := h.create(t, "de_dust2", "classic")
	from := func(ip string) []testpeer.Option {
		return []testpeer.Option{testpeer.WithHeader("X-Forwarded-For", ip)}
	}
	a1 := h.dial(t, r, from("198.51.100.1")...)
	h.dial(t, r, from("198.51.100.1")...)
	_, resp, err := testpeer.Dial(ctx15(t), h.url, r.ID, from("198.51.100.1")...)
	var e struct{ Code string }
	if resp != nil {
		json.NewDecoder(resp.Body).Decode(&e)
	}
	if err == nil || resp == nil || resp.StatusCode != 429 || e.Code != "too_many_sessions" {
		t.Fatalf("third session from one IP: err=%v resp=%v code=%q, want 429 too_many_sessions", err, resp, e.Code)
	}
	other := h.dial(t, r, from("198.51.100.2")...) // another client IP is unaffected
	a1.Close()                                     // closing frees a slot
	waitFor(t, 8*time.Second, "a slot to free after close", func() bool {
		p, _, err := testpeer.Dial(ctx15(t), h.url, r.ID, from("198.51.100.1")...)
		if err == nil {
			t.Cleanup(func() { p.Close() })
		}
		return err == nil
	})

	// oversize upstream datagram (> 4096 B) is dropped, a 4000-byte one is not
	big := append(oobMsg("whoami"), make([]byte, 5000-10)...)
	mid := append(oobMsg("whoami"), make([]byte, 4000-10)...)
	if len(big) != 5000 || len(mid) != 4000 {
		t.Fatal("test bug")
	}
	other.Send(big)
	other.Send(mid)
	if s, ok := recv(other, 5*time.Second); !ok || !strings.HasPrefix(s, "seen ") {
		t.Fatalf("4000-byte datagram got no reply (%q)", s)
	}
	if s, ok := recv(other, 500*time.Millisecond); ok {
		t.Fatalf("5000-byte datagram was delivered (second reply %q)", s)
	}
}

func TestSessionEndsWithRoom(t *testing.T) {
	h := start(t)
	h.login(t)
	r := h.create(t, "de_dust2", "classic")
	p := h.dial(t, r)
	whoami(t, p)
	h.expect(t, "DELETE", "/api/rooms/"+r.ID, nil, h.adminHdr(), 204, "")
	select {
	case <-p.Closed():
	case <-time.After(2 * time.Second):
		t.Fatal("peer channel still open 2s after the room was deleted")
	}
	h.expect(t, "POST", "/api/rooms/"+r.ID+"/session", "v=0", map[string]string{"Content-Type": "application/sdp"}, 404, "room_not_found")
}

// Amendment A: the host picks the room size (2-32, default 16).
func TestCreateMaxPlayers(t *testing.T) {
	h := start(t)
	h.login(t)
	raw := h.expect(t, "POST", "/api/rooms", map[string]any{"map": "de_dust2", "mode": "classic", "maxPlayers": 10}, nil, 201, "")
	var r roomJSON
	json.Unmarshal(raw, &r)
	if r.MaxPlayers != 10 {
		t.Fatalf("maxPlayers %d, want 10: %s", r.MaxPlayers, raw)
	}
	if got, ok := h.has(t, r.ID); !ok || got.MaxPlayers != 10 {
		t.Fatalf("listed room %+v ok=%v", got, ok)
	}
	if a := readLines(h.argvLog); len(a) != 1 || tokenAfter(a[0], "-maxplayers") != "10" {
		t.Fatalf("argv %q, want -maxplayers 10", a)
	}
	h.expect(t, "DELETE", "/api/rooms/"+r.ID, nil, h.adminHdr(), 204, "")

	if d := h.create(t, "de_dust2", "classic"); d.MaxPlayers != 16 { // omitted -> default
		t.Fatalf("default maxPlayers %d, want 16", d.MaxPlayers)
	}
	for _, bad := range []any{1, 33, 0, -4, "10", 10.5} {
		h.expect(t, "POST", "/api/rooms", map[string]any{"map": "de_dust2", "mode": "classic", "maxPlayers": bad}, nil, 400, "bad_request")
	}
	if n := len(readLines(h.argvLog)); n != 2 {
		t.Fatalf("hlds started %d times, want 2 (invalid sizes must not spawn)", n)
	}
}

// Amendment B: host/admin bot controls over RCON.
func TestBots(t *testing.T) {
	h := start(t)
	h.login(t)
	r := h.create(t, "de_dust2", "classic")
	add := func(body any, hdr map[string]string, status int, code string) {
		t.Helper()
		h.expect(t, "POST", "/api/rooms/"+r.ID+"/bots", body, hdr, status, code)
	}
	add(map[string]any{"team": "ct", "difficulty": 2}, r.bearer(), 204, "")
	add(map[string]any{"team": "t", "difficulty": 0}, h.adminHdr(), 204, "")
	for _, bad := range []any{map[string]any{"team": "spec", "difficulty": 1}, map[string]any{"team": "ct", "difficulty": 4},
		map[string]any{"team": "ct;quit", "difficulty": 1}, map[string]any{"team": "ct", "difficulty": "2"}} {
		add(bad, r.bearer(), 400, "bad_request")
	}
	add(map[string]any{"team": "ct", "difficulty": 1}, nil, 403, "not_host")
	h.expect(t, "DELETE", "/api/rooms/"+r.ID+"/bots", nil, nil, 403, "not_host")
	h.expect(t, "DELETE", "/api/rooms/"+r.ID+"/bots", nil, r.bearer(), 204, "")
	var got []string
	for _, l := range readLines(h.rconLog) {
		if strings.HasPrefix(l, "bot_") {
			got = append(got, l)
		}
	}
	want := []string{"bot_difficulty 2", "bot_add_ct", "bot_difficulty 0", "bot_add_t", "bot_kick"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("bot rcon lines %q, want %q", got, want)
	}
}

// Amendment B: the creator's own session (opened with the host token) becomes an AMXX admin of that room only.
func TestHostIsAdminInOwnRoomOnly(t *testing.T) {
	h := start(t)
	r := h.create(t, "de_dust2", "classic")
	other := h.create(t, "de_nuke", "classic")
	host := h.dial(t, r, testpeer.WithHeader("Authorization", "Bearer "+r.HostToken))
	guest := h.dial(t, r)
	stranger := h.dial(t, other, testpeer.WithHeader("Authorization", "Bearer "+r.HostToken)) // wrong room's token
	hostIP, guestIP, strangerIP := whoami(t, host).Addr().String(), whoami(t, guest).Addr().String(), whoami(t, stranger).Addr().String()
	adminLine := func(ip string) bool { // users.ini format, IP-based, no password, wherever the backend keeps it
		found := false
		filepath.WalkDir(h.hldsDir, func(p string, d fs.DirEntry, err error) error {
			if err == nil && !d.IsDir() && strings.HasSuffix(p, ".ini") {
				b, _ := os.ReadFile(p)
				for _, l := range strings.Split(string(b), "\n") {
					if strings.HasPrefix(strings.TrimSpace(l), `"`+ip+`"`) && strings.Contains(l, `"de"`) {
						found = true
					}
				}
			}
			return nil
		})
		return found
	}
	waitFor(t, 5*time.Second, "the host's IP registered as admin", func() bool { return adminLine(hostIP) })
	if adminLine(guestIP) || adminLine(strangerIP) {
		t.Fatal("only the host's own session in its own room may be admin")
	}
	if len(linesWithPrefix(h.rconLog, "amx_reloadadmins")) == 0 {
		t.Fatal("admins were not reloaded after registering the host")
	}
	host.Close()
	waitFor(t, 10*time.Second, "the host's admin entry removed after its session ended", func() bool { return !adminLine(hostIP) })
}

// Amendment B: any player with a live session in the room can start a stock map or kick vote.
func TestPlayerVotes(t *testing.T) {
	h := start(t)
	r := h.create(t, "de_dust2", "classic")
	other := h.create(t, "de_nuke", "classic")
	p, resp, err := testpeer.Dial(ctx15(t), h.url, r.ID)
	if err != nil || resp.StatusCode != 201 {
		t.Fatalf("dial: %v (%v)", err, resp)
	}
	t.Cleanup(func() { p.Close() })
	tok := resp.Header.Get("X-Session-Token")
	if len(tok) < 32 {
		t.Fatalf("X-Session-Token %q", tok)
	}
	victim := h.dial(t, r)
	vid := join(t, victim, "victim")
	sess := map[string]string{"Authorization": "Session " + tok}
	vote := func(room string, body any, hdr map[string]string, status int, code string) {
		t.Helper()
		h.expect(t, "POST", "/api/rooms/"+room+"/votes", body, hdr, status, code)
	}
	vote(r.ID, map[string]any{"kind": "map", "maps": []string{"de_inferno"}}, nil, 403, "")
	vote(other.ID, map[string]any{"kind": "map", "maps": []string{"de_inferno"}}, sess, 403, "") // token belongs to another room
	vote(r.ID, map[string]any{"kind": "map", "maps": []string{"cs_italy"}}, sess, 400, "unknown_map")
	vote(r.ID, map[string]any{"kind": "kick", "userid": 9999}, sess, 404, "")
	vote(r.ID, map[string]any{"kind": "nuke"}, sess, 400, "bad_request")
	vote(r.ID, map[string]any{"kind": "map", "maps": []string{"de_inferno", "de_nuke"}}, sess, 202, "")
	vote(r.ID, map[string]any{"kind": "map", "maps": []string{"de_nuke"}}, sess, 409, "vote_running")
	if v := linesWithPrefix(h.rconLog, "amx_votemap"); len(v) == 0 || v[0] != "amx_votemap de_inferno de_nuke" {
		t.Fatalf("vote lines %q", v)
	}
	// AMXX runs one vote at a time: a kick vote during the running map vote is refused by the server
	vote(r.ID, map[string]any{"kind": "kick", "userid": vid}, map[string]string{"Authorization": "Bearer " + r.HostToken}, 409, "vote_running")
	victim2 := h.dial(t, other)
	vid2 := join(t, victim2, "victim2")
	vote(other.ID, map[string]any{"kind": "kick", "userid": vid2}, map[string]string{"Authorization": "Bearer " + other.HostToken}, 202, "")
	if v := linesWithPrefix(h.rconLog, "amx_votekick"); len(v) != 2 || v[1] != fmt.Sprintf("amx_votekick #%d", vid2) {
		t.Fatalf("votekick lines %q", v)
	}
	p.Close()
	waitFor(t, 10*time.Second, "a closed session's token to stop working", func() bool {
		resp, _ := h.do("POST", "/api/rooms/"+r.ID+"/votes", map[string]any{"kind": "map", "maps": []string{"de_nuke"}}, sess)
		return resp.StatusCode == 403
	})
}

// Votes, kicks and bots each cost RCON round trips, so one address can't spam them.
func TestCommandsRateLimited(t *testing.T) {
	h := start(t)
	r := h.create(t, "de_dust2", "classic")
	for i := 0; i < 60; i++ { // burst 20 at 1/s refill: a tight loop hits the limit well before 60
		req, _ := http.NewRequest("DELETE", h.url+"/api/rooms/"+r.ID+"/bots", nil)
		for k, v := range r.bearer() {
			req.Header.Set(k, v)
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode == 429 {
			if i < 20 {
				t.Fatalf("limited after %d commands, want at least the burst of 20", i)
			}
			return
		}
	}
	t.Fatal("60 commands in a row were never rate limited")
}

// A create that fails on our side (no free port) gives the caller's token back.
func TestFailedCreateIsNotCharged(t *testing.T) {
	h := start(t, func(s *spec) {
		s.cfg.Rooms.Ports = freePorts(t, 1)
		s.cfg.CreateLimitBurst, s.cfg.CreateLimitEvery = 2, time.Hour
	})
	h.login(t)
	a := h.create(t, "de_dust2", "classic")
	for i := 0; i < 3; i++ {
		h.expect(t, "POST", "/api/rooms", map[string]string{"map": "de_dust2", "mode": "classic"}, nil, 503, "rooms_full")
	}
	h.expect(t, "DELETE", "/api/rooms/"+a.ID, nil, h.adminHdr(), 204, "")
	h.create(t, "de_dust2", "classic") // the second token is still there
}

// One address can't hold every room port: it may keep RoomsPerIP rooms running; admins are exempt.
func TestRoomsPerIP(t *testing.T) {
	h := start(t, func(s *spec) { s.cfg.RoomsPerIP = 1 })
	ip := func(a string) map[string]string { return map[string]string{"X-Forwarded-For": a} }
	a := h.create(t, "de_dust2", "classic", ip("198.51.100.1"))
	h.expect(t, "POST", "/api/rooms", map[string]string{"map": "de_dust2", "mode": "classic"}, ip("198.51.100.1"), 429, "too_many_rooms")
	h.create(t, "de_dust2", "classic", ip("198.51.100.2"))
	h.login(t)
	h.expect(t, "DELETE", "/api/rooms/"+a.ID, nil, h.adminHdr(), 204, "")
	h.create(t, "de_dust2", "classic", ip("198.51.100.1")) // its room is gone, so it may create again
}

// Players in the room may list it (to pick a Vote kick target); outsiders and other rooms' tokens may not.
func TestPlayersListOpenToRoomMembers(t *testing.T) {
	h := start(t)
	r := h.create(t, "de_dust2", "classic")
	other := h.create(t, "de_nuke", "classic")
	p, resp, err := testpeer.Dial(ctx15(t), h.url, r.ID)
	if err != nil || resp.StatusCode != 201 {
		t.Fatalf("dial: %v (%v)", err, resp)
	}
	t.Cleanup(func() { p.Close() })
	sess := map[string]string{"Authorization": "Session " + resp.Header.Get("X-Session-Token")}
	h.expect(t, "GET", "/api/rooms/"+r.ID+"/players", nil, sess, 200, "")
	h.expect(t, "GET", "/api/rooms/"+other.ID+"/players", nil, sess, 403, "not_host")
	h.expect(t, "POST", "/api/rooms/"+r.ID+"/kick", map[string]int{"userid": 5}, sess, 403, "not_host")
}
