package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
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

	key := extractSessionKey([]ChatMessage{}, hdr)
	if key != "hdr_sess_12345" {
		t.Errorf("expected hdr_sess_12345, got %s", key)
	}

	// Fallback to hashing system prompt and first user message
	emptyHdr := http.Header{}
	msgs := []ChatMessage{
		{Role: "system", Content: json.RawMessage(`"You are a helpful assistant."`)},
		{Role: "user", Content: json.RawMessage(`"Hello world!"`)},
	}
	key2 := extractSessionKey(msgs, emptyHdr)
	if len(key2) != 16 {
		t.Errorf("expected 16-hex-char hash, got %s (len=%d)", key2, len(key2))
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
