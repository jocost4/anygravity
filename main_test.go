package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestParseToolCalls_TagFormat(t *testing.T) {
	input := `I will check the files.
<tool_call>
{"name": "view_file", "arguments": {"AbsolutePath": "/tmp/test.go"}}
</tool_call>
Done.`

	cleanText, toolCalls := parseToolCalls(input)
	if len(toolCalls) != 1 {
		t.Fatalf("expected 1 tool call, got %d", len(toolCalls))
	}
	if toolCalls[0].Function.Name != "view_file" {
		t.Errorf("expected view_file, got %s", toolCalls[0].Function.Name)
	}
	if !strings.Contains(toolCalls[0].Function.Arguments, "/tmp/test.go") {
		t.Errorf("expected arguments to contain /tmp/test.go, got %s", toolCalls[0].Function.Arguments)
	}
	if strings.Contains(cleanText, "<tool_call>") {
		t.Errorf("expected cleanText not to contain <tool_call>, got %s", cleanText)
	}
}

func TestParseToolCalls_BlockFormat(t *testing.T) {
	input := "Sure, let's run the command:\n```tool_call\n{\"name\": \"run_command\", \"arguments\": {\"CommandLine\": \"ls -la\"}}\n```\nOutput expected."

	cleanText, toolCalls := parseToolCalls(input)
	if len(toolCalls) != 1 {
		t.Fatalf("expected 1 tool call, got %d", len(toolCalls))
	}
	if toolCalls[0].Function.Name != "run_command" {
		t.Errorf("expected run_command, got %s", toolCalls[0].Function.Name)
	}
	if strings.Contains(cleanText, "```tool_call") {
		t.Errorf("expected cleanText to strip code block, got %s", cleanText)
	}
}

func TestParseToolCalls_BareFormat(t *testing.T) {
	input := "Calling tool:\ntool_call: {\"name\": \"get_weather\", \"arguments\": {\"city\": \"Curitiba\"}}"

	_, toolCalls := parseToolCalls(input)
	if len(toolCalls) != 1 {
		t.Fatalf("expected 1 tool call, got %d", len(toolCalls))
	}
	if toolCalls[0].Function.Name != "get_weather" {
		t.Errorf("expected get_weather, got %s", toolCalls[0].Function.Name)
	}
}

func TestNormalizeArgs(t *testing.T) {
	cases := []struct {
		input    string
		expected string
	}{
		{`{"a": 1}`, `{"a":1}`},
		{`"{\"a\": 1}"`, `{"a": 1}`},
		{`""`, `{"input":""}`},
	}

	for _, c := range cases {
		out := normalizeArgs(json.RawMessage(c.input))
		if strings.TrimSpace(out) != strings.TrimSpace(c.expected) {
			t.Errorf("normalizeArgs(%s) = %s; want %s", c.input, out, c.expected)
		}
	}
}

func TestExtractSessionKey(t *testing.T) {
	hdr := http.Header{}
	hdr.Set("X-Hermes-Session-Id", "sess_12345")

	key, isAux := extractSessionKey([]ChatMessage{}, nil, hdr)
	if isAux {
		t.Errorf("expected isAux=false, got true")
	}
	if key != "hdr_sess_12345" {
		t.Errorf("expected hdr_sess_12345, got %s", key)
	}

	emptyHdr := http.Header{}

	// Direct detection: Session ID in system prompt
	sidMsgs := []ChatMessage{
		{Role: "system", Content: json.RawMessage(`"You are a helpful assistant.\n\nSession ID: 20261020_064839_a26d4b\nModel: gemini-3.8-flash"`)},
		{Role: "user", Content: json.RawMessage(`"Hello world!"`)},
	}
	keySID, isAuxSID := extractSessionKey(sidMsgs, nil, emptyHdr)
	if isAuxSID {
		t.Errorf("expected isAuxSID=false, got true")
	}
	if keySID != "sid_20261020_064839_a26d4b" {
		t.Errorf("expected sid_20261020_064839_a26d4b, got %s", keySID)
	}

	// Fallback to hashing system prompt and first user message
	msgs := []ChatMessage{
		{Role: "system", Content: json.RawMessage(`"You are a helpful assistant."`)},
		{Role: "user", Content: json.RawMessage(`"Hello world!"`)},
	}
	key2, isAux2 := extractSessionKey(msgs, nil, emptyHdr)
	if isAux2 {
		t.Errorf("expected isAux2=false, got true")
	}
	if len(key2) != 32 {
		t.Errorf("expected 32-hex-char hash, got %s (len=%d)", key2, len(key2))
	}

	// Test auxiliary request detection
	auxMsgs := []ChatMessage{
		{Role: "system", Content: json.RawMessage(`"You name chat sessions. Given the user's opening message..."`)},
	}
	_, isAux3 := extractSessionKey(auxMsgs, nil, emptyHdr)
	if !isAux3 {
		t.Errorf("expected isAux3=true for titling request, got false")
	}
}

func TestUnstreamedSemanticMatching(t *testing.T) {
	cleanText := "Hello world! Here is the full answer."
	alreadyStreamed := "Hello world!"

	var unstreamed string
	if strings.HasPrefix(cleanText, alreadyStreamed) {
		unstreamed = strings.TrimPrefix(cleanText, alreadyStreamed)
	}

	expected := " Here is the full answer."
	if unstreamed != expected {
		t.Errorf("expected %q, got %q", expected, unstreamed)
	}
}

func TestParseAGYModels(t *testing.T) {
	output := "Fetching available models...\n" +
		"gemini-3.8-flash-high	Gemini 3.8 Flash (High)\n" +
		"claude-sonnet-5-5-high	Claude Sonnet 5.5 (High)\n" +
		"gemini-3.8-flash-high	Gemini 3.8 Flash (High)\n"

	models := parseAGYModels(output)
	if len(models) != 2 {
		t.Fatalf("expected 2 unique models, got %#v", models)
	}
	if models[0].ID != "gemini-3.8-flash-high" || models[0].Name != "Gemini 3.8 Flash (High)" {
		t.Errorf("unexpected first model: %#v", models[0])
	}
}

