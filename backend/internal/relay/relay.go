// Package relay maps one unreliable WebRTC data channel per browser onto one connected UDP
// socket per session. The destination is fixed by the caller (Target), never read from the wire.
package relay

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/ice/v4"
	"github.com/pion/webrtc/v4"
	"golang.org/x/time/rate"
)

const (
	maxDatagram = 4096
	pktRate     = 250 // datagrams/s per session, upstream
	pktBurst    = 500
	maxBuffered = 1 << 20
	gameChannel = "game"
)

var (
	ErrTooManySessions = errors.New("relay: too many sessions")
	ErrBadOffer        = errors.New("relay: bad offer")
	errClosed          = errors.New("relay: closed")
)

type Config struct {
	ICEPort           int           // the single UDP port for every WebRTC session
	PublicIP          netip.Addr    // advertised host candidate
	IdleTimeout       time.Duration // SESSION_IDLE_TIMEOUT (no upstream datagram for this long closes the session); 0 = 60s
	SessionLimitPerIP int           // SESSION_LIMIT_PER_IP; 0 = 16
}

// Target is everything the relay may know about a room; it comes from the room manager.
type Target struct {
	Addr        netip.AddrPort
	Done        <-chan struct{} // closed when the room stops
	MaxSessions int
	// Attach, if set, is told the source address each new session got and returns the function the
	// relay calls exactly once when that session ends (before the address can be reused).
	Attach func(src netip.Addr) (detach func())
}

type Relay struct {
	api *webrtc.API
	mux ice.UDPMux
	cfg Config

	mu        sync.Mutex
	perIP     map[netip.Addr]int
	perTarget map[netip.AddrPort]int
	srcInUse  map[netip.Addr]struct{}
	nextSrc   uint32
	isClosed  bool
	closing   chan struct{}
	wg        sync.WaitGroup
}

func New(cfg Config) (*Relay, error) {
	if cfg.IdleTimeout <= 0 {
		cfg.IdleTimeout = 60 * time.Second
	}
	if cfg.SessionLimitPerIP <= 0 {
		cfg.SessionLimitPerIP = 16
	}
	// One wildcard socket: a per-interface MultiUDPMux registers a session's ufrag only on the mux of the
	// first listen address (the rewrite below collapses them all to PublicIP), so STUN sent to any other
	// local IP, PublicIP included, was dropped.
	udpConn, err := net.ListenUDP("udp4", &net.UDPAddr{Port: cfg.ICEPort})
	if err != nil {
		return nil, err
	}
	mux := ice.NewUDPMuxDefault(ice.UDPMuxParams{UDPConn: udpConn})
	se := webrtc.SettingEngine{}
	se.SetLite(true)
	se.SetICEUDPMux(mux)
	se.SetNetworkTypes([]webrtc.NetworkType{webrtc.NetworkTypeUDP4})
	if cfg.PublicIP.IsValid() {
		err = se.SetICEAddressRewriteRules(webrtc.ICEAddressRewriteRule{
			External:        []string{cfg.PublicIP.String()},
			AsCandidateType: webrtc.ICECandidateTypeHost,
		})
		if err != nil {
			mux.Close()
			return nil, err
		}
	}
	return &Relay{
		api:       webrtc.NewAPI(webrtc.WithSettingEngine(se)),
		mux:       mux,
		cfg:       cfg,
		perIP:     map[netip.Addr]int{},
		perTarget: map[netip.AddrPort]int{},
		srcInUse:  map[netip.Addr]struct{}{},
		closing:   make(chan struct{}),
	}, nil
}

// reserve checks the limits and takes a free 127.10.x.y source address.
func (r *Relay) reserve(client netip.Addr, t Target) (netip.Addr, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.isClosed {
		return netip.Addr{}, errClosed
	}
	if r.perIP[client] >= r.cfg.SessionLimitPerIP || r.perTarget[t.Addr] >= t.MaxSessions {
		return netip.Addr{}, ErrTooManySessions
	}
	for range 1 << 16 {
		n := r.nextSrc
		r.nextSrc = (r.nextSrc + 1) & 0xffff
		if n&0xff == 0 || n&0xff == 0xff {
			continue
		}
		src := netip.AddrFrom4([4]byte{127, 10, byte(n >> 8), byte(n)})
		if _, used := r.srcInUse[src]; used {
			continue
		}
		r.srcInUse[src] = struct{}{}
		r.perIP[client]++
		r.perTarget[t.Addr]++
		r.wg.Add(1)
		return src, nil
	}
	return netip.Addr{}, ErrTooManySessions
}

