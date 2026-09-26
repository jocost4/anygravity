package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// -----------------------------------------------------------------------------
// Constants & Configuration
// -----------------------------------------------------------------------------
const (
	Port           = "20130"
	Version        = "3.2-go"
	DefaultTimeout = 180 * time.Second
)

var (
	agyBin = filepath.Join(os.Getenv("HOME"), ".local/bin/agy")

	fallbackModels = []ModelItem{
		{ID: "gemini-3.8-flash-high", Name: "Gemini 3.8 Flash (High Effort)"},
		{ID: "gemini-3.8-flash-low", Name: "Gemini 3.8 Flash (Low Effort)"},
		{ID: "gemini-3.8-pro", Name: "Gemini 3.8 Pro"},
		{ID: "claude-sonnet-4-6", Name: "Claude Sonnet 4.6"},
		{ID: "claude-opus-4-6-thinking", Name: "Claude Opus 4.6 Thinking"},
		{ID: "gpt-5-mini", Name: "GPT-5 Mini"},
	}

	sessionMapFile = filepath.Join(os.Getenv("HOME"), ".hermes/antigravity_session_map.json")
	sessionMu      sync.RWMutex
	sessionCache   = make(map[string]string)

	brainDirs = []string{
		filepath.Join(os.Getenv("HOME"), ".gemini/antigravity-cli/brain"),
		filepath.Join(os.Getenv("HOME"), ".gemini/antigravity/brain"),
	}

	startTime = time.Now()
	reqCount  atomic.Uint64
)

type ModelItem struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// -----------------------------------------------------------------------------
// Session Map Management
// -----------------------------------------------------------------------------
func loadSessionMap() {
	sessionMu.Lock()
	defer sessionMu.Unlock()
	data, err := os.ReadFile(sessionMapFile)
	if err == nil {
		_ = json.Unmarshal(data, &sessionCache)
	}
}

func saveSessionMap(key, convID string) {
	if key == "" || convID == "" {
		return
	}
	sessionMu.Lock()
	defer sessionMu.Unlock()
	sessionCache[key] = convID
	if len(sessionCache) > 1000 {
		for k := range sessionCache {
			delete(sessionCache, k)
			if len(sessionCache) <= 800 {
				break
			}
		}
	}
	data, err := json.MarshalIndent(sessionCache, "", "  ")
	if err == nil {
		_ = os.WriteFile(sessionMapFile, data, 0644)
	}
}

func getSessionID(key string) string {
	sessionMu.RLock()
	defer sessionMu.RUnlock()
	return sessionCache[key]
}

func extractSessionKey(messages []ChatMessage, headers http.Header) string {
	for _, h := range []string{"X-Session-Id", "X-Conversation-Id", "X-Hermes-Session-Id"} {
		if val := headers.Get(h); val != "" {
			return "hdr_" + strings.TrimSpace(val)
		}
	}
	systemText := ""
	firstUser := ""
	for _, m := range messages {
		if m.Role == "system" && systemText == "" {
			systemText = m.ContentString()
			if len(systemText) > 150 {
				systemText = systemText[:150]
			}
		} else if m.Role == "user" && firstUser == "" {
			firstUser = m.ContentString()
			if len(firstUser) > 250 {
				firstUser = firstUser[:250]
			}
		}
		if systemText != "" && firstUser != "" {
			break
		}
	}
	h := sha256.Sum256([]byte(systemText + "|" + firstUser))
	return hex.EncodeToString(h[:8])
}

// -----------------------------------------------------------------------------
// Dynamic Model Registry
// -----------------------------------------------------------------------------
type ModelRegistry struct {
	mu          sync.RWMutex
	lastRefresh time.Time
	models      []ModelItem
}

var registry = &ModelRegistry{models: fallbackModels}

