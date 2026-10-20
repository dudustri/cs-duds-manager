// fakehlds is a TEST DOUBLE for hlds_linux (test infrastructure, not product code).
//
// It parses the argv a real hlds gets (-port P, +ip, +rcon_password, +mapchangecfgfile F, +map M,
// +localinfo k v ...), binds 127.0.0.1:P with cwd = HLDSDir and speaks the GoldSrc wire format:
//
//	RCON   "challenge rcon" -> "challenge rcon N"; `rcon N "pw" cmd` -> \xff\xff\xff\xffl<text>\x00
//	       (always at least one reply packet; bad password -> "Bad rcon_password.")
//	cvars  `<cvar>` -> `"cvar" is "value"` (values come from the mode cfg named by +mapchangecfgfile,
//	       resolved as cstrike/<file>; `name value // comment` lines). Unset cvar -> `Unknown command: x`.
//	       FAKEHLDS_BREAK_PROBE=1 ignores the cfg, so every mode probe fails.
//	status real ReHLDS layout; `kick #N`; `amx_votemap a b ..` ("Voting has started..." once, then
//	       "There is already one voting..."); `amx_votekick ..` shares that one-vote-at-a-time state; every authenticated RCON line is appended to $FAKEHLDS_LOG.
//	OOB    whoami -> "seen <src ip:port>"; echo... -> the same datagram back; join <name> -> adds a human
//	       whose adr is the sender (reply "joined <userid>"); leave -> removes it;
//	       "TSource Engine Query\0" -> A2S_INFO ('I').
//
// Other env: FAKEHLDS_ARGV (append one argv line per start), FAKEHLDS_SIGTERMS (append a line per
// SIGTERM, then exit 0), FAKEHLDS_READY_DELAY (default 100ms; the socket is bound at once, but
// unanswered until ready). Map `crash` exits 1, 1.5 s after becoming ready.
package main

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

var oob = []byte{0xff, 0xff, 0xff, 0xff}

type player struct {
	slot, userid int
	name, uid    string
	adr          *net.UDPAddr
	joined       time.Time
}

type server struct {
	mu        sync.Mutex
	port      int
	mapName   string
	maxPlr    int
	password  string
	challenge uint32
	cvars     map[string]string
	players   []*player
	nextUser  int
	voting    bool
	logFile   string
}

func appendLine(path, line string) {
	if path == "" {
		return
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.WriteString(line + "\n")
}

func main() {
	args := os.Args[1:]
	appendLine(os.Getenv("FAKEHLDS_ARGV"), strings.Join(args, " "))
	s := &server{port: 27015, maxPlr: 6, challenge: 1234567891, cvars: map[string]string{}, nextUser: 3, logFile: os.Getenv("FAKEHLDS_LOG")}
	ip, game, cfgFile := "127.0.0.1", "valve", ""
	for i := 0; i < len(args); i++ {
		a := args[i]
		if !strings.HasPrefix(a, "-") && !strings.HasPrefix(a, "+") {
			continue
		}
		n := 1
		if a == "+localinfo" {
			n = 2
		}
		var v []string
		for j := 1; j <= n && i+j < len(args) && !strings.HasPrefix(args[i+j], "+") && !strings.HasPrefix(args[i+j], "-"); j++ {
			v = append(v, args[i+j])
		}
		if len(v) == 0 {
			continue
		}
		switch a {
		case "-port":
			s.port, _ = strconv.Atoi(v[0])
		case "-maxplayers":
			s.maxPlr, _ = strconv.Atoi(v[0])
		case "-game":
			game = v[0]
		case "+ip":
			ip = v[0]
		case "+rcon_password":
			s.password = v[0]
		case "+mapchangecfgfile":
			cfgFile = v[0]
		case "+map":
			s.mapName = v[0]
		}
		i += len(v)
	}
	if cfgFile != "" && os.Getenv("FAKEHLDS_BREAK_PROBE") != "1" {
		s.loadCfg(filepath.Join(game, cfgFile))
	}
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.ParseIP(ip), Port: s.port})
	if err != nil {
		fmt.Fprintln(os.Stderr, "fakehlds: bind:", err)
		os.Exit(1)
	}
	term := make(chan os.Signal, 1)
	signal.Notify(term, syscall.SIGTERM)
	go func() {
		<-term
		appendLine(os.Getenv("FAKEHLDS_SIGTERMS"), "SIGTERM")
		os.Exit(0)
	}()
	delay := 100 * time.Millisecond
	if d, err := time.ParseDuration(os.Getenv("FAKEHLDS_READY_DELAY")); err == nil {
		delay = d
	}
	readyAt := time.Now().Add(delay)
	if s.mapName == "crash" {
		time.AfterFunc(delay+1500*time.Millisecond, func() { os.Exit(1) })
	}
	buf := make([]byte, 65535)
	for {
		n, from, err := conn.ReadFromUDP(buf)
		if err != nil {
			os.Exit(1)
		}
		if time.Now().Before(readyAt) {
			continue
		}
		for _, r := range s.handle(append([]byte(nil), buf[:n]...), from) {
			_, _ = conn.WriteToUDP(r, from)
		}
	}
}

func (s *server) loadCfg(rel string) {
	b, err := os.ReadFile(rel)
	if err != nil {
		fmt.Fprintln(os.Stderr, "fakehlds: cfg:", err)
		return
	}
	for _, line := range strings.Split(string(b), "\n") {
		if i := strings.Index(line, "//"); i >= 0 {
			line = line[:i]
		}
		f := strings.Fields(line)
		if len(f) >= 2 {
			s.cvars[f[0]] = strings.Trim(strings.Join(f[1:], " "), `"`)
		}
	}
}

