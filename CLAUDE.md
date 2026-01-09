# CLAUDE.md

## Project Overview

roborev is an automatic code review daemon for git commits. It runs locally, triggered by post-commit hooks, and uses AI agents (Codex, Claude Code) to review commits in parallel.

## Architecture

```
CLI (roborev) → HTTP API → Daemon (roborevd) → Worker Pool → Agents
                              ↓
                          SQLite DB
```

- **Daemon**: HTTP server on port 7373 (auto-finds available port if busy)
- **Workers**: Pool of 4 (configurable) parallel review workers
- **Storage**: SQLite at `~/.roborev/reviews.db` with WAL mode
- **Config**: Global at `~/.roborev/config.toml`, per-repo at `.roborev.toml`

## Key Files

| Path | Purpose |
|------|---------|
| `cmd/roborev/main.go` | CLI entry point, all commands |
| `cmd/roborevd/main.go` | Daemon entry point |
| `internal/daemon/server.go` | HTTP API handlers |
| `internal/daemon/worker.go` | Worker pool, job processing |
| `internal/storage/` | SQLite operations |
| `internal/agent/` | Agent interface + implementations |
| `internal/config/config.go` | Config loading, agent resolution |

## Conventions

- **HTTP over gRPC**: We use simple HTTP/JSON for the daemon API
- **No CGO in releases**: Build with `CGO_ENABLED=0` for static binaries (except sqlite which needs CGO locally)
- **Test agent**: Use `agent = "test"` for testing without calling real AI
- **Isolated tests**: All tests use `t.TempDir()` for temp directories

## Commands

```bash
go build ./...           # Build
go test ./...            # Test
go install ./cmd/...     # Install locally
roborev init             # Initialize in a repo
roborev status           # Check daemon/queue
```

## Adding a New Agent

1. Create `internal/agent/newagent.go`
2. Implement the `Agent` interface:
   ```go
   type Agent interface {
       Name() string
       Review(ctx context.Context, repoPath, commitSHA, prompt string) (string, error)
   }
   ```
3. Call `Register()` in `init()`

## Database Schema

Tables: `repos`, `commits`, `review_jobs`, `reviews`, `responses`

Job states: `queued` → `running` → `done`/`failed`

## Port Handling

Daemon writes runtime info to `~/.roborev/daemon.json`:
```json
{"pid": 1234, "addr": "127.0.0.1:7373", "port": 7373}
```

CLI reads this to find the daemon. If port 7373 is busy, daemon auto-increments.

## Style Preferences

- Keep it simple, no over-engineering
- Prefer stdlib over external dependencies
- Tests should be fast and isolated
- No emojis in code or output (except commit messages)
- Never amend commits; always create new commits for fixes
- Never push or pull unless explicitly asked by the user
