// Package dbusapi exposes llmgw's management plane over D-Bus.
//
// The daemon owns the well-known bus name io.kleinia.ModelGateway with a
// single object at /io/kleinia/ModelGateway carrying five interfaces:
//
//	io.kleinia.Manager1   read-only properties: Version, Backend, HTTPAddr
//	io.kleinia.Sources1   upstream source CRUD, live test, health signal
//	io.kleinia.Keys1      gateway key issue / suspend / resume / revoke
//	io.kleinia.Usage1     read-only usage aggregates and recent rows
//	io.kleinia.Quota1     QuotaWarning signal
//
// Three transports are supported. "--dbus=system" and "--dbus=session"
// connect to a real bus daemon, request the well-known name and serve
// through godbus's normal connection machinery (system mode polkit-gates
// every mutating method). "--dbus-p2p=<path>" serves the same object tree
// over a plain unix socket with no bus daemon at all — godbus v5 has no
// server-side SASL responder, so p2p.go implements the D-Bus handshake
// itself and then decodes/encodes the wire protocol directly. That mode is
// the test path and the direct-connect path for local GUIs:
//
//	dbus.Connect("unix:path=/run/llmgw/manager.sock")
//
// Complex payloads are JSON strings ("s" / "as") so every method keeps a
// shallow D-Bus signature. Secrets never leave the daemon: ListSources
// exposes only has_key and ListKeys never includes the secret.
package dbusapi

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/godbus/dbus/v5"

	"kleinia.io/llmgw/internal/router"
	"kleinia.io/llmgw/internal/server"
	"kleinia.io/llmgw/internal/store"
)

// Well-known names of the management plane.
const (
	BusName = "io.kleinia.ModelGateway"
	Path    = dbus.ObjectPath("/io/kleinia/ModelGateway")
	APIVer  = uint32(2)
	Backend = "real"
	// ErrorNotAuthorized is the D-Bus error name returned when polkit
	// denies a mutating method.
	ErrorNotAuthorized = "io.kleinia.ModelGateway.NotAuthorized"

	// Polkit action ids (system bus mode only).
	ActionGrant  = "io.kleinia.llmgw.grant"  // issue gateway keys
	ActionManage = "io.kleinia.llmgw.manage" // every other mutation

	ifaceManager = "io.kleinia.Manager1"
	ifaceSources = "io.kleinia.Sources1"
	ifaceKeys    = "io.kleinia.Keys1"
	ifaceModels  = "io.kleinia.Models1"
	ifaceUsage   = "io.kleinia.Usage1"
	ifaceQuota   = "io.kleinia.Quota1"

	ifaceDBus        = "org.freedesktop.DBus"
	dbusServicePath  = dbus.ObjectPath("/org/freedesktop/DBus")
	polkitAuthority  = "org.freedesktop.PolicyKit1.Authority"
	polkitAuthorPath = dbus.ObjectPath("/org/freedesktop/PolicyKit1/Authority")

	healthPollInterval = 5 * time.Second
)

// Options configures the D-Bus management plane.
//
// Mode is "system", "session", "p2p", "" or "off". A non-empty P2PPath
// serves bus-less peer-to-peer clients regardless of Mode, so "system" plus
// a socket path runs both listeners concurrently.
type Options struct {
	Mode    string // "system" | "session" | "p2p" | "" | "off"
	P2PPath string // unix socket path for bus-less operation
	// Polkit gates mutating methods via PolicyKit when Mode is "system".
	// p2p and session modes never consult polkit (the flag is honored only
	// in system mode); default it in the caller, e.g. on for system.
	Polkit bool

	Store *store.Store
	// Router supplies live health for ListSources and the
	// SourceHealthChanged signal; nil disables both.
	Router *router.Router
	// Pricer backs the Models1 pricing methods; nil disables them
	// (returns an error) — main passes the same instance the HTTP
	// server uses, so SetPrice is live for in-flight traffic.
	Pricer *server.Pricer
	// RefreshSources is invoked after every successful mutation so the
	// running gateway serves the new configuration immediately (main wires
	// it to server.(*Server).RefreshSources; settable post-construction
	// with SetRefreshSources).
	RefreshSources func()
	// HTTPAddr is exposed as the Manager1.HTTPAddr property.
	HTTPAddr string
	// OnQuotaWarn, when set, is invoked (in addition to emitting the
	// io.kleinia.Quota1.QuotaWarning signal) on every EmitQuotaWarning —
	// a local hook for tests and daemons without a D-Bus client.
	OnQuotaWarn func(ident, window string, pct float64)
}

