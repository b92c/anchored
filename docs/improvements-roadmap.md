# Anchored Improvements Roadmap

Roadmap for improving the current local Anchored product before and alongside Team Sync / Anchored Cloud.

The goal is to keep Anchored's current value proposition intact — local-first, single binary, low-ops, fast memory retrieval — while adding the primitives needed for teams, privacy, curation, and cloud sync later.

---

## Principles

- Preserve the local-first experience. Anchored must keep working without cloud, accounts, or network access.
- Do not leak user-local data. Paths, usernames, local environment details, access patterns, and preferences stay local unless explicitly shared.
- Improve the local product first. Cloud/team sync should reuse solid local primitives, not bypass them.
- Keep the binary simple. Prefer Go stdlib and existing patterns before adding dependencies.
- Make memory inspectable. Users should be able to see, edit, delete, and understand what Anchored knows.

---

## P0 — Sync-Ready Foundation

These changes unblock future Team Sync and also improve the current local product.

### Stable Project Identity

Problem: local `project_id` values can differ across machines for the same repository.

Plan:

- Add a stable project identity derived from non-personal repository metadata.
- Prefer normalized git remote URL hash when available.
- Fall back to current local project detection when no remote exists.
- Store both:
  - local project ID: internal SQLite identity
  - remote project key: stable non-personal identity for sync/cloud

Acceptance:

- Two machines with the same git remote derive the same remote project key.
- Local filesystem paths are not used as remote identity.
- Existing local projects continue working.

### Sync Metadata Columns

Problem: local memories have no sync state.

Plan:

- Add SQLite migration for:
  - `sync_dirty BOOLEAN DEFAULT FALSE`
  - `sync_origin TEXT DEFAULT 'local'`
  - `author TEXT`
  - `remote_project_key TEXT`
- Add `sync_state` table:

```sql
CREATE TABLE IF NOT EXISTS sync_state (
    project_id TEXT PRIMARY KEY,
    remote_project_key TEXT,
    watermark TEXT,
    last_sync DATETIME,
    client_id TEXT NOT NULL
);
```

Acceptance:

- New local saves are marked dirty only when they are eligible for remote sync.
- Remote-origin records can be distinguished from local-origin records.

### Service Observer Hooks

Problem: `Service.Save`, `Update`, and `Forget` have no clean extension point for sync, audit, or future side effects.

Plan:

- Add a small observer interface to `memory.Service`:

```go
type MemoryObserver interface {
    OnMemorySaved(ctx context.Context, m Memory)
    OnMemoryUpdated(ctx context.Context, m Memory)
    OnMemoryDeleted(ctx context.Context, id string, projectID *string)
}
```

- Keep it optional and non-blocking.
- Never let observer failures break local memory operations.

Acceptance:

- Existing behavior remains unchanged with no observer configured.
- A test observer receives save/update/delete events.

---

## P1 — Privacy and Preference Model

These changes make the product safer and clarify what is personal vs shared.

### Preference Scope

Problem: `preference` currently mixes personal preferences, project conventions, and team rules.

Plan:

- Keep the existing `category = preference` for compatibility.
- Add metadata-level scope:
  - `user_preference`
  - `project_preference`
  - `team_preference`
- Default all inferred preferences to `user_preference`.
- Require explicit user action or policy to promote to project/team preference.

Acceptance:

- Existing preference searches still work.
- Sync filters can block personal preferences while allowing explicit project/team preferences.

### Remote-Safe Content Filter

Problem: sanitizer redacts secrets, but sync/cloud also needs to block local paths and personal environment details.

Plan:

- Add a separate `RemoteSafetyFilter` for content leaving the machine.
- Detect and block/redact:
  - `/home/<user>/...`
  - `/Users/<user>/...`
  - `C:\Users\<user>\...`
  - home-relative paths with personal context
  - shell history/cache/temp paths
  - personal usernames/emails when not needed
- Convert local paths to repo-relative paths when possible.

Acceptance:

- Remote-eligible content containing absolute personal paths is rejected or rewritten.
- Local memory save remains allowed; only remote push is blocked.

### Configurable Sanitizer Patterns

Problem: config supports sanitizer patterns conceptually, but custom patterns should be first-class for companies.

Plan:

- Wire `SanitizerConfig.Patterns` into the sanitizer.
- Add tests for custom redaction rules.
- Document examples for internal domains, ticket IDs, customer IDs, and private infra names.

Acceptance:

- User-defined patterns redact content before local save and before remote push.

---

## P2 — Local UX and Trust

These changes help users understand and curate what Anchored knows.

### Memory Inspection CLI

Problem: users need confidence in the memory store before trusting sync/cloud.

Plan:

- Improve `anchored list` with filters:
  - category
  - project
  - source
  - sync origin
  - deleted/non-deleted
- Add `anchored inspect <id>` with full metadata.
- Add `anchored export --project <id>` for audit/review.

