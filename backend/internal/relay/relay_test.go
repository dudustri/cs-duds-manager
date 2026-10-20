package relay

import (
	"context"
	"net"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pion/webrtc/v4"
)

// echoRoom is a UDP "room" that echoes "echo:"+payload and records each sender address.
func echoRoom(t *testing.T) (*net.UDPConn, *sync.Map) {
	t.Helper()
	c, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	seen := &sync.Map{}
	go func() {
		buf := make([]byte, 65535)
		for {
			n, from, err := c.ReadFromUDP(buf)
			if err != nil {
				return
			}
			seen.Store(from.IP.String(), true)
			c.WriteToUDP(append([]byte("echo:"), buf[:n]...), from)
		}
	}()
	return c, seen
}

func freeUDPPort(t *testing.T) int {
	c, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	return c.LocalAddr().(*net.UDPAddr).Port
}

type client struct {
	pc     *webrtc.PeerConnection
	dc     *webrtc.DataChannel
	in     chan string
	opened chan struct{}
	closed chan struct{}
}

func dial(t *testing.T, r *Relay, tg Target, ip string, label string, ordered bool) (*client, error) {
	t.Helper()
	se := webrtc.SettingEngine{}
	se.SetNetworkTypes([]webrtc.NetworkType{webrtc.NetworkTypeUDP4})
	se.SetIncludeLoopbackCandidate(true)
	pc, err := webrtc.NewAPI(webrtc.WithSettingEngine(se)).NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pc.Close() })
	zero := uint16(0)
	dc, err := pc.CreateDataChannel(label, &webrtc.DataChannelInit{Ordered: &ordered, MaxRetransmits: &zero})
	if err != nil {
		t.Fatal(err)
	}
	c := &client{pc: pc, dc: dc, in: make(chan string, 16), opened: make(chan struct{}), closed: make(chan struct{})}
	dc.OnOpen(func() { close(c.opened) })
	dc.OnClose(func() { close(c.closed) })
	dc.OnMessage(func(m webrtc.DataChannelMessage) { c.in <- string(m.Data) })
	offer, err := pc.CreateOffer(nil)
	if err != nil {
		t.Fatal(err)
	}
	gathered := webrtc.GatheringCompletePromise(pc)
	if err := pc.SetLocalDescription(offer); err != nil {
		t.Fatal(err)
	}
	<-gathered
	ans, err := r.Accept(context.Background(), tg, netip.MustParseAddr(ip), pc.LocalDescription().SDP)
	if err != nil {
		return nil, err
	}
	if err := pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: ans}); err != nil {
		t.Fatal(err)
	}
	return c, nil
}

func (c *client) wait(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(10 * time.Second):
		t.Fatal("timeout waiting for " + what)
	}
}

func setup(t *testing.T, limit int) (*Relay, Target, *sync.Map, chan struct{}) {
	t.Helper()
	room, seen := echoRoom(t)
	r, err := New(Config{ICEPort: freeUDPPort(t), PublicIP: netip.MustParseAddr("127.0.0.1"), IdleTimeout: 30 * time.Second, SessionLimitPerIP: limit})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	done := make(chan struct{})
	return r, Target{Addr: room.LocalAddr().(*net.UDPAddr).AddrPort(), Done: done, MaxSessions: 20}, seen, done
}

func TestRoundTripAndDistinctSources(t *testing.T) {
	r, tg, seen, _ := setup(t, 16)
	for i := range 2 {
		c, err := dial(t, r, tg, "10.0.0.1", "game", false)
		if err != nil {
			t.Fatal(err)
		}
		c.wait(t, c.opened, "open")
		c.dc.Send([]byte("hi"))
		select {
		case m := <-c.in:
			if m != "echo:hi" {
				t.Fatalf("session %d: got %q", i, m)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("no echo")
		}
	}
	n := 0
	seen.Range(func(k, _ any) bool {
		if !strings.HasPrefix(k.(string), "127.10.") {
			t.Errorf("source %v not in 127.10/16", k)
		}
		n++
		return true
	})
	if n != 2 {
		t.Fatalf("want 2 distinct sources, got %d", n)
	}
}

func TestBadChannelClosed(t *testing.T) {
	r, tg, _, _ := setup(t, 16)
	for _, tc := range []struct {
		label   string
		ordered bool
	}{{"other", false}, {"game", true}} {
		c, err := dial(t, r, tg, "10.0.0.2", tc.label, tc.ordered)
		if err != nil {
			t.Fatal(err)
		}
		select {
		case <-c.closed:
		case <-time.After(10 * time.Second):
			t.Fatalf("%+v: channel not closed", tc)
		}
	}
}

func TestPerIPLimitAndRelease(t *testing.T) {
	r, tg, _, done := setup(t, 2)
	for range 2 {
		if _, err := dial(t, r, tg, "10.0.0.3", "game", false); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := dial(t, r, tg, "10.0.0.3", "game", false); err != ErrTooManySessions {
		t.Fatalf("want ErrTooManySessions, got %v", err)
	}
	if _, err := dial(t, r, tg, "10.0.0.4", "game", false); err != nil { // other IP is fine
		t.Fatal(err)
	}
	close(done) // room stops: every session releases
	deadline := time.Now().Add(10 * time.Second)
	for {
		r.mu.Lock()
		n := len(r.srcInUse)
		r.mu.Unlock()
		if n == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d sessions still held after room done", n)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestBadOffer(t *testing.T) {
	r, tg, _, _ := setup(t, 16)
	if _, err := r.Accept(context.Background(), tg, netip.MustParseAddr("10.0.0.5"), "garbage"); err != ErrBadOffer {
		t.Fatalf("want ErrBadOffer, got %v", err)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.srcInUse) != 0 {
		t.Fatal("leaked reservation")
	}
}