func TestResolveModel(t *testing.T) {
	registry.mu.Lock()
	previous := registry.models
	registry.models = []ModelItem{
		{ID: "gemini-4-argon-high", Name: "Gemini 4 Argon (High)"},
		{ID: "gemini-4-argon-medium", Name: "Gemini 4 Argon (Medium)"},
		{ID: "gemini-4-argon-low", Name: "Gemini 4 Argon (Low)"},
		{ID: "gemini-3.8-flash-high", Name: "Gemini 3.8 Flash (High)"},
		{ID: "gemini-3.8-flash-medium", Name: "Gemini 3.8 Flash (Medium)"},
		{ID: "gemini-3.8-flash-low", Name: "Gemini 3.8 Flash (Low)"},
		{ID: "claude-opus-5-5-low", Name: "Claude Opus 5.5 (Low)"},
		{ID: "claude-opus-5-5-medium", Name: "Claude Opus 5.5 (Medium)"},
		{ID: "claude-opus-5-5-high", Name: "Claude Opus 5.5 (High)"},
		{ID: "claude-sonnet-5-5-low", Name: "Claude Sonnet 5.5 (Low)"},
		{ID: "claude-sonnet-5-5-medium", Name: "Claude Sonnet 5.5 (Medium)"},
		{ID: "claude-sonnet-5-5-high", Name: "Claude Sonnet 5.5 (High)"},
		{ID: "gpt-oss-120b-medium", Name: "GPT-OSS 120B (Medium)"},
	}
	registry.mu.Unlock()
	defer func() {
		registry.mu.Lock()
		registry.models = previous
		registry.mu.Unlock()
	}()

	cases := []struct {
		input       string
		expected    string
		expectError bool
	}{
		{"", "gemini-3.8-flash-high", false},
		{"auto", "gemini-3.8-flash-high", false},
		{"custom:gemini-3.8-flash-high", "gemini-3.8-flash-high", false},
		{"claude-sonnet-5-5-low", "claude-sonnet-5-5-low", false},
		{"claude-sonnet-5-5-medium", "claude-sonnet-5-5-medium", false},
		{"claude-sonnet-5-5-high", "claude-sonnet-5-5-high", false},
		{"claude-opus-5-5-low", "claude-opus-5-5-low", false},
		{"claude-opus-5-5-medium", "claude-opus-5-5-medium", false},
		{"claude-opus-5-5-high", "claude-opus-5-5-high", false},
		{"gpt-oss-120b", "gpt-oss-120b-medium", false},
		{"invalid-nonexistent-model-xyz", "", true},
	}

	for _, c := range cases {
		res, err := resolveModel(c.input)
		if c.expectError && err == nil {
			t.Errorf("resolveModel(%q) expected error, got nil", c.input)
		}
		if !c.expectError && (err != nil || res != c.expected) {
			t.Errorf("resolveModel(%q) = %q, err=%v; want %q", c.input, res, err, c.expected)
		}
	}
}

func TestAuthMiddleware(t *testing.T) {
	origAnyKey := os.Getenv("ANYGRAVITY_API_KEY")
	origHermesKey := os.Getenv("HERMESGRAVITY_API_KEY")
	defer func() {
		os.Setenv("ANYGRAVITY_API_KEY", origAnyKey)
		os.Setenv("HERMESGRAVITY_API_KEY", origHermesKey)
	}()

	os.Setenv("ANYGRAVITY_API_KEY", "secret-test-key")
	os.Unsetenv("HERMESGRAVITY_API_KEY")

	handler := authMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))

	// 1. Health should always be public
	reqHealth := httptest.NewRequest("GET", "/health", nil)
	rrHealth := httptest.NewRecorder()
	handler.ServeHTTP(rrHealth, reqHealth)
	if rrHealth.Code != http.StatusOK {
		t.Errorf("expected /health to return 200, got %d", rrHealth.Code)
	}

	// 2. Protected endpoint with missing key should return 401
	reqMissing := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	rrMissing := httptest.NewRecorder()
	handler.ServeHTTP(rrMissing, reqMissing)
	if rrMissing.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 Unauthorized for missing key, got %d", rrMissing.Code)
	}

	// 3. Protected endpoint with invalid key should return 401
	reqInvalid := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	reqInvalid.Header.Set("Authorization", "Bearer wrong-key")
	rrInvalid := httptest.NewRecorder()
	handler.ServeHTTP(rrInvalid, reqInvalid)
	if rrInvalid.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 Unauthorized for invalid key, got %d", rrInvalid.Code)
	}

	// 4. Protected endpoint with valid key should return 200
	reqValid := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	reqValid.Header.Set("Authorization", "Bearer secret-test-key")
	rrValid := httptest.NewRecorder()
	handler.ServeHTTP(rrValid, reqValid)
	if rrValid.Code != http.StatusOK {
		t.Errorf("expected 200 OK for valid key, got %d", rrValid.Code)
	}
}

func TestHandleModelInfo_NotFound(t *testing.T) {
	req := httptest.NewRequest("GET", "/v1/models/non-existent-model", nil)
	rr := httptest.NewRecorder()
	handleModelInfo(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Errorf("expected 404 Not Found for unknown model, got %d", rr.Code)
	}
}

func TestSlidingWindowMarkerDetection(t *testing.T) {
	// Simulate streaming chunks where <tool_call> is split across two events
	chunk1 := "Hello! Let me execute that for you: <tool_"
	chunk2 := "call>\n{\"name\": \"run_cmd\", \"arguments\": {\"cmd\": \"ls\"}}\n</tool_call>"

	markers := []string{"<tool_call>", "<function_call>", "call:default_api:", "```tool_call"}

	var streamPending strings.Builder
	var streamed strings.Builder
	isBufferingTool := false

	processChunk := func(delta string) {
		streamPending.WriteString(delta)
		pending := streamPending.String()

		toolMarkerIdx := -1
		for _, m := range markers {
			if idx := strings.Index(pending, m); idx != -1 {
				if toolMarkerIdx == -1 || idx < toolMarkerIdx {
					toolMarkerIdx = idx
				}
			}
		}

		if toolMarkerIdx != -1 {
			toEmit := pending[:toolMarkerIdx]
			if toEmit != "" {
				streamed.WriteString(toEmit)
			}
			isBufferingTool = true
			streamPending.Reset()
		} else {
			splitIdx := len(pending)
			const maxMarkerLen = 16
			maxK := len(pending)
			if maxK > maxMarkerLen {
				maxK = maxMarkerLen
			}
			foundPrefix := false
			for k := maxK; k > 0; k-- {
				suffix := pending[len(pending)-k:]
				for _, m := range markers {
					if strings.HasPrefix(m, suffix) {
						splitIdx = len(pending) - k
						foundPrefix = true
						break
					}
				}
				if foundPrefix {
					break
				}
			}

			toEmit := pending[:splitIdx]
			toHold := pending[splitIdx:]

			if toEmit != "" {
				streamed.WriteString(toEmit)
			}
			streamPending.Reset()
			streamPending.WriteString(toHold)
		}
	}

	processChunk(chunk1)
	if isBufferingTool {
		t.Errorf("should not be buffering tool yet after chunk 1")
	}
	if strings.Contains(streamed.String(), "<tool_") {
		t.Errorf("should not stream partial marker '<tool_'; got %q", streamed.String())
	}
	expectedFirst := "Hello! Let me execute that for you: "
	if streamed.String() != expectedFirst {
		t.Errorf("expected %q, got %q", expectedFirst, streamed.String())
	}

	processChunk(chunk2)
	if !isBufferingTool {
		t.Errorf("expected isBufferingTool=true after chunk 2")
	}
	// Verify that the streamed text did not leak any part of the tool call
	if strings.Contains(streamed.String(), "<tool_call>") || strings.Contains(streamed.String(), "run_cmd") {
		t.Errorf("leaked tool call in streamed text: %q", streamed.String())
	}
}