func (r *ModelRegistry) GetModels() []ModelItem {
	r.mu.RLock()
	if time.Since(r.lastRefresh) < 5*time.Minute && len(r.models) > 0 {
		defer r.mu.RUnlock()
		return r.models
	}
	r.mu.RUnlock()

	r.mu.Lock()
	defer r.mu.Unlock()
	if time.Since(r.lastRefresh) < 5*time.Minute && len(r.models) > 0 {
		return r.models
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, agyBin, "models")
	cmd.Env = append(os.Environ(), "PATH="+filepath.Join(os.Getenv("HOME"), ".local/bin")+":"+os.Getenv("PATH"))
	out, err := cmd.Output()
	if err != nil {
		r.lastRefresh = time.Now().Add(-3 * time.Minute)
		return r.models
	}

	var parsed []ModelItem
	lines := strings.Split(string(out), "\n")
	for _, line := range lines {
		clean := strings.TrimSpace(line)
		if clean == "" || strings.HasPrefix(clean, "⠋") || strings.HasPrefix(clean, "⠙") {
			continue
		}
		parts := strings.Fields(clean)
		if len(parts) >= 2 {
			parsed = append(parsed, ModelItem{ID: parts[0], Name: strings.Join(parts[1:], " ")})
		} else if len(parts) == 1 {
			parsed = append(parsed, ModelItem{ID: parts[0], Name: parts[0]})
		}
	}

	if len(parsed) > 0 {
		r.models = parsed
		r.lastRefresh = time.Now()
	}
	return r.models
}

// -----------------------------------------------------------------------------
// Tool Calling & Prompt Formatting
// -----------------------------------------------------------------------------
type ToolItem struct {
	Type     string      `json:"type"`
	Function FunctionDef `json:"function"`
}

type FunctionDef struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

type ChatMessage struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content"`
	Name       string          `json:"name,omitempty"`
	ToolCallID string          `json:"tool_call_id,omitempty"`
	ToolCalls  json.RawMessage `json:"tool_calls,omitempty"`
}

func (m *ChatMessage) ContentString() string {
	if len(m.Content) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(m.Content, &s); err == nil {
		return s
	}
	var parts []map[string]interface{}
	if err := json.Unmarshal(m.Content, &parts); err == nil {
		var sb strings.Builder
		for _, p := range parts {
			if t, ok := p["type"].(string); ok && t == "text" {
				if txt, ok := p["text"].(string); ok {
					sb.WriteString(txt)
				}
			}
		}
		return sb.String()
	}
	return string(m.Content)
}

func formatToolsPrompt(tools []ToolItem) string {
	if len(tools) == 0 {
		return ""
	}
	b, _ := json.MarshalIndent(tools, "", "  ")
	return fmt.Sprintf(`
[AVAILABLE TOOLS]
%s

[CRITICAL INSTRUCTIONS FOR HERMES AGENT]
You are acting as the backend AI completion model for Hermes Agent.
- You DO NOT have permission to use or call any local system tools directly.
- When a tool is needed, you MUST choose from [AVAILABLE TOOLS] and respond ONLY with the exact tag:
<tool_call>
{"name": "tool_name", "arguments": {"param": "value"}}
</tool_call>
- Do NOT output commentary or conversational filler before or after the <tool_call> tag when calling a tool.
- If no tool is needed, respond directly to the user in normal helpful text.
`, string(b))
}

func formatConversation(messages []ChatMessage, toolsPrompt string) string {
	var sb strings.Builder
	if toolsPrompt != "" {
		sb.WriteString(toolsPrompt)
		sb.WriteString("\n\n")
	}

	for _, msg := range messages {
		c := msg.ContentString()
		switch msg.Role {
		case "system":
			sb.WriteString("[System Instructions]:\n" + c + "\n\n")
		case "user":
			sb.WriteString("User: " + c + "\n\n")
		case "assistant":
			if len(msg.ToolCalls) > 0 && string(msg.ToolCalls) != "null" {
				var tcList []struct {
					Function struct {
						Name      string          `json:"name"`
						Arguments json.RawMessage `json:"arguments"`
					} `json:"function"`
				}
				_ = json.Unmarshal(msg.ToolCalls, &tcList)
				for _, tc := range tcList {
					argStr := string(tc.Function.Arguments)
					if strings.HasPrefix(argStr, "\"") {
						var unq string
						_ = json.Unmarshal(tc.Function.Arguments, &unq)
						argStr = unq
					}
					sb.WriteString(fmt.Sprintf("<tool_call>\n{\"name\": \"%s\", \"arguments\": %s}\n</tool_call>\n\n", tc.Function.Name, argStr))
				}
			} else if c != "" {
				sb.WriteString("Assistant: " + c + "\n\n")
			}
		case "tool":
			toolName := msg.Name
			if toolName == "" {
				toolName = "tool"
			}
			sb.WriteString(fmt.Sprintf("[Tool Result for '%s']:\n%s\n\n", toolName, c))
		}
	}

	sb.WriteString("Assistant:")
	return sb.String()
}

