package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// -----------------------------------------------------------------------------
// Constants & Configuration
// -----------------------------------------------------------------------------
const (
	DefaultHost    = "127.0.0.1"
	DefaultPort    = "20130"
	Version        = "4.0-anygravity"
	DefaultTimeout = 180 * time.Second
	StreamTimeout  = 600 * time.Second
)

func getHost() string {
	if h := os.Getenv("HOST"); h != "" {
		return h
	}
	return DefaultHost
}

func getPort() string {
	if p := os.Getenv("PORT"); p != "" {
		return p
	}
	return DefaultPort
}

func getAPIKey() string {
	if k := os.Getenv("ANYGRAVITY_API_KEY"); k != "" {
		return k
	}
	if k := os.Getenv("HERMESGRAVITY_API_KEY"); k != "" {
		return k
	}
	return os.Getenv("API_KEY")
}

var (
	agyBin       = filepath.Join(os.Getenv("HOME"), ".local/bin/agy")
	workspaceDir = filepath.Join(os.Getenv("HOME"), ".hermes/anygravity_workspace")

	fallbackModels = []ModelItem{
		{ID: "gemini-3.8-flash", Name: "Gemini 3.8 Flash"},
		{ID: "gemini-3.7-flash", Name: "Gemini 3.7 Flash"},
		{ID: "gemini-3.6-flash", Name: "Gemini 3.6 Flash"},
		{ID: "gemini-3.1-pro", Name: "Gemini 3.1 Pro"},
		{ID: "claude-sonnet-4-6", Name: "Claude Sonnet 4.6"},
		{ID: "claude-opus-4-6-thinking", Name: "Claude Opus 4.6"},
		{ID: "gpt-oss-120b-medium", Name: "GPT-OSS 120B"},
	}

	agySem = make(chan struct{}, getMaxConcurrency())

	brainDirs = []string{
		filepath.Join(os.Getenv("HOME"), ".gemini/antigravity-cli/brain"),
		filepath.Join(os.Getenv("HOME"), ".gemini/antigravity/brain"),
	}

	startTime = time.Now()
	reqCount  atomic.Uint64
)

func getMaxConcurrency() int {
	if val := os.Getenv("AGY_MAX_CONCURRENCY"); val != "" {
		var n int
		if _, err := fmt.Sscanf(val, "%d", &n); err == nil && n > 0 {
			return n
		}
	}
	return 4 // default 4 parallel execution slots
}

type ModelItem struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// -----------------------------------------------------------------------------
// Workspace & Customization Guard
// -----------------------------------------------------------------------------
func initWorkspace() {
	agentsDir := filepath.Join(workspaceDir, ".agents")
	_ = os.MkdirAll(agentsDir, 0755)

	geminiMd := filepath.Join(workspaceDir, "GEMINI.md")
	geminiContent := `# ANYGRAVITY COMPLETION ENGINE
You are acting as the backend AI completion model for an external AI Agent.
You MUST NOT execute any local system tools or commands directly.
When a tool is needed, respond ONLY with:
<tool_call>
{"name": "tool_name", "arguments": {...}}
</tool_call>
`
	_ = os.WriteFile(geminiMd, []byte(geminiContent), 0644)

	hooksJson := filepath.Join(agentsDir, "hooks.json")
	hooksContent := `{
  "deny-all-tools": {
    "PreToolUse": [
      {
        "matcher": "*",
        "hooks": [
          {
            "command": "echo '{\"decision\": \"deny\", \"reason\": \"Local tool execution is disabled by Anygravity. Output tool calls as text <tool_call>...\"}'"
          }
        ]
      }
    ]
  }
}
`
	_ = os.WriteFile(hooksJson, []byte(hooksContent), 0644)
}

// -----------------------------------------------------------------------------
// Session Tracking & Delta State Engine
// -----------------------------------------------------------------------------

type SessionState struct {
	ConvID        string    `json:"conv_id"`
	Model         string    `json:"model"`
	CreatedAt     time.Time `json:"created_at"`
	LastSeen      time.Time `json:"last_seen"`
	MessageCount  int       `json:"message_count"`
	HistoryHashes []string  `json:"history_hashes"`
	ToolsHash     string    `json:"tools_hash"`
	TotalTurns    int       `json:"total_turns"`
}

type SessionManager struct {
	mu       sync.RWMutex
	sessions map[string]*SessionState
	filePath string
	trigger  chan struct{}
}

var globalSessions = &SessionManager{
	sessions: make(map[string]*SessionState),
	filePath: filepath.Join(os.Getenv("HOME"), ".hermes/antigravity_session_map.json"),
	trigger:  make(chan struct{}, 1),
}

func initSessionManager(ctx context.Context) {
	globalSessions.load()
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-globalSessions.trigger:
				globalSessions.flush()
			case <-ticker.C:
				globalSessions.cleanupExpired()
			case <-ctx.Done():
				globalSessions.flush()
				return
			}
		}
	}()
}

func (sm *SessionManager) load() {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	data, err := os.ReadFile(sm.filePath)
	if err == nil {
		_ = json.Unmarshal(data, &sm.sessions)
	}
}

func (sm *SessionManager) flush() {
	sm.mu.RLock()
	data, err := json.MarshalIndent(sm.sessions, "", "  ")
	sm.mu.RUnlock()
	if err == nil {
		if writeErr := os.WriteFile(sm.filePath, data, 0644); writeErr != nil {
			log.Printf("[SessionManager] flush error: %v", writeErr)
		}
	}
}

func (sm *SessionManager) notifySave() {
	select {
	case sm.trigger <- struct{}{}:
	default:
	}
}

func (sm *SessionManager) cleanupExpired() {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	now := time.Now()
	changed := false
	for k, s := range sm.sessions {
		if now.Sub(s.LastSeen) > 4*time.Hour {
			delete(sm.sessions, k)
			changed = true
		}
	}
	if changed {
		sm.notifySave()
	}
}

func (sm *SessionManager) Get(key string) *SessionState {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	if s, ok := sm.sessions[key]; ok {
		cp := *s
		cp.HistoryHashes = append([]string(nil), s.HistoryHashes...)
		return &cp
	}
	return nil
}

func (sm *SessionManager) Update(key string, state *SessionState) {
	sm.mu.Lock()
	state.LastSeen = time.Now()
	oldState := sm.sessions[key]
	sm.sessions[key] = state
	if len(sm.sessions) > 500 {
		type sessionAge struct {
			key      string
			lastSeen time.Time
		}
		var list []sessionAge
		for k, v := range sm.sessions {
			if k != key {
				list = append(list, sessionAge{key: k, lastSeen: v.LastSeen})
			}
		}
		sort.Slice(list, func(i, j int) bool {
			return list[i].lastSeen.Before(list[j].lastSeen)
		})
		toRemove := len(sm.sessions) - 400
		if toRemove > len(list) {
			toRemove = len(list)
		}
		for i := 0; i < toRemove; i++ {
			delete(sm.sessions, list[i].key)
		}
	}
	sm.mu.Unlock()
	sm.notifySave()

	if oldState != nil && oldState.ConvID != "" && oldState.ConvID != state.ConvID {
		log.Printf("[Proxy-Cleanup] Session %s migrated from conv %s to %s. Removing old conv folder.", key, oldState.ConvID, state.ConvID)
		go func(oldCID string) {
			time.Sleep(3 * time.Second)
			sm.mu.RLock()
			inUse := false
			for _, s := range sm.sessions {
				if s.ConvID == oldCID {
					inUse = true
					break
				}
			}
			sm.mu.RUnlock()
			if inUse {
				return
			}
			for _, bDir := range brainDirs {
				p := filepath.Join(bDir, oldCID)
				if _, err := os.Stat(p); err == nil {
					_ = os.RemoveAll(p)
				}
			}
		}(oldState.ConvID)
	}
}

