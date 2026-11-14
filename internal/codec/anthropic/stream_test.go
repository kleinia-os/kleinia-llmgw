package anthropic

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"kleinia.io/llmgw/internal/ir"
)

// ---- shared test helpers ----

// feedGoldenStream feeds a golden SSE capture through a fresh parser and
// returns all IR events.
func feedGoldenStream(t *testing.T, name string) []ir.StreamEvent {
	t.Helper()
	data := readGolden(t, name)
	p := NewUpstreamStreamParser()
	var evs []ir.StreamEvent
	for _, line := range strings.Split(string(data), "\n") {
		got, err := p.Line(line)
		if err != nil {
			t.Fatalf("Line(%q): %v", line, err)
		}
		evs = append(evs, got...)
	}
	return evs
}

func countKind(evs []ir.StreamEvent, kind ir.StreamKind) int {
	n := 0
	for _, ev := range evs {
		if ev.Kind == kind {
			n++
		}
	}
	return n
}

func joinKind(evs []ir.StreamEvent, kind ir.StreamKind) string {
	var sb strings.Builder
	for _, ev := range evs {
		if ev.Kind == kind {
			sb.WriteString(ev.Text)
		}
	}
	return sb.String()
}

type sseEvent struct {
	name string
	data string
}

// parseSSE splits a recorded SSE body into its named events.
func parseSSE(t *testing.T, raw string) []sseEvent {
	t.Helper()
	var out []sseEvent
	for _, block := range strings.Split(raw, "\n\n") {
		block = strings.TrimSpace(block)
		if block == "" {
			continue
		}
		var ev sseEvent
		for _, line := range strings.Split(block, "\n") {
			if name, ok := strings.CutPrefix(line, "event: "); ok {
				ev.name = name
			}
			if data, ok := strings.CutPrefix(line, "data: "); ok {
				ev.data = data
			}
		}
		if ev.name == "" || ev.data == "" {
			t.Fatalf("malformed SSE block %q", block)
		}
		out = append(out, ev)
	}
	return out
}

// eventNames returns the event-name chain, optionally collapsing
// content_block_delta runs.
func eventNames(evs []sseEvent, collapseDeltas bool) []string {
	var names []string
	for _, ev := range evs {
		if collapseDeltas && ev.name == "content_block_delta" {
			continue
		}
		names = append(names, ev.name)
	}
	return names
}

func equalsSeq(got, want []string) bool {
	return strings.Join(got, "|") == strings.Join(want, "|")
}

// ---- upstream stream parser, driven by golden captures ----

func TestUpstreamStreamParserAnthStream(t *testing.T) {
	raw := string(readGolden(t, "anth_stream.txt"))
	evs := feedGoldenStream(t, "anth_stream.txt")

	if len(evs) == 0 || evs[0].Kind != ir.EvStart || evs[0].Model != "test-model" {
		t.Fatalf("first event = %+v, want EvStart with model", evs[0])
	}
	// every wire delta surfaces as an IR delta, none lost or invented
	if got, want := countKind(evs, ir.EvThinkingDelta), strings.Count(raw, `"type":"thinking_delta"`); got != want {
		t.Fatalf("thinking deltas = %d, want %d", got, want)
	}
	if got, want := countKind(evs, ir.EvTextDelta), strings.Count(raw, `"type":"text_delta"`); got != want {
		t.Fatalf("text deltas = %d, want %d", got, want)
	}
	if got := joinKind(evs, ir.EvTextDelta); got != "我是由TestLabs开发的AI助手TestBot，乐于用中文为你提供清晰、可靠的帮助。" {
		t.Fatalf("joined text = %q", got)
	}
	if n := countKind(evs, ir.EvToolStart); n != 0 {
		t.Fatalf("unexpected tool events: %d", n)
	}
	// message_start's input estimate (10) must NOT leak; message_delta is
	// authoritative: final input 34 (+0 cache read), output 100, reasoning 76.
	if n := countKind(evs, ir.EvUsage); n != 1 {
		t.Fatalf("usage events = %d, want 1", n)
	}
	var usage *ir.Usage
	for _, ev := range evs {
		if ev.Kind == ir.EvUsage {
			usage = &ev.Usage
		}
	}
	if usage == nil || *usage != (ir.Usage{InputTokens: 34, OutputTokens: 100, ReasoningTokens: 76}) {
		t.Fatalf("usage = %+v", usage)
	}
	if n := countKind(evs, ir.EvFinish); n != 1 {
		t.Fatalf("finish events = %d, want 1", n)
	}
	last := evs[len(evs)-1]
	if last.Kind != ir.EvFinish || last.Finish != ir.FinishStop {
		t.Fatalf("last event = %+v, want EvFinish stop", last)
	}
}

