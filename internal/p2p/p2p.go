package p2p

import (
	"fmt"
	"net"
	"strconv"
	"sync"
	"time"

	"github.com/pion/stun/v4"
	"github.com/pion/webrtc/v4"
)

// Signal is exchanged over the control plane. File data never travels here;
// only SDP and ICE candidates do.
type Signal struct {
	Kind        string                     `json:"kind"`
	Description *webrtc.SessionDescription `json:"description,omitempty"`
	Candidate   *webrtc.ICECandidateInit   `json:"candidate,omitempty"`
	Name        string                     `json:"name,omitempty"`
	Size        int64                      `json:"size,omitempty"`
}

type Conn struct {
	PC *webrtc.PeerConnection

	mu        sync.Mutex
	remoteSet bool
	pending   []webrtc.ICECandidateInit
	closed    bool
}

func NewConn(stunURLs []string) (*Conn, error) {
	cfg := webrtc.Configuration{}
	if len(stunURLs) > 0 {
		cfg.ICEServers = []webrtc.ICEServer{{URLs: stunURLs}}
	}
	pc, err := webrtc.NewPeerConnection(cfg)
	if err != nil {
		return nil, err
	}
	return &Conn{PC: pc}, nil
}

func (c *Conn) Close() error {
	c.mu.Lock()
	c.closed = true
	c.mu.Unlock()
	return c.PC.Close()
}

func (c *Conn) SetRemote(desc webrtc.SessionDescription) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.PC.SetRemoteDescription(desc); err != nil {
		return err
	}
	c.remoteSet = true
	for _, cand := range c.pending {
		if err := c.PC.AddICECandidate(cand); err != nil {
			return err
		}
	}
	c.pending = nil
	return nil
}

func (c *Conn) AddCandidate(cand webrtc.ICECandidateInit) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil
	}
	if !c.remoteSet {
		c.pending = append(c.pending, cand)
		return nil
	}
	return c.PC.AddICECandidate(cand)
}

func NewOffer(pc *webrtc.PeerConnection) (Signal, error) {
	offer, err := pc.CreateOffer(nil)
	if err != nil {
		return Signal{}, err
	}
	if err := pc.SetLocalDescription(offer); err != nil {
		return Signal{}, err
	}
	desc := pc.LocalDescription()
	if desc == nil {
		return Signal{}, fmt.Errorf("empty local description")
	}
	return Signal{Kind: "offer", Description: desc}, nil
}

func NewAnswer(pc *webrtc.PeerConnection, offer webrtc.SessionDescription) (Signal, error) {
	if err := pc.SetRemoteDescription(offer); err != nil {
		return Signal{}, err
	}
	answer, err := pc.CreateAnswer(nil)
	if err != nil {
		return Signal{}, err
	}
	if err := pc.SetLocalDescription(answer); err != nil {
		return Signal{}, err
	}
	desc := pc.LocalDescription()
	if desc == nil {
		return Signal{}, fmt.Errorf("empty local description")
	}
	return Signal{Kind: "answer", Description: desc}, nil
}

// ForwardCandidates streams locally gathered candidates to send.
// Install it before creating a local description.
func ForwardCandidates(pc *webrtc.PeerConnection, send func(Signal) error) {
	pc.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c == nil {
			return
		}
		init := c.ToJSON()
		_ = send(Signal{Kind: "candidate", Candidate: &init})
	})
}

// MappedAddr asks the first reachable STUN server for this host's public
// transport address. The address reflects the NAT mapping of the probe; a
// later WebRTC connection usually gets a different port.
func MappedAddr(stunURLs []string, timeout time.Duration) (string, error) {
	var lastErr error
	for _, raw := range stunURLs {
		uri, err := stun.ParseURI(raw)
		if err != nil {
			lastErr = err
			continue
		}
		addr, err := probeSTUN(uri, timeout)
		if err == nil {
			return addr, nil
		}
		lastErr = err
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("未配置 STUN 服务器")
	}
	return "", lastErr
}

func probeSTUN(uri *stun.URI, timeout time.Duration) (string, error) {
	client, err := stun.DialURI(uri, &stun.DialConfig{})
	if err != nil {
		return "", err
	}
	defer client.Close()

	var event stun.Event
	done := make(chan error, 1)
	go func() {
		done <- client.Do(stun.MustBuild(stun.TransactionID, stun.BindingRequest), func(ev stun.Event) {
			event = ev
		})
	}()
	select {
	case err := <-done:
		if err != nil {
			return "", err
		}
	case <-time.After(timeout):
		return "", fmt.Errorf("STUN 响应超时")
	}
	if event.Error != nil {
		return "", event.Error
	}
	var mapped stun.XORMappedAddress
	if err := mapped.GetFrom(event.Message); err != nil {
		return "", err
	}
	return net.JoinHostPort(mapped.IP.String(), strconv.Itoa(mapped.Port)), nil
}
