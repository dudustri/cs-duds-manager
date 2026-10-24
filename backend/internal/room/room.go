// Package room owns hlds processes: the catalog, RCON, and the Manager that is the single
// owner of rooms and ports.
package room

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

const (
	DefaultMaxPlayers = 16
	stopGrace         = 3 * time.Second
)

var (
	ErrFull        = errors.New("room: no free port")
	ErrSpawn       = errors.New("room: server did not start")
	ErrStartFailed = errors.New("room: server not ready or wrong mode")
	ErrNotFound    = errors.New("room: not found")
	ErrVoteRunning = errors.New("room: a vote is already running")
	ErrVoteTooSoon = errors.New("room: too soon after the last vote")
	ErrNoPlayer    = errors.New("room: no such player")
)

type Config struct {
	HLDSDir      string        // cwd for the hlds binary
	HLDSBin      string        // ./hlds_linux in prod; the fakehlds binary in tests
	Ports        []int         // one game port per room; len(Ports) is the room cap
	IdleTimeout  time.Duration // ROOM_IDLE_TIMEOUT (default 1h)
	StatusEvery  time.Duration // default 5s
	ReadyTimeout time.Duration // default 30s; create blocks at most this long, probe included
	Log          *slog.Logger
}

// Manager is the single owner of the rooms map. The map holds a room from port reservation
// until its process has exited, so the ports in use are derived from it.
type Manager struct {
	cfg    Config
	mu     sync.Mutex
	rooms  map[string]*Room
	closed bool
	wg     sync.WaitGroup // one supervisor per room
}

// Room's identity fields never change. Only its supervisor goroutine writes the snapshot,
// and only the Manager (under its mutex) writes listed.
type Room struct {
	ID         string
	Mode       Mode
	CreatedAt  time.Time
	MaxPlayers int

	mp        Map
	set       Settings
	host      string
	maps      []string
	hostToken string
	port      int
	addr      netip.AddrPort
	rcon      rcon
	ctx       context.Context
	stop      context.CancelFunc // idempotent
	started   chan error         // supervisor -> Create: nil once ready and listed
	done      chan struct{}      // closed after the process exited and the port was released
	snap      atomic.Pointer[Status]
	listed    bool
	log       *slog.Logger
	cfgDir    string // this room's AMXX configs dir (see admin.go)
	sess      sessions
}

// Info is the Room JSON of the public API.
type Info struct {
	ID         string    `json:"id"`
	Map        string    `json:"map"`
	Mode       string    `json:"mode"`
	Players    int       `json:"players"`
	MaxPlayers int       `json:"maxPlayers"`
	CreatedAt  time.Time `json:"createdAt"`
	Host       string    `json:"host,omitempty"`     // the creator's player name, for display only
	Settings   []Setting `json:"settings,omitempty"` // the mode options it was created with
}

func NewManager(cfg Config) *Manager {
	if cfg.IdleTimeout <= 0 {
		cfg.IdleTimeout = time.Hour
	}
	if cfg.StatusEvery <= 0 {
		cfg.StatusEvery = 5 * time.Second
	}
	if cfg.ReadyTimeout <= 0 {
		cfg.ReadyTimeout = 30 * time.Second
	}
	if cfg.Log == nil {
		cfg.Log = slog.New(slog.DiscardHandler)
	}
	return &Manager{cfg: cfg, rooms: map[string]*Room{}}
}

// reserve picks the lowest port that no room holds and that nothing else has bound.
func (m *Manager) reserve(mp Map, mode Mode, maxPlayers int) (*Room, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil, ErrFull
	}
	for _, p := range m.cfg.Ports {
		if slices.ContainsFunc(sliceOf(m.rooms), func(r *Room) bool { return r.port == p }) || !bindable(p) {
			continue
		}
		addr := netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), uint16(p))
		r := &Room{
			ID: strings.ToLower(rand.Text()[:8]), Mode: mode, CreatedAt: time.Now().UTC(), MaxPlayers: maxPlayers, log: m.cfg.Log,
			mp: mp, hostToken: base64.RawURLEncoding.EncodeToString(randBytes(32)), port: p, addr: addr,
			rcon: rcon{addr, hex.EncodeToString(randBytes(16))}, started: make(chan error, 1), done: make(chan struct{}),
		}
		r.cfgDir = filepath.Join(m.cfg.HLDSDir, "cstrike", roomConfigsRel(r.ID))
		r.ctx, r.stop = context.WithCancel(context.Background())
		m.rooms[r.ID] = r
		m.wg.Add(1)
		return r, nil
	}
	return nil, ErrFull
}