// -----------------------------------------------------------------------------
// Tool Call Parsing & Output Sanitization
// -----------------------------------------------------------------------------
type ParsedToolCall struct {
	Index    int    `json:"index"`
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

var (
	tagRegex      = regexp.MustCompile(`(?s)<(?:tool_call|function_call)>(.*?)</(?:tool_call|function_call)>`)
	blockRegex    = regexp.MustCompile("(?s)```tool_call\\s*(.*?)\\s*```")
	internalRegex = regexp.MustCompile(`call:(?:default_api:)?([a-zA-Z0-9_-]+)\{([^}]+)\}`)
	thinkRegex    = regexp.MustCompile(`(?s)<(?:think|thought)>(.*?)</(?:think|thought)>`)
)

func parseToolCalls(text string) (cleanText string, toolCalls []ParsedToolCall) {
	cleaned := text

	matches := tagRegex.FindAllStringSubmatch(text, -1)
	for _, m := range matches {
		raw := strings.TrimSpace(m[1])
		raw = strings.TrimPrefix(raw, "```json")
		raw = strings.TrimPrefix(raw, "```")
		raw = strings.TrimSuffix(raw, "```")
		raw = strings.TrimSpace(raw)

		var single struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal([]byte(raw), &single); err == nil && single.Name != "" {
			tc := ParsedToolCall{
				Index: len(toolCalls),
				ID:    fmt.Sprintf("call_%x", time.Now().UnixNano()%0xffffff),
				Type:  "function",
			}
			tc.Function.Name = strings.TrimSpace(single.Name)
			tc.Function.Arguments = normalizeArgs(single.Arguments)
			toolCalls = append(toolCalls, tc)
		}
	}
	cleaned = tagRegex.ReplaceAllString(cleaned, "")

	bMatches := blockRegex.FindAllStringSubmatch(cleaned, -1)
	for _, m := range bMatches {
		raw := strings.TrimSpace(m[1])
		var single struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal([]byte(raw), &single); err == nil && single.Name != "" {
			tc := ParsedToolCall{
				Index: len(toolCalls),
				ID:    fmt.Sprintf("call_%x", time.Now().UnixNano()%0xffffff),
				Type:  "function",
			}
			tc.Function.Name = strings.TrimSpace(single.Name)
			tc.Function.Arguments = normalizeArgs(single.Arguments)
			toolCalls = append(toolCalls, tc)
		}
	}
	cleaned = blockRegex.ReplaceAllString(cleaned, "")

	iMatches := internalRegex.FindAllStringSubmatch(cleaned, -1)
	for _, m := range iMatches {
		tname := m[1]
		argsStr := "{" + m[2] + "}"
		tc := ParsedToolCall{
			Index: len(toolCalls),
			ID:    fmt.Sprintf("call_%x", time.Now().UnixNano()%0xffffff),
			Type:  "function",
		}
		tc.Function.Name = strings.TrimSpace(tname)
		tc.Function.Arguments = argsStr
		toolCalls = append(toolCalls, tc)
	}
	cleaned = internalRegex.ReplaceAllString(cleaned, "")

	cleanText = strings.TrimSpace(cleaned)
	return cleanText, toolCalls
}

func normalizeArgs(raw json.RawMessage) string {
	if len(raw) == 0 {
		return "{}"
	}
	var obj map[string]interface{}
	if err := json.Unmarshal(raw, &obj); err == nil {
		b, _ := json.Marshal(obj)
		return string(b)
	}
	var str string
	if err := json.Unmarshal(raw, &str); err == nil {
		if strings.HasPrefix(strings.TrimSpace(str), "{") {
			return str
		}
		b, _ := json.Marshal(map[string]string{"input": str})
		return string(b)
	}
	return "{}"
}

// -----------------------------------------------------------------------------
// Transcript Reasoning & Tools Extractor
// -----------------------------------------------------------------------------
func extractReasoningFromTranscript(convID string) string {
	if convID == "" {
		return ""
	}
	var transcriptPath string
	for _, dir := range brainDirs {
		p := filepath.Join(dir, convID, ".system_generated/logs/transcript.jsonl")
		if _, err := os.Stat(p); err == nil {
			transcriptPath = p
			break
		}
	}
	if transcriptPath == "" {
		return ""
	}

	f, err := os.Open(transcriptPath)
	if err != nil {
		return ""
	}
	defer f.Close()

	var executedTools []string
	var thinkingBlocks []string

	scanner := bufio.NewScanner(f)
	buf := make([]byte, 1024*1024)
	scanner.Buffer(buf, 1024*1024)

	for scanner.Scan() {
		line := scanner.Bytes()
		var entry struct {
			Type      string `json:"type"`
			Thinking  string `json:"thinking"`
			ToolCalls []struct {
				Name string            `json:"name"`
				Args map[string]string `json:"args"`
			} `json:"tool_calls"`
		}
		if err := json.Unmarshal(line, &entry); err != nil {
			continue
		}
		if entry.Type == "USER_INPUT" {
			executedTools = executedTools[:0]
			thinkingBlocks = thinkingBlocks[:0]
		}
		if th := strings.TrimSpace(entry.Thinking); th != "" {
			thinkingBlocks = append(thinkingBlocks, th)
		}
		for _, tc := range entry.ToolCalls {
			switch tc.Name {
			case "run_command":
				cmd := tc.Args["CommandLine"]
				executedTools = append(executedTools, fmt.Sprintf("● Bash(%s)", cmd))
			case "view_file":
				p := filepath.Base(tc.Args["AbsolutePath"])
				executedTools = append(executedTools, fmt.Sprintf("● View(%s)", p))
			case "replace_file_content", "write_to_file":
				p := filepath.Base(tc.Args["TargetFile"])
				executedTools = append(executedTools, fmt.Sprintf("● Edit(%s)", p))
			case "manage_task":
				executedTools = append(executedTools, fmt.Sprintf("● Task(%s)", tc.Args["Action"]))
			case "search_web":
				executedTools = append(executedTools, fmt.Sprintf("● Search(%s)", tc.Args["query"]))
			default:
				executedTools = append(executedTools, fmt.Sprintf("● %s()", tc.Name))
			}
		}
	}

	var parts []string
	if len(executedTools) > 0 {
		parts = append(parts, strings.Join(executedTools, "\n"))
	}
	if len(thinkingBlocks) > 0 {
		parts = append(parts, strings.Join(thinkingBlocks, "\n\n"))
	}
	return strings.Join(parts, "\n\n")
}

// -----------------------------------------------------------------------------
// HTTP Handlers
// -----------------------------------------------------------------------------
type ChatCompletionRequest struct {
	Model           string        `json:"model"`
	Messages        []ChatMessage `json:"messages"`
	Stream          bool          `json:"stream"`
	Tools           []ToolItem    `json:"tools"`
	ReasoningEffort string        `json:"reasoning_effort"`
}

func handleHealth(w http.ResponseWriter, r *http.Request) {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	res := map[string]interface{}{
		"status":          "ok",
		"service":         "antigravity-go-proxy",
		"version":         Version,
		"uptime_seconds":  int(time.Since(startTime).Seconds()),
		"total_requests":  reqCount.Load(),
		"active_routines": runtime.NumGoroutine(),
		"mem_alloc_mb":    float64(m.Alloc) / 1024 / 1024,
		"mem_sys_mb":      float64(m.Sys) / 1024 / 1024,
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(res)
}

func handleMetrics(w http.ResponseWriter, r *http.Request) {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	w.Header().Set("Content-Type", "text/plain")
	fmt.Fprintf(w, "# HELP proxy_requests_total Total HTTP requests handled\n")
	fmt.Fprintf(w, "# TYPE proxy_requests_total counter\n")
	fmt.Fprintf(w, "proxy_requests_total %d\n", reqCount.Load())
	fmt.Fprintf(w, "# HELP proxy_mem_alloc_bytes Memory currently allocated\n")
	fmt.Fprintf(w, "# TYPE proxy_mem_alloc_bytes gauge\n")
	fmt.Fprintf(w, "proxy_mem_alloc_bytes %d\n", m.Alloc)
}

func handleModels(w http.ResponseWriter, r *http.Request) {
	models := registry.GetModels()
	type ModelEntry struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		OwnedBy string `json:"owned_by"`
		Name    string `json:"name"`
	}
	var data []ModelEntry
	for _, m := range models {
		data = append(data, ModelEntry{
			ID:      m.ID,
			Object:  "model",
			OwnedBy: "antigravity",
			Name:    m.Name,
		})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"object": "list",
		"data":   data,
	})
}