func (sm *SessionManager) Invalidate(key string) {
	sm.mu.Lock()
	delete(sm.sessions, key)
	sm.mu.Unlock()
	sm.notifySave()
}

func hashMessage(m ChatMessage) string {
	h := sha256.New()
	sep := []byte{0}
	h.Write([]byte(m.Role))
	h.Write(sep)
	h.Write([]byte(m.Name))
	h.Write(sep)
	h.Write([]byte(m.ToolCallID))
	h.Write(sep)
	h.Write(m.Content)
	h.Write(sep)
	h.Write(m.ToolCalls)
	return hex.EncodeToString(h.Sum(nil)[:8])
}

func hashTools(tools []ToolItem) string {
	if len(tools) == 0 {
		return ""
	}
	b, _ := json.Marshal(tools)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:8])
}

func isAuxiliaryRequest(messages []ChatMessage, tools []ToolItem) bool {
	if len(messages) == 0 {
		return false
	}
	checkMsg := func(m ChatMessage) bool {
		c := strings.ToLower(m.ContentString())
		return strings.Contains(c, "you name chat sessions") ||
			strings.Contains(c, "conversation summarizer") ||
			strings.Contains(c, "extract facts") ||
			strings.Contains(c, "generate a title") ||
			strings.Contains(c, "the following command was flagged as:") ||
			strings.Contains(c, "command execution confirmation") ||
			strings.Contains(c, "review the conversation above and update") ||
			strings.Contains(c, "update the skill library") ||
			strings.Contains(c, "update two things:\n\n**memory**") ||
			strings.Contains(c, "memory extraction")
	}
	if checkMsg(messages[0]) {
		return true
	}
	start := 0
	if len(messages) > 10 {
		start = len(messages) - 10
	}
	for _, m := range messages[start:] {
		if checkMsg(m) {
			return true
		}
	}
	return false
}