func TestSanitizeJSON_RawNewlinesAndInvalidEscapes(t *testing.T) {
	raw := "{\"name\": \"terminal\", \"arguments\": {\"command\": \"ssh oracle \\\"cat << 'EOF' > /file\nline 1 \\`var\\`\nline 2 \\'single\\'\nEOF\\\"\"}}"
	sanitized := sanitizeJSON(raw)

	var parsed struct {
		Name      string `json:"name"`
		Arguments struct {
			Command string `json:"command"`
		} `json:"arguments"`
	}
	if err := json.Unmarshal([]byte(sanitized), &parsed); err != nil {
		t.Fatalf("failed to unmarshal sanitized JSON: %v", err)
	}
	if parsed.Name != "terminal" {
		t.Errorf("expected terminal, got %s", parsed.Name)
	}
	if !strings.Contains(parsed.Arguments.Command, "line 1") || !strings.Contains(parsed.Arguments.Command, "line 2") {
		t.Errorf("command missing content: %s", parsed.Arguments.Command)
	}
}

func TestParseToolCalls_MultilineScript(t *testing.T) {
	input := "<tool_call>\n" +
		"{\"name\": \"terminal\", \"arguments\": {\"command\": \"ssh oracle \\\"cat << 'EOF' > /home/ubuntu/Iris/lib/Commands/Eleicao/index.js\n" +
		"/* eslint-disable max-len */\n" +
		"const envInfo = (\\`envInfo\\`);\n" +
		"export default resetLocal();\n" +
		"EOF\\\"\"}}\n" +
		"</tool_call>"

	cleanText, toolCalls := parseToolCalls(input)
	if len(toolCalls) != 1 {
		t.Fatalf("expected 1 tool call parsed, got %d", len(toolCalls))
	}
	if toolCalls[0].Function.Name != "terminal" {
		t.Errorf("expected tool terminal, got %s", toolCalls[0].Function.Name)
	}
	if !strings.Contains(toolCalls[0].Function.Arguments, "eslint-disable") {
		t.Errorf("expected script content in arguments, got %s", toolCalls[0].Function.Arguments)
	}
	if cleanText != "" {
		t.Errorf("expected cleanText to be empty when tool call parsed, got %q", cleanText)
	}
}

func TestIsAuxiliaryRequest_WithTools(t *testing.T) {
	msgs := []ChatMessage{
		{
			Role:    "user",
			Content: json.RawMessage(`"Review the conversation above and update the skill library. Be ACTIVE — most sessions develop patterns..."`),
		},
	}
	tools := []ToolItem{
		{Type: "function", Function: FunctionDef{Name: "view_file"}},
	}
	if !isAuxiliaryRequest(msgs, tools) {
		t.Errorf("expected isAuxiliaryRequest=true even when tools are present")
	}
}

func TestSanitizeJSON_UnicodeEscapes(t *testing.T) {
	// Valid \u escape
	validInput := `{"str": "\u0041"}`
	validSan := sanitizeJSON(validInput)
	var validObj map[string]string
	if err := json.Unmarshal([]byte(validSan), &validObj); err != nil {
		t.Fatalf("failed to unmarshal valid unicode: %v", err)
	}
	if validObj["str"] != "A" {
		t.Errorf("expected 'A', got %q", validObj["str"])
	}

	// Invalid \u escape like \user or \u12
	invalidInput := `{"path": "C:\user\test\u12"}`
	invalidSan := sanitizeJSON(invalidInput)
	var invalidObj map[string]string
	if err := json.Unmarshal([]byte(invalidSan), &invalidObj); err != nil {
		t.Fatalf("failed to unmarshal sanitized invalid unicode: %v", err)
	}
	if !strings.Contains(invalidObj["path"], "user") {
		t.Errorf("expected path to contain 'user', got %q", invalidObj["path"])
	}
}

func TestParseToolCalls_Deduplication(t *testing.T) {
	input := "<tool_call>{\"name\": \"get_weather\", \"arguments\": {\"city\": \"Paris\"}}</tool_call>\n" +
		"<tool_call>{\"name\": \"get_weather\", \"arguments\": {\"city\": \"Paris\"}}</tool_call>"

	_, toolCalls := parseToolCalls(input)
	if len(toolCalls) != 1 {
		t.Fatalf("expected 1 deduplicated tool call, got %d", len(toolCalls))
	}
	if toolCalls[0].Function.Name != "get_weather" {
		t.Errorf("expected get_weather, got %s", toolCalls[0].Function.Name)
	}
}

func TestSessionManager_LRUEviction(t *testing.T) {
	sm := &SessionManager{
		filePath: filepath.Join(t.TempDir(), "sessions.json"),
		sessions: make(map[string]*SessionState),
		trigger:  make(chan struct{}, 1),
	}
	baseTime := time.Now().Add(-1 * time.Hour)

	// Fill with 501 sessions
	for i := 0; i < 501; i++ {
		key := fmt.Sprintf("sess_%04d", i)
		sm.Update(key, &SessionState{
			ConvID:    fmt.Sprintf("conv_%04d", i),
			LastSeen:  baseTime.Add(time.Duration(i) * time.Minute),
			CreatedAt: baseTime,
		})
	}

	sm.mu.RLock()
	defer sm.mu.RUnlock()
	if len(sm.sessions) > 405 {
		t.Errorf("expected <= 405 sessions after LRU eviction, got %d", len(sm.sessions))
	}

	// Oldest session (sess_0000) should have been evicted
	if _, exists := sm.sessions["sess_0000"]; exists {
		t.Errorf("expected oldest session sess_0000 to be evicted")
	}

	// Newest session (sess_0500) should definitely be retained
	if _, exists := sm.sessions["sess_0500"]; !exists {
		t.Errorf("expected newest session sess_0500 to be preserved")
	}
}

