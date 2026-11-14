package quota

import (
	"strings"
	"testing"
	"time"
)

func TestCheckUnlimited(t *testing.T) {
	v := Check(1_000_000, 1_000_000_000, 1_000_000_000, Limits{})
	if !v.Allowed || v.Reason != "" || v.RetryAfterSec != 0 {
		t.Errorf("no limits: %+v; want allowed", v)
	}
}

func TestCheckRPM(t *testing.T) {
	lim := Limits{RPM: ptr(10)}
	if v := Check(9, 0, 0, lim); !v.Allowed {
		t.Errorf("rpm 9 under limit 10 denied: %+v", v)
	}
	v := Check(10, 0, 0, lim)
	if v.Allowed {
		t.Fatalf("rpm 10 at limit 10 allowed: %+v", v)
	}
	if !strings.Contains(v.Reason, "10") {
		t.Errorf("reason %q lacks the numbers", v.Reason)
	}
	if v.RetryAfterSec < 1 || v.RetryAfterSec > 60 {
		t.Errorf("RetryAfterSec = %d; want 1..60 (next whole minute)", v.RetryAfterSec)
	}
	// The retry hint matches the distance to the next minute boundary.
	sec := time.Now().Second()
	want := 60 - sec
	if v.RetryAfterSec != want && v.RetryAfterSec != want%60+1 {
		t.Errorf("RetryAfterSec = %d; want %d (now.Second()=%d)", v.RetryAfterSec, want, sec)
	}
}

func TestCheckTPM(t *testing.T) {
	lim := Limits{TPM: ptr(1000)}
	if v := Check(0, 999, 0, lim); !v.Allowed {
		t.Errorf("tpm 999 under limit denied: %+v", v)
	}
	v := Check(0, 1000, 0, lim)
	if v.Allowed || !strings.Contains(v.Reason, "1000") {
		t.Errorf("tpm at limit = %+v; want denial with numbers", v)
	}
	if v.RetryAfterSec < 1 || v.RetryAfterSec > 60 {
		t.Errorf("RetryAfterSec = %d; want 1..60", v.RetryAfterSec)
	}
}

func TestCheckDailyTokens(t *testing.T) {
	lim := Limits{DailyTokens: ptr(5000)}
	if v := Check(0, 0, 4999, lim); !v.Allowed {
		t.Errorf("daily 4999 under limit denied: %+v", v)
	}
	v := Check(0, 0, 5000, lim)
	if v.Allowed || !strings.Contains(v.Reason, "5000") {
		t.Errorf("daily at limit = %+v; want denial with numbers", v)
	}
	// Ceiling until 00:00 UTC: at most a day, never zero.
	if v.RetryAfterSec < 1 || v.RetryAfterSec > 86400 {
		t.Errorf("RetryAfterSec = %d; want 1..86400", v.RetryAfterSec)
	}
}

func TestCheckOrderRPMFirst(t *testing.T) {
	lim := Limits{RPM: ptr(1), TPM: ptr(1), DailyTokens: ptr(1)}
	v := Check(1, 1, 1, lim)
	if v.Allowed || !strings.Contains(v.Reason, "rpm") {
		t.Errorf("all limits hit = %+v; want the rpm denial first", v)
	}
	// TPM wins once RPM has headroom.
	v = Check(0, 1, 1, lim)
	if v.Allowed || !strings.Contains(v.Reason, "tpm") {
		t.Errorf("rpm clear = %+v; want the tpm denial", v)
	}
	// Daily wins once the minute windows are clear.
	v = Check(0, 0, 1, lim)
	if v.Allowed || !strings.Contains(v.Reason, "daily") {
		t.Errorf("minute windows clear = %+v; want the daily denial", v)
	}
}

func TestLimitsWithNilFields(t *testing.T) {
	// Only daily set: high rpm/tpm must not deny.
	lim := Limits{DailyTokens: ptr(5)}
	if v := Check(10_000, 10_000_000, 4, lim); !v.Allowed {
		t.Errorf("unset limits denied: %+v", v)
	}
	if v := Check(0, 0, 5, lim); v.Allowed {
		t.Errorf("daily at limit allowed: %+v", v)
	}
}

func ptr(v int64) *int64 { return &v }
