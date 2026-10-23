// Package api serves the HTTP API: catalog, rooms, host actions, admin login and the WebRTC
// session endpoint. It wires catalog -> manager -> relay -> routes.
package api

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"
	"golang.org/x/time/rate"

	"cs-duds/backend/internal/relay"
	"cs-duds/backend/internal/room"
)

const (
	cookieName = "cs_admin"
	cookieTTL  = 12 * time.Hour
	maxOffer   = 64 << 10
)

// Config wires catalog -> manager -> relay -> routes.
type Config struct {
	Rooms             room.Config
	ManifestPath      string // manifest.json: the map catalog
	ModesDir          string // modes/<mode>/mode.cfg: the mode catalog + probe lines
	Relay             relay.Config
	AdminPasswordHash []byte // bcrypt; empty -> admin disabled (404 admin_disabled)

	CreateLimitBurst int
	CreateLimitEvery time.Duration
	LoginLimitBurst  int
	LoginLimitEvery  time.Duration
	RoomsPerIP       int // live rooms one address may hold at once (admins exempt); 0 = no limit
}

type Server struct {
	cat      *room.Catalog
	rooms    *room.Manager
	relay    *relay.Relay
	createLm *ipLimiter
	loginLm  *ipLimiter
	cmdLm    *ipLimiter // votes, kicks, bots: each one costs the game server RCON round trips
	nameLm   *ipLimiter // name claims: one address can't squat on every name
	names    names
	adminKey [32]byte // random per process: admin cookies die with the process, like rooms
	adminPw  []byte
	mux      *http.ServeMux

	roomsPerIP int
	creatorsMu sync.Mutex
	creators   map[string]netip.Addr // room id -> the address that created it
}

func New(cfg Config) (*Server, error) {
	cat, err := room.LoadCatalog(cfg.ManifestPath, cfg.ModesDir)
	if err != nil {
		return nil, err
	}
	rl, err := relay.New(cfg.Relay)
	if err != nil {
		return nil, err
	}
	s := &Server{
		cat: cat, rooms: room.NewManager(cfg.Rooms), relay: rl, adminPw: cfg.AdminPasswordHash, mux: http.NewServeMux(),
		createLm: newLimiter(cfg.CreateLimitBurst, cfg.CreateLimitEvery),
		loginLm:  newLimiter(cfg.LoginLimitBurst, cfg.LoginLimitEvery),
		cmdLm:    newLimiter(20, time.Second),
		nameLm:   newLimiter(10, 3*time.Second),
		names:    names{m: map[string]nameClaim{}},

		roomsPerIP: cfg.RoomsPerIP,
		creators:   map[string]netip.Addr{},
	}
	rand.Read(s.adminKey[:])
	s.mux.HandleFunc("GET /api/catalog", s.catalog)
	s.mux.HandleFunc("GET /api/rooms", s.listRooms)
	s.mux.HandleFunc("GET /api/players", s.onlinePlayers)
	s.mux.HandleFunc("GET /api/names/{name}", s.nameAvailable)
	s.mux.HandleFunc("POST /api/names", s.claimName)
	s.mux.HandleFunc("POST /api/rooms", s.createRoom)
	s.mux.HandleFunc("DELETE /api/rooms/{id}", s.deleteRoom)
	s.mux.HandleFunc("GET /api/rooms/{id}/players", s.players)
	s.mux.HandleFunc("POST /api/rooms/{id}/kick", s.kick)
	s.mux.HandleFunc("POST /api/rooms/{id}/vote-map", s.voteMap)
	s.mux.HandleFunc("POST /api/rooms/{id}/votes", s.votes)
	s.mux.HandleFunc("POST /api/rooms/{id}/bots", s.addBot)
	s.mux.HandleFunc("POST /api/rooms/{id}/stop", s.stopRoom)
	s.mux.HandleFunc("DELETE /api/rooms/{id}/bots", s.kickBots)
	s.mux.HandleFunc("POST /api/rooms/{id}/session", s.session)
	s.mux.HandleFunc("POST /api/admin/login", s.login)
	s.mux.HandleFunc("GET /api/admin", s.adminCheck)
	return s, nil
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.mux.ServeHTTP(w, r) }