func oobReply(text string) []byte { return append(append([]byte(nil), oob...), text...) }

var rconRe = regexp.MustCompile(`^rcon (\d+) "([^"]*)" ?(.*)$`)

func (s *server) handle(pkt []byte, from *net.UDPAddr) [][]byte {
	if !bytes.HasPrefix(pkt, oob) {
		return nil
	}
	body := string(pkt[4:])
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case strings.HasPrefix(body, "challenge rcon"):
		return [][]byte{oobReply(fmt.Sprintf("challenge rcon %d\n\x00", s.challenge))}
	case strings.HasPrefix(body, "rcon "):
		m := rconRe.FindStringSubmatch(strings.TrimRight(body, "\n\x00"))
		switch {
		case m == nil || m[1] != strconv.Itoa(int(s.challenge)):
			return [][]byte{oobReply("lBad challenge.\n\x00")}
		case m[2] != s.password:
			return [][]byte{oobReply("lBad rcon_password.\n\x00")}
		}
		appendLine(s.logFile, m[3])
		return [][]byte{oobReply("l" + s.exec(m[3]) + "\x00")}
	case strings.HasPrefix(body, "whoami"):
		return [][]byte{oobReply("seen " + from.String())}
	case strings.HasPrefix(body, "echo"):
		return [][]byte{pkt}
	case strings.HasPrefix(body, "join "):
		p := s.join(strings.TrimRight(body[5:], "\n\x00"), from)
		return [][]byte{oobReply(fmt.Sprintf("joined %d", p.userid))}
	case strings.HasPrefix(body, "leave"):
		s.remove(func(p *player) bool { return p.adr.String() == from.String() })
		return [][]byte{oobReply("left")}
	case strings.HasPrefix(body, "TSource Engine Query\x00"):
		return [][]byte{s.a2sInfo()}
	}
	return nil
}

func (s *server) join(name string, from *net.UDPAddr) *player {
	for _, p := range s.players {
		if p.adr.String() == from.String() {
			p.name = name
			return p
		}
	}
	used := map[int]bool{}
	for _, p := range s.players {
		used[p.slot] = true
	}
	slot := 1
	for used[slot] {
		slot++
	}
	ip4 := from.IP.To4()
	p := &player{slot: slot, userid: s.nextUser, name: name, adr: from, joined: time.Now(),
		uid: fmt.Sprintf("STEAM_0:1:%d", int(ip4[2])*256+int(ip4[3])+1)}
	s.nextUser++
	s.players = append(s.players, p)
	sort.Slice(s.players, func(i, j int) bool { return s.players[i].slot < s.players[j].slot })
	return p
}

func (s *server) remove(match func(*player) bool) {
	kept := s.players[:0]
	for _, p := range s.players {
		if !match(p) {
			kept = append(kept, p)
		}
	}
	s.players = kept
}

func (s *server) exec(line string) string {
	f := strings.Fields(line)
	if len(f) == 0 {
		return ""
	}
	switch f[0] {
	case "status":
		return s.status()
	case "kick":
		if len(f) >= 2 && strings.HasPrefix(f[1], "#") {
			id, _ := strconv.Atoi(f[1][1:])
			s.remove(func(p *player) bool { return p.userid == id })
		}
		return ""
	case "amx_votemap", "amx_votekick": // AMXX runs one vote at a time, whatever its kind
		if s.voting {
			return "There is already one voting...\n"
		}
		s.voting = true
		return "Voting has started...\n"
	}
	if v, ok := s.cvars[f[0]]; ok {
		if len(f) > 1 {
			s.cvars[f[0]] = strings.Trim(strings.Join(f[1:], " "), `"`)
			return ""
		}
		return fmt.Sprintf("\"%s\" is \"%s\"\n", f[0], v)
	}
	return "Unknown command: " + f[0] + "\n"
}

func (s *server) status() string {
	var b strings.Builder
	fmt.Fprintf(&b, "hostname:  fakehlds\nversion :  48/1.1.2.7/Stdio 8684 secure  (10)\ntcp/ip  :  127.0.0.1:%d\n", s.port)
	fmt.Fprintf(&b, "map     :  %s at: 0 x, 0 y, 0 z\nplayers :  %d active (%d max)\n\n", s.mapName, len(s.players), s.maxPlr)
	b.WriteString("#      name userid uniqueid frag time ping loss adr\n")
	for _, p := range s.players {
		d := int(time.Since(p.joined).Seconds())
		fmt.Fprintf(&b, "#%2d \"%s\" %d %s %d %02d:%02d %d %d %s\n", p.slot, p.name, p.userid, p.uid, 0, d/60, d%60, 12, 0, p.adr)
	}
	return b.String()
}

func (s *server) a2sInfo() []byte {
	b := append([]byte(nil), oob...)
	b = append(b, 'I', 48)
	for _, f := range []string{"fakehlds", s.mapName, "cstrike", "Counter-Strike"} {
		b = append(b, f...)
		b = append(b, 0)
	}
	b = binary.LittleEndian.AppendUint16(b, 10)
	return append(b, byte(len(s.players)), byte(s.maxPlr), 0, 'd', 'l', 0, 0)
}