func TestResolveModelAndEffort(t *testing.T) {
	registry.mu.Lock()
	previous := registry.models
	registry.models = []ModelItem{
		{ID: "gemini-4-argon-high", Name: "Gemini 4 Argon (High)"},
		{ID: "gemini-4-argon-medium", Name: "Gemini 4 Argon (Medium)"},
		{ID: "gemini-4-argon-low", Name: "Gemini 4 Argon (Low)"},
		{ID: "gemini-3.8-flash-high", Name: "Gemini 3.8 Flash (High)"},
		{ID: "gemini-3.8-flash-medium", Name: "Gemini 3.8 Flash (Medium)"},
		{ID: "gemini-3.8-flash-low", Name: "Gemini 3.8 Flash (Low)"},
		{ID: "claude-opus-5-5-low", Name: "Claude Opus 5.5 (Low)"},
		{ID: "claude-opus-5-5-medium", Name: "Claude Opus 5.5 (Medium)"},
		{ID: "claude-opus-5-5-high", Name: "Claude Opus 5.5 (High)"},
		{ID: "claude-sonnet-5-5-low", Name: "Claude Sonnet 5.5 (Low)"},
		{ID: "claude-sonnet-5-5-medium", Name: "Claude Sonnet 5.5 (Medium)"},
		{ID: "claude-sonnet-5-5-high", Name: "Claude Sonnet 5.5 (High)"},
		{ID: "gpt-oss-120b-medium", Name: "GPT-OSS 120B (Medium)"},
	}
	registry.mu.Unlock()
	defer func() {
		registry.mu.Lock()
		registry.models = previous
		registry.mu.Unlock()
	}()

	cases := []struct {
		modelIn     string
		effortIn    string
		wantModel   string
		wantEffort  string
		expectError bool
	}{
		{"gemini-3.8-flash-high", "", "gemini-3.8-flash-high", "", false},
		{"gemini-3.8-flash-high", "medium", "gemini-3.8-flash-medium", "", false},
		{"gemini-3.8-flash-low", "", "gemini-3.8-flash-low", "", false},
		{"gemini-3.8-flash", "low", "gemini-3.8-flash-low", "", false},
		{"gemini-3.8-flash", "minimal", "gemini-3.8-flash-low", "", false},
		{"claude-sonnet-5-5-high", "high", "claude-sonnet-5-5-high", "", false},
		{"claude-opus-5-5-high", "medium", "claude-opus-5-5-medium", "", false},
		{"claude-opus-5-5", "low", "claude-opus-5-5-low", "", false},
		{"claude-opus-5-5", "", "claude-opus-5-5-high", "", false},
		{"claude-sonnet-5-5", "medium", "claude-sonnet-5-5-medium", "", false},
		{"gpt-oss-120b", "high", "gpt-oss-120b-medium", "", false},
		{"gpt-oss-120b", "", "gpt-oss-120b-medium", "", false},
		{"auto", "", "gemini-3.8-flash-high", "", false},
		{"gemini-4-argon", "", "gemini-4-argon-high", "", false},
		{"gemini-4-argon", "medium", "gemini-4-argon-medium", "", false},
		{"gemini-3.7-flash", "", "gemini-3.8-flash-high", "", false},
		{"nonexistent-xyz", "", "", "", true},
	}

	for _, c := range cases {
		m, eff, err := resolveModelAndEffort(c.modelIn, c.effortIn)
		if c.expectError && err == nil {
			t.Errorf("expected error for model=%q, got nil", c.modelIn)
		}
		if !c.expectError {
			if err != nil {
				t.Errorf("unexpected error for model=%q, eff=%q: %v", c.modelIn, c.effortIn, err)
			}
			if m != c.wantModel || eff != c.wantEffort {
				t.Errorf("resolveModelAndEffort(%q, %q) = (%q, %q); want (%q, %q)",
					c.modelIn, c.effortIn, m, eff, c.wantModel, c.wantEffort)
			}
		}
	}
}

func TestParseToolCalls_BatchArray(t *testing.T) {
	input := `<tool_call>
[
  {"name": "delegate_task", "arguments": {"tasks": [{"goal": "Check shaders"}]}},
  {"name": "read_file", "arguments": {"path": "/tmp/shader.spv"}}
]
</tool_call>`

	cleanText, toolCalls := parseToolCalls(input)
	if len(toolCalls) != 2 {
		t.Fatalf("expected 2 tool calls, got %d", len(toolCalls))
	}
	if toolCalls[0].Function.Name != "delegate_task" {
		t.Errorf("expected delegate_task, got %s", toolCalls[0].Function.Name)
	}
	if toolCalls[1].Function.Name != "read_file" {
		t.Errorf("expected read_file, got %s", toolCalls[1].Function.Name)
	}
	// Fallback announcement should have been generated for delegate_task since cleanText was empty
	if !strings.Contains(cleanText, "Despachando") || !strings.Contains(cleanText, "Check shaders") {
		t.Errorf("expected subagent announcement in cleanText, got: %s", cleanText)
	}
}

func TestParseToolCalls_SubagentListAnnouncement(t *testing.T) {
	input := `<tool_call>
{"name": "delegate_task", "arguments": {"action": "list"}}
</tool_call>`

	cleanText, toolCalls := parseToolCalls(input)
	if len(toolCalls) != 1 {
		t.Fatalf("expected 1 tool call, got %d", len(toolCalls))
	}
	if !strings.Contains(cleanText, "Consultando status dos subagentes ativos") {
		t.Errorf("expected list status announcement, got: %s", cleanText)
	}
}

func TestParseToolCalls_PreservesExplicitText(t *testing.T) {
	input := `Iniciando subagente de auditoria BDA.
<tool_call>
{"name": "delegate_task", "arguments": {"tasks": [{"goal": "Audit BDA"}]}}
</tool_call>`

	cleanText, toolCalls := parseToolCalls(input)
	if len(toolCalls) != 1 {
		t.Fatalf("expected 1 tool call, got %d", len(toolCalls))
	}
	if !strings.Contains(cleanText, "Iniciando subagente de auditoria BDA.") {
		t.Errorf("expected user-facing text to be preserved, got: %s", cleanText)
	}
}

func TestBuildToolCallMap_Resolution(t *testing.T) {
	messages := []ChatMessage{
		{
			Role: "assistant",
			Content: json.RawMessage(`"Iniciando subagente"`),
			ToolCalls: json.RawMessage(`[
				{"id": "call_abc123", "type": "function", "function": {"name": "delegate_task", "arguments": "{}"}}
			]`),
		},
		{
			Role: "tool",
			ToolCallID: "call_abc123",
			Content: json.RawMessage(`"{\"status\": \"dispatched\", \"count\": 1}"`),
		},
	}

	toolMap := buildToolCallMap(messages)
	if toolMap["call_abc123"] != "delegate_task" {
		t.Errorf("expected delegate_task, got %s", toolMap["call_abc123"])
	}

	formatted := formatConversation(messages, "")
	if !strings.Contains(formatted, "[Tool Result for 'delegate_task']:") {
		t.Errorf("expected [Tool Result for 'delegate_task']:, got:\n%s", formatted)
	}
	if !strings.Contains(formatted, "Assistant: Iniciando subagente") {
		t.Errorf("expected Assistant content preserved, got:\n%s", formatted)
	}
}