// Gateway is the D-Bus management plane. Construct it with New, then Run it
// in its own goroutine; EmitQuotaWarning may be called from any goroutine
// before and during Run.
type Gateway struct {
	opts     Options
	polkitOn bool
	guid     string
	tree     dispatchTree

	mu      sync.Mutex
	busConn *dbus.Conn
	conns   map[*p2pConn]struct{}
	refresh func()
	names   map[string]bool // p2p names acquired by peers (shim bookkeeping)
}

// New validates the options and builds a Gateway without starting any I/O.
func New(opts Options) (*Gateway, error) {
	switch opts.Mode {
	case "", "off", "system", "session", "p2p":
	default:
		return nil, fmt.Errorf("dbusapi: unknown mode %q (want system, session, p2p or off)", opts.Mode)
	}
	if opts.Mode == "p2p" && opts.P2PPath == "" {
		return nil, errors.New("dbusapi: mode p2p needs a socket path")
	}
	if (opts.Mode == "" || opts.Mode == "off") && opts.P2PPath == "" {
		return nil, errors.New("dbusapi: nothing to serve (set Mode or P2PPath)")
	}
	if opts.Store == nil {
		return nil, errors.New("dbusapi: Options.Store is required")
	}
	var guid [16]byte
	if _, err := rand.Read(guid[:]); err != nil {
		return nil, fmt.Errorf("dbusapi: guid: %w", err)
	}
	g := &Gateway{
		opts:     opts,
		polkitOn: opts.Polkit && opts.Mode == "system",
		guid:     hex.EncodeToString(guid[:]),
		conns:    make(map[*p2pConn]struct{}),
		refresh:  opts.RefreshSources,
		names:    make(map[string]bool),
	}
	g.tree = newDispatchTree()
	return g, nil
}

// Run builds a Gateway from opts and serves until ctx is cancelled. Startup
// failures (socket bind, bus connect, name conflict) are returned; after a
// successful start Run blocks for the lifetime of ctx.
func Run(ctx context.Context, opts Options) error {
	g, err := New(opts)
	if err != nil {
		return err
	}
	return g.Run(ctx)
}

// SetRefreshSources installs the hot-reload callback when it was not (or
// could not be) supplied in Options — main constructs the Gateway before
// the HTTP server exists, then hands over srv.RefreshSources.
func (g *Gateway) SetRefreshSources(f func()) {
	g.mu.Lock()
	g.refresh = f
	g.mu.Unlock()
}

func (g *Gateway) refreshSources() {
	g.mu.Lock()
	f := g.refresh
	g.mu.Unlock()
	if f != nil {
		f()
	}
}

// Run serves the configured transports until ctx is cancelled, then closes
// the bus connection, the p2p listener and every peer connection.
func (g *Gateway) Run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var wg sync.WaitGroup
	errCh := make(chan error, 2)
	start := func(f func() error) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := f(); err != nil {
				select {
				case errCh <- err:
				default:
				}
			}
		}()
	}
	if g.opts.P2PPath != "" {
		start(func() error { return g.serveP2P(ctx) })
	}
	if g.opts.Mode == "system" || g.opts.Mode == "session" {
		start(func() error { return g.serveBus(ctx) })
	}
	go g.pollHealth(ctx)

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case err := <-errCh:
		cancel()
		<-done
		return err
	case <-done:
		return nil
	}
}

// EmitQuotaWarning broadcasts io.kleinia.Quota1.QuotaWarning(ident, window,
// pct) and fans out to Options.OnQuotaWarn. It is goroutine-safe and is the
// hook main plugs into server.Config.OnQuotaWarn; the signal itself is
// always serialized through the D-Bus connections, never the caller's
// goroutine state.
func (g *Gateway) EmitQuotaWarning(ident, window string, pct float64) {
	g.emitSignal(Path, ifaceQuota, "QuotaWarning", ident, window, pct)
	if g.opts.OnQuotaWarn != nil {
		g.opts.OnQuotaWarn(ident, window, pct)
	}
}