func (r *Relay) release(client netip.Addr, t Target, src netip.Addr) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.srcInUse, src)
	if r.perIP[client]--; r.perIP[client] <= 0 {
		delete(r.perIP, client)
	}
	if r.perTarget[t.Addr]--; r.perTarget[t.Addr] <= 0 {
		delete(r.perTarget, t.Addr)
	}
	r.wg.Done()
}

// Accept creates one session for client -> t and returns the complete (non-trickle) answer SDP.
func (r *Relay) Accept(ctx context.Context, t Target, client netip.Addr, offerSDP string) (string, error) {
	src, err := r.reserve(client, t)
	if err != nil {
		return "", err
	}
	s := &session{done: make(chan struct{})}
	s.release = func() { r.release(client, t, src) }
	s.lim = rate.NewLimiter(pktRate, pktBurst)
	s.lastUp.Store(time.Now().UnixNano())
	s.conn, err = net.DialUDP("udp4", &net.UDPAddr{IP: src.AsSlice()}, net.UDPAddrFromAddrPort(t.Addr))
	if err != nil {
		s.release()
		return "", err
	}
	if s.pc, err = r.api.NewPeerConnection(webrtc.Configuration{}); err != nil {
		s.conn.Close()
		s.release()
		return "", err
	}
	if t.Attach != nil {
		detach, release := t.Attach(src), s.release
		s.release = func() { detach(); release() }
	}
	pc := s.pc
	pc.OnDataChannel(func(dc *webrtc.DataChannel) {
		mr := dc.MaxRetransmits()
		if dc.Label() != gameChannel || dc.Ordered() || mr == nil || *mr != 0 || !s.attached.CompareAndSwap(false, true) {
			go s.close()
			return
		}
		s.attach(dc)
	})
	pc.OnConnectionStateChange(func(st webrtc.PeerConnectionState) {
		if st == webrtc.PeerConnectionStateFailed || st == webrtc.PeerConnectionStateClosed {
			go s.close()
		}
	})
	go s.watch(t.Done, r.closing, r.cfg.IdleTimeout)

	if err := pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: offerSDP}); err != nil {
		s.close()
		return "", ErrBadOffer
	}
	answer, err := pc.CreateAnswer(nil)
	if err == nil {
		gathered := webrtc.GatheringCompletePromise(pc)
		if err = pc.SetLocalDescription(answer); err == nil {
			select {
			case <-gathered:
			case <-ctx.Done():
				err = ctx.Err()
			case <-s.done:
				err = errClosed
			}
		}
	}
	if err != nil {
		s.close()
		return "", err
	}
	return pc.LocalDescription().SDP, nil
}

// Close closes every session and the ICE port, then waits for the sessions to release.
func (r *Relay) Close() error {
	r.mu.Lock()
	if !r.isClosed {
		r.isClosed = true
		close(r.closing)
	}
	r.mu.Unlock()
	r.wg.Wait()
	return r.mux.Close()
}

type session struct {
	pc       *webrtc.PeerConnection
	conn     *net.UDPConn
	lim      *rate.Limiter
	lastUp   atomic.Int64
	attached atomic.Bool
	once     sync.Once
	done     chan struct{}
	release  func()
}

func (s *session) attach(dc *webrtc.DataChannel) {
	dc.OnMessage(func(m webrtc.DataChannelMessage) {
		if m.IsString || len(m.Data) > maxDatagram || !s.lim.Allow() {
			return
		}
		s.conn.Write(m.Data) // ECONNREFUSED etc.: UDP semantics, drop
		s.lastUp.Store(time.Now().UnixNano())
	})
	dc.OnOpen(func() { go s.downstream(dc) })
	dc.OnClose(func() { go s.close() })
}

func (s *session) downstream(dc *webrtc.DataChannel) {
	buf := make([]byte, 65535)
	for {
		n, err := s.conn.Read(buf)
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			continue // e.g. ICMP port unreachable surfaces as an error on a connected socket
		}
		if dc.BufferedAmount() > maxBuffered {
			continue // drop, as UDP would
		}
		dc.Send(buf[:n])
	}
}

func (s *session) watch(roomDone, relayClosing <-chan struct{}, idle time.Duration) {
	tick := time.NewTicker(max(idle/4, 10*time.Millisecond))
	defer tick.Stop()
	for {
		select {
		case <-s.done:
			return
		case <-roomDone:
		case <-relayClosing:
		case <-tick.C:
			if time.Since(time.Unix(0, s.lastUp.Load())) < idle {
				continue
			}
		}
		s.close()
		return
	}
}

func (s *session) close() {
	s.once.Do(func() {
		close(s.done)
		if s.conn != nil {
			s.conn.Close()
		}
		if s.pc != nil {
			s.pc.Close()
		}
		s.release()
	})
}