func TestParseToolCalls_DropsHallucinatedToolResultsAndDuplicatePreamble(t *testing.T) {
	input := `<tool_call>
{"name": "write_file", "arguments": {"path": "/tmp/test.md", "content": "test"}}
</tool_call>

[Tool Result for 'write_file']:
{"status": "success", "path": "/tmp/test.md"}

O arquivo foi gerado com sucesso! Aqui esta o roteiro completo.`

	cleanText, toolCalls := parseToolCalls(input)
	if len(toolCalls) != 1 {
		t.Fatalf("expected 1 tool call, got %d", len(toolCalls))
	}
	if toolCalls[0].Function.Name != "write_file" {
		t.Errorf("expected write_file, got %s", toolCalls[0].Function.Name)
	}
	// Post-tool hallucinated text and fake tool results must be discarded
	if cleanText != "" {
		t.Errorf("expected empty cleanText to avoid duplicated response, got: %q", cleanText)
	}
}

func TestParseToolCalls_CleansHallucinatedToolResultWithoutToolCalls(t *testing.T) {
	input := `[Tool Result for 'write_file']:
{"status": "success"}

O arquivo foi salvo com sucesso!`

	cleanText, toolCalls := parseToolCalls(input)
	if len(toolCalls) != 0 {
		t.Fatalf("expected 0 tool calls, got %d", len(toolCalls))
	}
	if strings.Contains(cleanText, "[Tool Result") {
		t.Errorf("expected cleanText to strip [Tool Result], got: %q", cleanText)
	}
	if !strings.Contains(cleanText, "O arquivo foi salvo com sucesso!") {
		t.Errorf("expected cleanText to preserve real response, got: %q", cleanText)
	}
}

func TestGetBaseModels(t *testing.T) {
	registry.mu.Lock()
	prev := registry.models
	registry.models = []ModelItem{
		{ID: "gemini-4-argon-medium", Name: "Gemini 4 Argon (Medium)"},
		{ID: "gemini-4-argon-high", Name: "Gemini 4 Argon (High)"},
		{ID: "gemini-4-argon-low", Name: "Gemini 4 Argon (Low)"},
		{ID: "gemini-3.8-flash-high", Name: "Gemini 3.8 Flash (High)"},
		{ID: "gemini-3.8-flash-medium", Name: "Gemini 3.8 Flash (Medium)"},
		{ID: "gemini-3.8-flash-low", Name: "Gemini 3.8 Flash (Low)"},
		{ID: "claude-opus-5-5-low", Name: "Claude Opus 5.5 (Low)"},
		{ID: "claude-opus-5-5-medium", Name: "Claude Opus 5.5 (Medium)"},
		{ID: "claude-opus-5-5-high", Name: "Claude Opus 5.5 (High)"},
		{ID: "claude-sonnet-5-5-low", Name: "Claude Sonnet 5.5 (Low)"},
		{ID: "claude-sonnet-5-5-medium", Name: "Claude Sonnet 5.5 (Medium)"},
		{ID: "claude-sonnet-5-5-high", Name: "Claude Sonnet 5.5 (High)"},
		{ID: "gpt-oss-120b-medium", Name: "GPT-OSS 120B (Medium)"},
	}
	registry.mu.Unlock()
	defer func() {
		registry.mu.Lock()
		registry.models = prev
		registry.mu.Unlock()
	}()

	baseModels := getBaseModels()
	if len(baseModels) != 5 {
		t.Fatalf("expected 5 base models, got %d", len(baseModels))
	}

	expected := map[string]string{
		"gemini-4-argon":    "Gemini 4 Argon",
		"gemini-3.8-flash":  "Gemini 3.8 Flash",
		"claude-opus-5-5":   "Claude Opus 5.5",
		"claude-sonnet-5-5": "Claude Sonnet 5.5",
		"gpt-oss-120b":      "GPT-OSS 120B",
	}

	for _, bm := range baseModels {
		expName, ok := expected[bm.ID]
		if !ok {
			t.Errorf("unexpected base model ID: %q", bm.ID)
		}
		if bm.Name != expName {
			t.Errorf("base model %q name = %q; want %q", bm.ID, bm.Name, expName)
		}
	}
}

func TestHandleModels_Collapsed(t *testing.T) {
	req := httptest.NewRequest("GET", "/v1/models", nil)
	rr := httptest.NewRecorder()
	handleModels(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d", rr.Code)
	}

	var resp struct {
		Object string `json:"object"`
		Data   []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to parse /v1/models response: %v", err)
	}

	for _, item := range resp.Data {
		for _, suffix := range []string{"-high", "-medium", "-low"} {
			if strings.HasSuffix(item.ID, suffix) {
				t.Errorf("model ID %q should be collapsed without suffix %q", item.ID, suffix)
			}
		}
	}
}

func TestFormatConversation_BudgetAndTailIntegrity(t *testing.T) {
	var msgs []ChatMessage
	// 1. System message
	msgs = append(msgs, ChatMessage{
		Role:    "system",
		Content: json.RawMessage(`"You are an elite coding assistant. Follow instructions precisely."`),
	})

	// 2. Add 500 historical messages with large outputs (~2.5 MB total)
	for i := 0; i < 250; i++ {
		msgs = append(msgs, ChatMessage{
			Role:    "assistant",
			Content: json.RawMessage(fmt.Sprintf(`"Executing step %d of the build diagnostic pipeline with extensive logs and analysis."`, i)),
			ToolCalls: json.RawMessage(fmt.Sprintf(`[{"id": "call_%d", "type": "function", "function": {"name": "run_command", "arguments": "{\"CommandLine\": \"cargo build --verbose\"}"}}]`, i)),
		})
		largeOutput := strings.Repeat(fmt.Sprintf("Compiling crate_%d v0.1.0 (/path/to/crate_%d)...\n", i, i), 100)
		msgs = append(msgs, ChatMessage{
			Role:       "tool",
			ToolCallID: fmt.Sprintf("call_%d", i),
			Name:       "run_command",
			Content:    json.RawMessage(fmt.Sprintf("%q", largeOutput)),
		})
	}

	// 3. The user's critical question at the tail
	prQuestion := "ok, com as alterações que fizemos no código hoje, temos uma PR de respeito para fazer ou você acha que ainda devemos segurar essa PR?"
	msgs = append(msgs, ChatMessage{
		Role:    "user",
		Content: json.RawMessage(fmt.Sprintf("%q", prQuestion)),
	})

	toolsPrompt := "[AVAILABLE TOOLS]\n[]"
	formatted := formatConversation(msgs, toolsPrompt)

	// Verify total length never exceeds budget (120 KB)
	if len(formatted) > maxAnchorPromptChars {
		t.Errorf("formatted prompt length %d exceeds maxAnchorPromptChars %d", len(formatted), maxAnchorPromptChars)
	}

	// Verify system prompt is preserved
	if !strings.Contains(formatted, "You are an elite coding assistant.") {
		t.Errorf("system prompt was unexpectedly omitted from formatted output")
	}

	// Verify the user's latest prompt is 100% intact at the tail
	if !strings.Contains(formatted, prQuestion) {
		t.Fatalf("CRITICAL BUG: User's latest prompt was truncated/lost from formatted conversation tail!")
	}

	// Verify older messages were omitted with note
	if !strings.Contains(formatted, "earlier conversation messages omitted to fit context window") {
		t.Errorf("expected omitted messages note in compacted prompt, got:\n%s", formatted[:500])
	}

	// Verify clean completion prompt tail
	if !strings.HasSuffix(strings.TrimSpace(formatted), "Assistant:") {
		t.Errorf("expected prompt to end with 'Assistant:', got tail: %q", formatted[len(formatted)-40:])
	}
}

