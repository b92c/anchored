package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"regexp"
	"strings"

	"github.com/jholhewres/anchored/pkg/debuglog"
)

// runHookPreToolUse inspects an anchored sandbox tool call before execution
// and blocks payloads that contain dangerous patterns. The hook IS registered
// in hooks/hooks.json with a narrow matcher (mcp__anchored__anchored_execute*)
// — the matcher exists because checkDangerousPattern is substring-based and
// would generate false positives if applied to general-purpose Bash. Limiting
// it to the sandbox tools means we only block code the user explicitly asked
// us to execute via anchored, where false positives are easier to reason
// about and the cost of a false negative (rm -rf /, mkfs, dd) is highest.
func runHookPreToolUse(args []string) {
	fs := newFlagSet("hook pretooluse")
	configPath := fs.String("config", "", "path to config file")
	fs.Parse(args)

	dlog := openDebugLogger(*configPath)
	defer dlog.Close()

	content, err := io.ReadAll(os.Stdin)
	if err != nil {
		slog.Error("failed to read stdin", "error", err)
		dlog.Event("hook.pretooluse", map[string]any{"stage": "stdin_error", "error": err.Error()})
		os.Exit(1)
	}

	// Claude Code's canonical PreToolUse payload is {tool_name, tool_input,
	// session_id, hook_event_name, cwd, ...}. Older drafts used {tool,
	// arguments}; we accept either so manual scripts keep working.
	var input struct {
		ToolName  string         `json:"tool_name"`
		ToolInput map[string]any `json:"tool_input"`
		Tool      string         `json:"tool"`
		Arguments map[string]any `json:"arguments"`
	}
	if err := json.Unmarshal(content, &input); err != nil {
		outputJSON(map[string]string{"decision": "allow"})
		return
	}
	tool := input.ToolName
	if tool == "" {
		tool = input.Tool
	}
	args2 := input.ToolInput
	if args2 == nil {
		args2 = input.Arguments
	}

	// Security checks for command execution tools
	if tool == "anchored_execute" || tool == "anchored_execute_file" || tool == "anchored_batch_execute" {
		code, _ := args2["code"].(string)
		if tool == "anchored_batch_execute" {
			if cmds, ok := args2["commands"].([]any); ok {
				for _, cmd := range cmds {
					if m, ok := cmd.(map[string]any); ok {
						if c, ok := m["command"].(string); ok && c != "" {
							code += "\n" + c
						}
					}
				}
			}
		}
		if blocked, pattern := checkDangerousPattern(code); blocked {
			dlog.Event("hook.pretooluse", map[string]any{
				"stage":   "blocked",
				"tool":    tool,
				"pattern": pattern,
				"args":    debuglog.Snippet(string(content), 240),
			})
			outputJSON(map[string]string{
				"decision": "block",
				"reason":   "dangerous pattern detected: " + pattern,
			})
			return
		}
	}

	if mentionsMemory(args2) {
		dlog.Event("hook.pretooluse", map[string]any{
			"stage": "memory_hint",
			"tool":  tool,
		})
		outputJSON(map[string]string{
			"decision": "allow",
			"reason":   "consider anchored_search for memory queries",
		})
		return
	}

	dlog.Event("hook.pretooluse", map[string]any{
		"stage": "allow",
		"tool":  tool,
	})
	outputJSON(map[string]string{"decision": "allow"})
}

func checkDangerousPattern(code string) (blocked bool, pattern string) {
	dangerous := []string{
		"rm -rf /",
		"rm -rf /*",
		":(){:|:&};:",
		"dd if=/dev/zero",
		"mkfs",
		"format c:",
		"curl",
		"wget",
		"nc -l",
	}
	lower := strings.ToLower(code)
	for _, d := range dangerous {
		if strings.Contains(lower, strings.ToLower(d)) {
			// Fine-grained: curl/wget only block if piping to shell
			if d == "curl" || d == "wget" {
				if strings.Contains(lower, "|") && (strings.Contains(lower, "sh") || strings.Contains(lower, "bash")) {
					return true, d + " piped to shell"
				}
				continue
			}
			return true, d
		}
	}
	return false, ""
}

// memoryWordRE matches whole words only — without \b we matched "memory leak"
// and "in-memory" as memory-related. The list stays English-only because this
// routing-advice hook is opt-in and English is the lowest-friction baseline;
// PT-BR triggers live in the routing block instead.
var memoryWordRE = regexp.MustCompile(`(?i)\b(memory|fact|decision|preference|preferences)\b`)

func mentionsMemory(args map[string]any) bool {
	for _, v := range args {
		s, ok := v.(string)
		if !ok {
			continue
		}
		if memoryWordRE.MatchString(s) {
			return true
		}
	}
	return false
}

// outputJSON writes a hook response as a single JSON line. Hook handlers
// promise to never block the upstream tool call (`os.Exit(1)` would do exactly
// that on Claude Code), so a marshal failure falls back to "{}" rather than
// terminating the process. Marshal errors here are practically unreachable —
// callers always pass map[string]any with primitive values — but the fallback
// keeps the contract honest.
func outputJSON(v any) {
	data, err := json.Marshal(v)
	if err != nil {
		slog.Warn("hook: outputJSON marshal failed; emitting empty object", "error", err)
		fmt.Println("{}")
		return
	}
	fmt.Println(string(data))
}
