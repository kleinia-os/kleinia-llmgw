//go:build live

// Live integration tests: they start a real llmgwd on a scratch database and
// call a real upstream. Gated behind the `live` build tag AND LLMGW_LIVE=1 so
// `go test ./...` stays offline-safe.
//
// Required env (no defaults — the tests are skipped when any is unset):
// LLMGW_TEST_KEY (upstream API key), LLMGW_TEST_BASE (upstream base URL),
// LLMGW_TEST_MODEL (upstream model id). Optional: LLMGW_BIN_DIR (default ../bin).
package test

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"testing"
	"time"
)

var (
	base   = os.Getenv("LLMGW_TEST_BASE")
	apiKey = os.Getenv("LLMGW_TEST_KEY")
	model  = os.Getenv("LLMGW_TEST_MODEL")
	gwAddr = "127.0.0.1:18484"
	gwURL  = "http://" + gwAddr
	daemon *exec.Cmd
	gwKey  string
)

func envOr(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

func TestMain(m *testing.M) {
	if os.Getenv("LLMGW_LIVE") != "1" {
		fmt.Println("LLMGW_LIVE != 1; skipping live tests")
		os.Exit(0)
	}
	if apiKey == "" {
		fmt.Println("LLMGW_TEST_KEY empty; skipping live tests")
		os.Exit(0)
	}
	if base == "" || model == "" {
		fmt.Println("LLMGW_TEST_BASE / LLMGW_TEST_MODEL not set; skipping live tests")
		os.Exit(0)
	}
	binDir := envOr("LLMGW_BIN_DIR", "../bin")
	tmp, _ := os.MkdirTemp("", "llmgw-live-*")
	dbPath := tmp + "/state.db"

	if err := run(binDir+"/llmgwctl", "--db", dbPath, "source", "add", "primary",
		"--type", "openai", "--base", base, "--key", apiKey,
		"--model", model); err != nil {
		fmt.Println("source add:", err)
		os.Exit(1)
	}
	// Same upstream through the Responses transport: exercises
	// chat→responses and responses→responses paths under alias dsr-flash.
	if err := run(binDir+"/llmgwctl", "--db", dbPath, "source", "add", "primary-resp",
		"--type", "openai-responses", "--base", base, "--key", apiKey,
		"--model", "dsr-flash="+model); err != nil {
		fmt.Println("source add (responses):", err)
		os.Exit(1)
	}
	out, err := runOut(binDir+"/llmgwctl", "--db", dbPath, "key", "add", "livetest")
	if err != nil {
		fmt.Println("key add:", err)
		os.Exit(1)
	}
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "sk-kln-") {
			gwKey = line
		}
	}
	if gwKey == "" {
		fmt.Println("no gateway key parsed from llmgwctl output:", out)
		os.Exit(1)
	}

	daemon = exec.Command(binDir+"/llmgwd", "--addr", gwAddr, "--db", dbPath)
	daemon.Stdout = io.Discard
	daemon.Stderr = os.Stderr
	if err := daemon.Start(); err != nil {
		fmt.Println("start llmgwd:", err)
		os.Exit(1)
	}
	if !waitUp(gwURL+"/healthz", 10*time.Second) {
		fmt.Println("llmgwd did not come up")
		daemon.Process.Kill()
		os.Exit(1)
	}
	code := m.Run()
	daemon.Process.Kill()
	daemon.Wait()
	os.RemoveAll(tmp)
	os.Exit(code)
}

func run(argv ...string) error { _, err := runOut(argv...); return err }
func runOut(argv ...string) (string, error) {
	out, err := exec.Command(argv[0], argv[1:]...).CombinedOutput()
	if err != nil {
		err = fmt.Errorf("%v: %s", err, out)
	}
	return string(out), err
}

func waitUp(url string, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		resp, err := http.Get(url)
		if err == nil {
			resp.Body.Close()
			return true
		}
		time.Sleep(100 * time.Millisecond)
	}
	return false
}

func gwDo(t *testing.T, method, path string, body any) (*http.Response, []byte) {
	t.Helper()
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, gwURL+path, rdr)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+gwKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", gwKey) // anthropic endpoint accepts either
	req.Header.Set("anthropic-version", "2023-06-01")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, b
}

func TestModels(t *testing.T) {
	resp, body := gwDo(t, "GET", "/v1/models", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), model) {
		t.Fatalf("models list missing %s: %s", model, body)
	}
}