func TestDeltaContinuityOnModelSwitch(t *testing.T) {
	// Setup a session manager with a known state initialized with model A
	sm := &SessionManager{
		filePath: filepath.Join(t.TempDir(), "sessions.json"),
		sessions: make(map[string]*SessionState),
		trigger:  make(chan struct{}, 1),
	}

	sessionKey := "sess_model_switch_test"
	baseMsgs := []ChatMessage{
		{Role: "system", Content: json.RawMessage(`"System instructions"`)},
		{Role: "user", Content: json.RawMessage(`"Turn 1 prompt"`)},
		{Role: "assistant", Content: json.RawMessage(`"Turn 1 response"`)},
	}
	baseHashes := []string{hashMessage(baseMsgs[0]), hashMessage(baseMsgs[1]), hashMessage(baseMsgs[2])}
	toolsHash := hashTools(nil)

	sm.Update(sessionKey, &SessionState{
		ConvID:        "agy-conv-abc12345",
		Model:         "gemini-3.8-flash-high",
		ToolsHash:     toolsHash,
		MessageCount:  len(baseHashes),
		HistoryHashes: baseHashes,
	})

	// User sends Turn 2 with model B
	newMsgs := append(baseMsgs, ChatMessage{
		Role:    "user",
		Content: json.RawMessage(`"Turn 2 question with new model"`),
	})
	currentHashes := make([]string, len(newMsgs))
	for i, m := range newMsgs {
		currentHashes[i] = hashMessage(m)
	}

	activeState := sm.Get(sessionKey)
	if activeState == nil {
		t.Fatalf("expected active state, got nil")
	}

	// Test the core decision logic from main.go
	isDelta := false
	var convID string
	var deltaPrompt string

	requestedNewModel := "gemini-4-argon-high"

	// New condition: activeState.ConvID != "" && activeState.ToolsHash == toolsHash (activeState.Model == model removed!)
	if activeState.ConvID != "" && activeState.ToolsHash == toolsHash {
		if len(newMsgs) > activeState.MessageCount {
			prefixOK := true
			for i := 0; i < activeState.MessageCount; i++ {
				if currentHashes[i] != activeState.HistoryHashes[i] {
					prefixOK = false
					break
				}
			}
			if prefixOK {
				deltaPrompt = formatDeltaPrompt(newMsgs, activeState.MessageCount)
				convID = activeState.ConvID
				isDelta = true
			}
		}
	}

	if !isDelta {
		t.Fatalf("expected isDelta=true across model switch, got false")
	}
	if convID != "agy-conv-abc12345" {
		t.Errorf("expected convID agy-conv-abc12345, got %s", convID)
	}
	if !strings.Contains(deltaPrompt, "Turn 2 question with new model") {
		t.Errorf("expected deltaPrompt to contain turn 2 prompt, got: %s", deltaPrompt)
	}
	if strings.Contains(deltaPrompt, "Turn 1 prompt") {
		t.Errorf("deltaPrompt should NOT contain historical turn 1 prompt; got: %s", deltaPrompt)
	}

	// Update session with new model and verify state transitions
	sm.Update(sessionKey, &SessionState{
		ConvID:        convID,
		Model:         requestedNewModel,
		ToolsHash:     toolsHash,
		MessageCount:  len(currentHashes),
		HistoryHashes: currentHashes,
	})

	updatedState := sm.Get(sessionKey)
	if updatedState.Model != "gemini-4-argon-high" {
		t.Errorf("expected updated state model to be gemini-4-argon-high, got %s", updatedState.Model)
	}
	if updatedState.ConvID != "agy-conv-abc12345" {
		t.Errorf("expected updated state convID to remain agy-conv-abc12345, got %s", updatedState.ConvID)
	}
}

func TestParseToolCalls_Step97HeredocWithPython(t *testing.T) {
	input := `<tool_call>
{"name": "terminal", "arguments": {"command": "ssh hermes << 'EOF'
cat << 'PYEOF' > /tmp/update_engine.py
import os, sys

path = "/home/ubuntu/caipirinha/internal/cobalt/engine.go"
with open(path, "r") as f:
content = f.read()

if '"net/url"' not in content:
content = content.replace('"net/http"', '"net/http"\n\t"net/url"')

func_strip = '''// stripTrackingParams
func stripTrackingParams(rawURL string) string {
\tu, err := url.Parse(rawURL)
}
'''

target1 = """return strings.TrimFunc(parts)"""

with open(path, "w") as f:
f.write(content)
print("SUCCESS")
PYEOF
python3 /tmp/update_engine.py
EOF"}}
</tool_call>`

	cleanText, toolCalls := parseToolCalls(input)
	if len(toolCalls) != 1 {
		t.Fatalf("expected 1 tool call parsed, got %d", len(toolCalls))
	}
	if toolCalls[0].Function.Name != "terminal" {
		t.Errorf("expected terminal, got %s", toolCalls[0].Function.Name)
	}

	var args map[string]interface{}
	if err := json.Unmarshal([]byte(toolCalls[0].Function.Arguments), &args); err != nil {
		t.Fatalf("arguments are not valid JSON: %v", err)
	}
	cmd, ok := args["command"].(string)
	if !ok || !strings.Contains(cmd, "update_engine.py") {
		t.Errorf("command missing expected content: %v", args["command"])
	}
	if cleanText != "" {
		t.Errorf("expected cleanText to be empty when tool call parsed, got %q", cleanText)
	}
}

func TestParseToolCalls_Step118PythonTripleQuotes(t *testing.T) {
	input := `<tool_call>
{"name": "execute_code", "arguments": {"language": "python", "code": """import subprocess

res = subprocess.run(["ssh", "hermes", "cat /path/engine.go"], capture_output=True, text=True)
if res.returncode != 0:
    raise RuntimeError("Failed")

target1 = '''return strings.TrimFunc(parts)'''
print("Successfully updated!")
"""}}
</tool_call>`

	cleanText, toolCalls := parseToolCalls(input)
	if len(toolCalls) != 1 {
		t.Fatalf("expected 1 tool call parsed, got %d", len(toolCalls))
	}
	if toolCalls[0].Function.Name != "execute_code" {
		t.Errorf("expected execute_code, got %s", toolCalls[0].Function.Name)
	}

	var args map[string]interface{}
	if err := json.Unmarshal([]byte(toolCalls[0].Function.Arguments), &args); err != nil {
		t.Fatalf("arguments are not valid JSON: %v", err)
	}
	if args["language"] != "python" {
		t.Errorf("expected language python, got %v", args["language"])
	}
	code, ok := args["code"].(string)
	if !ok || !strings.Contains(code, "Successfully updated!") {
		t.Errorf("code missing expected content: %v", args["code"])
	}
	if cleanText != "" {
		t.Errorf("expected cleanText to be empty, got %q", cleanText)
	}
}