func TestUpstreamStreamParserAnthToolsStream(t *testing.T) {
	raw := string(readGolden(t, "anth_tools_stream.txt"))
	evs := feedGoldenStream(t, "anth_tools_stream.txt")

	if evs[0].Kind != ir.EvStart || evs[0].Model != "test-model" {
		t.Fatalf("first event = %+v", evs[0])
	}
	// tool block lifecycle
	var start, end *ir.StreamEvent
	var deltas int
	var json strings.Builder
	for i, ev := range evs {
		switch ev.Kind {
		case ir.EvToolStart:
			start = &evs[i]
		case ir.EvToolEnd:
			end = &evs[i]
		case ir.EvToolDelta:
			deltas++
			json.WriteString(ev.Text)
		}
	}
	if start == nil || start.ToolID != "call_00_v9ep899vk05z45vm6mqfwfi8" || start.ToolName != "get_weather" {
		t.Fatalf("tool start = %+v", start)
	}
	if end == nil || end.ToolID != start.ToolID {
		t.Fatalf("tool end = %+v", end)
	}
	if got, want := deltas, strings.Count(raw, `"type":"input_json_delta"`); got != want {
		t.Fatalf("tool deltas = %d, want %d", got, want)
	}
	if got := json.String(); got != `{"city": "北京"}` {
		t.Fatalf("joined arguments = %q", got)
	}
	if n := countKind(evs, ir.EvTextDelta); n != 0 {
		t.Fatalf("unexpected text deltas: %d", n)
	}
	// final usage: wire input 46 + cache read 256 = IR 302; output 53.
	var usage ir.Usage
	var finish ir.Finish
	for _, ev := range evs {
		if ev.Kind == ir.EvUsage {
			usage = ev.Usage
		}
		if ev.Kind == ir.EvFinish {
			finish = ev.Finish
		}
	}
	if usage != (ir.Usage{InputTokens: 302, OutputTokens: 53, CacheReadTokens: 256, ReasoningTokens: 14}) {
		t.Fatalf("usage = %+v", usage)
	}
	if finish != ir.FinishToolCalls {
		t.Fatalf("finish = %q", finish)
	}
	// tool_end must arrive after the last tool delta
	var lastDelta, endIdx int
	for i, ev := range evs {
		if ev.Kind == ir.EvToolDelta {
			lastDelta = i
		}
		if ev.Kind == ir.EvToolEnd {
			endIdx = i
		}
	}
	if endIdx < lastDelta {
		t.Fatal("EvToolEnd before final EvToolDelta")
	}
}

