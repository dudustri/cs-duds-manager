package main

import (
	"bytes"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Sanity test for the test double itself: it must really work before the acceptance tests lean on it.
func TestFakeHLDSSpeaksRCON(t *testing.T) {
	tmp := t.TempDir()
	bin := filepath.Join(tmp, "fakehlds")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	hlds := filepath.Join(tmp, "hlds")
	os.MkdirAll(filepath.Join(hlds, "cstrike/modes/dm"), 0o755)
	os.WriteFile(filepath.Join(hlds, "cstrike/modes/dm/mode.cfg"), []byte("// c\nmp_forcerespawn 1 // probe\n"), 0o644)
	l, _ := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	port := l.LocalAddr().(*net.UDPAddr).Port
	l.Close()
	sig := filepath.Join(tmp, "sigterms")
	cmd := exec.Command(bin, "-game", "cstrike", "-port", fmt.Sprint(port), "+ip", "127.0.0.1", "+rcon_password", "pw",
		"+mapchangecfgfile", "modes/dm/mode.cfg", "+map", "de_dust2")
	cmd.Dir = hlds
	cmd.Env = append(os.Environ(), "FAKEHLDS_SIGTERMS="+sig)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	conn, _ := net.DialUDP("udp4", nil, &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: port})
	defer conn.Close()
	ask := func(msg string) string {
		t.Helper()
		conn.SetReadDeadline(time.Now().Add(time.Second))
		conn.Write(append([]byte{0xff, 0xff, 0xff, 0xff}, msg...))
		b := make([]byte, 2000)
		n, err := conn.Read(b)
		if err != nil {
			t.Fatalf("%q: %v", msg, err)
		}
		return string(bytes.TrimPrefix(b[:n], []byte{0xff, 0xff, 0xff, 0xff}))
	}
	var ch string
	for i := 0; i < 20 && !strings.HasPrefix(ch, "challenge rcon "); i++ {
		time.Sleep(50 * time.Millisecond)
		conn.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
		conn.Write([]byte("\xff\xff\xff\xffchallenge rcon\n"))
		b := make([]byte, 100)
		if n, err := conn.Read(b); err == nil {
			ch = string(b[4:n])
		}
	}
	if !strings.HasPrefix(ch, "challenge rcon ") {
		t.Fatalf("no challenge: %q", ch)
	}
	n := strings.Trim(strings.TrimPrefix(ch, "challenge rcon "), "\n\x00 ")
	rcon := func(pw, c string) string { return ask(fmt.Sprintf("rcon %s \"%s\" %s\n", n, pw, c)) }
	if got := rcon("bad", "status"); !strings.Contains(got, "Bad rcon_password.") {
		t.Errorf("bad pw: %q", got)
	}
	if got := rcon("pw", "status"); !strings.HasPrefix(got, "l") || !strings.Contains(got, "map     :  de_dust2") {
		t.Errorf("status: %q", got)
	}
	if got := rcon("pw", "mp_forcerespawn"); !strings.Contains(got, `"mp_forcerespawn" is "1"`) {
		t.Errorf("cvar: %q", got)
	}
	if got := ask("whoami"); !strings.HasPrefix(got, "seen 127.0.0.1:") {
		t.Errorf("whoami: %q", got)
	}
	if got := ask(`join a";quit;"`); !strings.HasPrefix(got, "joined ") {
		t.Fatalf("join: %q", got)
	}
	st := rcon("pw", "status")
	if !strings.Contains(st, `"a";quit;"" 3 STEAM_0:1:`) || !strings.Contains(st, "1 active") {
		t.Errorf("status with player: %q", st)
	}
	rcon("pw", "kick #3")
	if st := rcon("pw", "status"); !strings.Contains(st, "0 active") {
		t.Errorf("kick did not remove: %q", st)
	}
	if a, b := rcon("pw", "amx_votemap a b"), rcon("pw", "amx_votemap a b"); !strings.Contains(a, "Voting has started") || !strings.Contains(b, "There is already one voting") {
		t.Errorf("vote: %q / %q", a, b)
	}
	if got := ask("TSource Engine Query\x00"); !strings.HasPrefix(got, "I") || !strings.Contains(got, "de_dust2") {
		t.Errorf("a2s: %q", got)
	}
	cmd.Process.Kill()
	cmd.Wait()
	// second run: SIGTERM -> exit 0 + sigterm line
	cmd2 := exec.Command(bin, "-port", fmt.Sprint(port+1), "+map", "x")
	cmd2.Dir = hlds
	cmd2.Env = append(os.Environ(), "FAKEHLDS_SIGTERMS="+sig)
	cmd2.Start()
	time.Sleep(200 * time.Millisecond)
	cmd2.Process.Signal(syscall.SIGTERM)
	if err := cmd2.Wait(); err != nil {
		t.Errorf("SIGTERM exit: %v", err)
	}
	if b, _ := os.ReadFile(sig); string(b) != "SIGTERM\n" {
		t.Errorf("sigterm file %q", b)
	}
}