func TestChatNonStream(t *testing.T) {
	resp, body := gwDo(t, "POST", "/v1/chat/completions", map[string]any{
		"model": model, "max_tokens": 256, "temperature": 0,
		"messages": []map[string]string{{"role": "user", "content": "只回复两个字：成功"}},
	})
	if resp.StatusCode != 200 {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
	var r struct {
		Choices []struct {
			Message struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		t.Fatalf("bad json: %v: %s", err, body)
	}
	if len(r.Choices) != 1 {
		t.Fatalf("no choices: %s", body)
	}
	// reasoning models can eat the whole budget: empty content is only
	// acceptable when the model was truncated.
	if r.Choices[0].Message.Content == "" && r.Choices[0].FinishReason != "length" {
		t.Fatalf("no content and not truncated: %s", body)
	}
	if r.Usage.PromptTokens == 0 || r.Usage.CompletionTokens == 0 {
		t.Fatalf("usage not reported: %s", body)
	}
}

func TestChatStream(t *testing.T) {
	req, _ := http.NewRequest("POST", gwURL+"/v1/chat/completions",
		strings.NewReader(`{"model":"`+model+`","stream":true,"max_tokens":64,"messages":[{"role":"user","content":"数到三"}]}`))
	req.Header.Set("Authorization", "Bearer "+gwKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("status %d: %s", resp.StatusCode, b)
	}
	sawDelta, sawDone, sawUsage := false, false, false
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "data: ") {
			payload := strings.TrimPrefix(line, "data: ")
			if payload == "[DONE]" {
				sawDone = true
				break
			}
			if strings.Contains(payload, `"delta"`) {
				sawDelta = true
			}
			if strings.Contains(payload, `"usage"`) {
				sawUsage = true
			}
		}
	}
	if !sawDelta || !sawDone {
		t.Fatalf("stream incomplete: delta=%v done=%v", sawDelta, sawDone)
	}
	_ = sawUsage // usage optional mid-stream for chat
}

var weatherTool = map[string]any{
	"type": "function",
	"function": map[string]any{
		"name":        "get_weather",
		"description": "查询城市天气",
		"parameters": map[string]any{
			"type":       "object",
			"properties": map[string]any{"city": map[string]string{"type": "string"}},
			"required":   []string{"city"},
		},
	},
}