// Close stops every room (which closes their sessions), then the relay.
func (s *Server) Close() {
	s.rooms.Close()
	s.relay.Close()
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]string{"error": msg, "code": code})
}

// decode reads a small JSON body; the caller answers 400 bad_request on error.
func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(v); err != nil {
		writeErr(w, 400, "bad_request", "invalid JSON body")
		return false
	}
	return true
}

func (s *Server) catalog(w http.ResponseWriter, r *http.Request) {
	opts := map[string][]room.Option{}
	modeMaps := map[string][]string{}
	for _, name := range s.cat.Modes() {
		m, _ := s.cat.Mode(name)
		opts[name] = m.Options()
		modeMaps[name] = s.cat.MapsFor(m)
	}
	writeJSON(w, 200, map[string]any{"maps": s.cat.Maps(), "modes": s.cat.Modes(), "options": opts, "modeMaps": modeMaps})
}

// onlinePlayers lists every human in every room, from the last status poll: names and frags only (no ids, addresses).
func (s *Server) onlinePlayers(w http.ResponseWriter, r *http.Request) {
	type online struct {
		Name  string `json:"name"`
		Frags int    `json:"frags"`
		Room  string `json:"room"`
		Map   string `json:"map"`
		Mode  string `json:"mode"`
	}
	out := []online{}
	for _, info := range s.rooms.List() {
		rm, ok := s.rooms.Get(info.ID)
		if !ok {
			continue
		}
		for _, p := range rm.Online() {
			out = append(out, online{p.Name, p.Frags, info.ID, info.Map, info.Mode})
		}
	}
	writeJSON(w, 200, out)
}

func (s *Server) listRooms(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, s.rooms.List())
}

func (s *Server) createRoom(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Map, Mode  string
		MaxPlayers *int              // optional; a non-integer fails the decode
		Options    map[string]string // optional; cvar -> one of the mode's values (GET /api/catalog lists them)
		Host       string            // optional; the creator's player name, shown in the room list
	}
	if !decode(w, r, &req) {
		return
	}
	maxPlayers := room.DefaultMaxPlayers
	if req.MaxPlayers != nil {
		if maxPlayers = *req.MaxPlayers; maxPlayers < 2 || maxPlayers > 32 {
			writeErr(w, 400, "bad_request", "maxPlayers must be an integer from 2 to 32")
			return
		}
	}
	mp, ok := s.cat.Map(req.Map)
	if !ok {
		writeErr(w, 400, "unknown_map", "unknown map")
		return
	}
	mode, ok := s.cat.Mode(req.Mode)
	if !ok {
		writeErr(w, 400, "unknown_mode", "unknown mode")
		return
	}
	if !s.cat.Fits(mode, mp) {
		writeErr(w, 400, "map_not_for_mode", "that map is not played in this mode")
		return
	}
	set, err := mode.Settings(req.Options)
	if err != nil {
		writeErr(w, 400, "bad_option", err.Error())
		return
	}
	// charged only for a room actually made: a full server or a failed start doesn't count against the caller
	// shortcut: concurrent creates from one address can overshoot the burst, bounded by the room ports
	ip := clientIP(r)
	if !s.createLm.has(ip) {
		writeErr(w, 429, "rate_limited", "too many rooms created, try again later")
		return
	}
	if !s.isAdmin(r) && !s.mayHoldAnother(ip) {
		writeErr(w, 429, "too_many_rooms", "you already have the maximum number of rooms running")
		return
	}
	rm, err := s.rooms.Create(r.Context(), mp, mode, maxPlayers, set, displayName(req.Host), s.cat.MapsFor(mode))
	if err == nil {
		s.createLm.allow(ip)
		s.creatorsMu.Lock()
		s.creators[rm.ID] = ip
		s.creatorsMu.Unlock()
	}
	switch {
	case errors.Is(err, room.ErrFull):
		writeErr(w, 503, "rooms_full", "no free room slot")
	case errors.Is(err, room.ErrStartFailed):
		writeErr(w, 504, "start_failed", "the server did not become ready in time")
	case err != nil:
		writeErr(w, 500, "spawn_failed", "the server did not start")
	default:
		writeJSON(w, 201, struct {
			room.Info
			HostToken string `json:"hostToken"`
		}{rm.Info(), rm.HostToken()})
	}
}

