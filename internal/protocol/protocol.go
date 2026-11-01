package protocol

import (
	"encoding/json"
	"time"
)

const (
	TypeHello   = "hello"
	TypeDevices = "devices"
	TypeSignal  = "signal"
	TypeInfo    = "info"
	TypeError   = "error"
)

type Device struct {
	ID         string    `json:"id"`
	Name       string    `json:"name"`
	Platform   string    `json:"platform"`
	Online     bool      `json:"online"`
	LastSeen   time.Time `json:"last_seen"`
	PublicAddr string    `json:"public_addr,omitempty"`
	Location   string    `json:"location,omitempty"`
}

type Envelope struct {
	Type       string          `json:"type"`
	PeerID     string          `json:"peer_id,omitempty"`
	Username   string          `json:"username,omitempty"`
	To         string          `json:"to,omitempty"`
	From       string          `json:"from,omitempty"`
	Data       json.RawMessage `json:"data,omitempty"`
	Devices    []Device        `json:"devices,omitempty"`
	Error      string          `json:"error,omitempty"`
	PublicAddr string          `json:"public_addr,omitempty"`
}