func TestParseToolCalls_ZeroDropGuarantee(t *testing.T) {
	// If a corrupted or unrecognizable tool call occurs, the text MUST NOT be erased into ""
	input := "Here is some important reasoning.\n<tool_call>corrupted nonsense without json</tool_call>"
	cleanText, toolCalls := parseToolCalls(input)
	if len(toolCalls) != 0 {
		t.Errorf("expected 0 tool calls, got %d", len(toolCalls))
	}
	if cleanText == "" {
		t.Fatalf("Zero-Drop guarantee failed: cleanText was dropped to empty string!")
	}
	if !strings.Contains(cleanText, "Here is some important reasoning") {
		t.Errorf("cleanText missing original content: %q", cleanText)
	}
}

func TestHashToolCall_O1Memory(t *testing.T) {
	h1 := hashToolCall("terminal", `{"command": "ls -la"}`)
	h2 := hashToolCall("terminal", `{"command": "ls -la"}`)
	h3 := hashToolCall("terminal", `{"command": "pwd"}`)

	if h1 != h2 {
		t.Errorf("expected hash to be deterministic: %d != %d", h1, h2)
	}
	if h1 == h3 {
		t.Errorf("expected different arguments to yield different hashes")
	}
}

func TestAnthropicConvert_TextAndSystem(t *testing.T) {
	req := AnthropicMessagesRequest{
		Model:  "claude-3-7-sonnet",
		System: json.RawMessage(`"You are a helpful coding assistant."`),
		Messages: []AnthropicMessage{
			{
				Role:    "user",
				Content: json.RawMessage(`"Hello Antigravity!"`),
			},
		},
	}

	msgs, tools, err := convertAnthropicToInternal(req)
	if err != nil {
		t.Fatalf("unexpected convert error: %v", err)
	}
	if len(msgs) != 2 {
		t.Fatalf("expected 2 messages (system + user), got %d", len(msgs))
	}
	if msgs[0].Role != "system" || !strings.Contains(msgs[0].ContentString(), "coding assistant") {
		t.Errorf("system message mismatch: %v", msgs[0])
	}
	if msgs[1].Role != "user" || !strings.Contains(msgs[1].ContentString(), "Hello Antigravity!") {
		t.Errorf("user message mismatch: %v", msgs[1])
	}
	if len(tools) != 0 {
		t.Errorf("expected 0 tools, got %d", len(tools))
	}
}

func TestAnthropicConvert_ToolUseAndToolResult(t *testing.T) {
	req := AnthropicMessagesRequest{
		Model: "gemini-3.8-flash",
		Messages: []AnthropicMessage{
			{
				Role: "assistant",
				Content: json.RawMessage(`[
					{"type": "text", "text": "I will list the directory."},
					{"type": "tool_use", "id": "call_abc123", "name": "bash", "input": {"command": "ls -la"}}
				]`),
			},
			{
				Role: "user",
				Content: json.RawMessage(`[
					{"type": "tool_result", "tool_use_id": "call_abc123", "content": "file1.txt\nfile2.txt"}
				]`),
			},
		},
		Tools: []AnthropicTool{
			{
				Name:        "bash",
				Description: "Run bash command",
				InputSchema: json.RawMessage(`{"type": "object", "properties": {"command": {"type": "string"}}}`),
			},
		},
	}

	msgs, tools, err := convertAnthropicToInternal(req)
	if err != nil {
		t.Fatalf("unexpected error converting: %v", err)
	}

	if len(tools) != 1 {
		t.Fatalf("expected 1 tool, got %d", len(tools))
	}
	if tools[0].Function.Name != "bash" {
		t.Errorf("expected tool name bash, got %s", tools[0].Function.Name)
	}

	if len(msgs) != 2 {
		t.Fatalf("expected 2 converted messages, got %d", len(msgs))
	}

	// Message 0: Assistant with tool call
	asstMsg := msgs[0]
	if asstMsg.Role != "assistant" {
		t.Errorf("expected role assistant, got %s", asstMsg.Role)
	}
	var calls []ParsedToolCall
	if err := json.Unmarshal(asstMsg.ToolCalls, &calls); err != nil {
		t.Fatalf("failed to unmarshal assistant tool calls: %v", err)
	}
	if len(calls) != 1 {
		t.Fatalf("expected 1 tool call in assistant message, got %d", len(calls))
	}
	tc := calls[0]
	if tc.ID != "call_abc123" {
		t.Errorf("expected tool call id call_abc123, got %s", tc.ID)
	}
	if tc.Function.Name != "bash" {
		t.Errorf("expected tool name bash, got %s", tc.Function.Name)
	}
	if !strings.Contains(tc.Function.Arguments, "ls -la") {
		t.Errorf("arguments missing ls -la: %s", tc.Function.Arguments)
	}

	// Message 1: Tool result
	toolMsg := msgs[1]
	if toolMsg.Role != "tool" {
		t.Errorf("expected role tool, got %s", toolMsg.Role)
	}
	if toolMsg.ToolCallID != "call_abc123" {
		t.Errorf("expected tool_call_id call_abc123, got %s", toolMsg.ToolCallID)
	}
	if !strings.Contains(toolMsg.ContentString(), "file1.txt") {
		t.Errorf("expected tool content to contain file1.txt, got %s", toolMsg.ContentString())
	}
}

func TestAnthropicMessages_CORSAndAuth(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/messages", handleAnthropicMessages)

	os.Setenv("ANYGRAVITY_API_KEY", "secret-key-42")
	defer os.Unsetenv("ANYGRAVITY_API_KEY")

	handler := authMiddleware(mux)

	// 1. CORS Preflight OPTIONS
	reqOpt := httptest.NewRequest("OPTIONS", "/v1/messages", nil)
	wOpt := httptest.NewRecorder()
	handler.ServeHTTP(wOpt, reqOpt)
	if wOpt.Code != http.StatusOK {
		t.Errorf("expected OPTIONS 200 OK, got %d", wOpt.Code)
	}
	if wOpt.Header().Get("Access-Control-Allow-Origin") != "*" {
		t.Errorf("missing CORS Allow-Origin header")
	}

	// 2. Request without auth -> 401
	reqNoAuth := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(`{}`))
	wNoAuth := httptest.NewRecorder()
	handler.ServeHTTP(wNoAuth, reqNoAuth)
	if wNoAuth.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 Unauthorized, got %d", wNoAuth.Code)
	}

	// 3. Request with X-Api-Key
	reqWithKey := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(`{"model":"invalid-model-for-test"}`))
	reqWithKey.Header.Set("X-Api-Key", "secret-key-42")
	wWithKey := httptest.NewRecorder()
	handler.ServeHTTP(wWithKey, reqWithKey)
	// Even if model is invalid, it passes auth middleware (code should not be 401)
	if wWithKey.Code == http.StatusUnauthorized {
		t.Errorf("expected X-Api-Key to pass authentication, but got 401")
	}
}