func extractSessionKey(messages []ChatMessage, tools []ToolItem, headers http.Header) (key string, isAux bool) {
	if isAuxiliaryRequest(messages, tools) {
		return "", true
	}

	for _, h := range []string{"X-Session-Id", "X-Hermes-Session-Id", "X-Conversation-Id"} {
		if val := headers.Get(h); val != "" {
			return "hdr_" + strings.TrimSpace(val), false
		}
	}

	systemText := ""
	firstUser := ""
	for _, m := range messages {
		if m.Role == "system" && systemText == "" {
			systemText = m.ContentString()
			if len(systemText) > 250 {
				systemText = systemText[:250]
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

	h := sha256.New()
	h.Write([]byte(systemText))
	h.Write([]byte("|"))
	h.Write([]byte(firstUser))
	return hex.EncodeToString(h.Sum(nil)[:16]), false
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
	defer r.mu.RUnlock()
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

[CRITICAL INSTRUCTIONS FOR AI AGENT COMPLETION]
You are acting as the backend AI completion model for an external AI Agent.
- You DO NOT have permission to use or execute any local system tools directly.
- When an action or information retrieval is genuinely needed, choose from [AVAILABLE TOOLS] and respond ONLY with:
<tool_call>
{"name": "tool_name", "arguments": {"param": "value"}}
</tool_call>
- Do NOT output commentary or conversational filler before or after the <tool_call> tag when calling a tool.
- IMPORTANT COMPLETION RULE: Once the tool output provides the necessary data or the requested action has been performed, DO NOT call any more tools. Respond directly to the user in normal, helpful text with your final answer, confirmation, or summary.
`, string(b))
}

func formatConversation(messages []ChatMessage, toolsPrompt string) string {
	var sb strings.Builder
	if toolsPrompt != "" {
		sb.WriteString(toolsPrompt)
		sb.WriteString("\n\n")
	}

	totalMsgs := len(messages)
	for i, msg := range messages {
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
			// If tool result is historical (not in the last 4 messages) and large (> 4000 chars),
			// truncate the middle to keep prompt size manageable and prevent gRPC stream stall.
			if totalMsgs-i > 4 && len(c) > 4000 {
				c = c[:2000] + "\n\n[... content truncated for brevity ...]\n\n" + c[len(c)-1000:]
			}
			sb.WriteString(fmt.Sprintf("[Tool Result for '%s']:\n%s\n\n", toolName, c))
		}
	}

	if toolsPrompt != "" {
		sb.WriteString("[CRITICAL REMINDER]\n")
		sb.WriteString("You are the completion engine for the AI Agent. Do NOT invoke local tools directly.\n")
		sb.WriteString("- If an action/tool call is genuinely needed, respond ONLY with:\n")
		sb.WriteString("<tool_call>\n{\"name\": \"tool_name\", \"arguments\": {...}}\n</tool_call>\n")
		sb.WriteString("- If the user request is satisfied or no further tool is needed, DO NOT call tools. Respond directly in normal text with your final response.\n\n")
	}

	sb.WriteString("Assistant:")
	return sb.String()
}

func formatDeltaPrompt(messages []ChatMessage, startIndex int) string {
	var sb strings.Builder

	for i := startIndex; i < len(messages); i++ {
		msg := messages[i]
		c := msg.ContentString()

		// If the first message in the delta is the assistant message from the immediately
		// preceding turn, AGY already has it recorded in its transcript as PLANNER_RESPONSE.
		// Skip it to avoid duplicate assistant monologue/tool tags!
		if i == startIndex && msg.Role == "assistant" {
			continue
		}

		switch msg.Role {
		case "tool":
			toolName := msg.Name
			if toolName == "" {
				toolName = "tool"
			}
			if len(c) > 4000 {
				c = c[:2000] + "\n\n[... content truncated for brevity ...]\n\n" + c[len(c)-1000:]
			}
			sb.WriteString(fmt.Sprintf("[Tool Result for '%s']:\n%s\n\n", toolName, c))

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
		}
	}

	sb.WriteString("[CRITICAL REMINDER]\n")
	sb.WriteString("You are the completion engine for the AI Agent. Do NOT invoke local tools directly.\n")
	sb.WriteString("- If an action/tool call is genuinely needed, respond ONLY with:\n")
	sb.WriteString("<tool_call>\n{\"name\": \"tool_name\", \"arguments\": {...}}\n</tool_call>\n")
	sb.WriteString("- If the previous tool result is sufficient or the task is finished, DO NOT call more tools. Respond directly to the user in normal text with your final response.\n\n")
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
	tagRegex        = regexp.MustCompile(`(?s)<(?:tool_call|function_call)>(.*?)</(?:tool_call|function_call)>`)
	blockRegex      = regexp.MustCompile("(?s)```(?:tool_call|tool_code|json|python)?\\s*(?:<tool_call>)?\\s*({[\\s\\S]*?\"name\"\\s*:\\s*\"[^\"]+\"[\\s\\S]*?})\\s*(?:</tool_call>)?\\s*```")
	bareToolRegex   = regexp.MustCompile(`(?s)(?:^|\n)\s*(?:tool_call|function_call):?\s*(\{\s*"name"\s*:\s*"[^"]+"[\s\S]*?\})`)
	bareMarkerRegex = regexp.MustCompile(`(?s)(?:^|\n)\s*(?:tool_call|function_call):?\s*\{`)
	internalRegex   = regexp.MustCompile(`call:(?:default_api:)?([a-zA-Z0-9_-]+)\{([^}]+)\}`)
	thinkRegex      = regexp.MustCompile(`(?s)<(?:think|thought)>(.*?)</(?:think|thought)>`)
	toolCallCounter atomic.Uint64
)

func extractJSONObject(s string) (string, int) {
	start := strings.Index(s, "{")
	if start == -1 {
		return "", -1
	}
	depth := 0
	inString := false
	escaped := false

	for i := start; i < len(s); i++ {
		ch := s[i]
		if inString {
			if escaped {
				escaped = false
			} else if ch == '\\' {
				escaped = true
			} else if ch == '"' {
				inString = false
			}
		} else {
			if ch == '"' {
				inString = true
			} else if ch == '{' {
				depth++
			} else if ch == '}' {
				depth--
				if depth == 0 {
					return s[start : i+1], i + 1
				}
			}
		}
	}
	return "", -1
}

func nextToolCallID() string {
	n := toolCallCounter.Add(1)
	return fmt.Sprintf("call_%d_%x", n, time.Now().UnixNano()&0xffff)
}

func sanitizeJSON(raw string) string {
	var sb strings.Builder
	sb.Grow(len(raw) + 64)
	inString := false
	n := len(raw)
	for i := 0; i < n; i++ {
		ch := raw[i]
		if !inString {
			if ch == '"' {
				inString = true
			}
			sb.WriteByte(ch)
		} else {
			if ch == '\\' {
				if i+1 < n {
					nxt := raw[i+1]
					switch nxt {
					case '"', '\\', '/', 'b', 'f', 'n', 'r', 't':
						sb.WriteByte('\\')
						sb.WriteByte(nxt)
						i++
					case 'u':
						isHex := func(b byte) bool {
							return (b >= '0' && b <= '9') || (b >= 'a' && b <= 'f') || (b >= 'A' && b <= 'F')
						}
						if i+5 < n && isHex(raw[i+2]) && isHex(raw[i+3]) && isHex(raw[i+4]) && isHex(raw[i+5]) {
							sb.WriteByte('\\')
							sb.WriteByte('u')
							sb.WriteByte(raw[i+2])
							sb.WriteByte(raw[i+3])
							sb.WriteByte(raw[i+4])
							sb.WriteByte(raw[i+5])
							i += 5
						} else {
							sb.WriteString(`\\`)
							sb.WriteByte('u')
							i++
						}
					default:
						// Invalid escape sequence like \` or \' or \$
						// Treat the backslash as literal by escaping it.
						sb.WriteString(`\\`)
						sb.WriteByte(nxt)
						i++
					}
				} else {
					sb.WriteString(`\\`)
				}
			} else if ch == '"' {
				inString = false
				sb.WriteByte(ch)
			} else if ch == '\n' {
				sb.WriteString(`\n`)
			} else if ch == '\r' {
				sb.WriteString(`\r`)
			} else if ch == '\t' {
				sb.WriteString(`\t`)
			} else {
				sb.WriteByte(ch)
			}
		}
	}
	return sb.String()
}

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
		if err := json.Unmarshal([]byte(raw), &single); err != nil {
			sanitized := sanitizeJSON(raw)
			_ = json.Unmarshal([]byte(sanitized), &single)
		}
		if single.Name != "" {
			tc := ParsedToolCall{
				Index: len(toolCalls),
				ID:    nextToolCallID(),
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
		if err := json.Unmarshal([]byte(raw), &single); err != nil {
			sanitized := sanitizeJSON(raw)
			_ = json.Unmarshal([]byte(sanitized), &single)
		}
		if single.Name != "" {
			tc := ParsedToolCall{
				Index: len(toolCalls),
				ID:    nextToolCallID(),
				Type:  "function",
			}
			tc.Function.Name = strings.TrimSpace(single.Name)
			tc.Function.Arguments = normalizeArgs(single.Arguments)
			toolCalls = append(toolCalls, tc)
		}
	}
	cleaned = blockRegex.ReplaceAllString(cleaned, "")

	for {
		loc := bareMarkerRegex.FindStringIndex(cleaned)
		if loc == nil {
			break
		}
		braceIdx := loc[1] - 1
		jsonStr, endOffset := extractJSONObject(cleaned[braceIdx:])
		if endOffset == -1 {
			break
		}
		var single struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal([]byte(jsonStr), &single); err != nil {
			sanitized := sanitizeJSON(jsonStr)
			_ = json.Unmarshal([]byte(sanitized), &single)
		}
		if single.Name != "" {
			tc := ParsedToolCall{
				Index: len(toolCalls),
				ID:    nextToolCallID(),
				Type:  "function",
			}
			tc.Function.Name = strings.TrimSpace(single.Name)
			tc.Function.Arguments = normalizeArgs(single.Arguments)
			toolCalls = append(toolCalls, tc)
		}
		cleaned = cleaned[:loc[0]] + cleaned[braceIdx+endOffset:]
	}

	iMatches := internalRegex.FindAllStringSubmatch(cleaned, -1)
	for _, m := range iMatches {
		tname := m[1]
		argsStr := "{" + m[2] + "}"
		tc := ParsedToolCall{
			Index: len(toolCalls),
			ID:    nextToolCallID(),
			Type:  "function",
		}
		tc.Function.Name = strings.TrimSpace(tname)
		tc.Function.Arguments = argsStr
		toolCalls = append(toolCalls, tc)
	}
	cleaned = internalRegex.ReplaceAllString(cleaned, "")

	cleanText = strings.TrimSpace(cleaned)
	// When calling tools, any preamble monologue is model thinking/reasoning,
	// never user-facing chat text. Clear cleanText so it does not leak to chat.
	if len(toolCalls) > 1 {
		var uniqueCalls []ParsedToolCall
		seen := make(map[string]bool)
		for _, tc := range toolCalls {
			key := tc.Function.Name + "|" + tc.Function.Arguments
			if !seen[key] {
				seen[key] = true
				tc.Index = len(uniqueCalls)
				uniqueCalls = append(uniqueCalls, tc)
			}
		}
		toolCalls = uniqueCalls
	}

	if len(toolCalls) > 0 {
		cleanText = ""
	} else if cleanText == "" && strings.TrimSpace(text) != "" {
		// If no tool call was successfully parsed, don't drop the model's text into an empty response!
		cleanText = strings.TrimSpace(text)
	}

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
		trimmed := strings.TrimSpace(str)
		if strings.HasPrefix(trimmed, "{") {
			return trimmed
		}
		b, _ := json.Marshal(map[string]string{"input": str})
		return string(b)
	}
	return "{}"
}

// -----------------------------------------------------------------------------
// Transcript Reasoning & Tools Extractor (Cached & Heap-Optimized)
// -----------------------------------------------------------------------------
type transcriptCacheEntry struct {
	modTime    time.Time
	fileSize   int64
	reasoning  string
	accessedAt time.Time
}

var (
	tCacheMu sync.RWMutex
	tCache   = make(map[string]transcriptCacheEntry)

	scanBufPool = sync.Pool{
		New: func() interface{} {
			b := make([]byte, 256*1024)
			return &b
		},
	}
)

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

	fi, err := os.Stat(transcriptPath)
	if err != nil {
		return ""
	}

	tCacheMu.RLock()
	cached, found := tCache[convID]
	if found && cached.modTime.Equal(fi.ModTime()) && cached.fileSize == fi.Size() {
		tCacheMu.RUnlock()
		return cached.reasoning
	}
	tCacheMu.RUnlock()

	f, err := os.Open(transcriptPath)
	if err != nil {
		return ""
	}
	defer f.Close()

	var executedTools []string
	var thinkingBlocks []string

	scanner := bufio.NewScanner(f)
	bufPtr := scanBufPool.Get().(*[]byte)
	defer scanBufPool.Put(bufPtr)
	scanner.Buffer(*bufPtr, 1024*1024)

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
			cleanTh, _ := parseToolCalls(th)
			cleanTh = strings.TrimSpace(cleanTh)
			if cleanTh != "" {
				thinkingBlocks = append(thinkingBlocks, cleanTh)
			}
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
	res := strings.Join(parts, "\n\n")

	tCacheMu.Lock()
	if len(tCache) > 100 {
		type cacheAge struct {
			key        string
			accessedAt time.Time
		}
		var list []cacheAge
		for k, v := range tCache {
			if k != convID {
				list = append(list, cacheAge{key: k, accessedAt: v.accessedAt})
			}
		}
		sort.Slice(list, func(i, j int) bool {
			return list[i].accessedAt.Before(list[j].accessedAt)
		})
		toRemove := len(tCache) - 50
		if toRemove > len(list) {
			toRemove = len(list)
		}
		for i := 0; i < toRemove; i++ {
			delete(tCache, list[i].key)
		}
	}
	tCache[convID] = transcriptCacheEntry{
		modTime:    fi.ModTime(),
		fileSize:   fi.Size(),
		reasoning:  res,
		accessedAt: time.Now(),
	}
	tCacheMu.Unlock()

	return res
}

// -----------------------------------------------------------------------------
// HTTP Handlers
// -----------------------------------------------------------------------------
type StreamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type ChatCompletionRequest struct {
	Model           string         `json:"model"`
	Messages        []ChatMessage  `json:"messages"`
	Stream          bool           `json:"stream"`
	StreamOptions   *StreamOptions `json:"stream_options,omitempty"`
	Tools           []ToolItem     `json:"tools"`
	ReasoningEffort string         `json:"reasoning_effort"`
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
	id = strings.TrimSpace(id)
	for _, m := range registry.GetModels() {
		if strings.EqualFold(m.ID, id) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"id":       m.ID,
				"object":   "model",
				"owned_by": "antigravity",
				"name":     m.Name,
			})
			return
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusNotFound)
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"error": map[string]string{
			"message": fmt.Sprintf("Model %q not found", id),
			"type":    "invalid_request_error",
			"code":    "model_not_found",
		},
	})
}