func TestUpstreamStreamParserFraming(t *testing.T) {
	p := NewUpstreamStreamParser()
	var evs []ir.StreamEvent
	feed := func(lines ...string) {
		t.Helper()
		for _, line := range lines {
			got, err := p.Line(line)
			if err != nil {
				t.Fatalf("Line(%q): %v", line, err)
			}
			evs = append(evs, got...)
		}
	}
	feed(
		": openai-ish comment",
		"",
		"event: message_start",
		`data: {"type":"message_start","message":{"model":"m","usage":{"input_tokens":5,"output_tokens":0}}}`,
		"",
		"event: content_block_start",
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"t0","name":"f","input":{}}}`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{}"}}`,
		`data: {"type":"content_block_stop","index":0}`,
		`data: {"type":"message_delta","usage":{"input_tokens":10,"cache_read_input_tokens":4,"output_tokens":7},"delta":{"stop_reason":"max_tokens"}}`,
		`data: {"type":"message_stop"}`,
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"}}`, // after stop: ignored
		`data: [DONE]`,
		"data: this is not json",
		"event: ping",
		`data: {"type":"ping"}`,
	)
	// start, tool_start, tool_delta, tool_end, usage, finish — and nothing
	// from the post-message_stop message_delta, [DONE] or the garbage lines.
	if len(evs) != 6 {
		t.Fatalf("events = %+v", evs)
	}
	if evs[0].Kind != ir.EvStart || evs[0].Model != "m" {
		t.Fatalf("ev0 = %+v", evs[0])
	}
	if evs[1].Kind != ir.EvToolStart || evs[1].ToolID != "t0" || evs[1].ToolName != "f" {
		t.Fatalf("ev1 = %+v", evs[1])
	}
	if evs[2].Kind != ir.EvToolDelta || evs[2].ToolID != "t0" || evs[2].Text != "{}" {
		t.Fatalf("ev2 = %+v", evs[2])
	}
	if evs[3].Kind != ir.EvToolEnd || evs[3].ToolID != "t0" {
		t.Fatalf("ev3 = %+v", evs[3])
	}
	if evs[4].Kind != ir.EvUsage || evs[4].Usage.InputTokens != 14 || evs[4].Usage.CacheReadTokens != 4 {
		t.Fatalf("ev4 = %+v", evs[4])
	}
	if evs[5].Kind != ir.EvFinish || evs[5].Finish != ir.FinishLength {
		t.Fatalf("ev5 = %+v", evs[5])
	}
	// message_delta without usage yields only the finish event
	q := NewUpstreamStreamParser()
	got, err := q.Line(`data: {"type":"message_delta","delta":{"stop_reason":"tool_use"}}`)
	if err != nil || len(got) != 1 || got[0].Kind != ir.EvFinish || got[0].Finish != ir.FinishToolCalls {
		t.Fatalf("usage-less message_delta = %+v (%v)", got, err)
	}
}

// ---- client stream encoder ----

