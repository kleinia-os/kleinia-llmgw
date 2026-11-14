package store

import (
	"database/sql"
	"errors"
	"regexp"
	"testing"
	"time"
)

func i64(v int64) *int64 { return &v }

func TestAddKeyGeneratesSecret(t *testing.T) {
	s := mustOpen(t)

	k, err := s.AddKey("opencode", "", i64(60), nil, i64(1_000_000))
	if err != nil {
		t.Fatalf("AddKey: %v", err)
	}
	if k.ID == 0 || !k.Enabled {
		t.Errorf("fresh key = %+v; want nonzero ID and enabled", k)
	}
	if k.Models != "*" {
		t.Errorf("Models = %q; want * default", k.Models)
	}
	if k.CreatedAt == "" || k.RevokedAt != "" {
		t.Errorf("CreatedAt=%q RevokedAt=%q; want timestamp and empty revoke", k.CreatedAt, k.RevokedAt)
	}
	re := regexp.MustCompile(`^sk-kln-opencode-[0-9a-f]{32}$`)
	if !re.MatchString(k.Key) {
		t.Errorf("key %q does not match sk-kln-opencode-<32hex>", k.Key)
	}
	if k.RPM == nil || *k.RPM != 60 || k.TPM != nil || k.DailyTokens == nil || *k.DailyTokens != 1_000_000 {
		t.Errorf("limits round-trip mismatch: %+v", k)
	}

	k2, err := s.AddKey("My App!", "*", nil, nil, nil)
	if err != nil {
		t.Fatalf("AddKey #2: %v", err)
	}
	re2 := regexp.MustCompile(`^sk-kln-my-app-[0-9a-f]{32}$`)
	if !re2.MatchString(k2.Key) {
		t.Errorf("key %q does not match sk-kln-my-app-<32hex>", k2.Key)
	}
	if k2.Key == k.Key {
		t.Error("two keys share a secret; entropy missing")
	}

	if _, err := s.AddKey("opencode", "*", nil, nil, nil); err == nil {
		t.Fatal("duplicate key name accepted")
	}
}

func TestFindKeyAndRevoke(t *testing.T) {
	s := mustOpen(t)
	k, err := s.AddKey("opencode", "model-a,test-model", i64(60), i64(500_000), nil)
	if err != nil {
		t.Fatalf("AddKey: %v", err)
	}

	got, err := s.FindKey(k.Key)
	if err != nil {
		t.Fatalf("FindKey: %v", err)
	}
	if got.ID != k.ID || got.Name != "opencode" || got.Models != "model-a,test-model" {
		t.Errorf("FindKey = %+v; want %+v", got, k)
	}
	if got.RPM == nil || *got.RPM != 60 || got.TPM == nil || *got.TPM != 500_000 || got.DailyTokens != nil {
		t.Errorf("FindKey limits = rpm %v tpm %v daily %v; want 60/500000/nil", got.RPM, got.TPM, got.DailyTokens)
	}

	if _, err := s.FindKey("sk-kln-nope-" + "0123456789abcdef0123456789abcdef"); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("FindKey(unknown) err = %v; want sql.ErrNoRows", err)
	}

	if err := s.RevokeKey("opencode"); err != nil {
		t.Fatalf("RevokeKey: %v", err)
	}
	if _, err := s.FindKey(k.Key); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("FindKey after revoke err = %v; want sql.ErrNoRows (enabled=1 filter)", err)
	}
	ks, err := s.ListKeys()
	if err != nil {
		t.Fatalf("ListKeys: %v", err)
	}
	if len(ks) != 1 || ks[0].Enabled || ks[0].RevokedAt == "" {
		t.Errorf("ListKeys after revoke = %+v; want disabled with revoked_at set", ks)
	}
	if err := s.RevokeKey("ghost"); err == nil {
		t.Error("RevokeKey of unknown name succeeded")
	}
}

func TestSuspendAndAidBinding(t *testing.T) {
	s := mustOpen(t)
	zero, one := int64(0), int64(1)
	k, err := s.AddKey("agentdemo", "*", &zero, &zero, &one)
	if err != nil {
		t.Fatalf("AddKey: %v", err)
	}

	// Bind a kernel agent identity; lookup by AID resolves the same limits.
	if err := s.BindKeyAID("agentdemo", "aid_12345678"); err != nil {
		t.Fatalf("BindKeyAID: %v", err)
	}
	got, err := s.FindKeyByAID("aid_12345678")
	if err != nil {
		t.Fatalf("FindKeyByAID: %v", err)
	}
	if got.ID != k.ID || got.AID != "aid_12345678" || got.DailyTokens == nil || *got.DailyTokens != 1 {
		t.Errorf("FindKeyByAID = %+v; want same row bound to aid", got)
	}
	if _, err := s.FindKeyByAID("aid_nope"); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("FindKeyByAID(unknown) err = %v; want sql.ErrNoRows", err)
	}

	// Suspension: FindKey by secret and by AID both refuse a frozen key.
	if err := s.SetKeySuspended("agentdemo", true); err != nil {
		t.Fatalf("SetKeySuspended(true): %v", err)
	}
	if _, err := s.FindKey(k.Key); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("FindKey while suspended err = %v; want sql.ErrNoRows", err)
	}
	if _, err := s.FindKeyByAID("aid_12345678"); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("FindKeyByAID while suspended err = %v; want sql.ErrNoRows", err)
	}
	ks, _ := s.ListKeys()
	if len(ks) != 1 || !ks[0].Enabled || !ks[0].Suspended {
		t.Errorf("ListKeys while suspended = %+v; want enabled=true suspended=true", ks)
	}
	if err := s.SetKeySuspended("agentdemo", false); err != nil {
		t.Fatalf("SetKeySuspended(false): %v", err)
	}
	if _, err := s.FindKey(k.Key); err != nil {
		t.Errorf("FindKey after resume: %v", err)
	}

	// Revoked keys cannot be suspended (SetKeySuspended guards enabled=1).
	if err := s.RevokeKey("agentdemo"); err != nil {
		t.Fatalf("RevokeKey: %v", err)
	}
	if err := s.SetKeySuspended("agentdemo", true); err == nil {
		t.Error("SetKeySuspended on revoked key succeeded")
	}

	// AidWindows derives windows from attributed usage rows.
	if err := s.InsertUsage(UsageRow{TS: time.Now().UTC().Format(time.RFC3339Nano),
		AID: "aid_12345678", In: 40, Out: 10}); err != nil {
		t.Fatalf("InsertUsage: %v", err)
	}
	rpm, tpm, daily, err := s.AidWindows("aid_12345678")
	if err != nil || rpm != 1 || tpm != 50 || daily != 50 {
		t.Errorf("AidWindows = %d,%d,%d err=%v; want 1,50,50", rpm, tpm, daily, err)
	}
}