// mayHoldAnother reports whether ip holds fewer live rooms than the limit, forgetting rooms that are gone.
func (s *Server) mayHoldAnother(ip netip.Addr) bool {
	if s.roomsPerIP <= 0 {
		return true
	}
	s.creatorsMu.Lock()
	defer s.creatorsMu.Unlock()
	n := 0
	for id, by := range s.creators {
		if _, ok := s.rooms.Get(id); !ok {
			delete(s.creators, id)
		} else if by == ip {
			n++
		}
	}
	return n < s.roomsPerIP
}

// stopRoom lets the room's host (or an admin) close it; it answers once the server process is gone.
func (s *Server) stopRoom(w http.ResponseWriter, r *http.Request) {
	rm := s.hostRoom(w, r)
	if rm == nil {
		return
	}
	if err := s.rooms.Stop(rm.ID); err != nil {
		writeErr(w, 404, "room_not_found", "no such room")
		return
	}
	w.WriteHeader(204)
}

func (s *Server) deleteRoom(w http.ResponseWriter, r *http.Request) {
	if !s.isAdmin(r) {
		writeErr(w, 403, "not_admin", "admin only")
		return
	}
	if err := s.rooms.Stop(r.PathValue("id")); err != nil {
		writeErr(w, 404, "room_not_found", "no such room")
		return
	}
	w.WriteHeader(204)
}

// displayName keeps a player name printable and short (it is shown, never executed): no control characters,
// quotes or backslashes, at most 31 characters like the engine's own limit.
func displayName(s string) string {
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f || strings.ContainsRune("\"';\\", r) {
			return -1
		}
		return r
	}, s)
	if r := []rune(strings.TrimSpace(s)); len(r) > 31 {
		return string(r[:31])
	}
	return strings.TrimSpace(s)
}

// role is what the caller may do in a room, in increasing order.
type role int

const (
	roleNone   role = iota
	rolePlayer      // holds a live session token of this room (`Authorization: Session <token>`)
	roleHost        // holds the room's host token (`Authorization: Bearer <token>`)
	roleAdmin       // holds the admin cookie
)

// caller resolves the room (writing 404 and returning nil if it doesn't exist) and the caller's role in it.
func (s *Server) caller(w http.ResponseWriter, r *http.Request) (*room.Room, role) {
	rm, ok := s.rooms.Get(r.PathValue("id"))
	if !ok {
		writeErr(w, 404, "room_not_found", "no such room")
		return nil, roleNone
	}
	scheme, tok, _ := strings.Cut(r.Header.Get("Authorization"), " ")
	switch { // host before admin: a host who is also logged in as admin still gets host rights in-game
	case tok != "" && scheme == "Bearer" && rm.IsHost(tok):
		return rm, roleHost
	case s.isAdmin(r):
		return rm, roleAdmin
	case tok != "" && scheme == "Session" && rm.HasSession(tok):
		return rm, rolePlayer
	}
	return rm, roleNone
}

// hostRoom resolves the room (404) and checks the caller is its host or an admin (403).
func (s *Server) hostRoom(w http.ResponseWriter, r *http.Request) *room.Room {
	rm, who := s.caller(w, r)
	if rm != nil && who < roleHost {
		writeErr(w, 403, "not_host", "host or admin only")
		return nil
	}
	return rm
}