Acceptance:

- A user can inspect exactly what would be synced.

### Interactive Configuration Wizard

Problem: `anchored config set <key> <value>` is useful for scripts but awkward for first-time users and for broader configuration review.

Plan:

- Add an interactive terminal wizard:

```bash
anchored config wizard
anchored config interactive
```

- Show the current value for each setting.
- Let the user press Enter to keep the current value.
- Group prompts by subsystem:
  - memory storage
  - embeddings
  - search
  - sanitizer
  - context optimizer
  - dream
  - debug/plugin
- Ask for final confirmation before writing `~/.anchored/config.yaml`.
- Keep `anchored config show` and `anchored config set` unchanged for non-interactive usage.

Acceptance:

- Running `anchored config wizard` can create or update `~/.anchored/config.yaml` without editing YAML manually.
- Existing `anchored config` behavior remains backward-compatible.
- Invalid numeric/boolean inputs re-prompt instead of writing broken config.

### Sync Preview

Problem: before enabling cloud/team sync, users should see what would leave the machine.

Plan:

- Add dry-run command:

```bash
anchored remote preview
```

- Output counts and sample IDs by category.
- Show blocked memories and reasons.

Acceptance:

- Preview sends no network request.
- Output clearly separates syncable, blocked, and needs-review memories.

### Memory Provenance

Problem: team/cloud views need to show where a memory came from.

Plan:

- Normalize source metadata:
  - tool: Claude Code, Cursor, OpenCode, CLI, import
  - source type: manual, hook, import, sync
  - author: local account/user when configured
- Avoid storing local paths as provenance for remote records.

Acceptance:

- Memories can show “added by X via Claude Code” without leaking local machine details.

---

## P3 — Search and Context Quality

These changes improve the core experience independent of cloud.

### Project Context Ranking

Problem: project-scoped memories should dominate when working inside a repo, but relevant global preferences still matter.

Plan:

- Keep project boost behavior.
- Add explicit result labels:
  - local personal
  - local project
  - remote project
  - global
- Tune category diversity for `anchored_context`.

Acceptance:

- `anchored_context` surfaces project facts/decisions before generic memories.
- User preferences remain visible but do not crowd out project decisions.

### Preference Retrieval Layer

Problem: user preferences are valuable but should not pollute project facts.

Plan:

- Add a dedicated preference retrieval section in context output.
- Keep it small and stable.
- Prefer explicit preferences over inferred ones.

Acceptance:

- Context output visibly separates “Your preferences” from “Project knowledge”.

### Remote-Origin Labeling

Problem: once sync exists, agents need to know whether a memory came from the current user or the team.

Plan:

- Add display metadata for remote-origin memories.
- In context/search output, mark team-shared memories as such.

Acceptance:

- Search results can show whether a memory is local-only or team-shared.

---

## P4 — Dream and Curation

These changes keep the memory base useful over time.

### Dream Dry Run

Problem: automatic cleanup is risky without review.

Plan:

- Make dream analysis produce a review report first:
  - duplicates
  - contradictions
  - stale memories
  - suggested merges
  - category corrections
- No destructive action by default.

Acceptance:

- `anchored dream --dry-run` produces actionable suggestions without modifying DB.

### Manual Apply

Problem: users need controlled cleanup.

Plan:

- Add commands to apply specific dream suggestions by ID.
- Keep audit trail in metadata.

Acceptance:

- A user can approve one merge/delete/category correction without applying all suggestions.

### Team Dream Compatibility

Problem: cloud/server dream should reuse local logic later.

Plan:

- Keep dream algorithms pure Go and store-agnostic where possible.
- Separate analysis from mutation.

Acceptance:

- The same dedup/contradiction logic can run against local SQLite and future server Postgres adapters.

---

## P5 — Remote / Cloud Readiness

These changes connect the local product to the future remote layer.

### Remote Config

Plan:

- Add `RemoteConfig` to `config.yaml`:

```yaml
remote:
  enabled: false
  server_url: ""
  api_key: ""
  projects: []
```

- Add CLI support:

```bash
anchored remote add
anchored remote status
anchored remote preview
anchored remote sync
anchored remote remove
```

Acceptance:

- Config can be created, listed, and removed without starting sync.

### Minimal Sync Client

Plan:

- Create `pkg/sync` with:
  - client
  - filter
  - safety checks
  - dry-run planner
  - syncer skeleton

Acceptance:

- Local client can prepare a valid sync payload without sending personal paths or blocked categories.

---

## Recommended Order

1. Stable project identity
2. Privacy/safety filter for remote-eligible content
3. Preference scope metadata
4. Service observer hooks
5. Sync metadata migration
6. Memory inspection and sync preview CLI
7. Remote config
8. Minimal `pkg/sync` dry-run client
9. Dream dry-run report
10. Team Sync server implementation

This order improves the current product before requiring any cloud/server work.