func resolveModelAndEffort(rawModel, rawEffort string) (model string, effort string, err error) {
	m := rawModel
	if strings.Contains(m, "/") {
		parts := strings.Split(m, "/")
		m = parts[len(parts)-1]
	}
	m = strings.TrimPrefix(m, "custom:")
	m = strings.TrimPrefix(m, "openai-")
	m = strings.TrimSpace(strings.ToLower(m))

	if m == "" || m == "auto" || m == "default" {
		m = "gemini-3.8-flash"
	}

	eff := strings.ToLower(strings.TrimSpace(rawEffort))
	eff = strings.ReplaceAll(eff, "-", "")
	eff = strings.ReplaceAll(eff, "_", "")
	eff = strings.ReplaceAll(eff, " ", "")

	// Extract embedded effort from model name suffix if present
	var embeddedEffort string
	switch {
	case strings.HasSuffix(m, "-high"):
		embeddedEffort = "high"
		m = strings.TrimSuffix(m, "-high")
	case strings.HasSuffix(m, "-medium") || strings.HasSuffix(m, "-med"):
		embeddedEffort = "medium"
		m = strings.TrimSuffix(m, "-medium")
		m = strings.TrimSuffix(m, "-med")
	case strings.HasSuffix(m, "-low"):
		embeddedEffort = "low"
		m = strings.TrimSuffix(m, "-low")
	}

	// Resolve base model
	switch {
	case strings.Contains(m, "gemini-3.8") || strings.Contains(m, "flash-3.8"):
		model = "gemini-3.8-flash"
	case strings.Contains(m, "gemini-3.7") || strings.Contains(m, "flash-3.7"):
		model = "gemini-3.7-flash"
	case strings.Contains(m, "gemini-3.6") || strings.Contains(m, "flash-3.6"):
		model = "gemini-3.6-flash"
	case strings.Contains(m, "gemini-3.1-pro") || strings.Contains(m, "pro-3.1") || m == "gemini-3.1":
		model = "gemini-3.1-pro"
	case strings.Contains(m, "opus"):
		model = "claude-opus-4-6-thinking"
	case strings.Contains(m, "sonnet") || strings.Contains(m, "claude"):
		model = "claude-sonnet-4-6"
	case strings.Contains(m, "gpt-oss") || m == "gpt-oss-120b":
		model = "gpt-oss-120b-medium"
	default:
		for _, item := range registry.GetModels() {
			if strings.EqualFold(item.ID, m) {
				model = item.ID
				break
			}
		}
		if model == "" {
			return "", "", fmt.Errorf("unsupported model: %s", rawModel)
		}
	}

	// Determine effort based on model type
	if strings.HasPrefix(model, "gemini-") {
		chosenEffort := "high"
		if eff != "" {
			switch eff {
			case "minimal", "low":
				chosenEffort = "low"
			case "medium", "med":
				chosenEffort = "medium"
			case "high", "xhigh", "extrahigh", "max", "ultra":
				chosenEffort = "high"
			}
		} else if embeddedEffort != "" {
			chosenEffort = embeddedEffort
		}

		if model == "gemini-3.1-pro" {
			if chosenEffort == "medium" {
				chosenEffort = "high"
			}
		}
		effort = chosenEffort
	} else {
		// Claude and GPT-OSS models do NOT accept --effort in agy
		effort = ""
	}

	return model, effort, nil
}

func resolveModel(m string) (string, error) {
	model, effort, err := resolveModelAndEffort(m, "")
	if err != nil {
		return "", err
	}
	if strings.HasPrefix(model, "gemini-") && effort != "" {
		return model + "-" + effort, nil
	}
	return model, nil
}

func handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	reqCount.Add(1)

	var req ChatCompletionRequest
	r.Body = http.MaxBytesReader(w, r.Body, 10<<20)
	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		var maxBytesErr *http.MaxBytesError
		if errors.As(err, &maxBytesErr) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusRequestEntityTooLarge)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"error": map[string]string{
					"message": "Request payload too large (max 10MB)",
					"type":    "invalid_request_error",
				},
			})
			return
		}
		http.Error(w, fmt.Sprintf("Invalid request body: %v", err), http.StatusBadRequest)
		return
	}
	if err := json.Unmarshal(bodyBytes, &req); err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"error": map[string]string{
				"message": fmt.Sprintf("Malformed JSON: %v", err),
				"type":    "invalid_request_error",
			},
		})
		return
	}

	model, effort, err := resolveModelAndEffort(req.Model, req.ReasoningEffort)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"error": map[string]string{
				"message": err.Error(),
				"type":    "invalid_request_error",
				"code":    "model_not_found",
			},
		})
		return
	}

	sessionKey, isAux := extractSessionKey(req.Messages, req.Tools, r.Header)
	convID := r.Header.Get("X-Conversation-Id")

	// Calculate message hashes for prefix integrity verification
	currentHashes := make([]string, len(req.Messages))
	for i, m := range req.Messages {
		currentHashes[i] = hashMessage(m)
	}
	toolsHash := hashTools(req.Tools)

	var activeState *SessionState
	if !isAux && sessionKey != "" {
		activeState = globalSessions.Get(sessionKey)
	}

	var finalPrompt string
	var fallbackFullPrompt string
	isDelta := false

	toolsPrompt := formatToolsPrompt(req.Tools)
	fallbackFullPrompt = formatConversation(req.Messages, toolsPrompt)

	if activeState != nil && activeState.ConvID != "" && activeState.Model == model && activeState.ToolsHash == toolsHash {
		if len(req.Messages) > activeState.MessageCount {
			prefixOK := true
			if len(activeState.HistoryHashes) < activeState.MessageCount || len(currentHashes) < activeState.MessageCount {
				prefixOK = false
			} else {
				for i := 0; i < activeState.MessageCount; i++ {
					if currentHashes[i] != activeState.HistoryHashes[i] {
						prefixOK = false
						break
					}
				}
			}

			if prefixOK {
				finalPrompt = formatDeltaPrompt(req.Messages, activeState.MessageCount)
				convID = activeState.ConvID
				isDelta = true
				log.Printf("[Proxy-Delta] Session %s continuing conv %s: delta (%d chars) for msgs %d..%d",
					sessionKey, convID, len(finalPrompt), activeState.MessageCount, len(req.Messages))
			} else {
				log.Printf("[Proxy-Delta] Desync detected for session %s (prefix changed). Re-anchoring.", sessionKey)
				globalSessions.Invalidate(sessionKey)
			}
		} else if len(req.Messages) == activeState.MessageCount && activeState.MessageCount > 0 {
			prefixOK := true
			if len(activeState.HistoryHashes) < activeState.MessageCount || len(currentHashes) < activeState.MessageCount {
				prefixOK = false
			} else {
				for i := 0; i < activeState.MessageCount-1; i++ {
					if currentHashes[i] != activeState.HistoryHashes[i] {
						prefixOK = false
						break
					}
				}
			}
			if prefixOK {
				finalPrompt = formatDeltaPrompt(req.Messages, activeState.MessageCount-1)
				convID = activeState.ConvID
				isDelta = true
				log.Printf("[Proxy-Delta] Session %s reprompt on same conv %s: delta (%d chars) for msg %d",
					sessionKey, convID, len(finalPrompt), activeState.MessageCount-1)
			}
		} else if len(req.Messages) < activeState.MessageCount {
			log.Printf("[Proxy-Delta] History truncation/compaction detected for session %s. Re-anchoring.", sessionKey)
			globalSessions.Invalidate(sessionKey)
		}
	}

	if !isDelta {
		finalPrompt = fallbackFullPrompt
		log.Printf("[Proxy-Full] Starting anchor prompt for session %s (aux=%v, msgs=%d, chars=%d)",
			sessionKey, isAux, len(req.Messages), len(finalPrompt))
	}

	lastMsgSummary := ""
	if len(req.Messages) > 0 {
		lastMsg := req.Messages[len(req.Messages)-1]
		contentStr := lastMsg.ContentString()
		limit := len(contentStr)
		if limit > 80 {
			limit = 80
		}
		lastMsgSummary = fmt.Sprintf("role=%s len=%d content=%q", lastMsg.Role, len(contentStr), contentStr[:limit])
	}
	log.Printf("[Proxy] Request #%d: model=%s (orig: %s) delta=%v stream=%v msgs=%d tools=%d promptLen=%d last=[%s]",
		reqCount.Load(), model, req.Model, isDelta, req.Stream, len(req.Messages), len(req.Tools), len(finalPrompt), lastMsgSummary)

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

	if effort != "" {
		args = append(args, "--effort", effort)
	}

	if convID != "" {
		args = append(args, "--conversation", convID)
	}

	totalPromptChars := 0
	for _, m := range req.Messages {
		totalPromptChars += len(m.Role) + len(m.Content) + len(m.Name) + len(m.ToolCalls)
	}
	for _, t := range req.Tools {
		totalPromptChars += len(t.Type) + len(t.Function.Name) + len(t.Function.Description) + len(t.Function.Parameters)
	}
	promptTokens := totalPromptChars / 4
	if promptTokens < 1 {
		promptTokens = len(finalPrompt) / 4
	}
	if promptTokens < 1 {
		promptTokens = 1
	}

	if req.Stream {
		includeUsageChunk := req.StreamOptions != nil && req.StreamOptions.IncludeUsage
		handleStreamingCompletion(w, r, args, completionID, model, createdTs, sessionKey, isAux, isDelta, convID, finalPrompt, fallbackFullPrompt, currentHashes, toolsHash, len(req.Tools) > 0, promptTokens, includeUsageChunk)
	} else {
		handleNonStreamingCompletion(w, r, args, completionID, model, createdTs, sessionKey, isAux, isDelta, convID, finalPrompt, fallbackFullPrompt, currentHashes, toolsHash, promptTokens)
	}
}