// players is open to everyone in the room (they pick Vote kick targets from it); kicking stays host-only.
func (s *Server) players(w http.ResponseWriter, r *http.Request) {
	rm, who := s.caller(w, r)
	if rm == nil {
		return
	}
	if who == roleNone {
		writeErr(w, 403, "not_host", "join the room to see its players")
		return
	}
	ps, err := rm.Players(r.Context())
	if err != nil {
		writeErr(w, 502, "server_unreachable", "the game server did not answer")
		return
	}
	writeJSON(w, 200, ps)
}

func (s *Server) kick(w http.ResponseWriter, r *http.Request) {
	if !s.cmdAllowed(w, r) {
		return
	}
	rm := s.hostRoom(w, r)
	if rm == nil {
		return
	}
	var req struct{ Userid int }
	if !decode(w, r, &req) {
		return
	}
	if req.Userid <= 0 {
		writeErr(w, 400, "bad_request", "userid must be a positive integer")
		return
	}
	if err := rm.Kick(r.Context(), req.Userid); err != nil {
		writeErr(w, 502, "server_unreachable", "the game server did not answer")
		return
	}
	w.WriteHeader(204)
}

func (s *Server) voteMap(w http.ResponseWriter, r *http.Request) {
	rm := s.hostRoom(w, r)
	if rm == nil {
		return
	}
	var req struct{ Maps []string }
	if !decode(w, r, &req) {
		return
	}
	maps, ok := s.voteMaps(w, req.Maps, rm.Mode)
	if !ok {
		return
	}
	writeVote(w, rm.VoteMap(r.Context(), maps))
}

// voteMaps turns 1-4 distinct catalog names that fit the room's mode into maps, or answers 400.
func (s *Server) voteMaps(w http.ResponseWriter, names []string, mode room.Mode) ([]room.Map, bool) {
	if len(names) < 1 || len(names) > 4 {
		writeErr(w, 400, "bad_request", "vote for 1 to 4 maps")
		return nil, false
	}
	var maps []room.Map
	seen := map[string]bool{}
	for _, n := range names {
		mp, ok := s.cat.Map(n)
		if !ok {
			writeErr(w, 400, "unknown_map", "unknown map")
			return nil, false
		}
		if seen[n] {
			writeErr(w, 400, "bad_request", "duplicate map")
			return nil, false
		}
		if !s.cat.Fits(mode, mp) {
			writeErr(w, 400, "map_not_for_mode", "that map is not played in this mode")
			return nil, false
		}
		seen[n] = true
		maps = append(maps, mp)
	}
	return maps, true
}

func writeVote(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, room.ErrVoteRunning):
		writeErr(w, 409, "vote_running", "a vote is already running")
	case errors.Is(err, room.ErrVoteTooSoon):
		writeErr(w, 409, "vote_too_soon", "too soon after the last vote")
	case errors.Is(err, room.ErrNoPlayer):
		writeErr(w, 404, "player_not_found", "no such player")
	case err != nil:
		writeErr(w, 502, "vote_failed", "the game server refused the vote")
	default:
		writeJSON(w, 202, struct{}{})
	}
}

// votes lets any player of the room (live session token), its host or an admin start a map or kick vote.
func (s *Server) votes(w http.ResponseWriter, r *http.Request) {
	if !s.cmdAllowed(w, r) {
		return
	}
	rm, who := s.caller(w, r)
	if rm == nil {
		return
	}
	if who == roleNone {
		writeErr(w, 403, "not_player", "join the room to start a vote")
		return
	}
	var req struct {
		Kind   string
		Maps   []string
		Userid int
	}
	if !decode(w, r, &req) {
		return
	}
	switch req.Kind {
	case "map":
		if maps, ok := s.voteMaps(w, req.Maps, rm.Mode); ok {
			writeVote(w, rm.VoteMap(r.Context(), maps))
		}
	case "kick":
		if req.Userid <= 0 {
			writeErr(w, 400, "bad_request", "userid must be a positive integer")
			return
		}
		writeVote(w, rm.VoteKick(r.Context(), req.Userid))
	default:
		writeErr(w, 400, "bad_request", "kind must be map or kick")
	}
}

