package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"unicode/utf8"

	"github.com/jholhewres/anchored/pkg/debuglog"
)

// postToolUseInsertSQL is exposed as a package-level constant so tests can
// run the same SQL against an in-memory database without duplicating it.
// 9 columns / 9 values / 6 placeholders / 6 binds — order matters; do not
// reorder columns without updating both the binds in runHookPostToolUse and
// the schema in pkg/context/migration.go.
const postToolUseInsertSQL = `INSERT INTO session_events
	(id, session_id, project_id, event_type, priority, tool_name, summary, metadata, created_at)
	VALUES (?, ?, ?, 'tool_call', 3, ?, ?, ?, datetime('now'))`

// runHookPostToolUse records a `tool_call` row in session_events for every
// tool invocation Claude Code emits. Claude Code (and other MCP-compatible
// hooks) deliver the event as JSON on stdin with the canonical fields:
//
//	{
//	  "session_id":       "...",
//	  "hook_event_name":  "PostToolUse",
//	  "cwd":              "...",
//	  "tool_name":        "Read|Bash|...",
//	  "tool_input":       { ... },
//	  "tool_response":    { ... }
//	}
//
// Older invocations / manual scripts may pass --session-id / --cwd as flags;
// those still work as fallbacks. Failures here must NEVER block the upstream
// tool call: every error path returns exit 0 with a graceful JSON response.
func runHookPostToolUse(args []string) {
	fs := newFlagSet("hook posttooluse")
	sessionIDFlag := fs.String("session-id", "", "session identifier (fallback when stdin lacks one)")
	configPath := fs.String("config", "", "path to config file")
	cwdFlag := fs.String("cwd", "", "current working directory (fallback when stdin lacks one)")
	fs.Parse(args)

	dlog := openDebugLogger(*configPath)
	defer dlog.Close()

	body, err := io.ReadAll(os.Stdin)
	if err != nil {
		dlog.Event("hook.posttooluse", map[string]any{"stage": "stdin_error", "error": err.Error()})
		outputJSON(map[string]any{"recorded": false, "error": "stdin read failed"})
		return
	}

	var input struct {
		SessionID     string          `json:"session_id"`
		HookEventName string          `json:"hook_event_name"`
		Cwd           string          `json:"cwd"`
		ToolName      string          `json:"tool_name"`
		ToolInput     json.RawMessage `json:"tool_input"`
		ToolResponse  json.RawMessage `json:"tool_response"`
	}
	if len(body) > 0 {
		if err := json.Unmarshal(body, &input); err != nil {
			dlog.Event("hook.posttooluse", map[string]any{"stage": "parse_error", "error": err.Error()})
			// Don't return — flag-only invocation is still allowed.
		}
	}

	sessionID := input.SessionID
	if sessionID == "" {
		sessionID = *sessionIDFlag
	}
	if sessionID == "" {
		dlog.Event("hook.posttooluse", map[string]any{"stage": "missing_session_id"})
		outputJSON(map[string]any{"recorded": false, "reason": "missing session_id"})
		return
	}

	cwdVal := input.Cwd
	if cwdVal == "" {
		cwdVal = *cwdFlag
	}
	if cwdVal == "" {
		cwdVal = "."
	}

	_, _, svc, err := initService(*configPath)
	if err != nil {
		slog.Warn("posttooluse: service init failed", "error", err)
		dlog.Event("hook.posttooluse", map[string]any{"stage": "service_init_failed", "error": err.Error()})
		outputJSON(map[string]any{"recorded": false, "reason": "service init failed"})
		return
	}
	defer svc.Close()

	projectID := svc.ResolveProject(cwdVal)
	summary := summarizeToolEvent(input.ToolResponse, input.ToolInput, 500)
	metadata := buildPostToolUseMetadata(cwdVal, input.HookEventName, len(body))
	eventID := newHookID()
	toolName := input.ToolName

	_, err = svc.StoreDB().ExecContext(context.Background(), postToolUseInsertSQL,
		eventID, sessionID, projectID, toolName, summary, metadata,
	)
	if err != nil {
		slog.Warn("posttooluse: insert failed", "error", err)
		dlog.Event("hook.posttooluse", map[string]any{"stage": "insert_failed", "error": err.Error()})
		outputJSON(map[string]any{"recorded": false, "reason": "db error"})
		return
	}

	dlog.Event("hook.posttooluse", map[string]any{
		"stage":      "recorded",
		"session_id": sessionID,
		"project_id": projectID,
		"tool":       toolName,
		"event_id":   eventID,
		"summary":    debuglog.Snippet(summary, 200),
	})
	outputJSON(map[string]any{
		"recorded": true,
		"event_id": eventID,
	})
}

// summarizeToolEvent picks the best human-readable summary for the row.
// Tool responses are usually structured (Bash returns {stdout, stderr, ...},
// Read returns {file: ...}). We compact the JSON via encoding/json's stream
// compactor — that preserves key order and number precision, unlike a
// Marshal(Unmarshal()) round-trip. If the response is empty we fall back to
// the input arguments so the row still carries some signal.
//
// Truncation is rune-aware so multibyte sequences (PT-BR/EN/CJK) are never
// split mid-character. `max` is in runes, not bytes.
func summarizeToolEvent(response, input json.RawMessage, max int) string {
	pick := func(raw json.RawMessage) string {
		if len(raw) == 0 || string(raw) == "null" {
			return ""
		}
		var buf bytes.Buffer
		if err := json.Compact(&buf, raw); err != nil {
			// Not valid JSON — keep raw bytes verbatim, callers prefer
			// imperfect signal over an empty summary.
			return string(raw)
		}
		return buf.String()
	}

	s := pick(response)
	if s == "" {
		s = pick(input)
	}
	if utf8.RuneCountInString(s) > max {
		runes := []rune(s)
		s = string(runes[:max])
	}
	return s
}

func buildPostToolUseMetadata(cwd, hookEvent string, rawLen int) string {
	meta, err := json.Marshal(map[string]any{
		"cwd":             cwd,
		"hook_event_name": hookEvent,
		"raw_length":      rawLen,
	})
	if err != nil {
		return "{}"
	}
	if len(meta) > 1024 {
		meta = meta[:1024]
	}
	return string(meta)
}

func newHookID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%d", os.Getpid())
	}
	return hex.EncodeToString(b)
}
