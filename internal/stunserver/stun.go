package stunserver

import (
	"context"
	"errors"
	"fmt"
	"net"

	"github.com/pion/stun/v4"
)

// Run starts a minimal STUN server (RFC 5389 Binding requests only).
// It reflects the caller's UDP source address, which is all ICE needs.
func Run(ctx context.Context, addr string, logf func(string, ...any)) error {
	conn, err := net.ListenPacket("udp4", addr)
	if err != nil {
		return fmt.Errorf("stun listen %s: %w", addr, err)
	}
	defer conn.Close()
	if logf != nil {
		logf("STUN 监听 %s/udp", addr)
	}
	go func() {
		<-ctx.Done()
		conn.Close()
	}()

	buf := make([]byte, 1500)
	for {
		n, src, err := conn.ReadFrom(buf)
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}
		udpSrc, ok := src.(*net.UDPAddr)
		if !ok {
			continue
		}
		m := &stun.Message{Raw: append([]byte(nil), buf[:n]...)}
		if err := m.Decode(); err != nil || m.Type != stun.BindingRequest {
			continue
		}
		res, err := stun.Build(
			stun.BindingSuccess,
			stun.NewTransactionIDSetter(m.TransactionID),
			&stun.XORMappedAddress{IP: udpSrc.IP, Port: udpSrc.Port},
			stun.Fingerprint,
		)
		if err != nil {
			continue
		}
		if _, err := conn.WriteTo(res.Raw, udpSrc); err != nil && ctx.Err() != nil {
			return nil
		}
	}
}
