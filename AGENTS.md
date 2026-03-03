# Repository Guidelines

## Project Structure & Module Organization
- `main.go` starts the Cobra CLI defined in `cmd/` (`review`, `serve`, `init`).
- Core workflow lives in `internal/`:
  - `orchestrator/`: multi-reviewer debate pipeline.
  - `provider/`: AI backends (Claude Code, Codex CLI, OpenAI).
  - `platform/`: GitHub/GitLab adapters and diff/comment logic.
  - `context/`: call-chain/history/docs context gathering.
  - `display/`: terminal rendering and Markdown export.
  - `server/`: webhook HTTP server for GitLab MR automation.
- Tests are colocated as `*_test.go` files. Use `testdata/` for fixtures. Design notes live in `docs/` and per-module `README.md` files.

## Build, Test, and Development Commands
- `go build -o hydra .` builds the local binary.
- `go install .` installs `hydra` to your Go bin path.
- `go test ./...` runs all tests.
- `go test ./internal/... -run TestName` runs focused tests.
- `hydra init` creates `~/.hydra/config.yaml`.
- `hydra review --local` reviews local changes.
- `hydra serve --webhook-secret <secret>` runs webhook mode.

## Local Tooling Notes (Go Path)
- On this machine, Go is available at `/usr/local/go/bin/go` (`go1.24.10`).
- If `go` is not found in a non-interactive shell, use the absolute path:
  - `/usr/local/go/bin/go test ./...`
  - `/usr/local/go/bin/go build -o hydra .`
- Or fix PATH once per session: `export PATH=/usr/local/go/bin:$PATH`.

## Coding Style & Naming Conventions
- Target Go `1.24` (see `go.mod`).
- Run `gofmt` on changed files before committing.
- Use idiomatic Go naming: package names lowercase; exported identifiers `CamelCase`; internal helpers `camelCase`.
- Keep CLI flags kebab-case (for example `--no-post-summary`, `--show-tool-trace`).
- Prefer small functions and brief comments only for non-obvious logic.

## Testing Guidelines
- Use the standard `testing` package.
- Test names should follow `Test<Function>_<Scenario>`.
- Prefer mocks/stubs for provider and platform interfaces; avoid depending on external CLIs in unit tests.
- Update tests whenever behavior, flags, parsing, or display output changes.

## Commit & Pull Request Guidelines
- Use concise, imperative commit messages; `feat:` and `fix:` prefixes are recommended and already common in history.
- Keep each commit focused on one logical change.
- PRs should include: purpose, touched modules, test evidence (`go test` output), and CLI output samples for UX/display changes.
- Link related issues and document any config or migration impact.

## Security & Configuration Tips
- Never commit secrets or tokens.
- Store credentials via environment variables in config (for example `${OPENAI_API_KEY}`, `${GITLAB_TOKEN}`).
- Keep local artifacts out of commits (for example the built `./hydra` binary).