// -----------------------------------------------------------------------------
// Streaming SSE Engine with Heartbeat & Error Recovery
// -----------------------------------------------------------------------------
func handleStreamingCompletion(
	w http.ResponseWriter,
	r *http.Request,
	args []string,
	completionID, model string,
	createdTs int64,
	sessionKey string,
	isAux, isDelta bool,
	initialConvID, finalPrompt, fallbackFullPrompt string,
	currentHashes []string,
	toolsHash string,
	hasTools bool,
	promptTokens int,
	includeUsageChunk bool,
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

	var writeMu sync.Mutex
	var lastWrite atomic.Int64
	lastWrite.Store(time.Now().Unix())

	writeSSE := func(msg string) {
		writeMu.Lock()
		defer writeMu.Unlock()
		fmt.Fprint(w, msg)
		flusher.Flush()
		lastWrite.Store(time.Now().Unix())
	}

	emitErrorSSE := func(errMsg string) {
		chunk := map[string]interface{}{
			"id":      completionID,
			"object":  "chat.completion.chunk",
			"created": createdTs,
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
		writeSSE(fmt.Sprintf("data: %s\n\n", b))
		writeSSE("data: [DONE]\n\n")
	}

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
	writeSSE(fmt.Sprintf("data: %s\n\n", initBytes))

	select {
	case agySem <- struct{}{}:
		defer func() { <-agySem }()
	case <-r.Context().Done():
		emitErrorSSE("Request canceled while waiting for execution slot")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), StreamTimeout)
	defer cancel()

	emitTextChunk := func(txt string) {
		chunk := map[string]interface{}{
			"id":      completionID,
			"object":  "chat.completion.chunk",
			"created": createdTs,
			"model":   model,
			"choices": []map[string]interface{}{
				{
					"index":         0,
					"delta":         map[string]interface{}{"content": txt},
					"finish_reason": nil,
				},
			},
		}
		b, _ := json.Marshal(chunk)
		writeSSE(fmt.Sprintf("data: %s\n\n", b))
	}

	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.Printf("[Proxy] Heartbeat recovered: %v", r)
			}
		}()
		for {
			select {
			case <-ticker.C:
				if time.Now().Unix()-lastWrite.Load() >= 10 {
					writeSSE(": ping\n\n")
				}
			case <-ctx.Done():
				return
			}
		}
	}()

	var accumulatedText strings.Builder
	var streamedText strings.Builder
	var streamPending strings.Builder
	activeConvID := initialConvID
	isBufferingTool := false
	var lastWaitErr error
	var lastStderr string

	for attempt := 1; attempt <= 2; attempt++ {
		if attempt > 1 {
			log.Printf("[Proxy] Retrying agy (attempt %d/2) for %s...", attempt, completionID)
			accumulatedText.Reset()
			streamedText.Reset()
			streamPending.Reset()
			isBufferingTool = false
			time.Sleep(500 * time.Millisecond)

			if isDelta {
				if ctx.Err() != nil || r.Context().Err() != nil {
					log.Printf("[Proxy-Delta] Client disconnected/canceled for convID %s. Preserving session.", initialConvID)
					return
				}
				log.Printf("[Proxy-Delta] Attempt 1 failed on convID %s. Falling back to fresh anchor.", initialConvID)
				if sessionKey != "" {
					globalSessions.Invalidate(sessionKey)
				}
				var filteredArgs []string
				for idx := 0; idx < len(args); idx++ {
					if args[idx] == "--conversation" {
						idx++
						continue
					}
					filteredArgs = append(filteredArgs, args[idx])
				}
				args = filteredArgs
				finalPrompt = fallbackFullPrompt
				isDelta = false
				activeConvID = ""
			}
		}

		cmd := exec.CommandContext(ctx, agyBin, args...)
		cmd.Dir = workspaceDir
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		cmd.Env = append(os.Environ(), "PATH="+filepath.Join(os.Getenv("HOME"), ".local/bin")+":"+os.Getenv("PATH"))

		stdin, err := cmd.StdinPipe()
		if err != nil {
			emitErrorSSE(fmt.Sprintf("StdinPipe error: %v", err))
			return
		}

		stdout, err := cmd.StdoutPipe()
		if err != nil {
			emitErrorSSE(fmt.Sprintf("StdoutPipe error: %v", err))
			return
		}
		stderr, err := cmd.StderrPipe()
		if err != nil {
			emitErrorSSE(fmt.Sprintf("StderrPipe error: %v", err))
			return
		}

		var stderrBuf bytes.Buffer
		var stderrWg sync.WaitGroup
		stderrWg.Add(1)
		go func() {
			defer stderrWg.Done()
			_, _ = io.Copy(&stderrBuf, stderr)
		}()

		if err := cmd.Start(); err != nil {
			log.Printf("[Proxy] Command start error: %v", err)
			emitErrorSSE(fmt.Sprintf("Process start error: %v", err))
			return
		}

		var processExited atomic.Bool
		doneProcess := make(chan struct{})
		go func() {
			select {
			case <-ctx.Done():
				if !processExited.Load() && cmd.Process != nil {
					_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
				}
			case <-doneProcess:
			}
		}()

		// Stream prompt through StdinPipe in background
		go func() {
			defer stdin.Close()
			if _, err := io.WriteString(stdin, finalPrompt+"\n"); err != nil {
				log.Printf("[Proxy] stdin write error: %v", err)
			}
		}()

		type streamEvent struct {
			line []byte
			err  error
		}
		eventChan := make(chan streamEvent, 4096)

		go func() {
			defer close(eventChan)
			scanner := bufio.NewScanner(stdout)
			buf := make([]byte, 1024*1024)
			scanner.Buffer(buf, 1024*1024)
			for scanner.Scan() {
				raw := scanner.Bytes()
				b := make([]byte, len(raw))
				copy(b, raw)
				select {
				case eventChan <- streamEvent{line: b}:
				case <-ctx.Done():
					return
				}
			}
			if err := scanner.Err(); err != nil {
				select {
				case eventChan <- streamEvent{err: err}:
				case <-ctx.Done():
					return
				}
			}
		}()

		for ev := range eventChan {
			if ev.err != nil {
				log.Printf("[Proxy] stdout scanner error: %v", ev.err)
				continue
			}
			line := ev.line
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
			} else if event.Event == "result" && event.Result.ConversationID != "" {
				activeConvID = event.Result.ConversationID
			} else if event.Event == "step_update" {
				delta := event.StepUpdate.TextDelta
				if delta != "" {
					accumulatedText.WriteString(delta)

					if !isBufferingTool {
						streamPending.WriteString(delta)
						pending := streamPending.String()

						// 1. Check if any complete tool marker is present
						markers := []string{"<tool_call>", "<function_call>", "call:default_api:", "```tool_call"}
						toolMarkerIdx := -1
						for _, m := range markers {
							if idx := strings.Index(pending, m); idx != -1 {
								if toolMarkerIdx == -1 || idx < toolMarkerIdx {
									toolMarkerIdx = idx
								}
							}
						}

						if toolMarkerIdx != -1 {
							// Found tool marker! Stream any text before the marker, then activate tool buffering.
							toEmit := pending[:toolMarkerIdx]
							if toEmit != "" {
								streamedText.WriteString(toEmit)
								emitTextChunk(toEmit)
							}
							isBufferingTool = true
							streamPending.Reset()
						} else {
							// 2. Check if trailing suffix matches an incomplete prefix of a tool marker
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
								streamedText.WriteString(toEmit)
								emitTextChunk(toEmit)
							}
							streamPending.Reset()
							streamPending.WriteString(toHold)
						}
					}
				}
			}
		}

		// Flush any pending text if no tool call was encountered
		if !isBufferingTool && streamPending.Len() > 0 {
			remaining := streamPending.String()
			streamedText.WriteString(remaining)
			emitTextChunk(remaining)
			streamPending.Reset()
		}

		lastWaitErr = cmd.Wait()
		processExited.Store(true)
		close(doneProcess)
		stderrWg.Wait()
		lastStderr = strings.TrimSpace(stderrBuf.String())

		// If success or we got output, break retry loop
		if lastWaitErr == nil || accumulatedText.Len() > 0 {
			break
		}
		log.Printf("[Proxy] agy attempt %d failed (err=%v, stderr=%s)", attempt, lastWaitErr, lastStderr)
		if strings.Contains(lastStderr, "--effort is not supported") {
			var filteredArgs []string
			for idx := 0; idx < len(args); idx++ {
				if args[idx] == "--effort" {
					idx++ // skip value too
					continue
				}
				filteredArgs = append(filteredArgs, args[idx])
			}
			args = filteredArgs
		}
	}

	if lastWaitErr != nil && accumulatedText.Len() == 0 {
		errDetails := lastStderr
		if errDetails == "" {
			errDetails = lastWaitErr.Error()
		}
		emitErrorSSE(errDetails)
		return
	}

	fullOutput := accumulatedText.String()
	cleanText, toolCalls := parseToolCalls(fullOutput)

	reasoning := ""
	if activeConvID != "" {
		reasoning = extractReasoningFromTranscript(activeConvID)
	}
	if thinkMatches := thinkRegex.FindAllStringSubmatch(cleanText, -1); len(thinkMatches) > 0 {
		var thinkParts []string
		for _, tm := range thinkMatches {
			if len(tm) > 1 {
				th := strings.TrimSpace(tm[1])
				if th != "" {
					thinkParts = append(thinkParts, th)
				}
			}
		}
		if len(thinkParts) > 0 {
			allThink := strings.Join(thinkParts, "\n\n")
			cleanText = strings.TrimSpace(thinkRegex.ReplaceAllString(cleanText, ""))
			if reasoning != "" {
				reasoning = reasoning + "\n\n" + allThink
			} else {
				reasoning = allThink
			}
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
		writeSSE(fmt.Sprintf("data: %s\n\n", rb))
	}

	// Flush remaining clean text if not streamed yet
	unstreamed := ""
	alreadyStreamed := streamedText.String()
	if alreadyStreamed == "" {
		unstreamed = cleanText
	} else if strings.HasPrefix(cleanText, alreadyStreamed) {
		unstreamed = cleanText[len(alreadyStreamed):]
	} else if trimmedStreamed := strings.TrimRight(alreadyStreamed, " \t\r\n"); strings.HasPrefix(cleanText, trimmedStreamed) {
		unstreamed = strings.TrimPrefix(cleanText, trimmedStreamed)
	}
	if unstreamed != "" {
		txtChunk := map[string]interface{}{
			"id":      completionID,
			"object":  "chat.completion.chunk",
			"created": createdTs,
			"model":   model,
			"choices": []map[string]interface{}{
				{
					"index":         0,
					"delta":         map[string]interface{}{"content": unstreamed},
					"finish_reason": nil,
				},
			},
		}
		tb, _ := json.Marshal(txtChunk)
		writeSSE(fmt.Sprintf("data: %s\n\n", tb))
	}

	inTok := promptTokens
	if inTok < 1 {
		inTok = len(finalPrompt) / 4
	}
	if inTok < 1 {
		inTok = 1
	}
	outTok := (accumulatedText.Len() + len(reasoning)) / 4
	if outTok < 1 {
		outTok = 1
	}
	totalTok := inTok + outTok

	usageMap := map[string]interface{}{
		"prompt_tokens":     inTok,
		"completion_tokens": outTok,
		"total_tokens":      totalTok,
	}

	if len(toolCalls) > 0 {
		for i, tc := range toolCalls {
			// Chunk 1: Announce tool call with name and ID
			chunkName := map[string]interface{}{
				"id":      completionID,
				"object":  "chat.completion.chunk",
				"created": createdTs,
				"model":   model,
				"choices": []map[string]interface{}{
					{
						"index": 0,
						"delta": map[string]interface{}{
							"tool_calls": []map[string]interface{}{
								{
									"index": i,
									"id":    tc.ID,
									"type":  "function",
									"function": map[string]string{
										"name":      tc.Function.Name,
										"arguments": "",
									},
								},
							},
						},
						"finish_reason": nil,
					},
				},
			}
			cnb, _ := json.Marshal(chunkName)
			writeSSE(fmt.Sprintf("data: %s\n\n", cnb))

			// Chunk 2: Stream complete arguments
			chunkArgs := map[string]interface{}{
				"id":      completionID,
				"object":  "chat.completion.chunk",
				"created": createdTs,
				"model":   model,
				"choices": []map[string]interface{}{
					{
						"index": 0,
						"delta": map[string]interface{}{
							"tool_calls": []map[string]interface{}{
								{
									"index": i,
									"function": map[string]string{
										"arguments": tc.Function.Arguments,
									},
								},
							},
						},
						"finish_reason": nil,
					},
				},
			}
			cab, _ := json.Marshal(chunkArgs)
			writeSSE(fmt.Sprintf("data: %s\n\n", cab))
		}

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
		if !includeUsageChunk {
			finChunk["usage"] = usageMap
		}
		finBytes, _ := json.Marshal(finChunk)
		writeSSE(fmt.Sprintf("data: %s\n\n", finBytes))
	} else {
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
		if !includeUsageChunk {
			stopChunk["usage"] = usageMap
		}
		sb, _ := json.Marshal(stopChunk)
		writeSSE(fmt.Sprintf("data: %s\n\n", sb))
	}

	if includeUsageChunk {
		// Dedicated usage chunk matching standard OpenAI SSE stream_options: {include_usage: true}
		usageChunk := map[string]interface{}{
			"id":      completionID,
			"object":  "chat.completion.chunk",
			"created": createdTs,
			"model":   model,
			"choices": []map[string]interface{}{},
			"usage":   usageMap,
		}
		ub, _ := json.Marshal(usageChunk)
		writeSSE(fmt.Sprintf("data: %s\n\n", ub))
	}

	writeSSE("data: [DONE]\n\n")

	if !isAux && sessionKey != "" && activeConvID != "" && lastWaitErr == nil {
		oldState := globalSessions.Get(sessionKey)
		turns := 1
		createdAt := time.Now()
		if oldState != nil {
			turns = oldState.TotalTurns + 1
			createdAt = oldState.CreatedAt
		}
		globalSessions.Update(sessionKey, &SessionState{
			ConvID:        activeConvID,
			Model:         model,
			CreatedAt:     createdAt,
			LastSeen:      time.Now(),
			MessageCount:  len(currentHashes),
			HistoryHashes: currentHashes,
			ToolsHash:     toolsHash,
			TotalTurns:    turns,
		})
	} else if isAux && activeConvID != "" {
		go func(cid string) {
			time.Sleep(5 * time.Second)
			for _, bDir := range brainDirs {
				p := filepath.Join(bDir, cid)
				if _, err := os.Stat(p); err == nil {
					_ = os.RemoveAll(p)
				}
			}
		}(activeConvID)
	}
}

