# CLAUDE.md

@AGENTS.md

<!--
AGENTS.md (imported above) is the cross-tool single source of truth for working in
this repo — project overview, build/test commands, structure, conventions, and the
architecture invariants. Claude Code does not read AGENTS.md natively, so this file
imports it and holds only Claude Code-specific extras. Put repo guidance in
AGENTS.md, not here.
-->

## Claude Code-specific notes

- The permission allowlist is in `.claude/settings.json` (standard Go set plus `protoc`).
- No subagents, slash commands, or skills yet — add them under `.claude/` as the repo earns them.
- Commits and PRs carry no AI-attribution trailers (see AGENTS.md → Code Conventions).