func TestClientStreamEncoderLazyOpen(t *testing.T) {
	rec := httptest.NewRecorder()
	enc := NewClientStreamEncoder(rec)
	if rec.Body.Len() != 0 {
		t.Fatalf("body written before first Emit: %q", rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content-type = %q", ct)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-cache" {
		t.Fatalf("cache-control = %q", cc)
	}
	if err := enc.Emit(ir.StreamEvent{Kind: ir.EvStart, Model: "test-model"}); err != nil {
		t.Fatal(err)
	}
	evs := parseSSE(t, rec.Body.String())
	if len(evs) != 1 || evs[0].name != "message_start" {
		t.Fatalf("events = %+v", evs)
	}
	var start struct {
		Message struct {
			ID      string `json:"id"`
			Model   string `json:"model"`
			Role    string `json:"role"`
			Content []any  `json:"content"`
			Usage   struct {
				InputTokens  int64 `json:"input_tokens"`
				OutputTokens int64 `json:"output_tokens"`
			} `json:"usage"`
		} `json:"message"`
	}
	if err := json.Unmarshal([]byte(evs[0].data), &start); err != nil {
		t.Fatal(err)
	}
	m := start.Message
	if !strings.HasPrefix(m.ID, "msg_") || m.Model != "test-model" ||
		m.Role != "assistant" || len(m.Content) != 0 {
		t.Fatalf("message_start = %s", evs[0].data)
	}
	// input unknown at open time: 0, corrected by message_delta later
	if m.Usage.InputTokens != 0 || m.Usage.OutputTokens != 0 {
		t.Fatalf("message_start usage = %+v", m.Usage)
	}
}

func TestClientStreamEncoderBlocks(t *testing.T) {
	rec := httptest.NewRecorder()
	enc := NewClientStreamEncoder(rec)
	steps := []ir.StreamEvent{
		{Kind: ir.EvStart, Model: "m"},
		{Kind: ir.EvTextDelta, Text: "a"},
		{Kind: ir.EvTextDelta, Text: "b"},
		{Kind: ir.EvThinkingDelta, Text: "t"},
		{Kind: ir.EvToolStart, ToolID: "t0", ToolName: "f"},
		{Kind: ir.EvToolDelta, ToolID: "t0", Text: `{"x":`},
		{Kind: ir.EvToolEnd, ToolID: "t0"},
		{Kind: ir.EvTextDelta, Text: "c"},
		{Kind: ir.EvUsage, Usage: ir.Usage{InputTokens: 302, OutputTokens: 53, CacheReadTokens: 256}},
		{Kind: ir.EvFinish, Finish: ir.FinishToolCalls},
	}
	for _, ev := range steps {
		if err := enc.Emit(ev); err != nil {
			t.Fatalf("Emit(%v): %v", ev, err)
		}
	}
	if err := enc.Close(ir.FinishToolCalls); err != nil {
		t.Fatal(err)
	}
	evs := parseSSE(t, rec.Body.String())

	wantSkeleton := []string{
		"message_start",
		"content_block_start", "content_block_stop",
		"content_block_start", "content_block_stop",
		"content_block_start", "content_block_stop",
		"content_block_start", "content_block_stop",
		"message_delta", "message_stop",
	}
	if got := eventNames(evs, true); !equalsSeq(got, wantSkeleton) {
		t.Fatalf("event chain = %v\nwant %v", got, wantSkeleton)
	}

	// block starts carry the right type, index and empty seed content
	type blockStart struct {
		Index        int `json:"index"`
		ContentBlock struct {
			Type string `json:"type"`
			ID   string `json:"id"`
			Name string `json:"name"`
			Text string `json:"text"`
			_    struct{}
		} `json:"content_block"`
	}
	var starts []blockStart
	for _, ev := range evs {
		if ev.name == "content_block_start" {
			var bs blockStart
			if err := json.Unmarshal([]byte(ev.data), &bs); err != nil {
				t.Fatal(err)
			}
			starts = append(starts, bs)
		}
	}
	if len(starts) != 4 {
		t.Fatalf("block starts = %+v", starts)
	}
	if starts[0].ContentBlock.Type != "text" || starts[0].Index != 0 || starts[0].ContentBlock.Text != "" {
		t.Fatalf("block 0 = %+v", starts[0])
	}
	if starts[1].ContentBlock.Type != "thinking" || starts[1].Index != 1 {
		t.Fatalf("block 1 = %+v", starts[1])
	}
	if starts[2].ContentBlock.Type != "tool_use" || starts[2].ContentBlock.ID != "t0" ||
		starts[2].ContentBlock.Name != "f" {
		t.Fatalf("block 2 = %+v", starts[2])
	}
	if starts[3].ContentBlock.Type != "text" || starts[3].Index != 3 {
		t.Fatalf("block 3 = %+v", starts[3])
	}

	// deltas address the right block index and kind
	type delta struct {
		Index int `json:"index"`
		Delta struct {
			Type        string `json:"type"`
			Text        string `json:"text"`
			Thinking    string `json:"thinking"`
			PartialJSON string `json:"partial_json"`
		} `json:"delta"`
	}
	var ds []delta
	for _, ev := range evs {
		if ev.name == "content_block_delta" {
			var d delta
			if err := json.Unmarshal([]byte(ev.data), &d); err != nil {
				t.Fatal(err)
			}
			ds = append(ds, d)
		}
	}
	if len(ds) != 5 {
		t.Fatalf("deltas = %+v", ds)
	}
	want := []struct {
		index int
		typ   string
		text  string
	}{
		{0, "text_delta", "a"},
		{0, "text_delta", "b"},
		{1, "thinking_delta", "t"},
		{2, "input_json_delta", `{"x":`},
		{3, "text_delta", "c"},
	}
	for i, w := range want {
		if ds[i].Index != w.index || ds[i].Delta.Type != w.typ {
			t.Fatalf("delta %d = %+v, want index %d %s", i, ds[i], w.index, w.typ)
		}
		switch w.typ {
		case "text_delta":
			if ds[i].Delta.Text != w.text {
				t.Fatalf("delta %d text = %q", i, ds[i].Delta.Text)
			}
		case "thinking_delta":
			if ds[i].Delta.Thinking != w.text {
				t.Fatalf("delta %d thinking = %q", i, ds[i].Delta.Thinking)
			}
		case "input_json_delta":
			if ds[i].Delta.PartialJSON != w.text {
				t.Fatalf("delta %d json = %q", i, ds[i].Delta.PartialJSON)
			}
		}
	}

	// message_delta: usage converted (302 IR input - 256 cache read = 46),
	// stop_reason mapped, stop_sequence null; exactly one message_stop at the end.
	var md struct {
		Usage struct {
			InputTokens          int64 `json:"input_tokens"`
			CacheReadInputTokens int64 `json:"cache_read_input_tokens"`
			OutputTokens         int64 `json:"output_tokens"`
		} `json:"usage"`
		Delta struct {
			StopReason   string  `json:"stop_reason"`
			StopSequence *string `json:"stop_sequence"`
		} `json:"delta"`
	}
	for _, ev := range evs {
		if ev.name == "message_delta" {
			if err := json.Unmarshal([]byte(ev.data), &md); err != nil {
				t.Fatal(err)
			}
		}
	}
	if md.Usage.InputTokens != 46 || md.Usage.CacheReadInputTokens != 256 || md.Usage.OutputTokens != 53 {
		t.Fatalf("message_delta usage = %+v", md.Usage)
	}
	if md.Delta.StopReason != "tool_use" || md.Delta.StopSequence != nil {
		t.Fatalf("message_delta stop = %+v", md.Delta)
	}
	if last := evs[len(evs)-1]; last.name != "message_stop" {
		t.Fatalf("last event = %+v", last)
	}
}

func TestClientStreamEncoderCloseOnly(t *testing.T) {
	rec := httptest.NewRecorder()
	enc := NewClientStreamEncoder(rec)
	if err := enc.Close(ir.FinishStop); err != nil {
		t.Fatal(err)
	}
	evs := parseSSE(t, rec.Body.String())
	want := []string{"message_start", "message_delta", "message_stop"}
	if got := eventNames(evs, false); !equalsSeq(got, want) {
		t.Fatalf("event chain = %v, want %v", got, want)
	}
	var md struct {
		Usage struct {
			InputTokens  int64 `json:"input_tokens"`
			OutputTokens int64 `json:"output_tokens"`
		} `json:"usage"`
		Delta struct {
			StopReason string `json:"stop_reason"`
		} `json:"delta"`
	}
	if err := json.Unmarshal([]byte(evs[1].data), &md); err != nil {
		t.Fatal(err)
	}
	if md.Usage.InputTokens != 0 || md.Usage.OutputTokens != 0 || md.Delta.StopReason != "end_turn" {
		t.Fatalf("message_delta = %+v", md)
	}
}

func TestClientStreamEncoderError(t *testing.T) {
	rec := httptest.NewRecorder()
	enc := NewClientStreamEncoder(rec)
	if err := enc.Emit(ir.StreamEvent{Kind: ir.EvError, ErrCode: ir.ErrInvalidKey, ErrMsg: "bad key", HTTPStatus: 401}); err != nil {
		t.Fatal(err)
	}
	if err := enc.Close(ir.FinishStop); err != nil {
		t.Fatal(err)
	}
	evs := parseSSE(t, rec.Body.String())
	want := []string{"message_start", "error", "message_stop"}
	if got := eventNames(evs, false); !equalsSeq(got, want) {
		t.Fatalf("event chain = %v, want %v (no message_delta after error)", got, want)
	}
	var env struct {
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(evs[1].data), &env); err != nil {
		t.Fatal(err)
	}
	if env.Error.Type != "authentication_error" || env.Error.Message != "bad key" {
		t.Fatalf("error event = %+v", env)
	}
}

func TestClientStreamEncoderOrphanToolFragments(t *testing.T) {
	rec := httptest.NewRecorder()
	enc := NewClientStreamEncoder(rec)
	// deltas with no matching open block are dropped, not mangled
	if err := enc.Emit(ir.StreamEvent{Kind: ir.EvToolDelta, ToolID: "ghost", Text: "{}"}); err != nil {
		t.Fatal(err)
	}
	if err := enc.Emit(ir.StreamEvent{Kind: ir.EvToolEnd, ToolID: "ghost"}); err != nil {
		t.Fatal(err)
	}
	if err := enc.Close(ir.FinishStop); err != nil {
		t.Fatal(err)
	}
	evs := parseSSE(t, rec.Body.String())
	want := []string{"message_start", "message_delta", "message_stop"}
	if got := eventNames(evs, false); !equalsSeq(got, want) {
		t.Fatalf("event chain = %v, want %v", got, want)
	}
}

// ---- full pipeline: golden upstream SSE -> parser -> IR -> encoder ----

func TestStreamPipelineGolden(t *testing.T) {
	tests := []struct {
		golden        string
		wantText      string // joined text deltas ("" for the tools stream)
		wantDeltas    int    // total content_block_delta events on the wire
		wantUsageIn   int64
		wantCacheRead int64
		wantUsageOut  int64
		wantStop      string
	}{
		{
			golden:       "anth_stream.txt",
			wantText:     "我是由TestLabs开发的AI助手TestBot，乐于用中文为你提供清晰、可靠的帮助。",
			wantDeltas:   strings.Count(string(readGolden(t, "anth_stream.txt")), `"type":"content_block_delta"`),
			wantUsageIn:  34,
			wantUsageOut: 100,
			wantStop:     "end_turn",
		},
		{
			golden:        "anth_tools_stream.txt",
			wantDeltas:    strings.Count(string(readGolden(t, "anth_tools_stream.txt")), `"type":"content_block_delta"`),
			wantUsageIn:   46,
			wantCacheRead: 256,
			wantUsageOut:  53,
			wantStop:      "tool_use",
		},
	}
	for _, tt := range tests {
		t.Run(tt.golden, func(t *testing.T) {
			raw := string(readGolden(t, tt.golden))
			evs := feedGoldenStream(t, tt.golden)
			if len(evs) == 0 {
				t.Fatal("parser produced no events")
			}

			rec := httptest.NewRecorder()
			enc := NewClientStreamEncoder(rec)
			for _, ev := range evs {
				if err := enc.Emit(ev); err != nil {
					t.Fatalf("Emit(%+v): %v", ev, err)
				}
			}
			finish := ir.FinishStop
			for _, ev := range evs {
				if ev.Kind == ir.EvFinish {
					finish = ev.Finish
				}
			}
			if err := enc.Close(finish); err != nil {
				t.Fatal(err)
			}

			out := parseSSE(t, rec.Body.String())
			// the re-encoded chain must mirror the upstream skeleton exactly:
			// message_start, per-block start/stop pairs, message_delta, message_stop
			starts := strings.Count(raw, `"type":"content_block_start"`)
			want := []string{"message_start"}
			for i := 0; i < starts; i++ {
				want = append(want, "content_block_start", "content_block_stop")
			}
			want = append(want, "message_delta", "message_stop")
			if got := eventNames(out, true); !equalsSeq(got, want) {
				t.Fatalf("event chain = %v\nwant %v", got, want)
			}

			deltas := 0
			var md struct {
				Usage struct {
					InputTokens          int64 `json:"input_tokens"`
					CacheReadInputTokens int64 `json:"cache_read_input_tokens"`
					OutputTokens         int64 `json:"output_tokens"`
				} `json:"usage"`
				Delta struct {
					StopReason string `json:"stop_reason"`
				} `json:"delta"`
			}
			for _, ev := range out {
				if ev.name == "content_block_delta" {
					deltas++
				}
				if ev.name == "message_delta" {
					if err := json.Unmarshal([]byte(ev.data), &md); err != nil {
						t.Fatal(err)
					}
				}
			}
			if deltas != tt.wantDeltas {
				t.Fatalf("delta events = %d, want %d", deltas, tt.wantDeltas)
			}
			if md.Usage.InputTokens != tt.wantUsageIn || md.Usage.CacheReadInputTokens != tt.wantCacheRead ||
				md.Usage.OutputTokens != tt.wantUsageOut {
				t.Fatalf("message_delta usage = %+v (want in=%d read=%d out=%d)",
					md.Usage, tt.wantUsageIn, tt.wantCacheRead, tt.wantUsageOut)
			}
			if md.Delta.StopReason != tt.wantStop {
				t.Fatalf("stop_reason = %q, want %q", md.Delta.StopReason, tt.wantStop)
			}
			if tt.wantText != "" {
				if got := joinKind(evs, ir.EvTextDelta); got != tt.wantText {
					t.Fatalf("text = %q", got)
				}
			}
		})
	}
}