// -----------------------------------------------------------------------------
// Non-Streaming Completion Engine
// -----------------------------------------------------------------------------
func handleNonStreamingCompletion(
	w http.ResponseWriter,
	r *http.Request,
	args []string,
	completionID, model string,
	createdTs int64,
	sessionKey string,
	isAux, isDelta bool,
	initialConvID, finalPrompt, fallbackFullPrompt string,
	currentHashes []string,
	toolsHash string,
	promptTokens int,
) {
	select {
	case agySem <- struct{}{}:
		defer func() { <-agySem }()
	case <-r.Context().Done():
		http.Error(w, "Request canceled while waiting for execution slot", http.StatusRequestTimeout)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), DefaultTimeout)
	defer cancel()

	var stdoutBuf, stderrBuf bytes.Buffer
	var waitErr error

	for attempt := 1; attempt <= 2; attempt++ {
		stdoutBuf.Reset()
		stderrBuf.Reset()

		if attempt > 1 {
			log.Printf("[Proxy] Non-stream retry (attempt %d/2) for %s...", attempt, completionID)
			if isDelta {
				if ctx.Err() != nil || r.Context().Err() != nil {
					log.Printf("[Proxy-Delta] Client disconnected/canceled for convID %s. Preserving session.", initialConvID)
					return
				}
				log.Printf("[Proxy-Delta] Non-stream attempt 1 failed on convID %s. Falling back to fresh anchor.", initialConvID)
				if sessionKey != "" {
					globalSessions.Invalidate(sessionKey)
				}
				var filteredArgs []string
				for idx := 0; idx < len(args); idx++ {
					if args[idx] == "--conversation" {
						idx++
						continue
					}
					filteredArgs = append(filteredArgs, args[idx])
				}
				args = filteredArgs
				finalPrompt = fallbackFullPrompt
				isDelta = false
			}
		}

		cmd := exec.CommandContext(ctx, agyBin, args...)
		cmd.Dir = workspaceDir
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		cmd.Env = append(os.Environ(), "PATH="+filepath.Join(os.Getenv("HOME"), ".local/bin")+":"+os.Getenv("PATH"))
		cmd.Stdin = strings.NewReader(finalPrompt)
		cmd.Stdout = &stdoutBuf
		cmd.Stderr = &stderrBuf

		if err := cmd.Start(); err != nil {
			log.Printf("[Proxy] Non-stream start error: %v", err)
			http.Error(w, fmt.Sprintf("AGY Error: %v", err), http.StatusInternalServerError)
			return
		}

		var processExited atomic.Bool
		doneProcess := make(chan struct{})
		go func(c *exec.Cmd) {
			select {
			case <-ctx.Done():
				if !processExited.Load() && c.Process != nil {
					_ = syscall.Kill(-c.Process.Pid, syscall.SIGKILL)
				}
			case <-doneProcess:
			}
		}(cmd)

		waitErr = cmd.Wait()
		processExited.Store(true)
		close(doneProcess)

		if waitErr == nil {
			break
		}

		log.Printf("[Proxy] Non-stream attempt %d failed (err=%v, stderr=%s)", attempt, waitErr, stderrBuf.String())
		if strings.Contains(stderrBuf.String(), "--effort is not supported") {
			var filteredArgs []string
			for idx := 0; idx < len(args); idx++ {
				if args[idx] == "--effort" {
					idx++
					continue
				}
				filteredArgs = append(filteredArgs, args[idx])
			}
			args = filteredArgs
		}
	}

	if waitErr != nil {
		http.Error(w, fmt.Sprintf("AGY Error: %v: %s", waitErr, stderrBuf.String()), http.StatusInternalServerError)
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

	cleanText, toolCalls := parseToolCalls(respText)
	finishReason := "stop"
	if len(toolCalls) > 0 {
		finishReason = "tool_calls"
	}

	reasoning := ""
	if activeConvID != "" {
		reasoning = extractReasoningFromTranscript(activeConvID)
	}
	if thinkMatches := thinkRegex.FindAllStringSubmatch(cleanText, -1); len(thinkMatches) > 0 {
		var thinkParts []string
		for _, tm := range thinkMatches {
			if len(tm) > 1 {
				th := strings.TrimSpace(tm[1])
				if th != "" {
					thinkParts = append(thinkParts, th)
				}
			}
		}
		if len(thinkParts) > 0 {
			allThink := strings.Join(thinkParts, "\n\n")
			cleanText = strings.TrimSpace(thinkRegex.ReplaceAllString(cleanText, ""))
			if reasoning != "" {
				reasoning = reasoning + "\n\n" + allThink
			} else {
				reasoning = allThink
			}
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
	if inTok == 0 {
		inTok = promptTokens
	}
	if inTok < 1 {
		inTok = len(finalPrompt) / 4
	}
	if inTok < 1 {
		inTok = 1
	}

	outTok := agyRes.Usage.OutputTokens
	if outTok == 0 {
		outTok = (len(respText) + len(reasoning)) / 4
	}
	if outTok < 1 {
		outTok = 1
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

	if !isAux && sessionKey != "" && activeConvID != "" && waitErr == nil {
		oldState := globalSessions.Get(sessionKey)
		turns := 1
		createdAt := time.Now()
		if oldState != nil {
			turns = oldState.TotalTurns + 1
			createdAt = oldState.CreatedAt
		}
		globalSessions.Update(sessionKey, &SessionState{
			ConvID:        activeConvID,
			Model:         model,
			CreatedAt:     createdAt,
			LastSeen:      time.Now(),
			MessageCount:  len(currentHashes),
			HistoryHashes: currentHashes,
			ToolsHash:     toolsHash,
			TotalTurns:    turns,
		})
	} else if isAux && activeConvID != "" {
		go func(cid string) {
			time.Sleep(5 * time.Second)
			for _, bDir := range brainDirs {
				p := filepath.Join(bDir, cid)
				if _, err := os.Stat(p); err == nil {
					_ = os.RemoveAll(p)
				}
			}
		}(activeConvID)
	}
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
func authMiddleware(next http.Handler) http.Handler {
	apiKey := getAPIKey()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Health endpoint is public for local probing
		if r.URL.Path == "/health" {
			next.ServeHTTP(w, r)
			return
		}

		if apiKey != "" {
			authHeader := r.Header.Get("Authorization")
			token := strings.TrimPrefix(authHeader, "Bearer ")
			token = strings.TrimSpace(token)
			if subtle.ConstantTimeCompare([]byte(token), []byte(apiKey)) != 1 {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusUnauthorized)
				_ = json.NewEncoder(w).Encode(map[string]interface{}{
					"error": map[string]string{
						"message": "Unauthorized: Invalid or missing API key",
						"type":    "auth_error",
						"code":    "invalid_api_key",
					},
				})
				return
			}
		}

		next.ServeHTTP(w, r)
	})
}

func main() {
	initWorkspace()

	sessionCtx, sessionCancel := context.WithCancel(context.Background())
	defer sessionCancel()
	initSessionManager(sessionCtx)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", handleHealth)
	mux.HandleFunc("GET /metrics", handleMetrics)
	mux.HandleFunc("GET /v1/models", handleModels)
	mux.HandleFunc("GET /api/v1/models", handleModels)
	mux.HandleFunc("GET /v1/models/", handleModelInfo)
	mux.HandleFunc("POST /v1/chat/completions", handleChatCompletions)
	mux.HandleFunc("POST /api/show", handleOllamaShow)
	mux.HandleFunc("GET /api/tags", handleOllamaTags)

	handler := authMiddleware(mux)

	host := getHost()
	port := getPort()
	addr := net.JoinHostPort(host, port)

	if (host == "0.0.0.0" || host == "") && getAPIKey() == "" {
		log.Printf("[SECURITY WARNING] Server is binding to all interfaces (%s) without ANYGRAVITY_API_KEY! Set ANYGRAVITY_API_KEY to protect your machine.", addr)
	}

	server := &http.Server{
		Addr:         addr,
		Handler:      handler,
		ReadTimeout:  180 * time.Second,
		WriteTimeout: 0, // Disabled for SSE streaming; request contexts manage timeouts
		IdleTimeout:  300 * time.Second,
	}

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)

	go func() {
		<-stop
		log.Println("[Anygravity] Shutting down gracefully...")
		sessionCancel()
		globalSessions.flush()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(ctx)
	}()

	log.Printf("[Anygravity] Service listening on http://%s (RSS: ~2.9MB)", addr)
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("Server error: %v", err)
	}
}
