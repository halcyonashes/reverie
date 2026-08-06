# Reverie

Persistent memory for AI coding agents.

Reverie is a personal fork of [Engram](https://github.com/Gentleman-Programming/engram) by Gentleman-Programming, which itself was inspired by [claude-mem](https://github.com/thedotmack/claude-mem). Engram gives an AI coding agent a memory that survives past the end of a session: a Go binary backed by SQLite + FTS5, exposed over CLI, HTTP, an MCP server, and a TUI.

> **Fork status**: the Go module and package paths have been renamed to `reverie`, but the CLI binary, environment variables, MCP tool names, and on-disk data directory are still the ones inherited from Engram (`engram`, `ENGRAM_*`, `~/.engram`, `mem_*`). Renaming those is a separate, larger change and hasn't happened yet — commands and paths below reflect what actually runs today.

```
Agent (Claude Code / OpenCode / Gemini CLI / Codex / VS Code / Cursor / Windsurf / ...)
    ↓ MCP stdio
engram (single Go binary, built from this fork)
    ↓
SQLite + FTS5 (~/.engram/engram.db)
```

## Building

There is no separate release channel for this fork yet. Build from source:

```bash
go build -o engram ./cmd/engram
```

Requirements and platform notes inherited from upstream (Homebrew tap, Windows paths, `go install`) are documented in [docs/INSTALLATION.md](docs/INSTALLATION.md) — treat the Homebrew/tap instructions there as pointing at upstream Engram, not this fork.

### Setting up an agent

```bash
./engram setup <agent>
```

Supported values: `pi`, `opencode`, `gemini-cli`, `codex`, `antigravity-cli`, `windsurf`, `qwen`, `kiro`, `cursor`, `vscode-copilot`, `kilocode`, `claude-code`. This writes the MCP config and any plugin files the agent needs; restart the agent afterward. Full per-agent details and the Memory Protocol snippet for surviving context compaction: [docs/AGENT-SETUP.md](docs/AGENT-SETUP.md).

You do not need to run `engram serve` yourself for stdio-based agents (Claude Code, Gemini CLI, Codex, VS Code, Cursor, Windsurf) — they launch `engram mcp` automatically as a short-lived subprocess. `engram serve` is only needed by plugins that talk to the HTTP API in the background (OpenCode, Pi); those setups start it automatically where possible, or you run it manually:

```bash
engram serve   # HTTP API, default port 7437
```

No Node.js, Python, or Docker required — one binary, one SQLite file.

## How it works

1. The agent completes work worth remembering (a bug fix, an architecture decision, a convention).
2. It calls `mem_save` with a title, type, and What/Why/Where/Learned.
3. Reverie persists the observation to SQLite with FTS5 indexing.
4. In a later session, the agent calls `mem_search` / `mem_context` to pull back relevant history.

Session lifecycle, topic keys, and memory hygiene: [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md).

`mem_search` matches all query tokens by default (`match_mode: "all"`). Pass `match_mode: "any"` when you only remember some of the keywords:

```json
{ "query": "authentication compliance session", "match_mode": "any" }
```

## MCP tools

| Category | Tools |
| --- | --- |
| Save & update | `mem_save`, `mem_update`, `mem_delete`, `mem_suggest_topic_key` |
| Search & retrieve | `mem_search`, `mem_context`, `mem_timeline`, `mem_get_observation` |
| Session lifecycle | `mem_session_start`, `mem_session_end`, `mem_session_summary` |
| Conflict surfacing | `mem_judge`, `mem_compare` |
| Lifecycle review | `mem_review` |
| Utilities | `mem_save_prompt`, `mem_stats`, `mem_capture_passive`, `mem_merge_projects`, `mem_current_project`, `mem_doctor` |

Full reference with parameters: [DOCS.md](DOCS.md#mcp-tools-20-tools).

## Terminal UI

```bash
engram tui
```

`j`/`k` to move, `Enter` to drill into an observation, `c` to copy content to the clipboard (OSC 52), `/` to search, `Esc` to go back.

## Git sync

Share memories across machines without a central server. `engram sync` exports new memories as compressed chunks committed alongside the repo — no merge conflicts, no large binary files. The local SQLite database stays authoritative; sync is export/import, not replication.

```bash
engram sync                    # export new memories as a compressed chunk
git add .engram/ && git commit -m "sync engram memories"
engram sync --import           # on another machine: import new chunks
engram sync --status           # check sync status
```

Full docs: [DOCS.md](DOCS.md).

## Cloud (opt-in)

Local SQLite is always the source of truth; cloud is opt-in replication for sharing memory across machines or a team.

```bash
docker compose -f docker-compose.cloud.yml up -d
engram cloud config --server http://127.0.0.1:18080
engram cloud enroll <project>
engram sync --cloud --project <project>
```

Cloud sync is always project-scoped — `--project` is required, and `--all` is intentionally blocked. `ENGRAM_CLOUD_ALLOWED_PROJECTS` must be set for `engram cloud serve` (use `*` to allow any project name). If a cloud sync/upsert fails in a way that's repairable, the original error is preserved and the CLI points at the `doctor`/`repair` flow below — nothing is auto-repaired during sync or autosync.

Troubleshooting, upgrade flow, and full env/runtime reference: [docs/engram-cloud/README.md](docs/engram-cloud/README.md), [DOCS.md](DOCS.md#cloud-upgrade-flow).

### Diagnosing and repairing an existing local database

```bash
engram cloud upgrade doctor --project <project>        # read-only readiness check
engram cloud upgrade repair --project <project> --dry-run
engram cloud upgrade repair --project <project> --apply
engram cloud upgrade bootstrap --project <project>      # resumable enroll + push + verify
engram cloud upgrade status --project <project>          # stage/class/reason
```

If `doctor` reports ready but the push still fails, the rescue helper repairs specific known-bad rows:

```bash
tools/repair-missing-session-directory.sh --apply --interactive --fix-exported <project>
engram sync --cloud --project <project>
```

## Testing the conflict-surfacing feature (upstream beta)

This fork carries the same in-progress conflict-detection work as upstream Engram: on save, a new memory can be checked against existing ones for `supersedes` / `conflicts_with` relations, optionally using an LLM judge for semantic (not just lexical) conflicts. To try it in isolation from your normal `~/.engram` data:

```bash
git clone https://github.com/Gentleman-Programming/engram.git engram-beta-repo
cd engram-beta-repo && git checkout feat/memory-conflict-surfacing-cloud-sync
docker compose -f docker-compose.beta.yml up -d
go build -o ./engram-beta ./cmd/engram

export ENGRAM_DATA_DIR=/tmp/engram-beta-data
export ENGRAM_CLOUD_SERVER=http://127.0.0.1:28080
export ENGRAM_CLOUD_TOKEN=beta-token-CHANGE-ME-please-32chars
mkdir -p "$ENGRAM_DATA_DIR"
```

Basic conflict detection on save:

```bash
./engram-beta save "Use Clean Architecture" "Layers: entities, use cases, adapters." --type architecture --project beta-test
./engram-beta save "Use Hexagonal Architecture" "Ports and adapters separate domain from infra." --type architecture --project beta-test
# second save returns candidates[] referencing the first memory's id
```

Cloud sync of the relation across two "machines":

```bash
./engram-beta cloud enroll beta-test
./engram-beta sync --cloud --project beta-test
ENGRAM_DATA_DIR=/tmp/engram-beta-data-2 ./engram-beta cloud enroll beta-test
ENGRAM_DATA_DIR=/tmp/engram-beta-data-2 ./engram-beta sync --cloud --project beta-test
ENGRAM_DATA_DIR=/tmp/engram-beta-data-2 ./engram-beta search "Architecture"
```

CLI/HTTP inspection:

```bash
./engram-beta conflicts list --project beta-test
./engram-beta conflicts stats --project beta-test
./engram-beta conflicts scan --project beta-test --dry-run
./engram-beta conflicts scan --project beta-test --apply --max-insert 10
```

Semantic scan using your existing agent CLI as the judge (no separate API key needed if you're already authenticated with that CLI):

```bash
export ENGRAM_AGENT_CLI=claude   # or opencode
./engram-beta conflicts scan --project beta-test --semantic --apply --max-semantic 5 --concurrency 3 --yes
```

FTS5 supplies the candidate pair by shared terms in the title; the LLM then judges whether the pair actually conflicts. It does not find pairs with no lexical overlap on its own.

Cleanup:

```bash
docker compose -f docker-compose.beta.yml down -v
rm -rf /tmp/engram-beta-data /tmp/engram-beta-data-2 ./engram-beta
```

This does not touch your normal `~/.engram` data or a production cloud instance. Full guide: [docs/BETA_TESTING.md](docs/BETA_TESTING.md).

## CLI reference

| Command | Description |
| --- | --- |
| `engram setup [agent]` | Install agent integration |
| `engram serve [port]` | Start HTTP API (default: 7437) |
| `engram mcp [--tools=PROFILE] [--project NAME]` | Start MCP server (stdio transport) |
| `engram tui` | Launch terminal UI |
| `engram search <query>` | Search memories |
| `engram save <title> <msg>` | Save a memory |
| `engram delete <obs_id>` | Delete an observation (soft by default; `--hard` removes permanently) |
| `engram delete session <id>` | Delete a session by ID (must have no observations) |
| `engram delete prompt <id>` | Delete a prompt by ID (permanent) |
| `engram delete project <name> [--hard]` | Cascade-delete a project (soft by default; `--hard` also removes sessions) |
| `engram timeline <obs_id>` | Chronological context |
| `engram context [project]` | Recent session context |
| `engram stats` | Memory statistics |
| `engram export [file]` | Export to JSON |
| `engram import <file>` | Import from JSON |
| `engram sync` | Git sync export/import |
| `engram conflicts <sub>` | Inspect and manage memory conflict relations |
| `engram doctor` | Run read-only operational diagnostics |
| `engram cloud <subcommand>` | Opt-in cloud config/status/enrollment + cloud runtime (`serve`) |
| `engram projects list\|consolidate\|prune` | Manage project names |
| `engram obsidian-export` | Export to Obsidian vault (beta) |
| `engram version` | Show version |

Full CLI with all flags: [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md#cli-reference).

### Key environment variables

| Variable | Description | Default |
| --- | --- | --- |
| `ENGRAM_DATA_DIR` | Override data directory | `~/.engram` |
| `ENGRAM_PORT` | Override HTTP server port | `7437` |
| `ENGRAM_URL` | Point the Pi plugin at an existing `engram serve` instead of auto-starting one (HTTP event capture only, not an MCP endpoint) | unset, defaults to `http://127.0.0.1:<ENGRAM_PORT>` |
| `ENGRAM_HTTP_TOKEN` | Bearer auth for the local HTTP server; when set, destructive/export routes require it | unset (open by default) |
| `ENGRAM_TIMEZONE` | Timezone for timestamps in TUI and cloud dashboard | system local |
| `ENGRAM_CLOUD_AUTOSYNC` | Set to `1` to enable background autosync (also needs `ENGRAM_CLOUD_TOKEN` + `ENGRAM_CLOUD_SERVER`) | unset |
| `ENGRAM_CLOUD_ALLOWED_PROJECTS` | Comma-separated project allowlist for `engram cloud serve` (`*` = all) | unset |
| `ENGRAM_CLOUD_TOKEN_PEPPER` | Secret used to hash managed cloud tokens; required to issue/authenticate managed tokens (falls back to legacy `ENGRAM_CLOUD_TOKEN`/`ENGRAM_CLOUD_ADMIN` if unset) | unset |

Full reference: [DOCS.md](DOCS.md#environment-variables).

## Documentation

| Doc | Description |
| --- | --- |
| [Installation](docs/INSTALLATION.md) | All install methods + platform support (upstream instructions; see build note above) |
| [Engram Cloud](docs/engram-cloud/README.md) | Cloud landing page and quickstart |
| [Agent Setup](docs/AGENT-SETUP.md) | Per-agent configuration + Memory Protocol |
| [Codebase Guide](docs/CODEBASE-GUIDE.md) | Repository structure, flows, and implementation landmarks |
| [Architecture](docs/ARCHITECTURE.md) | How it works, MCP tools, project structure |
| [Plugins](docs/PLUGINS.md) | OpenCode & Claude Code plugin details |
| [Comparison](docs/COMPARISON.md) | Engram vs. claude-mem |
| [Intended Usage](docs/intended-usage.md) | Mental model for how memory is meant to be used |
| [Obsidian export](docs/beta/obsidian-brain.md) | Export memories as an Obsidian knowledge graph (beta) |
| [Contributing](CONTRIBUTING.md) | Contribution workflow + standards |
| [Full docs](DOCS.md) | Complete technical reference |

If you modify `.templ` files under `internal/cloud/dashboard/`, run `make templ` to regenerate before committing.

## License

MIT. See [LICENSE](LICENSE).
