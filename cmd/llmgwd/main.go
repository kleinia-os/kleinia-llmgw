// llmgwd is the Kleinia model gateway daemon (G0: local HTTP aggregation
// gateway; the agentfs skill surface lands in G1).
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"kleinia.io/llmgw/internal/dbusapi"
	"kleinia.io/llmgw/internal/router"
	"kleinia.io/llmgw/internal/server"
	"kleinia.io/llmgw/internal/store"
)

// loadOrCreateToken reads the sys.llm shared secret, creating it (0600)
// when missing. It is never printed.
func loadOrCreateToken(path string) (string, error) {
	if path == "" {
		return "", nil
	}
	if b, err := os.ReadFile(path); err == nil {
		if t := string(b); t != "" {
			return t, nil
		}
	}
	var buf [24]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", err
	}
	token := hex.EncodeToString(buf[:])
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", err
	}
	if err := os.WriteFile(path, []byte(token), 0o600); err != nil {
		return "", err
	}
	return token, nil
}

func main() {
	addr := flag.String("addr", "127.0.0.1:8484", "listen address")
	dbPath := flag.String("db", "var/state.db", "SQLite state database path")
	prices := flag.String("prices", "prices/prices.json", "model price table (optional)")
	tokenFile := flag.String("internal-token-file", "var/internal.token",
		"shared secret file for the sys.llm skill carrier (created 0600 if missing; empty disables internal auth)")
	aidDaily := flag.Int64("aid-daily-tokens", 0,
		"daily in+out token limit for AID-only agent identities without a bound key (0=unlimited)")
	dbusMode := flag.String("dbus", "",
		"D-Bus management plane bus: \"system\" or \"session\" (empty = none)")
	dbusP2P := flag.String("dbus-p2p", "",
		"unix socket path serving bus-less peer-to-peer D-Bus clients (e.g. /run/llmgw/manager.sock)")
	dbusPolkit := flag.String("polkit", "",
		"polkit gating of D-Bus change methods: \"on\"/\"off\" (default: on for --dbus=system, off otherwise)")
	flag.Parse()

	st, err := store.Open(*dbPath)
	if err != nil {
		log.Fatalf("open db: %v", err)
	}
	defer st.Close()

	internalToken, err := loadOrCreateToken(*tokenFile)
	if err != nil {
		log.Fatalf("internal token: %v", err)
	}

	rt := router.New(nil)
	var aidDefault *int64
	if *aidDaily > 0 {
		aidDefault = aidDaily
	}

	// One Pricer instance shared by HTTP accounting and the D-Bus plane:
	// Models1.SetPrice is live for in-flight traffic and persists to the
	// prices.json path.
	pricer := server.LoadPrices(*prices)

	// D-Bus management plane (optional): built before the HTTP server so
	// its signal emitter can be wired into server.Config.OnQuotaWarn; the
	// hot-reload callback is handed over once the server exists.
	var gw *dbusapi.Gateway
	if *dbusMode != "" || *dbusP2P != "" {
		polkit := *dbusMode == "system"
		if *dbusPolkit == "off" {
			polkit = false
		} else if *dbusPolkit == "on" {
			polkit = true
		}
		gw, err = dbusapi.New(dbusapi.Options{
			Mode: *dbusMode, P2PPath: *dbusP2P, Polkit: polkit,
			Store: st, Router: rt, HTTPAddr: *addr, Pricer: pricer,
		})
		if err != nil {
			log.Fatalf("dbus: %v", err)
		}
	}
	cfg := server.Config{
		Addr: *addr, InternalToken: internalToken, AIDDailyDefault: aidDefault,
	}
	if gw != nil {
		cfg.OnQuotaWarn = gw.EmitQuotaWarning
	}
	srv := server.New(cfg, st, rt, pricer)
	srv.SetCodecs(server.WireDefault())
	srv.RefreshSources()

	// The D-Bus plane runs alongside HTTP until shutdown; p2p/bus startup
	// failures are fatal (log.Fatalf) so misconfiguration is visible.
	dbusCtx, dbusCancel := context.WithCancel(context.Background())
	defer dbusCancel()
	if gw != nil {
		gw.SetRefreshSources(srv.RefreshSources)
		go func() {
			if err := gw.Run(dbusCtx); err != nil {
				log.Fatalf("dbus management plane: %v", err)
			}
		}()
	}

	// Sources may be edited by llmgwctl while running: reload periodically
	// and on SIGHUP. Keys are read per-request from the store (always fresh).
	go func() {
		t := time.NewTicker(30 * time.Second)
		defer t.Stop()
		for range t.C {
			srv.RefreshSources()
		}
	}()
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGHUP)
	go func() {
		for range sig {
			srv.RefreshSources()
		}
	}()

	httpSrv := &http.Server{
		Addr:              *addr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		log.Printf("llmgwd listening on http://%s", *addr)
		if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("serve: %v", err)
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	dbusCancel() // tear the D-Bus plane down while HTTP drains
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(ctx)
	log.Println("llmgwd stopped")
}
