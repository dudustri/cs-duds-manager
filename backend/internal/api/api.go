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
}

type Server struct {
	cat      *room.Catalog
	rooms    *room.Manager
	relay    *relay.Relay
	createLm *ipLimiter
	loginLm  *ipLimiter
	adminKey [32]byte // random per process: admin cookies die with the process, like rooms
	adminPw  []byte
	mux      *http.ServeMux
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
	}
	rand.Read(s.adminKey[:])
	s.mux.HandleFunc("GET /api/catalog", s.catalog)
	s.mux.HandleFunc("GET /api/rooms", s.listRooms)
	s.mux.HandleFunc("POST /api/rooms", s.createRoom)
	s.mux.HandleFunc("DELETE /api/rooms/{id}", s.deleteRoom)
	s.mux.HandleFunc("GET /api/rooms/{id}/players", s.players)
	s.mux.HandleFunc("POST /api/rooms/{id}/kick", s.kick)
	s.mux.HandleFunc("POST /api/rooms/{id}/vote-map", s.voteMap)
	s.mux.HandleFunc("POST /api/rooms/{id}/votes", s.votes)
	s.mux.HandleFunc("POST /api/rooms/{id}/bots", s.addBot)
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
	writeJSON(w, 200, map[string][]string{"maps": s.cat.Maps(), "modes": s.cat.Modes()})
}

func (s *Server) listRooms(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, s.rooms.List())
}

func (s *Server) createRoom(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Map, Mode  string
		MaxPlayers *int // optional; a non-integer fails the decode
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
	if !s.createLm.allow(clientIP(r)) {
		writeErr(w, 429, "rate_limited", "too many rooms created, try again later")
		return
	}
	rm, err := s.rooms.Create(r.Context(), mp, mode, maxPlayers)
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

// hostRoom resolves the room (404) and checks the caller is its host or an admin (403).
func (s *Server) hostRoom(w http.ResponseWriter, r *http.Request) *room.Room {
	rm, ok := s.rooms.Get(r.PathValue("id"))
	if !ok {
		writeErr(w, 404, "room_not_found", "no such room")
		return nil
	}
	tok, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !(tok != "" && rm.IsHost(tok)) && !s.isAdmin(r) {
		writeErr(w, 403, "not_host", "host or admin only")
		return nil
	}
	return rm
}

func (s *Server) players(w http.ResponseWriter, r *http.Request) {
	rm := s.hostRoom(w, r)
	if rm == nil {
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
	maps, ok := s.voteMaps(w, req.Maps)
	if !ok {
		return
	}
	writeVote(w, rm.VoteMap(r.Context(), maps))
}

// voteMaps turns 1-4 distinct catalog names into maps, or answers 400.
func (s *Server) voteMaps(w http.ResponseWriter, names []string) ([]room.Map, bool) {
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
	rm, ok := s.rooms.Get(r.PathValue("id"))
	if !ok {
		writeErr(w, 404, "room_not_found", "no such room")
		return
	}
	scheme, tok, _ := strings.Cut(r.Header.Get("Authorization"), " ")
	if !(scheme == "Session" && rm.HasSession(tok)) && !(scheme == "Bearer" && rm.IsHost(tok)) && !s.isAdmin(r) {
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
		if maps, ok := s.voteMaps(w, req.Maps); ok {
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

func (s *Server) addBot(w http.ResponseWriter, r *http.Request) {
	rm := s.hostRoom(w, r)
	if rm == nil {
		return
	}
	var req struct {
		Team       string
		Difficulty *int
	}
	if !decode(w, r, &req) {
		return
	}
	if (req.Team != "ct" && req.Team != "t") || req.Difficulty == nil || *req.Difficulty < 0 || *req.Difficulty > 3 {
		writeErr(w, 400, "bad_request", `want {"team":"ct"|"t","difficulty":0-3}`)
		return
	}
	if err := rm.AddBot(r.Context(), req.Team == "ct", *req.Difficulty); err != nil {
		writeErr(w, 502, "server_unreachable", "the game server did not answer")
		return
	}
	w.WriteHeader(204)
}

func (s *Server) kickBots(w http.ResponseWriter, r *http.Request) {
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
	rm, ok := s.rooms.Get(r.PathValue("id"))
	if !ok {
		writeErr(w, 404, "room_not_found", "no such room")
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
	// Accept calls Attach synchronously, so token is set by the time it returns.
	hostTok, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	host := hostTok != "" && rm.IsHost(hostTok)
	var token string
	attach := func(src netip.Addr) func() {
		var end func()
		token, end = rm.OpenSession(src, host)
		return end
	}
	answer, err := s.relay.Accept(r.Context(), relay.Target{Addr: rm.Addr(), Done: rm.Done(), MaxSessions: rm.MaxPlayers + 4, Attach: attach},
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
		w.Header().Set("X-Session-Token", token)
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

type ipLimiter struct {
	mu    sync.Mutex
	every time.Duration
	burst int
	m     map[netip.Addr]*rate.Limiter
}

func newLimiter(burst int, every time.Duration) *ipLimiter {
	return &ipLimiter{every: every, burst: burst, m: map[netip.Addr]*rate.Limiter{}}
}

func (l *ipLimiter) allow(ip netip.Addr) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.m) > 1024 { // prune full buckets: they carry no information
		for k, v := range l.m {
			if v.Tokens() >= float64(l.burst) {
				delete(l.m, k)
			}
		}
	}
	lim := l.m[ip]
	if lim == nil {
		lim = rate.NewLimiter(rate.Every(l.every), l.burst)
		l.m[ip] = lim
	}
	return lim.Allow()
}
