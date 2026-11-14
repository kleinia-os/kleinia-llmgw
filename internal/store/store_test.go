package store

import (
	"testing"
	"time"
)

func mustOpen(t *testing.T) *Store {
	t.Helper()
	s, err := Open(":memory:")
	if err != nil {
		t.Fatalf("Open(:memory:): %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestOpenMemoryCreatesSchema(t *testing.T) {
	s := mustOpen(t)
	want := map[string]bool{
		"sources": false, "models": false, "keys": false, "usage": false,
		"schema_version": false, "idx_usage_ts": false, "idx_usage_key_id_ts": false,
		"idx_keys_aid": false, "idx_usage_aid_ts": false,
	}
	rows, err := s.db.Query(`SELECT name FROM sqlite_master WHERE type IN ('table', 'index')`)
	if err != nil {
		t.Fatalf("query sqlite_master: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan sqlite_master: %v", err)
		}
		if _, ok := want[name]; ok {
			want[name] = true
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("sqlite_master rows: %v", err)
	}
	for name, seen := range want {
		if !seen {
			t.Errorf("schema object %q missing after migration", name)
		}
	}
	var version int
	if err := s.db.QueryRow(`SELECT version FROM schema_version`).Scan(&version); err != nil || version != len(migrations) {
		t.Errorf("schema_version = %d, err=%v; want %d, nil", version, err, len(migrations))
	}
}

func TestFileDBPersistsAndMigrationIdempotent(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/state.db"

	s1, err := Open(path)
	if err != nil {
		t.Fatalf("open file db: %v", err)
	}
	var journal string
	if err := s1.db.QueryRow(`PRAGMA journal_mode`).Scan(&journal); err != nil || journal != "wal" {
		t.Errorf("journal_mode = %q, err=%v; want wal", journal, err)
	}
	id, err := s1.AddSource(Source{Name: "primary", Type: "openai", BaseURL: "https://upstream.example.test/v1",
		APIKey: "sk-up", Priority: 5, Weight: 10, Enabled: true},
		[]ModelMapping{{Alias: "model-a", UpstreamModel: "model-a"}})
	if err != nil {
		t.Fatalf("AddSource: %v", err)
	}
	if err := s1.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	s2, err := Open(path) // migration must be a no-op, data intact
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s2.Close()
	srcs, err := s2.ListSources()
	if err != nil || len(srcs) != 1 {
		t.Fatalf("ListSources after reopen = %v (err %v); want 1 source", srcs, err)
	}
	if srcs[0].ID != id || srcs[0].Name != "primary" || srcs[0].APIKey != "sk-up" {
		t.Errorf("source round-trip mismatch: %+v", srcs[0])
	}
}

func TestSourceCRUD(t *testing.T) {
	s := mustOpen(t)

	id, err := s.AddSource(Source{Name: "primary", Type: "openai", BaseURL: "https://upstream.example.test/v1",
		APIKey: "k1", Priority: 3, Weight: 7, Enabled: true}, []ModelMapping{
		{Alias: "model-a", UpstreamModel: "model-a"},
		{Alias: "model-b", UpstreamModel: "model-b"},
	})
	if err != nil {
		t.Fatalf("AddSource: %v", err)
	}
	if id == 0 {
		t.Fatal("AddSource returned id 0")
	}
	if _, err := s.AddSource(Source{Name: "primary", Type: "openai", BaseURL: "u"}, nil); err == nil {
		t.Fatal("duplicate source name accepted")
	}

	srcs, err := s.ListSources()
	if err != nil {
		t.Fatalf("ListSources: %v", err)
	}
	if len(srcs) != 1 {
		t.Fatalf("ListSources returned %d rows; want 1", len(srcs))
	}
	got := srcs[0]
	if got.ID != id || got.Name != "primary" || got.Type != "openai" || got.BaseURL != "https://upstream.example.test/v1" ||
		got.APIKey != "k1" || got.Priority != 3 || got.Weight != 7 || !got.Enabled {
		t.Errorf("source fields mismatch: %+v", got)
	}

	ms, err := s.SourceModels(id)
	if err != nil {
		t.Fatalf("SourceModels: %v", err)
	}
	if len(ms) != 2 || ms[0] != (ModelMapping{"model-a", "model-a"}) || ms[1] != (ModelMapping{"model-b", "model-b"}) {
		t.Errorf("SourceModels = %+v; want model-a then model-b (alias order)", ms)
	}

	// Full replace: new fields, new model set.
	if err := s.UpdateSource(Source{ID: id, Name: "primary2", Type: "anthropic", BaseURL: "https://x/v1",
		APIKey: "k2", Priority: 9, Weight: 0, Enabled: false},
		[]ModelMapping{{Alias: "model-c", UpstreamModel: "model-c-up"}}); err != nil {
		t.Fatalf("UpdateSource: %v", err)
	}
	srcs, _ = s.ListSources()
	if len(srcs) != 1 || srcs[0].Name != "primary2" || srcs[0].Priority != 9 || srcs[0].Enabled {
		t.Errorf("after update: %+v", srcs)
	}
	ms, _ = s.SourceModels(id)
	if len(ms) != 1 || ms[0].Alias != "model-c" || ms[0].UpstreamModel != "model-c-up" {
		t.Errorf("models after replace = %+v; want only model-c", ms)
	}

	// Update of a missing source is an error, not a silent no-op.
	if err := s.UpdateSource(Source{ID: id + 100, Name: "ghost"}, nil); err == nil {
		t.Error("UpdateSource of missing id succeeded")
	}

	// Delete cascades the mappings away.
	if err := s.DeleteSource(id); err != nil {
		t.Fatalf("DeleteSource: %v", err)
	}
	if err := s.DeleteSource(id); err == nil {
		t.Error("DeleteSource of missing id succeeded")
	}
	srcs, _ = s.ListSources()
	if len(srcs) != 0 {
		t.Errorf("sources after delete = %+v; want none", srcs)
	}
	if ms, _ := s.SourceModels(id); len(ms) != 0 {
		t.Errorf("models after cascade delete = %+v; want none", ms)
	}
}

func TestNowRFC3339IsUTC(t *testing.T) {
	ts := nowRFC3339()
	parsed, err := time.Parse(time.RFC3339Nano, ts)
	if err != nil {
		t.Fatalf("nowRFC3339 %q unparseable: %v", ts, err)
	}
	if _, offset := parsed.Zone(); offset != 0 {
		t.Errorf("nowRFC3339 %q has offset %d; want UTC", ts, offset)
	}
}

func TestSlug(t *testing.T) {
	cases := map[string]string{
		"opencode":  "opencode",
		"My App!":   "my-app",
		"dev--test": "dev-test",
		"--":        "key",
		"":          "key",
		"A_B.C":     "a-b-c",
		"汉":         "key",
	}
	for in, want := range cases {
		if got := slug(in); got != want {
			t.Errorf("slug(%q) = %q; want %q", in, got, want)
		}
	}
}
