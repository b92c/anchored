package main

import (
	"encoding/json"
	"io"
	"os"

	"github.com/jholhewres/anchored/pkg/debuglog"
	"github.com/jholhewres/anchored/pkg/mcp"
)

// runHookUserPromptSubmit re-injects the anchored routing block on every user
// prompt so the model is reminded to call anchored_search/anchored_kg_query
// before answering — even after long sessions where the SessionStart reminder
// has been compacted away. The Claude Code contract here is the same shape as
// SessionStart but with hookEventName="UserPromptSubmit".
func runHookUserPromptSubmit(args []string) {
	fs := newFlagSet("hook userpromptsubmit")
	configPath := fs.String("config", "", "path to config file")
	fs.Parse(args)

	dlog := openDebugLogger(*configPath)
	defer dlog.Close()

	// Drain stdin so Claude Code doesn't observe a closed pipe; the prompt
	// text is opportunistically inspected for debug telemetry — we never
	// gate injection on it, since the routing block is small enough to
	// re-emit unconditionally.
	body, _ := io.ReadAll(os.Stdin)
	var parsed struct {
		SessionID string `json:"session_id"`
		Prompt    string `json:"prompt"`
	}
	_ = json.Unmarshal(body, &parsed)
	dlog.Event("hook.userpromptsubmit", map[string]any{
		"stage":       "emitted",
		"session_id":  parsed.SessionID,
		"prompt_len":  len(parsed.Prompt),
		"prompt_head": debuglog.Snippet(parsed.Prompt, 240),
	})

	// We could parse `body` here to gate injection on memory triggers ("do you
	// remember", "we decided", etc.). For now the routing block is small
	// enough that re-injecting unconditionally is the safer default — context-
	// mode does the same with its own block.

	outputJSON(map[string]any{
		"hookSpecificOutput": map[string]any{
			"hookEventName":     "UserPromptSubmit",
			"additionalContext": mcp.AnchoredRoutingBlock,
		},
	})
}

