package dbusapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/godbus/dbus/v5"

	"kleinia.io/llmgw/internal/router"
	"kleinia.io/llmgw/internal/server"
	"kleinia.io/llmgw/internal/store"
)

// testEnv is one p2p gateway plus a connected godbus client. Everything runs
// without a bus daemon: the client dials the daemon's unix socket directly.
type testEnv struct {
	g   *Gateway
	st  *store.Store
	rt  *router.Router
	cli *dbus.Conn
	obj dbus.BusObject
}

func startEnv(t *testing.T) *testEnv {
	t.Helper()

	dir, err := os.MkdirTemp("", "llmgw-dbus-test-")
	if err != nil {
		t.Fatalf("temp dir: %v", err)
	}
	st, err := store.Open(filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	rt := router.New(nil)
	refresh := func() { // mirrors server.(*Server).RefreshSources
		srcs, err := st.ListSources()
		if err != nil {
			return
		}
		var rs []router.Source
		for _, src := range srcs {
			maps, err := st.SourceModels(src.ID)
			if err != nil {
				continue
			}
			m := make(map[string]string, len(maps))
			for _, mm := range maps {
				m[mm.Alias] = mm.UpstreamModel
			}
			rs = append(rs, router.Source{
				ID: src.ID, Name: src.Name, Type: src.Type, BaseURL: src.BaseURL,
				APIKey: src.APIKey, Priority: src.Priority, Weight: src.Weight,
				Enabled: src.Enabled, Models: m,
			})
		}
		rt.Update(rs)
	}

	// Keep the socket path short: unix sockets cap at ~104 bytes.
	sock := filepath.Join(os.TempDir(), fmt.Sprintf("llmgw-dbus-%d.sock", time.Now().UnixNano()))
	g, err := New(Options{
		Mode: "p2p", P2PPath: sock, Store: st, Router: rt,
		RefreshSources: refresh, HTTPAddr: "127.0.0.1:18484",
		Pricer: server.LoadPrices(filepath.Join(dir, "prices.json")),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() { runDone <- g.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-runDone; err != nil {
			t.Errorf("Run: %v", err)
		}
		st.Close()
		os.RemoveAll(dir)
		os.Remove(sock)
	})

	// Wait for the socket, failing fast if Run died.
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(sock); err == nil {
			break
		}
		select {
		case err := <-runDone:
			t.Fatalf("gateway exited: %v", err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("p2p socket never appeared")
		}
		time.Sleep(5 * time.Millisecond)
	}

	cli, err := dbus.Connect("unix:path=" + sock)
	if err != nil {
		t.Fatalf("client connect (handshake/Hello): %v", err)
	}
	t.Cleanup(func() { cli.Close() })
	return &testEnv{g: g, st: st, rt: rt, cli: cli, obj: cli.Object(BusName, Path)}
}

func call(t *testing.T, env *testEnv, method string, args ...any) *dbus.Call {
	t.Helper()
	call := env.obj.Call(method, 0, args...)
	if call.Err != nil {
		t.Fatalf("%s: %v", method, call.Err)
	}
	return call
}

func listJSON(t *testing.T, env *testEnv, method string, args ...any) []map[string]any {
	t.Helper()
	var arr []string
	call(t, env, method, args...).Store(&arr)
	out := make([]map[string]any, 0, len(arr))
	for _, s := range arr {
		var m map[string]any
		if err := json.Unmarshal([]byte(s), &m); err != nil {
			t.Fatalf("%s: bad json %q: %v", method, s, err)
		}
		out = append(out, m)
	}
	return out
}

func TestSourcesLifecycle(t *testing.T) {
	env := startEnv(t)

	add := `{"name":"primary","type":"openai","base_url":"http://127.0.0.1:9/v1",` +
		`"api_key":"sk-upstream-secret","models":[{"alias":"model-a","upstream":"model-a"},` +
		`{"alias":"model-b","upstream":"model-b"}],"priority":3,"weight":5}`
	var id uint32
	call(t, env, "io.kleinia.Sources1.AddSource", add).Store(&id)
	if id == 0 {
		t.Fatal("AddSource returned id 0")
	}

	srcs := listJSON(t, env, "io.kleinia.Sources1.ListSources")
	if len(srcs) != 1 {
		t.Fatalf("ListSources: got %d sources, want 1", len(srcs))
	}
	s := srcs[0]
	if s["has_key"] != true {
		t.Errorf("has_key = %v, want true", s["has_key"])
	}
	if _, leaked := s["api_key"]; leaked {
		t.Error("ListSources leaked api_key")
	}
	if s["base_url"] != "http://127.0.0.1:9/v1" || s["priority"] != float64(3) {
		t.Errorf("unexpected source fields: %v", s)
	}
	models, _ := s["models"].([]any)
	if len(models) != 2 || models[0].(map[string]any)["alias"] != "model-a" {
		t.Errorf("models = %v", s["models"])
	}
	health, _ := s["health"].(map[string]any)
	if health["state"] != "ok" {
		t.Errorf("health.state = %v, want ok", health["state"])
	}

	// SetWeight persists and is visible in ListSources.
	call(t, env, "io.kleinia.Sources1.SetWeight", id, int32(42))
	srcs = listJSON(t, env, "io.kleinia.Sources1.ListSources")
	if srcs[0]["weight"] != float64(42) {
		t.Errorf("weight after SetWeight = %v, want 42", srcs[0]["weight"])
	}

	// Unknown ids must error, not silently succeed.
	if err := env.obj.Call("io.kleinia.Sources1.SetWeight", 0, uint32(9999), int32(1)).Err; err == nil {
		t.Error("SetWeight on unknown id unexpectedly succeeded")
	}

	call(t, env, "io.kleinia.Sources1.RemoveSource", id)
	srcs = listJSON(t, env, "io.kleinia.Sources1.ListSources")
	if len(srcs) != 0 {
		t.Fatalf("RemoveSource left %d sources", len(srcs))
	}
}

func TestTestSourceLive(t *testing.T) {
	env := startEnv(t)

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Authorization") != "Bearer sk-up" {
			http.Error(w, "missing key", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"object":"list","data":[{"id":"model-a"},{"id":"model-b"}]}`)
	}))
	defer ts.Close()

	add := fmt.Sprintf(`{"name":"live","type":"openai","base_url":%q,"api_key":"sk-up",`+
		`"models":[{"alias":"model-a","upstream":"model-a"}]}`, ts.URL)
	var id uint32
	call(t, env, "io.kleinia.Sources1.AddSource", add).Store(&id)

	var raw string
	call(t, env, "io.kleinia.Sources1.TestSource", id).Store(&raw)
	var res map[string]any
	if err := json.Unmarshal([]byte(raw), &res); err != nil {
		t.Fatalf("TestSource json: %v", err)
	}
	if res["ok"] != true {
		t.Errorf("ok = %v (raw %s)", res["ok"], raw)
	}
	if res["model_count"] != float64(2) {
		t.Errorf("model_count = %v, want 2", res["model_count"])
	}
	if res["error"] != "" {
		t.Errorf("error = %v", res["error"])
	}
}

func TestKeysLifecycle(t *testing.T) {
	env := startEnv(t)

	var (
		secret string
		kid    uint32
	)
	call(t, env, "io.kleinia.Keys1.IssueKey", "app1",
		`{"models":"*","rpm":10,"daily_tokens":1000000}`).Store(&secret, &kid)
	if !strings.HasPrefix(secret, "sk-kln-") {
		t.Fatalf("secret %q lacks sk-kln- prefix", secret)
	}
	if kid == 0 {
		t.Fatal("IssueKey returned id 0")
	}

	keys := listJSON(t, env, "io.kleinia.Keys1.ListKeys")
	if len(keys) != 1 {
		t.Fatalf("ListKeys: got %d keys, want 1", len(keys))
	}
	k := keys[0]
	if k["name"] != "app1" || k["suspended"] != false || k["rpm"] != float64(10) {
		t.Errorf("key fields wrong: %v", k)
	}
	for _, leaked := range []string{"secret", "key"} {
		if _, ok := k[leaked]; ok {
			t.Errorf("ListKeys leaked field %q", leaked)
		}
	}

	call(t, env, "io.kleinia.Keys1.SuspendKey", "app1")
	keys = listJSON(t, env, "io.kleinia.Keys1.ListKeys")
	if keys[0]["suspended"] != true {
		t.Errorf("suspended after SuspendKey = %v", keys[0]["suspended"])
	}

	// UpdateKey: change models + tpm, clear rpm (0 → unlimited), detach-less
	// AID binding applied; absent daily_tokens keeps its value.
	call(t, env, "io.kleinia.Keys1.UpdateKey", "app1",
		`{"models":"m1,m2","rpm":0,"tpm":5000,"aid":"aid-1"}`)
	keys = listJSON(t, env, "io.kleinia.Keys1.ListKeys")
	k = keys[0]
	if k["models"] != "m1,m2" || k["rpm"] != nil || k["tpm"] != float64(5000) ||
		k["daily_tokens"] != float64(1000000) || k["aid"] != "aid-1" {
		t.Errorf("key fields after UpdateKey wrong: %v", k)
	}

	call(t, env, "io.kleinia.Keys1.ResumeKey", "app1")
	keys = listJSON(t, env, "io.kleinia.Keys1.ListKeys")
	if keys[0]["suspended"] != false {
		t.Errorf("suspended after ResumeKey = %v", keys[0]["suspended"])
	}

	call(t, env, "io.kleinia.Keys1.RevokeKey", "app1")
	keys = listJSON(t, env, "io.kleinia.Keys1.ListKeys")
	if keys[0]["enabled"] != false || keys[0]["revoked_at"] == "" {
		t.Errorf("key not revoked: %v", keys[0])
	}
}

func TestUsage(t *testing.T) {
	env := startEnv(t)

	var (
		secret string
		kid    uint32
	)
	call(t, env, "io.kleinia.Keys1.IssueKey", "app1", `{"rpm":10}`).Store(&secret, &kid)
	var srcID uint32
	call(t, env, "io.kleinia.Sources1.AddSource",
		`{"name":"primary","type":"openai","base_url":"http://127.0.0.1:9/v1","api_key":"k",`+
			`"models":[{"alias":"model-a","upstream":"model-a"}]}`).Store(&srcID)

	now := time.Now().UTC().Format(time.RFC3339Nano)
	rows := []store.UsageRow{
		{TS: now, KeyID: int64(kid), KeyName: "app1", SourceID: int64(srcID), SourceName: "primary",
			ClientFormat: "chat", ModelAlias: "model-a", UpstreamModel: "model-a",
			In: 100, Out: 50, Status: 200, LatencyMS: 42},
		{TS: now, KeyID: int64(kid), KeyName: "app1", SourceID: int64(srcID), SourceName: "primary",
			ClientFormat: "chat", ModelAlias: "model-a", UpstreamModel: "model-a",
			In: 30, Out: 20, Status: 200},
		{TS: now, KeyName: "agent", AID: "agent-1", ClientFormat: "anthropic",
			ModelAlias: "model-a", UpstreamModel: "model-a", In: 10, Out: 5, Status: 200},
	}
	for _, r := range rows {
		if err := env.st.InsertUsage(r); err != nil {
			t.Fatalf("InsertUsage: %v", err)
		}
	}

	var sums []string
	call(t, env, "io.kleinia.Usage1.Summary", "2000-01-01T00:00:00Z").Store(&sums)
	if len(sums) != 4 {
		t.Fatalf("Summary: got %d arrays, want 4 (by_key, by_model, by_source, by_aid)", len(sums))
	}
	for i, name := range []string{"by_key", "by_model", "by_source", "by_aid"} {
		var aggs []map[string]any
		if err := json.Unmarshal([]byte(sums[i]), &aggs); err != nil || len(aggs) == 0 {
			t.Errorf("%s = %q (err %v): want a non-empty aggregate array", name, sums[i], err)
		}
	}

	var recent []string
	call(t, env, "io.kleinia.Usage1.Recent", uint32(2)).Store(&recent)
	if len(recent) != 2 {
		t.Fatalf("Recent(2): got %d rows", len(recent))
	}
	var row map[string]any
	if err := json.Unmarshal([]byte(recent[0]), &row); err != nil {
		t.Fatalf("Recent row json: %v", err)
	}
	if row["aid"] != "agent-1" || row["key_name"] != "agent" || row["model_alias"] != "model-a" {
		t.Errorf("newest row fields wrong: %v", row)
	}

	var winRaw string
	call(t, env, "io.kleinia.Usage1.KeyWindows", "key:app1").Store(&winRaw)
	var win struct {
		RPM    float64 `json:"rpm"`
		TPM    float64 `json:"tpm"`
		Daily  float64 `json:"daily"`
		Limits struct {
			RPM         *int64 `json:"rpm"`
			DailyTokens *int64 `json:"daily_tokens"`
		} `json:"limits"`
	}
	if err := json.Unmarshal([]byte(winRaw), &win); err != nil {
		t.Fatalf("KeyWindows json: %v", err)
	}
	if win.RPM != 2 || win.TPM != 200 || win.Daily != 200 {
		t.Errorf("windows = %+v, want rpm 2 tpm 200 daily 200", win)
	}
	if win.Limits.RPM == nil || *win.Limits.RPM != 10 {
		t.Errorf("limits.rpm = %+v, want 10", win.Limits.RPM)
	}

	// aid: windows resolve without a bound key row (limits null).
	var aidWinRaw string
	call(t, env, "io.kleinia.Usage1.KeyWindows", "aid:agent-1").Store(&aidWinRaw)
	if !strings.Contains(aidWinRaw, `"daily":15`) {
		t.Errorf("aid window = %s, want daily 15", aidWinRaw)
	}

	// Bad ident → InvalidArgs.
	if err := env.obj.Call("io.kleinia.Usage1.KeyWindows", 0, "bogus:x").Err; err == nil {
		t.Error("KeyWindows accepted a malformed ident")
	}
}

func TestQuotaWarningSignal(t *testing.T) {
	env := startEnv(t)

	sigCh := make(chan *dbus.Signal, 4)
	env.cli.Signal(sigCh)

	// Emit from a foreign goroutine (the HTTP path does the same).
	go env.g.EmitQuotaWarning("key:app1", "daily", 0.87)

	select {
	case sig := <-sigCh:
		if sig.Name != "io.kleinia.Quota1.QuotaWarning" {
			t.Fatalf("signal name = %s", sig.Name)
		}
		if got := sig.Body[0].(string); got != "key:app1" {
			t.Errorf("ident = %q", got)
		}
		if got := sig.Body[1].(string); got != "daily" {
			t.Errorf("window = %q", got)
		}
		if got := sig.Body[2].(float64); got != 0.87 {
			t.Errorf("pct = %v", got)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("QuotaWarning signal not received")
	}
}

func TestManagerProperties(t *testing.T) {
	env := startEnv(t)

	var v dbus.Variant
	call(t, env, "org.freedesktop.DBus.Properties.Get",
		"io.kleinia.Manager1", "Version").Store(&v)
	if got, ok := v.Value().(uint32); !ok || got != 2 {
		t.Errorf("Version = %#v, want uint32(2)", v.Value())
	}

	var props map[string]dbus.Variant
	call(t, env, "org.freedesktop.DBus.Properties.GetAll",
		"io.kleinia.Manager1").Store(&props)
	if got := props["Backend"].Value(); got != "real" {
		t.Errorf("Backend = %v", got)
	}
	if got := props["HTTPAddr"].Value(); got != "127.0.0.1:18484" {
		t.Errorf("HTTPAddr = %v", got)
	}

	// Unknown property → InvalidArgs, not a crash.
	if err := env.obj.Call("org.freedesktop.DBus.Properties.Get", 0,
		"io.kleinia.Manager1", "Nope").Err; err == nil {
		t.Error("Get of unknown property succeeded")
	}

	// Introspection smoke: enough XML for a GUI to discover the surface.
	var xml string
	call(t, env, "org.freedesktop.DBus.Introspectable.Introspect").Store(&xml)
	for _, want := range []string{
		"io.kleinia.Sources1", "io.kleinia.Keys1", "io.kleinia.Usage1",
		"SourceHealthChanged", "QuotaWarning",
	} {
		if !strings.Contains(xml, want) {
			t.Errorf("introspection XML missing %q:\n%s", want, xml)
		}
	}
}

func TestHistogramAndStatsMethods(t *testing.T) {
	env := startEnv(t)
	now := time.Now().UTC()
	for _, r := range []store.UsageRow{
		{TS: now.Add(-30 * time.Minute).Format(time.RFC3339Nano), KeyName: "a",
			ModelAlias: "model-a", In: 100, Out: 50, LatencyMS: 800, Status: 200},
		{TS: now.Add(-3 * time.Hour).Format(time.RFC3339Nano), KeyName: "b",
			ModelAlias: "model-a", In: 5, Out: 5, LatencyMS: 500, Status: 429},
	} {
		if err := env.st.InsertUsage(r); err != nil {
			t.Fatalf("InsertUsage: %v", err)
		}
	}
	since := now.Add(-24 * time.Hour).Format(time.RFC3339)

	var buckets []string
	if err := env.obj.Call(ifaceUsage+".Histogram", 0,
		since, "hour", "key_name").Store(&buckets); err != nil {
		t.Fatalf("Histogram: %v", err)
	}
	if len(buckets) != 2 {
		t.Fatalf("histogram buckets = %v", buckets)
	}
	var hb store.HistBucket
	if err := json.Unmarshal([]byte(buckets[0]), &hb); err != nil {
		t.Fatalf("bucket json: %v", err)
	}
	if hb.Group == "" {
		t.Fatalf("dimension slice missing group: %+v", hb)
	}

	var statsJSON string
	if err := env.obj.Call(ifaceUsage+".Stats", 0, since).Store(&statsJSON); err != nil {
		t.Fatalf("Stats: %v", err)
	}
	var st store.Stats
	if err := json.Unmarshal([]byte(statsJSON), &st); err != nil {
		t.Fatalf("stats json: %v", err)
	}
	if st.Requests != 2 || st.In != 105 || st.LatencyP50MS != 500 || st.LatencyP95MS != 800 {
		t.Fatalf("stats = %+v", st)
	}
	if st.ErrorRate < 0.49 || st.ErrorRate > 0.51 {
		t.Fatalf("error rate = %v; want 0.5", st.ErrorRate)
	}

	if err := env.obj.Call(ifaceUsage+".Histogram", 0,
		since, "week", "").Err; err == nil {
		t.Fatal("week bucket accepted")
	}
}

func TestPrices(t *testing.T) {
	env := startEnv(t)

	call(t, env, "io.kleinia.Models1.SetPrice", "model-a",
		`{"input_per_mtok":0.14,"output_per_mtok":0.42,
		  "cache_read_per_mtok":0.014,"cache_write_per_mtok":0.175}`)
	call(t, env, "io.kleinia.Models1.SetPrice", "model-b",
		`{"input_per_mtok":1.0,"output_per_mtok":2.0}`)

	list := listJSON(t, env, "io.kleinia.Models1.ListPrices")
	if len(list) != 2 {
		t.Fatalf("ListPrices: got %d, want 2", len(list))
	}
	if list[0]["alias"] != "model-a" ||
		list[0]["input_per_mtok"] != float64(0.14) ||
		list[0]["output_per_mtok"] != float64(0.42) ||
		list[0]["cache_read_per_mtok"] != float64(0.014) ||
		list[0]["cache_write_per_mtok"] != float64(0.175) {
		t.Errorf("model-a entry wrong: %v", list[0])
	}
	// 未启用分量不出现在 wire JSON 里
	if _, ok := list[1]["cache_read_per_mtok"]; ok {
		t.Errorf("model-b should not carry cache components: %v", list[1])
	}

	// 负值 / 空分量拒绝
	if err := env.obj.Call("io.kleinia.Models1.SetPrice", 0,
		"model-x", `{"input_per_mtok":-1}`).Err; err == nil {
		t.Error("negative price accepted")
	}
	if err := env.obj.Call("io.kleinia.Models1.SetPrice", 0,
		"model-x", `{}`).Err; err == nil {
		t.Error("empty price accepted")
	}

	call(t, env, "io.kleinia.Models1.RemovePrice", "model-a")
	list = listJSON(t, env, "io.kleinia.Models1.ListPrices")
	if len(list) != 1 || list[0]["alias"] != "model-b" {
		t.Fatalf("ListPrices after remove = %v", list)
	}
}

func TestLogsPage(t *testing.T) {
	env := startEnv(t)
	st := env.st

	now := time.Now().UTC()
	for i := 0; i < 7; i++ {
		ts := now.Add(-time.Duration(i) * time.Minute).Format(time.RFC3339Nano)
		status := 200
		ekind := ""
		if i == 3 {
			status, ekind = 500, "upstream"
		}
		if err := st.InsertUsage(store.UsageRow{TS: ts, ReqID: fmt.Sprintf("req-%d", i),
			KeyName: "app1", ModelAlias: "model-a", SourceName: "primary",
			In: 10, Out: 5, Status: status, ErrorKind: ekind}); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}

	since := now.Add(-time.Hour).Format(time.RFC3339)
	var rows []string
	var total uint32
	call(t, env, "io.kleinia.Usage1.Logs", since, uint32(0), uint32(3), "").
		Store(&rows, &total)
	if total != 7 || len(rows) != 3 {
		t.Fatalf("Logs page1: rows=%d total=%d, want 3/7", len(rows), total)
	}
	var first map[string]any
	if err := json.Unmarshal([]byte(rows[0]), &first); err != nil {
		t.Fatalf("row json: %v", err)
	}
	if first["req_id"] != "req-0" || first["key_name"] != "app1" {
		t.Fatalf("newest row wrong: %v", first)
	}
	// 错误过滤
	onlyErr := true
	call(t, env, "io.kleinia.Usage1.Logs", since, uint32(0), uint32(50),
		fmt.Sprintf("{\"errors\":%t}", onlyErr)).Store(&rows, &total)
	if total != 1 || len(rows) != 1 {
		t.Fatalf("Logs errors: rows=%d total=%d, want 1/1", len(rows), total)
	}
	// 搜索
	call(t, env, "io.kleinia.Usage1.Logs", since, uint32(0), uint32(50),
		`{"search":"upstream"}`).Store(&rows, &total)
	if total != 1 {
		t.Fatalf("Logs search total=%d, want 1", total)
	}
}
