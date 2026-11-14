// Package quota decides whether a key may make one more request given its
// live usage windows.
//
// Check is a pure decision function: the server feeds in the windows read
// from the usage ledger (store.KeyWindows) plus the key's limits, and turns
// the verdict into a 429 with Retry-After when denied. The wall clock is
// read only to compute RetryAfterSec.
package quota

import (
	"fmt"
	"time"
)

// Limits are a key's quota ceilings. A nil field means unlimited.
type Limits struct {
	RPM         *int64 // requests per rolling 60s
	TPM         *int64 // input+output tokens per rolling 60s
	DailyTokens *int64 // input+output tokens per UTC day
}

// Verdict is Check's decision. When Allowed is false, Reason is a
// human-readable explanation with the numbers involved and RetryAfterSec a
// positive hint for the Retry-After header.
type Verdict struct {
	Allowed       bool
	Reason        string
	RetryAfterSec int
}

// Check evaluates the live windows in order — RPM, then TPM, then daily
// tokens — and returns the first limit reached. Windows count the request
// being checked, so a key at limit-1 is still allowed its last request.
func Check(rpm, tpm, dailyTokens int64, lim Limits) Verdict {
	if lim.RPM != nil && rpm >= *lim.RPM {
		return Verdict{
			Allowed:       false,
			Reason:        fmt.Sprintf("rpm limit %d reached (%d requests in the last 60s)", *lim.RPM, rpm),
			RetryAfterSec: secondsToNextMinute(),
		}
	}
	if lim.TPM != nil && tpm >= *lim.TPM {
		return Verdict{
			Allowed:       false,
			Reason:        fmt.Sprintf("tpm limit %d reached (%d tokens in the last 60s)", *lim.TPM, tpm),
			RetryAfterSec: secondsToNextMinute(),
		}
	}
	if lim.DailyTokens != nil && dailyTokens >= *lim.DailyTokens {
		return Verdict{
			Allowed:       false,
			Reason:        fmt.Sprintf("daily token limit %d reached (%d tokens since 00:00 UTC)", *lim.DailyTokens, dailyTokens),
			RetryAfterSec: secondsToNextUTCMidnight(),
		}
	}
	return Verdict{Allowed: true}
}

// secondsToNextMinute returns 1..60: whole-minute boundaries are when a
// fresh minute-aligned retry hint expires.
func secondsToNextMinute() int {
	return 60 - time.Now().Second()
}

// secondsToNextUTCMidnight returns the seconds until 00:00 UTC (>= 1).
func secondsToNextUTCMidnight() int {
	now := time.Now().UTC()
	tomorrow := now.AddDate(0, 0, 1)
	midnight := time.Date(tomorrow.Year(), tomorrow.Month(), tomorrow.Day(), 0, 0, 0, 0, time.UTC)
	d := midnight.Sub(now)
	if d < time.Second {
		return 1
	}
	return int((d + time.Second - 1) / time.Second) // ceil
}
