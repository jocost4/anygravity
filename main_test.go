package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
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

	// Fallback to hashing system prompt and first user message
	emptyHdr := http.Header{}
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

func TestResolveModel(t *testing.T) {
	cases := []struct {
		input       string
		expected    string
		expectError bool
	}{
		{"", "gemini-3.8-flash-high", false},
		{"auto", "gemini-3.8-flash-high", false},
		{"default", "gemini-3.8-flash-high", false},
		{"custom:gemini-3.8-flash-high", "gemini-3.8-flash-high", false},
		{"claude-sonnet-4-6", "claude-sonnet-4-6", false},
		{"claude-opus-4-6-thinking", "claude-opus-4-6-thinking", false},
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
	cases := []struct {
		modelIn     string
		effortIn    string
		wantModel   string
		wantEffort  string
		expectError bool
	}{
		{"gemini-3.8-flash-high", "", "gemini-3.8-flash", "high", false},
		{"gemini-3.8-flash-high", "medium", "gemini-3.8-flash", "medium", false},
		{"gemini-3.8-flash", "low", "gemini-3.8-flash", "low", false},
		{"gemini-3.8-flash", "extra-high", "gemini-3.8-flash", "high", false},
		{"gemini-3.8-flash", "ultra", "gemini-3.8-flash", "high", false},
		{"gemini-3.1-pro", "medium", "gemini-3.1-pro", "high", false},
		{"claude-sonnet-4-6", "high", "claude-sonnet-4-6", "", false},
		{"claude-opus-4-6-thinking", "medium", "claude-opus-4-6-thinking", "", false},
		{"auto", "", "gemini-3.8-flash", "high", false},
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