func handleModelInfo(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/v1/models/")
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"id":       id,
		"object":   "model",
		"owned_by": "antigravity",
	})
}

func handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	reqCount.Add(1)

	var req ChatCompletionRequest
	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "Invalid body", http.StatusBadRequest)
		return
	}
	if err := json.Unmarshal(bodyBytes, &req); err != nil {
		http.Error(w, "Malformed JSON", http.StatusBadRequest)
		return
	}

	model := req.Model
	if model == "" || model == "auto" {
		model = "gemini-3.8-flash-high"
	}

	sessionKey := extractSessionKey(req.Messages, r.Header)
	convID := r.Header.Get("X-Conversation-Id")
	if convID == "" {
		convID = getSessionID(sessionKey)
	}

	toolsPrompt := formatToolsPrompt(req.Tools)
	finalPrompt := formatConversation(req.Messages, toolsPrompt)

	createdTs := time.Now().Unix()
	completionID := fmt.Sprintf("chatcmpl-agy-%d", createdTs)

	// Build CLI arguments WITHOUT -p to completely avoid ARG_MAX (E2BIG)!
	// The prompt is passed via StdinPipe.
	args := []string{
		"--disable-slash-commands",
		"--dangerously-skip-permissions",
	}

	if req.Stream {
		args = append(args, "--output-format", "stream-json")
	} else {
		args = append(args, "--output-format", "json")
	}

	if model != "" {
		args = append(args, "--model", model)
	}

	effort := req.ReasoningEffort
	modelHasEffort := strings.HasSuffix(model, "-high") || strings.HasSuffix(model, "-medium") || strings.HasSuffix(model, "-low") || strings.HasSuffix(model, "-max")
	if !modelHasEffort {
		if effort == "" {
			effort = "medium"
		}
		args = append(args, "--effort", effort)
	}

	if convID != "" {
		args = append(args, "--conversation", convID)
	}

	if req.Stream {
		handleStreamingCompletion(w, r, args, completionID, model, createdTs, sessionKey, convID, finalPrompt)
	} else {
		handleNonStreamingCompletion(w, args, completionID, model, createdTs, sessionKey, convID, finalPrompt)
	}
}