func testNetResolver() netResolver {
	return netResolver{
		listInterfaces: func() ([]netInterfaceInfo, error) {
			return []netInterfaceInfo{
				{
					Name: "lo",
					Up:   true,
					Addrs: []net.IP{
						net.ParseIP("127.0.0.1"),
						net.ParseIP("::1"),
					},
				},
				{
					Name: "eth0",
					Up:   true,
					Addrs: []net.IP{
						net.ParseIP("192.168.1.50"),
						net.ParseIP("fe80::1"),
					},
				},
				{
					Name: "tailscale0",
					Up:   true,
					Addrs: []net.IP{
						net.ParseIP("100.64.10.20"),
						net.ParseIP("fd7a:115c:a1e0::42"),
					},
				},
				{
					Name: "down0",
					Up:   false,
					Addrs: []net.IP{
						net.ParseIP("10.0.0.99"),
					},
				},
			}, nil
		},
		lookupIP: func(host string) ([]net.IP, error) {
			switch host {
			case "mybox.tail1234.ts.net":
				return []net.IP{
					net.ParseIP("100.64.10.20"),
					net.ParseIP("fd7a:115c:a1e0::42"),
				}, nil
			case "remote.example.com":
				return []net.IP{net.ParseIP("93.184.216.34")}, nil
			default:
				return nil, fmt.Errorf("unknown host: %s", host)
			}
		},
	}
}

func TestResolveBindAddrs(t *testing.T) {
	resolver := testNetResolver()

	tests := []struct {
		name        string
		specs       []string
		defaultPort string
		want        []string
		wantErr     bool
	}{
		{
			name:        "default loopback",
			specs:       nil,
			defaultPort: "20130",
			want:        []string{"127.0.0.1:20130"},
		},
		{
			name:        "multiple comma-separated IPs with deduplication",
			specs:       []string{"127.0.0.1, 192.168.1.50, 127.0.0.1"},
			defaultPort: "20130",
			want:        []string{"127.0.0.1:20130", "192.168.1.50:20130"},
		},
		{
			name:        "per-entry custom port and IPv6",
			specs:       []string{"127.0.0.1:20130", "100.64.10.20:20131", "::1"},
			defaultPort: "20130",
			want:        []string{"127.0.0.1:20130", "100.64.10.20:20131", "[::1]:20130"},
		},
		{
			name:        "lan keyword excludes loopback, down, and tailscale",
			specs:       []string{"127.0.0.1,lan"},
			defaultPort: "20130",
			want:        []string{"127.0.0.1:20130", "192.168.1.50:20130"},
		},
		{
			name:        "tailscale keyword picks IPv4 and IPv6",
			specs:       []string{"127.0.0.1", "tailscale"},
			defaultPort: "20130",
			want:        []string{"127.0.0.1:20130", "100.64.10.20:20130", "[fd7a:115c:a1e0::42]:20130"},
		},
		{
			name:        "interface name resolution skips link-local",
			specs:       []string{"eth0"},
			defaultPort: "20130",
			want:        []string{"192.168.1.50:20130"},
		},
		{
			name:        "tailscale MagicDNS hostname resolves to local Tailscale IPs",
			specs:       []string{"mybox.tail1234.ts.net"},
			defaultPort: "20130",
			want:        []string{"100.64.10.20:20130", "[fd7a:115c:a1e0::42]:20130"},
		},
		{
			name:        "all keyword maps to 0.0.0.0",
			specs:       []string{"all"},
			defaultPort: "20130",
			want:        []string{"0.0.0.0:20130"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveBindAddrs(tc.specs, tc.defaultPort, resolver)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil (%v)", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestResolveBindAddrs_ErrorsWhenMissingInterfaces(t *testing.T) {
	emptyResolver := netResolver{
		listInterfaces: func() ([]netInterfaceInfo, error) {
			return []netInterfaceInfo{
				{Name: "lo", Up: true, Addrs: []net.IP{net.ParseIP("127.0.0.1")}},
			}, nil
		},
		lookupIP: func(host string) ([]net.IP, error) {
			return nil, fmt.Errorf("not found")
		},
	}

	if _, err := resolveBindAddrs([]string{"lan"}, "20130", emptyResolver); err == nil {
		t.Errorf("expected error for lan when no LAN IP exists")
	}
	if _, err := resolveBindAddrs([]string{"tailscale"}, "20130", emptyResolver); err == nil {
		t.Errorf("expected error for tailscale when no Tailscale IP exists")
	}
}

func TestClassifyBindAddrs(t *testing.T) {
	w, nl := classifyBindAddrs([]string{"127.0.0.1:20130", "[::1]:20130", "localhost:20130"})
	if w || nl {
		t.Errorf("loopback-only should be (false, false), got (%v, %v)", w, nl)
	}

	w, nl = classifyBindAddrs([]string{"127.0.0.1:20130", "100.64.10.20:20130"})
	if w || !nl {
		t.Errorf("loopback + tailscale should be (false, true), got (%v, %v)", w, nl)
	}

	w, nl = classifyBindAddrs([]string{"0.0.0.0:20130"})
	if !w || !nl {
		t.Errorf("0.0.0.0 should be (true, true), got (%v, %v)", w, nl)
	}
}

func TestMultiListenerServer(t *testing.T) {
	ln1, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen 1: %v", err)
	}
	ln2, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		_ = ln1.Close()
		t.Fatalf("listen 2: %v", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", handleHealth)

	srv := &http.Server{Handler: authMiddleware(mux)}
	var wg sync.WaitGroup
	for _, ln := range []net.Listener{ln1, ln2} {
		wg.Add(1)
		go func(l net.Listener) {
			defer wg.Done()
			_ = srv.Serve(l)
		}(ln)
	}

	for _, addr := range []string{ln1.Addr().String(), ln2.Addr().String()} {
		resp, err := http.Get("http://" + addr + "/health")
		if err != nil {
			t.Fatalf("GET http://%s/health failed: %v", addr, err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("expected 200 OK from %s, got %d", addr, resp.StatusCode)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		t.Fatalf("shutdown failed: %v", err)
	}
	wg.Wait()
}