// maxBotsPerCall bounds the RCON round trips one request can cost (a 32-slot server fills in two).
const maxBotsPerCall = 16

func (s *Server) addBot(w http.ResponseWriter, r *http.Request) {
	if !s.cmdAllowed(w, r) {
		return
	}
	rm := s.hostRoom(w, r)
	if rm == nil {
		return
	}
	var req struct {
		Team       string
		Difficulty *int
		Count      int // optional: 1 when left out
	}
	if !decode(w, r, &req) {
		return
	}
	if req.Count == 0 {
		req.Count = 1
	}
	if (req.Team != "ct" && req.Team != "t" && req.Team != "both") || req.Difficulty == nil || *req.Difficulty < 0 ||
		*req.Difficulty > 3 || req.Count < 1 || req.Count > maxBotsPerCall {
		writeErr(w, 400, "bad_request", `want {"team":"ct"|"t"|"both","difficulty":0-3,"count":1-16}`)
		return
	}
	if err := rm.AddBots(r.Context(), req.Team, *req.Difficulty, req.Count); err != nil {
		writeErr(w, 502, "server_unreachable", "the game server did not answer")
		return
	}
	w.WriteHeader(204)
}

func (s *Server) kickBots(w http.ResponseWriter, r *http.Request) {
	if !s.cmdAllowed(w, r) {
		return
	}
	rm := s.hostRoom(w, r)
	if rm == nil {
		return
	}
	if err := rm.KickBots(r.Context()); err != nil {
		writeErr(w, 502, "server_unreachable", "the game server did not answer")
		return
	}
	w.WriteHeader(204)
}

func (s *Server) session(w http.ResponseWriter, r *http.Request) {
	rm, who := s.caller(w, r)
	if rm == nil {
		return
	}
	offer, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxOffer))
	var tooBig *http.MaxBytesError
	if errors.As(err, &tooBig) {
		writeErr(w, 413, "offer_too_large", "offer too large")
		return
	} else if err != nil {
		writeErr(w, 400, "bad_offer", "unreadable offer")
		return
	}
	// shortcut: a few addresses can fill MaxSessions with idle sessions that never join hlds (per-IP cap only);
	// a per-room per-IP cap would lock out LAN parties behind one NAT, so add one only if this is abused
	sess := rm.NewSession(who == roleHost)
	answer, err := s.relay.Accept(r.Context(), relay.Target{Addr: rm.Addr(), Done: rm.Done(), MaxSessions: rm.MaxPlayers + 4, Attach: sess.Attach},
		clientIP(r), string(offer))
	switch {
	case errors.Is(err, relay.ErrTooManySessions):
		writeErr(w, 429, "too_many_sessions", "too many sessions")
	case errors.Is(err, relay.ErrBadOffer):
		writeErr(w, 400, "bad_offer", "invalid SDP offer")
	case err != nil:
		writeErr(w, 500, "session_failed", "could not create the session")
	default:
		w.Header().Set("Content-Type", "application/sdp")
		w.Header().Set("X-Session-Token", sess.Token)
		w.WriteHeader(201)
		io.WriteString(w, answer)
	}
}

// ---- admin ----

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	if len(s.adminPw) == 0 {
		writeErr(w, 404, "admin_disabled", "admin is disabled")
		return
	}
	if !s.loginLm.allow(clientIP(r)) {
		writeErr(w, 429, "rate_limited", "too many login attempts")
		return
	}
	var req struct{ Password string }
	if !decode(w, r, &req) {
		return
	}
	if bcrypt.CompareHashAndPassword(s.adminPw, []byte(req.Password)) != nil {
		writeErr(w, 401, "bad_password", "wrong password")
		return
	}
	exp := strconv.FormatInt(time.Now().Add(cookieTTL).Unix(), 10)
	http.SetCookie(w, &http.Cookie{
		Name: cookieName, Value: "v1." + exp + "." + s.mac(exp), Path: "/api", MaxAge: int(cookieTTL.Seconds()),
		HttpOnly: true, Secure: r.Header.Get("X-Forwarded-Proto") == "https", SameSite: http.SameSiteStrictMode,
	})
	w.WriteHeader(204)
}