func sliceOf(m map[string]*Room) []*Room {
	out := make([]*Room, 0, len(m))
	for _, r := range m {
		out = append(out, r)
	}
	return out
}

func bindable(port int) bool {
	c, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: port})
	if err != nil {
		return false
	}
	c.Close()
	return true
}

func randBytes(n int) []byte {
	b := make([]byte, n)
	rand.Read(b)
	return b
}

// Create starts hlds and returns once RCON answers and the mode's probe cvar matches. On any
// failure (including ctx cancellation) the process is killed and the port freed before it returns.
// maps are the catalog maps of mode (Catalog.MapsFor): the room's own map cycle and vote list.
func (m *Manager) Create(ctx context.Context, mp Map, mode Mode, maxPlayers int, set Settings, host string, maps []string) (*Room, error) {
	r, err := m.reserve(mp, mode, maxPlayers)
	if err != nil {
		return nil, err
	}
	r.set, r.host, r.maps = set, host, maps // before the supervisor starts: it reads them for argv and configs
	go m.supervise(r)
	select {
	case err = <-r.started:
	case <-ctx.Done():
		err = ctx.Err()
	}
	if err != nil {
		r.stop()
		<-r.done
		return nil, err
	}
	return r, nil
}

// supervise is the room's only writer after creation and the single path that tears it down.
func (m *Manager) supervise(r *Room) {
	defer m.wg.Done()
	cmd := exec.Command(m.cfg.HLDSBin, launchArgs(r.port, r.Mode, r.mp, r.set, r.rcon.password, r.MaxPlayers, r.ID, len(r.maps) > 0)...)
	cmd.Dir = m.cfg.HLDSDir
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} // no Pdeathsig: init:true reaps on backend exit
	cmd.Stdout = &logWriter{log: m.cfg.Log, id: r.ID, secret: r.rcon.password}
	cmd.Stderr = cmd.Stdout
	cmd.WaitDelay = time.Second
	var exited chan struct{}
	if err := m.mkConfigs(r); err != nil {
		m.cfg.Log.Error("room configs", "room", r.ID, "err", err)
		r.started <- ErrSpawn
	} else if err := cmd.Start(); err != nil {
		m.cfg.Log.Error("hlds start", "room", r.ID, "err", err)
		r.started <- ErrSpawn
	} else {
		exited = make(chan struct{})
		go func() { cmd.Wait(); close(exited) }()
		m.live(r, exited)
	}
	if exited != nil && !closed(exited) {
		m.setListed(r, false)
		syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
		select {
		case <-exited:
		case <-time.After(stopGrace):
			syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			<-exited
		}
	}
	os.RemoveAll(r.cfgDir) // the room's AMXX configs, admins included
	os.Remove(filepath.Join(m.cfg.HLDSDir, "cstrike", mapcycleRel(r.ID)))
	m.mu.Lock()
	delete(m.rooms, r.ID) // unlists and frees the port in one step
	m.mu.Unlock()
	close(r.done)
}

func closed(c <-chan struct{}) bool {
	select {
	case <-c:
		return true
	default:
		return false
	}
}

func (m *Manager) setListed(r *Room, v bool) {
	m.mu.Lock()
	r.listed = v
	m.mu.Unlock()
}

// live waits for readiness, lists the room, then polls status until the room should end.
func (m *Manager) live(r *Room, exited <-chan struct{}) {
	st, err := m.waitReady(r, exited)
	if err != nil {
		r.started <- err
		return
	}
	r.snap.Store(&st)
	m.setListed(r, true)
	r.started <- nil
	idleSince, fails := time.Now(), 0
	tick := time.NewTicker(m.cfg.StatusEvery)
	defer tick.Stop()
	for {
		select {
		case <-r.ctx.Done():
			return
		case <-exited:
			return
		case now := <-tick.C:
			st, err := r.status(r.ctx)
			if err != nil {
				// wedged server; but zBot freezes it while it builds a map's first navigation mesh, so allow 2 minutes
				if fails++; time.Duration(fails)*m.cfg.StatusEvery >= wedgedAfter && fails >= 3 {
					return
				}
				continue
			}
			fails = 0
			r.snap.Store(&st)
			if st.Humans() > 0 {
				idleSince = now
			} else if now.Sub(idleSince) >= m.cfg.IdleTimeout {
				return
			}
		}
	}
}

