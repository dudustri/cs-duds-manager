// Package testpeer is a pion "browser" for tests (test infrastructure, not product code).
package testpeer

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"

	"github.com/pion/webrtc/v4"
)

type options struct {
	ordered     bool
	retransmits uint16
	label       string
	extra       bool
	header      http.Header
}

type Option func(*options)

func WithOrdered() Option             { return func(o *options) { o.ordered = true } }
func WithRetransmits(n uint16) Option { return func(o *options) { o.retransmits = n } }
func WithLabel(s string) Option       { return func(o *options) { o.label = s } }
func WithExtraChannel() Option        { return func(o *options) { o.extra = true } }
func WithHeader(k, v string) Option   { return func(o *options) { o.header.Set(k, v) } }

type Peer struct {
	pc     *webrtc.PeerConnection
	dc     *webrtc.DataChannel
	in     chan []byte
	closed chan struct{}
	once   sync.Once
}

func (p *Peer) markClosed() { p.once.Do(func() { close(p.closed) }) }

// Dial POSTs an offer to baseURL/api/rooms/{id}/session and waits for the main channel (default
// label "game", unordered, maxRetransmits 0) to open, or for the relay to close the connection (then
// it returns the Peer with Closed() already closed and a nil error).
// The HTTP response is returned even when the status is not 201: then the Peer is nil, err is
// non-nil, and resp.Body is a re-readable copy of the body (for the {"error","code"} JSON).
func Dial(ctx context.Context, baseURL, roomID string, opts ...Option) (*Peer, *http.Response, error) {
	o := options{label: "game", header: http.Header{}}
	for _, f := range opts {
		f(&o)
	}
	se := webrtc.SettingEngine{}
	se.SetNetworkTypes([]webrtc.NetworkType{webrtc.NetworkTypeUDP4})
	se.SetIncludeLoopbackCandidate(true) // the relay under test advertises 127.0.0.1
	pc, err := webrtc.NewAPI(webrtc.WithSettingEngine(se)).NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		return nil, nil, err
	}
	p := &Peer{pc: pc, in: make(chan []byte, 4096), closed: make(chan struct{})}
	fail := func(err error) (*Peer, *http.Response, error) { pc.Close(); return nil, nil, err }
	init := &webrtc.DataChannelInit{Ordered: &o.ordered, MaxRetransmits: &o.retransmits}
	if p.dc, err = pc.CreateDataChannel(o.label, init); err != nil {
		return fail(err)
	}
	if o.extra {
		if _, err = pc.CreateDataChannel("extra", init); err != nil {
			return fail(err)
		}
	}
	opened := make(chan struct{})
	p.dc.OnOpen(func() { close(opened) })
	p.dc.OnClose(p.markClosed)
	p.dc.OnMessage(func(m webrtc.DataChannelMessage) {
		select {
		case p.in <- append([]byte(nil), m.Data...):
		default:
		}
	})
	pc.OnConnectionStateChange(func(s webrtc.PeerConnectionState) {
		if s == webrtc.PeerConnectionStateFailed || s == webrtc.PeerConnectionStateClosed {
			p.markClosed()
		}
	})

	offer, err := pc.CreateOffer(nil)
	if err != nil {
		return fail(err)
	}
	gathered := webrtc.GatheringCompletePromise(pc)
	if err = pc.SetLocalDescription(offer); err != nil {
		return fail(err)
	}
	select {
	case <-gathered:
	case <-ctx.Done():
		return fail(ctx.Err())
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(baseURL, "/")+"/api/rooms/"+roomID+"/session",
		strings.NewReader(pc.LocalDescription().SDP))
	if err != nil {
		return fail(err)
	}
	req.Header = o.header.Clone()
	req.Header.Set("Content-Type", "application/sdp")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fail(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	resp.Body = io.NopCloser(bytes.NewReader(body))
	if resp.StatusCode != http.StatusCreated {
		pc.Close()
		return nil, resp, fmt.Errorf("session: %s: %s", resp.Status, body)
	}
	if err = pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: string(body)}); err != nil {
		pc.Close()
		return nil, resp, err
	}
	select {
	case <-opened:
	case <-p.closed:
	case <-ctx.Done():
		pc.Close()
		return nil, resp, ctx.Err()
	}
	return p, resp, nil
}

func (p *Peer) Send(b []byte) error { return p.dc.Send(b) }

func (p *Peer) Recv(ctx context.Context) ([]byte, error) {
	select {
	case b := <-p.in:
		return b, nil
	case <-p.closed:
		return nil, io.EOF
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (p *Peer) Closed() <-chan struct{} { return p.closed }

func (p *Peer) Close() error { p.markClosed(); return p.pc.Close() }

var oob = []byte{0xff, 0xff, 0xff, 0xff}

// A2SInfo queries the server through the relay (test-only; A2S is not in production code).
// Returns keys name, map, folder, game, players, max_players.
func (p *Peer) A2SInfo(ctx context.Context) (map[string]string, error) {
	q := append(append([]byte(nil), oob...), "TSource Engine Query\x00"...)
	if err := p.Send(q); err != nil {
		return nil, err
	}
	for i := 0; i < 2; i++ {
		b, err := p.Recv(ctx)
		if err != nil {
			return nil, err
		}
		if len(b) >= 9 && bytes.HasPrefix(b, oob) && b[4] == 'A' { // challenge: resend with it appended
			if err := p.Send(append(append([]byte(nil), q...), b[5:9]...)); err != nil {
				return nil, err
			}
			continue
		}
		return parseA2SInfo(b)
	}
	return nil, errors.New("a2s: challenge loop")
}

func parseA2SInfo(b []byte) (map[string]string, error) {
	if len(b) < 6 || !bytes.HasPrefix(b, oob) {
		return nil, fmt.Errorf("a2s: bad reply %q", b)
	}
	kind, r := b[4], b[5:]
	str := func() string {
		i := bytes.IndexByte(r, 0)
		if i < 0 {
			i = len(r)
		}
		s := string(r[:i])
		r = r[min(i+1, len(r)):]
		return s
	}
	m := map[string]string{}
	switch kind {
	case 'I': // Source: protocol, name, map, folder, game, id(2), players, max
		if len(r) < 1 {
			return nil, errors.New("a2s: short")
		}
		r = r[1:]
		m["name"], m["map"], m["folder"], m["game"] = str(), str(), str(), str()
		if len(r) < 4 {
			return nil, errors.New("a2s: short")
		}
		m["players"], m["max_players"] = strconv.Itoa(int(r[2])), strconv.Itoa(int(r[3]))
	case 'm': // GoldSrc: address, name, map, folder, game, players, max
		str()
		m["name"], m["map"], m["folder"], m["game"] = str(), str(), str(), str()
		if len(r) < 2 {
			return nil, errors.New("a2s: short")
		}
		m["players"], m["max_players"] = strconv.Itoa(int(r[0])), strconv.Itoa(int(r[1]))
	default:
		return nil, fmt.Errorf("a2s: unexpected reply type %q", kind)
	}
	return m, nil
}