// -----------------------------------------------------------------------------
// Streaming SSE Engine with Heartbeat & Error Recovery
// -----------------------------------------------------------------------------
func emitErrorAndDone(w http.ResponseWriter, flusher http.Flusher, id, model string, created int64, errMsg string) {
	chunk := map[string]interface{}{
		"id":      id,
		"object":  "chat.completion.chunk",
		"created": created,
		"model":   model,
		"choices": []map[string]interface{}{
			{
				"index": 0,
				"delta": map[string]interface{}{
					"content": fmt.Sprintf("\n\n[Antigravity Error: %s]", errMsg),
				},
				"finish_reason": "stop",
			},
		},
	}
	b, _ := json.Marshal(chunk)
	fmt.Fprintf(w, "data: %s\n\n", b)
	fmt.Fprintf(w, "data: [DONE]\n\n")
	flusher.Flush()
}

func handleStreamingCompletion(
	w http.ResponseWriter,
	r *http.Request,
	args []string,
	completionID, model string,
	createdTs int64,
	sessionKey, initialConvID, finalPrompt string,
) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "Streaming unsupported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	// Emit initial role chunk (<5ms)
	initChunk := map[string]interface{}{
		"id":      completionID,
		"object":  "chat.completion.chunk",
		"created": createdTs,
		"model":   model,
		"choices": []map[string]interface{}{
			{
				"index":         0,
				"delta":         map[string]interface{}{"role": "assistant"},
				"finish_reason": nil,
			},
		},
	}
	initBytes, _ := json.Marshal(initChunk)
	fmt.Fprintf(w, "data: %s\n\n", initBytes)
	flusher.Flush()

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	cmd := exec.CommandContext(ctx, agyBin, args...)
	cmd.Env = append(os.Environ(), "PATH="+filepath.Join(os.Getenv("HOME"), ".local/bin")+":"+os.Getenv("PATH"))

	stdin, err := cmd.StdinPipe()
	if err != nil {
		emitErrorAndDone(w, flusher, completionID, model, createdTs, fmt.Sprintf("StdinPipe error: %v", err))
		return
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		emitErrorAndDone(w, flusher, completionID, model, createdTs, fmt.Sprintf("StdoutPipe error: %v", err))
		return
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		emitErrorAndDone(w, flusher, completionID, model, createdTs, fmt.Sprintf("StderrPipe error: %v", err))
		return
	}

	var stderrBuf bytes.Buffer
	go func() {
		_, _ = io.Copy(&stderrBuf, stderr)
	}()

	if err := cmd.Start(); err != nil {
		log.Printf("[Proxy] Command start error: %v", err)
		emitErrorAndDone(w, flusher, completionID, model, createdTs, fmt.Sprintf("Process start error: %v", err))
		return
	}

	// Stream prompt through StdinPipe in background
	go func() {
		defer stdin.Close()
		_, _ = io.WriteString(stdin, finalPrompt)
	}()

	var accumulatedText strings.Builder
	activeConvID := initialConvID
	isBufferingTool := false
	var toolBuffer strings.Builder

	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	var lastWrite atomic.Int64
	lastWrite.Store(time.Now().Unix())

	go func() {
		for {
			select {
			case <-ticker.C:
				if time.Now().Unix()-lastWrite.Load() >= 10 {
					fmt.Fprintf(w, ": ping\n\n")
					flusher.Flush()
				}
			case <-ctx.Done():
				return
			}
		}
	}()

	scanner := bufio.NewScanner(stdout)
	buf := make([]byte, 1024*1024)
	scanner.Buffer(buf, 1024*1024)

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		var event struct {
			Event          string `json:"event"`
			ConversationID string `json:"conversation_id"`
			StepUpdate     struct {
				TextDelta string `json:"text_delta"`
			} `json:"step_update"`
			Result struct {
				ConversationID string `json:"conversation_id"`
			} `json:"result"`
		}

		if err := json.Unmarshal(line, &event); err != nil {
			continue
		}

		if event.Event == "init" && event.ConversationID != "" {
			activeConvID = event.ConversationID
			if sessionKey != "" {
				saveSessionMap(sessionKey, activeConvID)
			}
		} else if event.Event == "result" && event.Result.ConversationID != "" {
			activeConvID = event.Result.ConversationID
			if sessionKey != "" {
				saveSessionMap(sessionKey, activeConvID)
			}
		} else if event.Event == "step_update" {
			delta := event.StepUpdate.TextDelta
			if delta != "" {
				accumulatedText.WriteString(delta)

				if strings.Contains(delta, "<tool_call>") || strings.Contains(delta, "<function_call>") || strings.Contains(delta, "call:") {
					isBufferingTool = true
				}

				if isBufferingTool {
					toolBuffer.WriteString(delta)
				} else {
					chunk := map[string]interface{}{
						"id":      completionID,
						"object":  "chat.completion.chunk",
						"created": createdTs,
						"model":   model,
						"choices": []map[string]interface{}{
							{
								"index":         0,
								"delta":         map[string]interface{}{"content": delta},
								"finish_reason": nil,
							},
						},
					}
					b, _ := json.Marshal(chunk)
					fmt.Fprintf(w, "data: %s\n\n", b)
					flusher.Flush()
					lastWrite.Store(time.Now().Unix())
				}
			}
		}
	}

	waitErr := cmd.Wait()
	if waitErr != nil && accumulatedText.Len() == 0 {
		errDetails := strings.TrimSpace(stderrBuf.String())
		if errDetails == "" {
			errDetails = waitErr.Error()
		}
		emitErrorAndDone(w, flusher, completionID, model, createdTs, errDetails)
		return
	}

	fullOutput := accumulatedText.String()
	cleanText, toolCalls := parseToolCalls(fullOutput)

	reasoning := ""
	if activeConvID != "" {
		reasoning = extractReasoningFromTranscript(activeConvID)
	}
	if thinkMatches := thinkRegex.FindStringSubmatch(cleanText); len(thinkMatches) > 1 {
		inThink := strings.TrimSpace(thinkMatches[1])
		cleanText = strings.TrimSpace(thinkRegex.ReplaceAllString(cleanText, ""))
		if reasoning != "" {
			reasoning = reasoning + "\n\n" + inThink
		} else {
			reasoning = inThink
		}
	}

	if reasoning != "" {
		rChunk := map[string]interface{}{
			"id":      completionID,
			"object":  "chat.completion.chunk",
			"created": createdTs,
			"model":   model,
			"choices": []map[string]interface{}{
				{
					"index":         0,
					"delta":         map[string]interface{}{"reasoning_content": reasoning},
					"finish_reason": nil,
				},
			},
		}
		rb, _ := json.Marshal(rChunk)
		fmt.Fprintf(w, "data: %s\n\n", rb)
		flusher.Flush()
	}

	if len(toolCalls) > 0 {
		tcChunk := map[string]interface{}{
			"id":      completionID,
			"object":  "chat.completion.chunk",
			"created": createdTs,
			"model":   model,
			"choices": []map[string]interface{}{
				{
					"index":         0,
					"delta":         map[string]interface{}{"tool_calls": toolCalls},
					"finish_reason": nil,
				},
			},
		}
		tcb, _ := json.Marshal(tcChunk)
		fmt.Fprintf(w, "data: %s\n\n", tcb)

		finChunk := map[string]interface{}{
			"id":      completionID,
			"object":  "chat.completion.chunk",
			"created": createdTs,
			"model":   model,
			"choices": []map[string]interface{}{
				{
					"index":         0,
					"delta":         map[string]interface{}{},
					"finish_reason": "tool_calls",
				},
			},
		}
		finBytes, _ := json.Marshal(finChunk)
		fmt.Fprintf(w, "data: %s\n\n", finBytes)
	} else {
		if isBufferingTool && cleanText != "" {
			txtChunk := map[string]interface{}{
				"id":      completionID,
				"object":  "chat.completion.chunk",
				"created": createdTs,
				"model":   model,
				"choices": []map[string]interface{}{
					{
						"index":         0,
						"delta":         map[string]interface{}{"content": cleanText},
						"finish_reason": nil,
					},
				},
			}
			tb, _ := json.Marshal(txtChunk)
			fmt.Fprintf(w, "data: %s\n\n", tb)
		}

		stopChunk := map[string]interface{}{
			"id":      completionID,
			"object":  "chat.completion.chunk",
			"created": createdTs,
			"model":   model,
			"choices": []map[string]interface{}{
				{
					"index":         0,
					"delta":         map[string]interface{}{},
					"finish_reason": "stop",
				},
			},
		}
		sb, _ := json.Marshal(stopChunk)
		fmt.Fprintf(w, "data: %s\n\n", sb)
	}

	fmt.Fprintf(w, "data: [DONE]\n\n")
	flusher.Flush()
}

