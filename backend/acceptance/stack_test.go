//go:build stack

// Black-box tests against the running compose stack. `scripts/test stack` starts it with
// ROOM_IDLE_TIMEOUT=45s ROOM_PORTS=27015-27022 ADMIN_PASSWORD_HASH=<bcrypt of "test-admin-pw"> and generous
// CREATE_LIMIT_*/LOGIN_LIMIT_* values, and sets STACK_URL. Without STACK_URL every test skips.
package acceptance

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"cs-duds/backend/internal/testpeer"
)

var modes = []string{"classic", "deathmatch", "gungame"}

func roomPorts() (lo, hi int) {
	spec := os.Getenv("STACK_ROOM_PORTS")
	if spec == "" {
		spec = "27015-27022"
	}
	a, b, _ := strings.Cut(spec, "-")
	lo, _ = strconv.Atoi(a)
	hi, _ = strconv.Atoi(b)
	return
}

func TestCatalogFromRealContent(t *testing.T) {
	base := stackURL(t)
	code, raw := call(t, "GET", base+"/api/catalog", nil, nil)
	var cat struct{ Maps, Modes []string }
	if code != 200 || json.Unmarshal(raw, &cat) != nil {
		t.Fatalf("catalog: %d %s", code, raw)
	}
	code, raw = call(t, "GET", base+"/manifest.json", nil, nil)
	var mf struct{ Maps map[string]json.RawMessage }
	if code != 200 || json.Unmarshal(raw, &mf) != nil {
		t.Fatalf("manifest: %d %s", code, raw)
	}
	var keys []string
	for k := range mf.Maps {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	sort.Strings(cat.Maps)
	if !reflect.DeepEqual(cat.Maps, keys) {
		t.Errorf("catalog maps %v != manifest maps %v (single source)", cat.Maps, keys)
	}
	found := false
	for _, m := range cat.Maps {
		found = found || m == "de_dust2"
	}
	if !found {
		t.Errorf("de_dust2 missing from %v", cat.Maps)
	}
	if !reflect.DeepEqual(cat.Modes, modes) {
		t.Errorf("modes %v, want %v", cat.Modes, modes)
	}
}

func TestRelayReachesRealRoom(t *testing.T) {
	base := stackURL(t)
	r := createRoom(t, base, "de_dust2", "classic")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	p, resp, err := testpeer.Dial(ctx, base, r.ID)
	if err != nil || resp.StatusCode != 201 {
		t.Fatalf("dial: %v %v", resp, err)
	}
	defer p.Close()
	info, err := p.A2SInfo(ctx)
	if err != nil || info["map"] != "de_dust2" || info["folder"] != "cstrike" {
		t.Fatalf("A2S_INFO through the relay: %v %v", info, err)
	}
}

func TestCannotReachNonRoomPorts(t *testing.T) {
	base := stackURL(t)
	lo, hi := roomPorts()
	var canaries []*net.UDPConn
	var counts []*atomic.Int64
	listen := func(port int) bool {
		c, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: port})
		if err != nil {
			return false
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
		canaries, counts = append(canaries, c), append(counts, n)
		return true
	}
	if !listen(27099) {
		t.Fatal("cannot bind 127.0.0.1:27099")
	}
	inRange := 0
	for p := hi; p >= lo && inRange == 0; p-- { // highest free port of ROOM_PORTS; rooms take the lowest
		if listen(p) {
			inRange = p
		}
	}
	if inRange == 0 {
		t.Fatal("no free port inside ROOM_PORTS to use as a canary")
	}
	room := createRoom(t, base, "de_dust2", "classic")
	gone := createRoom(t, base, "de_dust2", "classic")
	call(t, "DELETE", base+"/api/rooms/"+gone.ID, nil, adminCookie(t, base))
	for _, id := range []string{"27099", "127.0.0.1:27099", "..%2Fx", strconv.Itoa(inRange), gone.ID} {
		if code, raw := call(t, "POST", base+"/api/rooms/"+id+"/session", nil, map[string]string{"Content-Type": "application/sdp"}); code != 404 {
			t.Errorf("session for %q: %d %s, want 404", id, code, raw)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for i := 0; i < 2; i++ {
		p, _, err := testpeer.Dial(ctx, base, room.ID)
		if err != nil {
			t.Fatal(err)
		}
		defer p.Close()
		for n := 0; n < 2500; n++ {
			for _, tg := range []string{"127.0.0.1:27099", fmt.Sprintf("127.0.0.1:%d", inRange)} {
				p.Send([]byte("\xff\xff\xff\xffconnect " + tg))
			}
		}
	}
	time.Sleep(time.Second)
	for i := range canaries {
		if counts[i].Load() != 0 {
			t.Errorf("canary %v received %d datagrams, want 0", canaries[i].LocalAddr(), counts[i].Load())
		}
	}
}

func TestModesConfigureServer(t *testing.T) {
	base := stackURL(t)
	for _, m := range modes {
		// 201 means the real hlds answered RCON and the mode's `// probe` cvar has the mode's value
		r := createRoom(t, base, "de_dust2", m)
		// the stock vote only starts when admin.amxx/adminvote.amxx are loaded in this room
		code, raw := call(t, "POST", base+"/api/rooms/"+r.ID+"/vote-map", map[string][]string{"maps": {"de_inferno", "de_nuke"}},
			map[string]string{"Authorization": "Bearer " + r.HostToken})
		if code != 202 {
			t.Errorf("mode %s: vote-map %d %s, want 202 (vote settings active)", m, code, raw)
		}
	}
}

func TestAdminFlow(t *testing.T) {
	base := stackURL(t)
	if code, _ := call(t, "POST", base+"/api/admin/login", map[string]string{"password": "wrong"}, nil); code != 401 {
		t.Errorf("wrong password: %d, want 401", code)
	}
	r := createRoom(t, base, "de_dust2", "classic")
	if code, _ := call(t, "DELETE", base+"/api/rooms/"+r.ID, nil, nil); code != 403 {
		t.Errorf("DELETE without cookie: %d, want 403", code)
	}
	if code, raw := call(t, "DELETE", base+"/api/rooms/"+r.ID, nil, adminCookie(t, base)); code != 204 {
		t.Fatalf("admin DELETE: %d %s", code, raw)
	}
	_, raw := call(t, "GET", base+"/api/rooms", nil, nil)
	if strings.Contains(string(raw), r.ID) {
		t.Errorf("room still listed: %s", raw)
	}
}

func TestIdleReapAndPortReuse(t *testing.T) {
	base := stackURL(t)
	lo, hi := roomPorts()
	idle := 45 * time.Second
	if d, err := time.ParseDuration(os.Getenv("STACK_IDLE_TIMEOUT")); err == nil {
		idle = d
	}
	var rooms []stackRoom
	for i := lo; i <= hi; i++ {
		rooms = append(rooms, createRoom(t, base, "de_dust2", "classic"))
	}
	start := time.Now()
	if code, raw := call(t, "POST", base+"/api/rooms", map[string]string{"map": "de_dust2", "mode": "classic"}, nil); code != 503 {
		t.Fatalf("create beyond the port range: %d %s, want 503 rooms_full", code, raw)
	}
	call(t, "DELETE", base+"/api/rooms/"+rooms[0].ID, nil, adminCookie(t, base))
	createRoom(t, base, "de_dust2", "classic") // the freed port is reusable
	// nobody joined any room: all are reaped within idle timeout + two polls (+ slack)
	deadline := start.Add(idle + 2*5*time.Second + 30*time.Second)
	for time.Now().Before(deadline) {
		_, raw := call(t, "GET", base+"/api/rooms", nil, nil)
		var l []stackRoom
		json.Unmarshal(raw, &l)
		if len(l) == 0 {
			return
		}
		time.Sleep(2 * time.Second)
	}
	t.Fatal("idle rooms were not reaped")
}
