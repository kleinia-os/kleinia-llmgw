package dbusapi

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net"
	"os"
	"path/filepath"

	"github.com/godbus/dbus/v5"
)

// serveP2P listens on a unix socket and serves bus-less D-Bus clients.
// godbus v5 only implements the client (initiator) side of the SASL
// handshake and offers no way to start the server-side read loop, so this
// file drives the protocol by hand: serverSASL answers the authentication
// conversation, then a per-connection loop decodes messages with
// dbus.DecodeMessage and dispatches them through the shared Gateway table,
// sending replies through a dbus.Conn constructed over the raw socket
// (which provides serial generation and wire encoding).
func (g *Gateway) serveP2P(ctx context.Context) error {
	if err := os.Remove(g.opts.P2PPath); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("dbusapi: remove stale socket %s: %w", g.opts.P2PPath, err)
	}
	if dir := filepath.Dir(g.opts.P2PPath); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("dbusapi: create socket dir %s: %w", dir, err)
		}
	}
	ln, err := net.Listen("unix", g.opts.P2PPath)
	if err != nil {
		return fmt.Errorf("dbusapi: listen p2p %s: %w", g.opts.P2PPath, err)
	}
	// The socket is the trust boundary (p2p skips polkit): owner-only.
	_ = os.Chmod(g.opts.P2PPath, 0o600)
	defer os.Remove(g.opts.P2PPath)
	defer g.closeConns()
	go func() {
		<-ctx.Done()
		ln.Close()
	}()
	log.Printf("dbusapi: p2p listening on unix:path=%s", g.opts.P2PPath)

	for {
		nc, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("dbusapi: p2p accept: %w", err)
		}
		uc, ok := nc.(*net.UnixConn)
		if !ok {
			nc.Close()
			continue
		}
		go g.serveP2PConn(uc)
	}
}

// p2pConn is one accepted peer. br feeds both the handshake and the message
// loop; dc is a godbus Conn layered over the same socket used only as a
// thread-safe encoder with serial generation (it never runs godbus's own
// read loop — that is what this file replaces).
type p2pConn struct {
	dc *dbus.Conn
	br *bufio.Reader
}

func (g *Gateway) serveP2PConn(nc *net.UnixConn) {
	br := bufio.NewReaderSize(nc, 1<<16)
	bw := bufio.NewWriter(nc)
	dc, err := dbus.NewConn(nc)
	if err != nil {
		nc.Close()
		return
	}
	c := &p2pConn{dc: dc, br: br}
	if err := serverSASL(br, bw, g.guid); err != nil {
		dc.Close()
		return
	}
	g.addConn(c)
	defer func() {
		g.removeConn(c)
		dc.Close()
	}()

	for {
		msg, err := dbus.DecodeMessage(br)
		if err != nil {
			return
		}
		if msg.Type != dbus.TypeMethodCall {
			continue // signals from peers are not part of the management API
		}
		go func(m *dbus.Message) {
			sender, _ := m.Headers[dbus.FieldSender].Value().(string)
			if m.Flags&dbus.FlagNoReplyExpected != 0 {
				_, _ = g.invoke(m) // fire-and-forget: errors have nowhere to go
				return
			}
			ret, derr := g.invoke(m)
			if derr != nil {
				c.sendError(derr, sender, m.Serial())
				return
			}
			c.sendReply(sender, m.Serial(), ret)
		}(msg)
	}
}

func (c *p2pConn) close() { _ = c.dc.Close() }

func (c *p2pConn) sendReply(dest string, replySerial uint32, body []any) {
	msg := &dbus.Message{
		Type: dbus.TypeMethodReply,
		Headers: map[dbus.HeaderField]dbus.Variant{
			dbus.FieldReplySerial: dbus.MakeVariant(replySerial),
		},
		Body: body,
	}
	finishHeaders(msg, dest, body)
	_ = c.dc.Send(msg, nil)
}

func (c *p2pConn) sendError(derr *dbus.Error, dest string, replySerial uint32) {
	msg := &dbus.Message{
		Type: dbus.TypeError,
		Headers: map[dbus.HeaderField]dbus.Variant{
			dbus.FieldReplySerial: dbus.MakeVariant(replySerial),
			dbus.FieldErrorName:   dbus.MakeVariant(derr.Name),
		},
		Body: derr.Body,
	}
	finishHeaders(msg, dest, derr.Body)
	_ = c.dc.Send(msg, nil)
}

func (c *p2pConn) sendSignal(path dbus.ObjectPath, iface, member string, values ...any) {
	msg := &dbus.Message{
		Type: dbus.TypeSignal,
		Headers: map[dbus.HeaderField]dbus.Variant{
			dbus.FieldPath:      dbus.MakeVariant(path),
			dbus.FieldInterface: dbus.MakeVariant(iface),
			dbus.FieldMember:    dbus.MakeVariant(member),
		},
		Body: values,
	}
	if len(values) > 0 {
		msg.Headers[dbus.FieldSignature] = dbus.MakeVariant(dbus.SignatureOf(values...))
	}
	_ = c.dc.Send(msg, nil)
}

// finishHeaders fills the optional destination and signature headers shared
// by replies and errors.
func finishHeaders(msg *dbus.Message, dest string, body []any) {
	if dest != "" {
		msg.Headers[dbus.FieldDestination] = dbus.MakeVariant(dest)
	}
	if len(body) > 0 {
		msg.Headers[dbus.FieldSignature] = dbus.MakeVariant(dbus.SignatureOf(body...))
	}
}

// serverSASL performs the server (responder) side of the D-Bus
// authentication conversation over the unix transport: an initial NUL byte
// followed by \r\n-terminated commands, ending at BEGIN. Only the EXTERNAL
// mechanism is offered — for a p2p unix socket the peer's ability to
// connect (filesystem permissions on the socket) is the identity. Unix FD
// passing is refused: the management API never passes file descriptors.
func serverSASL(br *bufio.Reader, bw *bufio.Writer, guid string) error {
	b, err := br.ReadByte()
	if err != nil {
		return err
	}
	if b != 0 {
		return fmt.Errorf("dbus: expected leading NUL byte, got %#02x", b)
	}
	for {
		line, err := readSASLLine(br)
		if err != nil {
			return err
		}
		parts := bytes.Split(line, []byte{' '})
		var reply string
		switch string(parts[0]) {
		case "AUTH":
			// EXTERNAL succeeds with or without an initial response: on a
			// unix socket the kernel-provided peer credentials are the
			// identity, so no DATA challenge is needed. Anything else —
			// including a bare "AUTH" probe or other mechanisms — is
			// re-rejected with the mechanism list.
			if len(parts) >= 2 && string(parts[1]) == "EXTERNAL" {
				reply = "OK " + guid
			} else {
				reply = "REJECTED EXTERNAL"
			}
		case "NEGOTIATE_UNIX_FD":
			reply = "ERROR"
		case "CANCEL":
			reply = "REJECTED EXTERNAL"
		case "BEGIN":
			return bw.Flush()
		default:
			reply = "ERROR"
		}
		if _, err := bw.WriteString(reply + "\r\n"); err != nil {
			return err
		}
		if err := bw.Flush(); err != nil {
			return err
		}
	}
}

func readSASLLine(br *bufio.Reader) ([]byte, error) {
	line, err := br.ReadBytes('\n')
	if err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(line, []byte("\r\n")), nil
}
