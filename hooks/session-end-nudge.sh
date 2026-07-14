#!/usr/bin/env bash
# Claude Code SessionEnd hook: one last headless turn nudging the agent to
# persist anything durable from the session via its Cognosis MCP tools.
#
# Deliberately a nudge, not a scraper: judgment about what's worth keeping
# stays on the agent's side (it calls write_note/write_reflection itself),
# rather than a regex deciding what counts as durable. Requires the `claude`
# CLI to be logged in; exits 0 quietly when it isn't, since a missing backstop
# must not break session teardown.
set -uo pipefail

# Same marker gate as SessionStart: unrelated repos are left alone.
dir="$PWD"
while [ "$dir" != "/" ]; do
  [ -f "$dir/.cognosis-project" ] && found=1 && break
  dir="$(dirname "$dir")"
done
[ "${found:-0}" = "1" ] || exit 0

command -v claude >/dev/null || exit 0

claude -p "The session is ending. If anything durable surfaced this session —
decisions, gotchas, open questions, completed work — persist it now with the
cognosis write_note tool (entries/ for raw capture), or write_reflection if a
notable moment warrants one. If nothing durable happened, do nothing." \
  --allowedTools "mcp__cognosis__write_note,mcp__cognosis__write_reflection,mcp__cognosis__list_personas,mcp__cognosis__get_persona" \
  >/dev/null 2>&1 || true
exit 0