// -----------------------------------------------------------------------------
// Non-Streaming Completion Engine
// -----------------------------------------------------------------------------
func handleNonStreamingCompletion(
	w http.ResponseWriter,
	args []string,
	completionID, model string,
	createdTs int64,
	sessionKey, initialConvID, finalPrompt string,
) {
	ctx, cancel := context.WithTimeout(context.Background(), DefaultTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, agyBin, args...)
	cmd.Env = append(os.Environ(), "PATH="+filepath.Join(os.Getenv("HOME"), ".local/bin")+":"+os.Getenv("PATH"))
	cmd.Stdin = strings.NewReader(finalPrompt)

	var stdoutBuf, stderrBuf bytes.Buffer
	cmd.Stdout = &stdoutBuf
	cmd.Stderr = &stderrBuf

	if err := cmd.Run(); err != nil {
		log.Printf("[Proxy] Non-stream run error: %v, stderr: %s", err, stderrBuf.String())
		http.Error(w, fmt.Sprintf("AGY Error: %v: %s", err, stderrBuf.String()), http.StatusInternalServerError)
		return
	}

	var agyRes struct {
		ConversationID string `json:"conversation_id"`
		Status         string `json:"status"`
		Response       string `json:"response"`
		Usage          struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
			TotalTokens  int `json:"total_tokens"`
		} `json:"usage"`
	}

	rawOut := stdoutBuf.Bytes()
	_ = json.Unmarshal(rawOut, &agyRes)

	respText := agyRes.Response
	if respText == "" {
		respText = string(rawOut)
	}

	activeConvID := agyRes.ConversationID
	if activeConvID == "" {
		activeConvID = initialConvID
	}
	if activeConvID != "" && sessionKey != "" {
		saveSessionMap(sessionKey, activeConvID)
	}

	cleanText, toolCalls := parseToolCalls(respText)
	finishReason := "stop"
	if len(toolCalls) > 0 {
		finishReason = "tool_calls"
	}

	reasoning := ""
	if activeConvID != "" {
		reasoning = extractReasoningFromTranscript(activeConvID)
	}
	if thinkMatches := thinkRegex.FindStringSubmatch(cleanText); len(thinkMatches) > 1 {
		inThink := strings.TrimSpace(thinkMatches[1])
		cleanText = strings.TrimSpace(thinkRegex.ReplaceAllString(cleanText, ""))
		if reasoning != "" {
			reasoning = reasoning + "\n\n" + inThink
		} else {
			reasoning = inThink
		}
	}

	msgPayload := map[string]interface{}{
		"role": "assistant",
	}
	if cleanText != "" {
		msgPayload["content"] = cleanText
	} else if len(toolCalls) == 0 {
		msgPayload["content"] = ""
	} else {
		msgPayload["content"] = nil
	}

	if reasoning != "" {
		msgPayload["reasoning_content"] = reasoning
	}

	if len(toolCalls) > 0 {
		msgPayload["tool_calls"] = toolCalls
	}

	inTok := agyRes.Usage.InputTokens
	outTok := agyRes.Usage.OutputTokens
	if inTok == 0 {
		inTok = len(finalPrompt) / 4
	}
	if outTok == 0 {
		outTok = len(respText) / 4
	}

	res := map[string]interface{}{
		"id":      completionID,
		"object":  "chat.completion",
		"created": createdTs,
		"model":   model,
		"choices": []map[string]interface{}{
			{
				"index":         0,
				"message":       msgPayload,
				"finish_reason": finishReason,
			},
		},
		"usage": map[string]interface{}{
			"prompt_tokens":     inTok,
			"completion_tokens": outTok,
			"total_tokens":      inTok + outTok,
		},
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(res)
}

