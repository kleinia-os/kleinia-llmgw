package ir

import (
	"encoding/json"
	"testing"
)

func TestPartJSONRoundtrip(t *testing.T) {
	p := Part{
		Type:         PartToolUse,
		ToolCallID:   "call_1",
		ToolName:     "get_weather",
		ToolArgsJSON: `{"city":"北京"}`,
	}
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	var q Part
	if err := json.Unmarshal(b, &q); err != nil {
		t.Fatal(err)
	}
	if q != p {
		t.Fatalf("roundtrip mismatch: %+v vs %+v", q, p)
	}
}

func TestUsageAdd(t *testing.T) {
	u := Usage{InputTokens: 10, OutputTokens: 5}
	u.Add(Usage{InputTokens: 1, OutputTokens: 2, CacheReadTokens: 3, ReasoningTokens: 4})
	want := Usage{InputTokens: 11, OutputTokens: 7, CacheReadTokens: 3, ReasoningTokens: 4}
	if u != want {
		t.Fatalf("Add mismatch: %+v vs %+v", u, want)
	}
	if got := u.Total(); got != 18 {
		t.Fatalf("Total = %d, want 18", got)
	}
}

func TestUpstreamErrorMessage(t *testing.T) {
	e := &UpstreamError{Status: 429, Kind: ErrRateLimited, Message: "slow down", Retryable: true}
	if e.Error() == "" {
		t.Fatal("empty error string")
	}
}