func (m *Manager) waitReady(r *Room, exited <-chan struct{}) (Status, error) {
	ctx, cancel := context.WithTimeout(r.ctx, m.cfg.ReadyTimeout)
	defer cancel()
	for {
		actx, acancel := context.WithTimeout(ctx, 500*time.Millisecond)
		st, err := r.status(actx)
		if err == nil {
			err = r.probe(actx)
		}
		acancel()
		if err == nil {
			return st, nil
		}
		select {
		case <-exited:
			return Status{}, ErrSpawn
		case <-ctx.Done():
			return Status{}, ErrStartFailed
		case <-time.After(100 * time.Millisecond):
		}
	}
}

var probeRe = regexp.MustCompile(`^"([^"]*)" is "([^"]*)"`)

// probe checks that the mode cfg was applied: the probe cvar must hold the value its mode.cfg sets.
func (r *Room) probe(ctx context.Context) error {
	out, err := r.rcon.exec(ctx, cvarCmd(r.Mode))
	if err != nil {
		return err
	}
	if m := probeRe.FindStringSubmatch(strings.TrimSpace(out)); m == nil || m[1] != r.Mode.probeCvar || m[2] != r.Mode.probeVal {
		return fmt.Errorf("probe %s: got %q, want %q", r.Mode.probeCvar, out, r.Mode.probeVal)
	}
	return nil
}

func (r *Room) status(ctx context.Context) (Status, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	out, err := r.rcon.exec(ctx, statusCmd())
	if err != nil {
		return Status{}, err
	}
	return ParseStatus(out)
}

// Get returns a listed (ready, not stopping) room.
func (m *Manager) Get(id string) (*Room, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r := m.rooms[id]
	return r, r != nil && r.listed
}

func (m *Manager) List() []Info {
	m.mu.Lock()
	defer m.mu.Unlock()
	rs := []*Room{}
	for _, r := range m.rooms {
		if r.listed {
			rs = append(rs, r)
		}
	}
	slices.SortFunc(rs, func(a, b *Room) int { return a.CreatedAt.Compare(b.CreatedAt) })
	out := make([]Info, len(rs))
	for i, r := range rs {
		out[i] = r.Info()
	}
	return out
}

// Stop is idempotent and safe to race with the reaper or a crash: it cancels, then waits for done.
func (m *Manager) Stop(id string) error {
	r, ok := m.Get(id)
	if !ok {
		return ErrNotFound
	}
	r.stop()
	<-r.done
	return nil
}

// Close stops every room and waits for all supervisors.
func (m *Manager) Close() {
	m.mu.Lock()
	m.closed = true
	rs := sliceOf(m.rooms)
	m.mu.Unlock()
	for _, r := range rs {
		r.stop()
	}
	m.wg.Wait()
}

func (r *Room) Info() Info {
	i := Info{ID: r.ID, Mode: r.Mode.name, MaxPlayers: r.MaxPlayers, CreatedAt: r.CreatedAt, Host: r.host, Settings: r.set.labels}
	if st := r.snap.Load(); st != nil {
		i.Map, i.Players = st.Map, st.Humans()
	}
	return i
}

func (r *Room) Addr() netip.AddrPort  { return r.addr }
func (r *Room) Done() <-chan struct{} { return r.done }
func (r *Room) HostToken() string     { return r.hostToken }

func (r *Room) IsHost(token string) bool {
	return subtle.ConstantTimeCompare([]byte(token), []byte(r.hostToken)) == 1
}

// Online is the human players as of the supervisor's last status poll (no RCON round trip).
func (r *Room) Online() []Player {
	st := r.snap.Load()
	if st == nil {
		return nil
	}
	var out []Player
	for _, p := range st.Players {
		if !p.Bot {
			out = append(out, p)
		}
	}
	return out
}

func (r *Room) Players(ctx context.Context) ([]Player, error) {
	st, err := r.status(ctx)
	if err != nil {
		return nil, err
	}
	if st.Players == nil {
		return []Player{}, nil
	}
	return st.Players, nil
}

// Kick an absent userid is success, like the server's own `kick`. A bot goes by bot_kick: a plain kick leaves the
// bot target as it was, and the server adds the bot straight back.
func (r *Room) Kick(ctx context.Context, userid int) error {
	if ps, err := r.Players(ctx); err == nil {
		for _, p := range ps {
			if p.UserID == userid && p.Bot && !strings.ContainsAny(p.Name, `";`) {
				_, err := r.rcon.exec(ctx, botKickNameCmd(p.Name))
				return err
			}
		}
	}
	_, err := r.rcon.exec(ctx, kickCmd(userid))
	return err
}