// -----------------------------------------------------------------------------
// Ollama Routes
// -----------------------------------------------------------------------------
func handleOllamaShow(w http.ResponseWriter, r *http.Request) {
	var body map[string]interface{}
	_ = json.NewDecoder(r.Body).Decode(&body)
	mName := "gemini-3.8-flash-high"
	if n, ok := body["name"].(string); ok && n != "" {
		mName = n
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"modelfile":  fmt.Sprintf("FROM %s\nPARAMETER temperature 0.7", mName),
		"parameters": "temperature 0.7",
		"template":   "{{ .Prompt }}",
		"details":    map[string]string{"format": "agy", "family": "antigravity"},
	})
}

func handleOllamaTags(w http.ResponseWriter, r *http.Request) {
	models := registry.GetModels()
	var list []map[string]interface{}
	for _, m := range models {
		list = append(list, map[string]interface{}{
			"name":    m.ID,
			"model":   m.ID,
			"details": map[string]string{"family": "antigravity"},
		})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{"models": list})
}

// -----------------------------------------------------------------------------
// Main Entrypoint
// -----------------------------------------------------------------------------
func main() {
	loadSessionMap()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", handleHealth)
	mux.HandleFunc("GET /metrics", handleMetrics)
	mux.HandleFunc("GET /v1/models", handleModels)
	mux.HandleFunc("GET /api/v1/models", handleModels)
	mux.HandleFunc("GET /v1/models/", handleModelInfo)
	mux.HandleFunc("POST /v1/chat/completions", handleChatCompletions)
	mux.HandleFunc("POST /api/show", handleOllamaShow)
	mux.HandleFunc("GET /api/tags", handleOllamaTags)

	server := &http.Server{
		Addr:         ":" + Port,
		Handler:      mux,
		ReadTimeout:  180 * time.Second,
		WriteTimeout: 180 * time.Second,
		IdleTimeout:  300 * time.Second,
	}

	log.Printf("[Hermesgravity] Service listening on :%s (RSS: ~4.9MB)", Port)
	if err := server.ListenAndServe(); err != nil {
		log.Fatalf("Server error: %v", err)
	}
}
