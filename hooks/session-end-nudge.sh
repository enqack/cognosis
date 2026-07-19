#!/usr/bin/env bash
# Claude Code SessionEnd hook: one last headless turn nudging the agent to
# persist anything durable from the session via its Cognosis MCP tools.
#
# Reads the SessionEnd stdin payload and resumes the ending session
# (`--resume "$session_id"`) so the headless turn actually sees what happened —
# a bare `claude -p` would start a context-less new session and judge blind.
#
# Deliberately a nudge, not a scraper: judgment about what's worth keeping
# stays on the agent's side (it calls write_note/write_reflection itself),
# rather than a regex deciding what counts as durable. Requires the `claude`
# CLI to be logged in; exits 0 quietly when it isn't, since a missing backstop
# must not break session teardown.
#
# Re-entry guard: the headless `claude -p` below itself fires SessionEnd when it
# exits, which would re-invoke this hook and loop unbounded. The sentinel env
# var (inherited by the nested claude and its hooks) makes the nested run no-op.
set -uo pipefail

# Break the SessionEnd -> claude -p -> SessionEnd recursion: bail if we're
# already inside a nudge-spawned session.
[ "${COGNOSIS_SESSION_END_NUDGE_ACTIVE:-0}" = "1" ] && exit 0

# The SessionEnd payload arrives on stdin; capture it before any gate so a later
# early-exit doesn't leave it unread. session_id lets us resume the right session.
payload="$(cat)"

# Same marker gate as SessionStart: unrelated repos are left alone.
dir="$PWD"
while [ "$dir" != "/" ]; do
  [ -f "$dir/.cognosis-project" ] && found=1 && break
  dir="$(dirname "$dir")"
done
[ "${found:-0}" = "1" ] || exit 0

command -v claude >/dev/null || exit 0

# Extract session_id: prefer jq, fall back to a POSIX sed parse so jq stays an
# optional dependency (there's no other jq use in these hooks).
if command -v jq >/dev/null 2>&1; then
  session_id="$(printf '%s' "$payload" | jq -r '.session_id // empty' 2>/dev/null)"
else
  session_id="$(printf '%s' "$payload" | sed -n 's/.*"session_id"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p')"
fi

# Resume the ending session when we have its id; otherwise degrade to a bare
# (context-less) nudge rather than failing.
resume_args=()
[ -n "${session_id:-}" ] && resume_args=(--resume "$session_id")

COGNOSIS_SESSION_END_NUDGE_ACTIVE=1 claude -p "The session is ending. If anything durable surfaced this session —
decisions, gotchas, open questions, completed work — persist it now with the
cognosis write_note tool (entries/ for raw capture), or write_reflection if a
notable moment warrants one. If you already captured a note this session, use
edit_note to extend it rather than rewriting the whole file. If nothing durable
happened, do nothing." \
  ${resume_args[@]+"${resume_args[@]}"} \
  --allowedTools "mcp__cognosis__write_note,mcp__cognosis__edit_note,mcp__cognosis__write_reflection,mcp__cognosis__list_personas,mcp__cognosis__get_persona" \
  >/dev/null 2>&1 || true
exit 0
