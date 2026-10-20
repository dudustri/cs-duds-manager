//go:build stack || soak

package acceptance

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

const adminPassword = "test-admin-pw" // the stack is started with ADMIN_PASSWORD_HASH of this

// stackURL is the running compose stack, e.g. http://localhost. Unset -> skip.
func stackURL(t testing.TB) string {
	t.Helper()
	u := os.Getenv("STACK_URL")
	if u == "" {
		t.Skip("STACK_URL not set; run scripts/test stack")
	}
	return strings.TrimRight(u, "/")
}

type stackRoom struct {
	ID        string `json:"id"`
	Map       string `json:"map"`
	Mode      string `json:"mode"`
	Players   int    `json:"players"`
	HostToken string `json:"hostToken"`
}

var httpc = &http.Client{Timeout: 60 * time.Second}

func call(t testing.TB, method, url string, body any, hdr map[string]string) (int, []byte) {
	t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, url, rd)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := httpc.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, raw
}

func adminCookie(t testing.TB, base string) map[string]string {
	t.Helper()
	req, _ := http.NewRequest("POST", base+"/api/admin/login", strings.NewReader(`{"password":"`+adminPassword+`"}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err := httpc.Do(req)
	if err != nil || resp.StatusCode != 204 {
		t.Fatalf("admin login: %v %v", resp, err)
	}
	for _, c := range resp.Cookies() {
		if c.Name == "cs_admin" {
			return map[string]string{"Cookie": "cs_admin=" + c.Value}
		}
	}
	t.Fatal("no cs_admin cookie")
	return nil
}

// createRoom makes a real room (blocks until the mode probe passed) and deletes it when the test ends.
func createRoom(t testing.TB, base, mp, mode string) stackRoom {
	t.Helper()
	code, raw := call(t, "POST", base+"/api/rooms", map[string]string{"map": mp, "mode": mode}, nil)
	if code != 201 {
		t.Fatalf("create %s/%s: %d %s", mp, mode, code, raw)
	}
	var r stackRoom
	if err := json.Unmarshal(raw, &r); err != nil || r.ID == "" {
		t.Fatalf("create body %s", raw)
	}
	t.Cleanup(func() { call(t, "DELETE", base+"/api/rooms/"+r.ID, nil, adminCookie(t, base)) })
	return r
}