func TestChatTools(t *testing.T) {
	resp, body := gwDo(t, "POST", "/v1/chat/completions", map[string]any{
		"model": model, "max_tokens": 128, "temperature": 0,
		"tools":    []any{weatherTool},
		"messages": []map[string]string{{"role": "user", "content": "北京今天天气如何？"}},
	})
	if resp.StatusCode != 200 {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
	var r struct {
		Choices []struct {
			Message struct {
				ToolCalls []struct {
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		t.Fatalf("bad json: %v: %s", err, body)
	}
	if len(r.Choices) == 0 || len(r.Choices[0].Message.ToolCalls) == 0 {
		t.Fatalf("no tool call: %s", body)
	}
	if r.Choices[0].Message.ToolCalls[0].Function.Name != "get_weather" {
		t.Fatalf("wrong tool: %s", body)
	}
}

func TestResponsesNonStream(t *testing.T) {
	resp, body := gwDo(t, "POST", "/v1/responses", map[string]any{
		"model": model, "max_output_tokens": 256,
		"input": []map[string]any{{
			"role": "user",
			"content": []map[string]any{
				{"type": "input_text", "text": "只回复两个字：成功"},
			},
		}},
	})
	if resp.StatusCode != 200 {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
	var r struct {
		Output []struct {
			Type    string `json:"type"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"output"`
		Usage struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		t.Fatalf("bad json: %v: %s", err, body)
	}
	var text string
	for _, o := range r.Output {
		for _, c := range o.Content {
			text += c.Text
		}
	}
	if text == "" {
		t.Fatalf("no output text: %s", body)
	}
	if r.Usage.InputTokens == 0 {
		t.Fatalf("no usage: %s", body)
	}
}

func TestResponsesStream(t *testing.T) {
	req, _ := http.NewRequest("POST", gwURL+"/v1/responses",
		strings.NewReader(`{"model":"`+model+`","stream":true,"max_output_tokens":64,"input":"从一数到三"}`))
	req.Header.Set("Authorization", "Bearer "+gwKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("status %d: %s", resp.StatusCode, b)
	}
	sawDelta, sawCompleted := false, false
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		line := sc.Text()
		if strings.Contains(line, "response.output_text.delta") {
			sawDelta = true
		}
		if strings.Contains(line, "response.completed") {
			sawCompleted = true
		}
	}
	if !sawDelta || !sawCompleted {
		t.Fatalf("responses stream incomplete: delta=%v completed=%v", sawDelta, sawCompleted)
	}
}

func TestAnthropicNonStream(t *testing.T) {
	resp, body := gwDo(t, "POST", "/v1/messages", map[string]any{
		"model": model, "max_tokens": 256,
		"system":   "你只回复四个字：收到指令",
		"messages": []map[string]string{{"role": "user", "content": "ping"}},
	})
	if resp.StatusCode != 200 {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
	var r struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		StopReason string `json:"stop_reason"`
		Usage      struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		t.Fatalf("bad json: %v: %s", err, body)
	}
	if len(r.Content) == 0 {
		t.Fatalf("no content blocks: %s", body)
	}
	hasText := false
	for _, c := range r.Content {
		if c.Type == "text" && c.Text != "" {
			hasText = true
		}
	}
	// thinking-only output is acceptable only when truncated (reasoning ate
	// the token budget), mirroring upstream behavior.
	if !hasText && r.StopReason != "max_tokens" {
		t.Fatalf("no text block and not truncated (stop_reason=%s): %s", r.StopReason, body)
	}
	if r.Usage.InputTokens == 0 || r.Usage.OutputTokens == 0 {
		t.Fatalf("no usage: %s", body)
	}
}

func TestAnthropicStream(t *testing.T) {
	req, _ := http.NewRequest("POST", gwURL+"/v1/messages",
		strings.NewReader(`{"model":"`+model+`","stream":true,"max_tokens":64,"messages":[{"role":"user","content":"数到三"}]}`))
	req.Header.Set("x-api-key", gwKey)
	req.Header.Set("anthropic-version", "2023-06-01")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("status %d: %s", resp.StatusCode, b)
	}
	saw := map[string]bool{}
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		line := sc.Text()
		for _, ev := range []string{"message_start", "content_block_start", "content_block_delta",
			"content_block_stop", "message_delta", "message_stop"} {
			if strings.Contains(line, `"type":"`+ev+`"`) {
				saw[ev] = true
			}
		}
	}
	for _, ev := range []string{"message_start", "content_block_start", "content_block_delta",
		"content_block_stop", "message_delta", "message_stop"} {
		if !saw[ev] {
			t.Fatalf("anthropic stream missing %s event: %v", ev, saw)
		}
	}
}

func TestAnthropicTools(t *testing.T) {
	resp, body := gwDo(t, "POST", "/v1/messages", map[string]any{
		"model": model, "max_tokens": 128,
		"tools": []any{map[string]any{
			"name":        "get_weather",
			"description": "查询城市天气",
			"input_schema": map[string]any{
				"type":       "object",
				"properties": map[string]any{"city": map[string]string{"type": "string"}},
				"required":   []string{"city"},
			},
		}},
		"messages": []map[string]string{{"role": "user", "content": "北京今天天气如何？"}},
	})
	if resp.StatusCode != 200 {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
	var r struct {
		Content []struct {
			Type  string          `json:"type"`
			Name  string          `json:"name"`
			Input json.RawMessage `json:"input"`
		} `json:"content"`
		StopReason string `json:"stop_reason"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		t.Fatalf("bad json: %v: %s", err, body)
	}
	found := false
	for _, c := range r.Content {
		if c.Type == "tool_use" && c.Name == "get_weather" {
			found = true
		}
	}
	if !found {
		t.Fatalf("no tool_use block: %s", body)
	}
}

func TestAuth(t *testing.T) {
	for _, tc := range []struct{ name, auth string }{
		{"missing", ""}, {"bad", "Bearer sk-kln-nope-0000"},
	} {
		req, _ := http.NewRequest("POST", gwURL+"/v1/chat/completions",
			strings.NewReader(`{"model":"`+model+`","messages":[{"role":"user","content":"x"}]}`))
		if tc.auth != "" {
			req.Header.Set("Authorization", tc.auth)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 401 {
			t.Fatalf("%s: status %d (want 401): %s", tc.name, resp.StatusCode, body)
		}
	}
}

func TestUnknownModel(t *testing.T) {
	resp, body := gwDo(t, "POST", "/v1/chat/completions", map[string]any{
		"model": "no-model", "messages": []map[string]string{{"role": "user", "content": "x"}},
	})
	if resp.StatusCode != 404 {
		t.Fatalf("status %d (want 404): %s", resp.StatusCode, body)
	}
}

// TestReqIDHeaderLive pins the x-kln-req-id contract on the real wire:
// present and well-formed on metered responses (stream and non-stream),
// absent on unmetered rejections.
func TestReqIDHeaderLive(t *testing.T) {
	re := regexp.MustCompile(`^req_[0-9a-f]{16}$`)

	resp, body := gwDo(t, "POST", "/v1/chat/completions", map[string]any{
		"model": model, "max_tokens": 16,
		"messages": []map[string]string{{"role": "user", "content": "ping"}},
	})
	if resp.StatusCode != 200 {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
	if id := resp.Header.Get("X-Kln-Req-Id"); !re.MatchString(id) {
		t.Fatalf("x-kln-req-id %q does not match req_<16hex>", id)
	}

	req, _ := http.NewRequest("POST", gwURL+"/v1/chat/completions",
		strings.NewReader(`{"model":"`+model+`","stream":true,"max_tokens":16,"messages":[{"role":"user","content":"ping"}]}`))
	req.Header.Set("Authorization", "Bearer "+gwKey)
	sresp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer sresp.Body.Close()
	// streaming: the id must ride the headers before the first SSE byte
	if id := sresp.Header.Get("X-Kln-Req-Id"); !re.MatchString(id) {
		t.Fatalf("stream x-kln-req-id %q does not match req_<16hex>", id)
	}
	io.Copy(io.Discard, sresp.Body)

	// unmetered rejection (unknown model writes no usage row): no id
	resp2, body2 := gwDo(t, "POST", "/v1/chat/completions", map[string]any{
		"model": "no-model", "messages": []map[string]string{{"role": "user", "content": "x"}},
	})
	if resp2.StatusCode != 404 {
		t.Fatalf("status %d (want 404): %s", resp2.StatusCode, body2)
	}
	if h := resp2.Header.Get("X-Kln-Req-Id"); h != "" {
		t.Fatalf("unknown-model rejection carried x-kln-req-id %q — must stay absent when no usage row exists", h)
	}
}

// TestResponsesUpstreamChatClient drives the OpenAI Responses transport
// upstream through a chat-completions client (cross-format translation) and
// a responses client (same-format) under the dsr-flash alias.
func TestResponsesUpstreamChatClient(t *testing.T) {
	resp, body := gwDo(t, "POST", "/v1/chat/completions", map[string]any{
		"model": "dsr-flash", "max_tokens": 256, "temperature": 0,
		"messages": []map[string]string{{"role": "user", "content": "只回复两个字：成功"}},
	})
	if resp.StatusCode != 200 {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
	var r struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		t.Fatalf("bad json: %v: %s", err, body)
	}
	if len(r.Choices) != 1 {
		t.Fatalf("no choices: %s", body)
	}
	if r.Choices[0].Message.Content == "" && r.Choices[0].FinishReason != "length" {
		t.Fatalf("no content and not truncated: %s", body)
	}
	if r.Usage.PromptTokens == 0 || r.Usage.CompletionTokens == 0 {
		t.Fatalf("usage not normalized: %s", body)
	}
}

func TestResponsesUpstreamSameFormat(t *testing.T) {
	resp, body := gwDo(t, "POST", "/v1/responses", map[string]any{
		"model": "dsr-flash", "max_output_tokens": 256,
		"input": "只回复两个字：成功",
	})
	if resp.StatusCode != 200 {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
	var r struct {
		Output []struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"output"`
		Usage struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		t.Fatalf("bad json: %v: %s", err, body)
	}
	var text string
	for _, o := range r.Output {
		for _, c := range o.Content {
			text += c.Text
		}
	}
	if text == "" {
		t.Fatalf("no output text: %s", body)
	}
	if r.Usage.InputTokens == 0 || r.Usage.OutputTokens == 0 {
		t.Fatalf("no usage: %s", body)
	}
}

// TestChatToolsForced pins the named tool_choice encoding through the
// openai-chat upstream (matrix check 2026-11-04 caught {"type":"tool"} being
// sent where OpenAI requires {"type":"function"}).
func TestChatToolsForced(t *testing.T) {
	resp, body := gwDo(t, "POST", "/v1/chat/completions", map[string]any{
		"model": model, "max_tokens": 256, "temperature": 0,
		"tools":       []any{weatherTool},
		"tool_choice": map[string]any{"type": "function", "function": map[string]string{"name": "get_weather"}},
		"messages":    []map[string]string{{"role": "user", "content": "北京今天天气如何？"}},
	})
	if resp.StatusCode != 200 {
		t.Fatalf("forced tool_choice status %d: %s", resp.StatusCode, body)
	}
	var r struct {
		Choices []struct {
			Message struct {
				ToolCalls []struct {
					Function struct {
						Name string `json:"name"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		t.Fatalf("bad json: %v: %s", err, body)
	}
	if len(r.Choices) == 0 || len(r.Choices[0].Message.ToolCalls) == 0 ||
		r.Choices[0].Message.ToolCalls[0].Function.Name != "get_weather" {
		t.Fatalf("forced tool call missing: %s", body)
	}
}