// emitSignal sends one signal message on every transport (bus connection
// plus each p2p peer). Signals carry no destination: on a bus the daemon
// fans them out to subscribers, p2p delivers them straight to the peer.
func (g *Gateway) emitSignal(path dbus.ObjectPath, iface, member string, values ...any) {
	g.mu.Lock()
	bus := g.busConn
	conns := make([]*p2pConn, 0, len(g.conns))
	for c := range g.conns {
		conns = append(conns, c)
	}
	g.mu.Unlock()

	if bus != nil {
		if err := bus.Emit(path, iface+"."+member, values...); err != nil {
			log.Printf("dbusapi: emit %s.%s on bus: %v", iface, member, err)
		}
	}
	for _, c := range conns {
		c.sendSignal(path, iface, member, values...)
	}
}

// healthPoint is one source's live health state, derived from the router.
type healthPoint struct {
	state string // "ok" | "cooling" | "disabled"
	fails int
}

// healthSnapshot merges the router's source view (enabled flags) with its
// failure/cooldown state. Sources absent from the router report "ok".
func (g *Gateway) healthSnapshot() map[int64]healthPoint {
	if g.opts.Router == nil {
		return nil
	}
	now := time.Now()
	srcs := g.opts.Router.Sources()
	health := g.opts.Router.Health()
	byID := make(map[int64]healthPoint, len(srcs))
	for _, s := range srcs {
		p := healthPoint{state: "ok"}
		if !s.Enabled {
			p.state = "disabled"
		}
		for _, h := range health {
			if h.ID == s.ID {
				p.fails = h.ConsecutiveFails
				if h.CoolingUntil.After(now) && p.state != "disabled" {
					p.state = "cooling"
				}
				break
			}
		}
		byID[s.ID] = p
	}
	return byID
}

// pollHealth watches the router and emits SourceHealthChanged whenever a
// source's state changes (first snapshot is the baseline, no signal).
func (g *Gateway) pollHealth(ctx context.Context) {
	if g.opts.Router == nil {
		return
	}
	t := time.NewTicker(healthPollInterval)
	defer t.Stop()
	var prev map[int64]healthPoint
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		snap := g.healthSnapshot()
		if prev == nil {
			prev = snap
			continue
		}
		for id, p := range snap {
			old, ok := prev[id]
			switch {
			case !ok:
				g.emitSignal(Path, ifaceSources, "SourceHealthChanged",
					uint32(id), p.state, "added")
			case old.state != p.state:
				reason := "recovered"
				switch p.state {
				case "cooling":
					reason = "cooling"
				case "disabled":
					reason = "disabled"
				}
				g.emitSignal(Path, ifaceSources, "SourceHealthChanged",
					uint32(id), p.state, reason)
			}
		}
		for id := range prev {
			if _, ok := snap[id]; !ok {
				g.emitSignal(Path, ifaceSources, "SourceHealthChanged",
					uint32(id), "removed", "removed")
			}
		}
		prev = snap
	}
}

func (g *Gateway) addConn(c *p2pConn) {
	g.mu.Lock()
	g.conns[c] = struct{}{}
	g.mu.Unlock()
}

func (g *Gateway) removeConn(c *p2pConn) {
	g.mu.Lock()
	delete(g.conns, c)
	g.mu.Unlock()
}

func (g *Gateway) closeConns() {
	g.mu.Lock()
	conns := make([]*p2pConn, 0, len(g.conns))
	for c := range g.conns {
		conns = append(conns, c)
		delete(g.conns, c)
	}
	g.mu.Unlock()
	for _, c := range conns {
		c.close()
	}
}

func (g *Gateway) setBusConn(conn *dbus.Conn) {
	g.mu.Lock()
	g.busConn = conn
	g.mu.Unlock()
}

func (g *Gateway) busConnSnapshot() *dbus.Conn {
	g.mu.Lock()
	conn := g.busConn
	g.mu.Unlock()
	return conn
}