// AddBots raises zBot's bot target by count (difficulty 0-3 and count checked at the API) for team "ct", "t", or
// "both" (the bots balance the teams). The server adds them itself, and it ignores bot_add while it builds a map's
// navigation (seconds, on a map's first bots), which a quota waits out. A full server just adds no more.
func (r *Room) AddBots(ctx context.Context, team string, difficulty, count int) error {
	join := map[string]string{"ct": "ct", "t": "t", "both": "any"}[team]
	if join == "" {
		return fmt.Errorf("bots: team %q", team)
	}
	out, err := r.rcon.exec(ctx, botQuotaReadCmd())
	if err != nil {
		return err
	}
	quota := 0
	if m := quotaRe.FindStringSubmatch(out); m != nil {
		quota, _ = strconv.Atoi(m[1])
	}
	for _, c := range []command{botDifficultyCmd(difficulty), botJoinTeamCmd(join), botQuotaCmd(quota + count)} {
		if _, err := r.rcon.exec(ctx, c); err != nil {
			return err
		}
	}
	return nil
}

var quotaRe = regexp.MustCompile(`"bot_quota" is "(\d+)`)

func (r *Room) KickBots(ctx context.Context) error {
	_, err := r.rcon.exec(ctx, botKickCmd())
	return err
}

// VoteKick starts the stock kick vote for a player that a fresh status lists.
func (r *Room) VoteKick(ctx context.Context, userid int) error {
	ps, err := r.Players(ctx)
	if err != nil {
		return err
	}
	if !slices.ContainsFunc(ps, func(p Player) bool { return p.UserID == userid }) {
		return ErrNoPlayer
	}
	out, err := r.rcon.exec(ctx, voteKickCmd(userid))
	return voteReply(out, err)
}

// VoteMap starts the stock map vote.
func (r *Room) VoteMap(ctx context.Context, maps []Map) error {
	out, err := r.rcon.exec(ctx, voteMapCmd(maps))
	return voteReply(out, err)
}

// voteReply classifies the stock adminvote (en) strings.
func voteReply(out string, err error) error {
	switch {
	case err != nil:
		return err
	case strings.Contains(out, "Voting has started"):
		return nil
	case strings.Contains(out, "There is already one voting"):
		return ErrVoteRunning
	case strings.Contains(out, "Voting not allowed at this time"):
		return ErrVoteTooSoon
	}
	return fmt.Errorf("vote: unexpected reply %q", out)
}

// wedgedAfter is how long a room may not answer status before it counts as hung and is stopped.
const wedgedAfter = 2 * time.Minute

// launchArgs is pure; its inputs are typed. +sv_rcon_condebug 0 must precede the map so ReHLDS
// never echoes the RCON password to its console.
func launchArgs(port int, mode Mode, mp Map, set Settings, rconPassword string, maxPlayers int, id string, withMaps bool) []string {
	args := []string{
		"-game", "cstrike", "-port", strconv.Itoa(port), "-sport", strconv.Itoa(port + 10000), "-maxplayers", strconv.Itoa(maxPlayers),
		"-heapsize", "131072", // KB: big community maps overflow the default memory pool ("greater then free hunk")
		"+ip", "127.0.0.1", "+rcon_password", rconPassword, "+sv_rcon_condebug", "0",
		"+localinfo", "amxx_plugins", pluginList(mode.name),
		"+localinfo", "amxx_configsdir", roomConfigsRel(id),
	}
	if set.s != "" { // our core plugin applies them on every map, after the cfg files (cs_duds_votes.sma)
		args = append(args, "+localinfo", "cs_duds_opts", set.s)
	}
	if withMaps {
		args = append(args, "+mapcyclefile", mapcycleRel(id)) // written by mkConfigs
	}
	// +map last: hlds ignores what follows it
	return append(args, "+mapchangecfgfile", "modes/"+mode.name+"/mode.cfg", "+map", mp.name)
}

// logWriter forwards hlds output to the logger line by line, hiding the RCON password.
const maxLogLine = 4096

type logWriter struct {
	log    *slog.Logger
	id     string
	secret string
	buf    []byte
}

func (w *logWriter) Write(p []byte) (int, error) {
	w.buf = append(w.buf, p...)
	for {
		i := bytes.IndexByte(w.buf, '\n')
		if i < 0 && len(w.buf) > maxLogLine { // output with no newline: log it in pieces rather than grow forever
			i = len(w.buf)
			w.buf = append(w.buf, '\n')
		}
		if i < 0 {
			return len(p), nil
		}
		w.log.Info(strings.ReplaceAll(string(w.buf[:i]), w.secret, "***"), "room", w.id)
		w.buf = w.buf[i+1:]
	}
}