func (s *Server) adminCheck(w http.ResponseWriter, r *http.Request) {
	if !s.isAdmin(r) {
		writeErr(w, 401, "unauthorized", "not logged in")
		return
	}
	w.WriteHeader(204)
}

func (s *Server) mac(exp string) string {
	h := hmac.New(sha256.New, s.adminKey[:])
	h.Write([]byte("v1." + exp))
	return hex.EncodeToString(h.Sum(nil))
}

// isAdmin accepts the cookie "v1.<exp>.<hex HMAC-SHA256(key, "v1."+exp)>" while exp is in the future.
func (s *Server) isAdmin(r *http.Request) bool {
	c, err := r.Cookie(cookieName)
	if err != nil {
		return false
	}
	parts := strings.Split(c.Value, ".")
	if len(parts) != 3 || parts[0] != "v1" {
		return false
	}
	exp, mac := parts[1], parts[2]
	if !hmac.Equal([]byte(mac), []byte(s.mac(exp))) {
		return false
	}
	n, err := strconv.ParseInt(exp, 10, 64)
	return err == nil && n > time.Now().Unix()
}

// clientIP: only Caddy reaches this server and it replaces X-Forwarded-For from untrusted
// clients, so the first hop is the real client. Falls back to RemoteAddr.
func clientIP(r *http.Request) netip.Addr {
	first, _, _ := strings.Cut(r.Header.Get("X-Forwarded-For"), ",")
	if ip, err := netip.ParseAddr(strings.TrimSpace(first)); err == nil {
		return ip.Unmap()
	}
	ap, _ := netip.ParseAddrPort(r.RemoteAddr)
	return ap.Addr().Unmap()
}

// ---- per-IP limiter ----

func (s *Server) cmdAllowed(w http.ResponseWriter, r *http.Request) bool {
	if s.cmdLm.allow(clientIP(r)) {
		return true
	}
	writeErr(w, 429, "rate_limited", "too many commands, slow down")
	return false
}

type ipLimiter struct {
	mu      sync.Mutex
	every   time.Duration
	burst   int
	m       map[netip.Addr]*rate.Limiter
	pruneAt int
}

// maxLimiterEntries bounds the map under an address-rotation flood.
const maxLimiterEntries = 100_000

func newLimiter(burst int, every time.Duration) *ipLimiter {
	return &ipLimiter{every: every, burst: burst, m: map[netip.Addr]*rate.Limiter{}, pruneAt: 1024}
}

func (l *ipLimiter) allow(ip netip.Addr) bool { return l.get(ip).Allow() }

// has reports whether ip has a token left, without spending it.
func (l *ipLimiter) has(ip netip.Addr) bool { return l.get(ip).Tokens() >= 1 }

func (l *ipLimiter) get(ip netip.Addr) *rate.Limiter {
	if ip.Is6() && !ip.Is4In6() { // one IPv6 subscriber usually owns a whole /64
		ip = netip.PrefixFrom(ip, 64).Masked().Addr()
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.m) >= l.pruneAt {
		for k, v := range l.m { // full buckets carry no information
			if v.Tokens() >= float64(l.burst) {
				delete(l.m, k)
			}
		}
		if len(l.m) >= maxLimiterEntries {
			clear(l.m) // forgetting everyone beats growing without bound
		}
		l.pruneAt = min(max(1024, 2*len(l.m)), maxLimiterEntries)
	}
	lim := l.m[ip]
	if lim == nil {
		lim = rate.NewLimiter(rate.Every(l.every), l.burst)
		l.m[ip] = lim
	}
	return lim
}
