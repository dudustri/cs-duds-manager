package testpeer

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pion/ice/v4"
	"github.com/pion/webrtc/v4"
)

// A throwaway ICE-lite echo answerer (the shape the real relay will have) proving Dial works:
// loopback candidates, channel options, header passthrough, echo, close detection.
func TestDialAgainstLiteAnswerer(t *testing.T) {
	pcc, _ := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	icePort := pcc.LocalAddr().(*net.UDPAddr).Port
	pcc.Close()
	mux, err := ice.NewMultiUDPMuxFromPort(icePort, ice.UDPMuxFromPortWithLoopback())
	if err != nil {
		t.Fatal(err)
	}
	defer mux.Close()
	se := webrtc.SettingEngine{}
	se.SetLite(true)
	se.SetICEUDPMux(mux)
	se.SetNetworkTypes([]webrtc.NetworkType{webrtc.NetworkTypeUDP4})
	se.SetICEAddressRewriteRules(webrtc.ICEAddressRewriteRule{External: []string{"127.0.0.1"}, AsCandidateType: webrtc.ICECandidateTypeHost})
	api := webrtc.NewAPI(webrtc.WithSettingEngine(se))
	var gotXFF atomic.Value
	var gotOrdered atomic.Bool
	closeNow := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotXFF.Store(r.Header.Get("X-Forwarded-For"))
		pc, _ := api.NewPeerConnection(webrtc.Configuration{})
		pc.OnDataChannel(func(dc *webrtc.DataChannel) {
			gotOrdered.Store(dc.Ordered())
			dc.OnMessage(func(m webrtc.DataChannelMessage) { dc.Send(m.Data) })
			go func() { <-closeNow; pc.Close() }()
		})
		b, _ := io.ReadAll(r.Body)
		pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: string(b)})
		ans, _ := pc.CreateAnswer(nil)
		g := webrtc.GatheringCompletePromise(pc)
		pc.SetLocalDescription(ans)
		<-g
		w.Header().Set("Content-Type", "application/sdp")
		w.WriteHeader(201)
		w.Write([]byte(pc.LocalDescription().SDP))
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	p, resp, err := Dial(ctx, srv.URL, "r1", WithHeader("X-Forwarded-For", "203.0.113.9"))
	if err != nil || resp.StatusCode != 201 {
		t.Fatalf("dial: %v %v", resp, err)
	}
	if gotXFF.Load() != "203.0.113.9" {
		t.Errorf("xff %v", gotXFF.Load())
	}
	if err := p.Send([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	if b, err := p.Recv(ctx); err != nil || string(b) != "hello" {
		t.Fatalf("echo %q %v", b, err)
	}
	if gotOrdered.Load() {
		t.Error("channel should be unordered")
	}
	close(closeNow)
	select {
	case <-p.Closed():
	case <-time.After(5 * time.Second):
		t.Fatal("Closed() not signalled when the answerer closes")
	}
	// non-201: response + body returned, no peer
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, `{"code":"x"}`, 429) }))
	defer bad.Close()
	p2, resp, err := Dial(ctx, bad.URL, "r")
	if p2 != nil || err == nil || resp.StatusCode != 429 {
		t.Fatalf("non-201: %v %v %v", p2, resp, err)
	}
}

func TestParseA2SInfo(t *testing.T) {
	b := append([]byte{0xff, 0xff, 0xff, 0xff, 'I', 48}, "srv\x00de_dust2\x00cstrike\x00Counter-Strike\x00\x0a\x00\x03\x10"...)
	m, err := parseA2SInfo(b)
	if err != nil || m["map"] != "de_dust2" || m["folder"] != "cstrike" || m["players"] != "3" || m["max_players"] != "16" {
		t.Fatalf("%v %v", m, err)
	}
}
