package dbusapi

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/godbus/dbus/v5"
)

// serveBus connects to the system or session bus, acquires BusName and
// serves the same dispatch tree p2p uses. The connection is created with a
// custom dbus.Handler so incoming method calls reach Gateway.invoke (which
// owns polkit gating and arg handling) instead of godbus's reflection
// exporter — one dispatch path for every transport.
func (g *Gateway) serveBus(ctx context.Context) error {
	var (
		conn *dbus.Conn
		err  error
	)
	if g.opts.Mode == "session" {
		conn, err = dbus.ConnectSessionBus(dbus.WithHandler(&gwHandler{g: g}))
	} else {
		conn, err = dbus.ConnectSystemBus(dbus.WithHandler(&gwHandler{g: g}))
	}
	if err != nil {
		return fmt.Errorf("dbusapi: connect %s bus: %w", g.opts.Mode, err)
	}
	reply, err := conn.RequestName(BusName, dbus.NameFlagDoNotQueue)
	if err != nil {
		conn.Close()
		return fmt.Errorf("dbusapi: request name %s: %w", BusName, err)
	}
	if reply != dbus.RequestNameReplyPrimaryOwner {
		conn.Close()
		return fmt.Errorf("dbusapi: bus name %s already taken", BusName)
	}
	g.setBusConn(conn)
	log.Printf("dbusapi: %s bus name %s acquired (object %s)", g.opts.Mode, BusName, Path)
	<-ctx.Done()
	g.setBusConn(nil)
	conn.Close()
	return nil
}

// gwHandler adapts Gateway to the godbus server-side Handler interface.
type gwHandler struct{ g *Gateway }

func (h *gwHandler) LookupObject(path dbus.ObjectPath) (dbus.ServerObject, bool) {
	if _, ok := h.g.tree[path]; ok {
		return &gwObject{g: h.g, path: path}, true
	}
	return nil, false
}

type gwObject struct {
	g    *Gateway
	path dbus.ObjectPath
}

func (o *gwObject) LookupInterface(name string) (dbus.Interface, bool) {
	ifaces := o.g.tree[o.path]
	if name == "" {
		return &gwIface{g: o.g, path: o.path, name: ""}, true
	}
	if _, ok := ifaces[name]; ok {
		return &gwIface{g: o.g, path: o.path, name: name}, true
	}
	return nil, false
}

type gwIface struct {
	g    *Gateway
	path dbus.ObjectPath
	name string
}

func (i *gwIface) LookupMethod(member string) (dbus.Method, bool) {
	ifaces := i.g.tree[i.path]
	if i.name == "" {
		for _, ie := range ifaces {
			if e, ok := ie.methods[member]; ok {
				return gatewayMethod{g: i.g, e: e}, true
			}
		}
		return nil, false
	}
	e, ok := ifaces[i.name].methods[member]
	if !ok {
		return nil, false
	}
	return gatewayMethod{g: i.g, e: e}, true
}

// gatewayMethod routes one method call into Gateway.invoke. godbus calls
// DecodeArguments with the raw message; Call then runs the shared dispatch
// (arity checks, polkit, implementation) off the conn's handler goroutine.
type gatewayMethod struct {
	g *Gateway
	e *methodEntry
}

func (m gatewayMethod) DecodeArguments(_ *dbus.Conn, _ string, msg *dbus.Message, _ []any) ([]any, error) {
	return []any{msg}, nil
}

func (m gatewayMethod) Call(args ...any) ([]any, error) {
	msg, _ := args[0].(*dbus.Message)
	if msg == nil {
		return nil, dbus.ErrMsgInvalidArg
	}
	ret, derr := m.g.invoke(msg)
	if derr != nil {
		return nil, derr
	}
	return ret, nil
}

// The reflection plumbing below is unused (DecodeArguments short-circuits
// the standard decode) but part of the dbus.Method contract.

func (m gatewayMethod) NumArguments() int     { return 1 }
func (m gatewayMethod) NumReturns() int       { return 1 }
func (m gatewayMethod) ArgumentValue(int) any { return dbus.Message{} }
func (m gatewayMethod) ReturnValue(int) any   { return nil }

// polkitSubject encodes CheckAuthorization's "(sa{sv})" subject.
type polkitSubject struct {
	Kind    string
	Details map[string]dbus.Variant
}

// authorize polkit-gates one mutating call. Behavior:
//
//   - Polkit disabled (p2p/session default, or --polkit=off): allow.
//   - Polkit enabled (system mode, default on) but no bus connection or no
//     sender (a p2p peer): allow — the socket permissions are the gate.
//   - Otherwise resolve the caller's pid via org.freedesktop.DBus and ask
//     polkit (AllowUserInteraction). Anything but an explicit "yes" is
//     denied with io.kleinia.ModelGateway.NotAuthorized.
func (g *Gateway) authorize(sender, action string) *dbus.Error {
	if !g.polkitOn {
		return nil
	}
	bus := g.busConnSnapshot()
	if bus == nil || sender == "" {
		return nil
	}
	var pid uint32
	if err := bus.Object(ifaceDBus, dbusServicePath).
		Call("org.freedesktop.DBus.GetConnectionUnixProcessID", 0, sender).
		Store(&pid); err != nil {
		return notAuthorized("cannot resolve caller %q: %v", sender, err)
	}
	subject := polkitSubject{
		Kind:    "system-bus-name",
		Details: map[string]dbus.Variant{"name": dbus.MakeVariant(sender)},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	call := bus.Object(polkitAuthority, polkitAuthorPath).CallWithContext(ctx,
		"org.freedesktop.PolicyKit1.Authority.CheckAuthorization", 0,
		subject, action, map[string]string{}, uint32(1), "")
	if call.Err != nil {
		return notAuthorized("polkit check for %s failed: %v", action, call.Err)
	}
	if ok, _ := call.Body[0].(bool); !ok {
		return notAuthorized("polkit denied action %s for pid %d", action, pid)
	}
	return nil
}

func notAuthorized(format string, a ...any) *dbus.Error {
	return &dbus.Error{Name: ErrorNotAuthorized, Body: []any{fmt.Sprintf(format, a...)}}
}
