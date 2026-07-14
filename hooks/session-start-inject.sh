#!/usr/bin/env bash
# Claude Code SessionStart hook: inject the Cognosis knowledge index.
#
# The marker gates everything: `cognosis context inject` exits 0 silently when
# no .cognosis-project marker exists above the working directory — a stopped
# daemon must never block sessions in repos unrelated to Cognosis. In marked
# repos the failure mode is loud: daemon unreachable within ~2s -> nonzero
# exit, and Claude Code will not proceed without context (a context-less
# session that looks normal is worse than one that visibly fails).
#
# Wire it up in .claude/settings.json (see hooks/settings.sample.json).
set -euo pipefail
exec cognosis context inject --budget "${COGNOSIS_INJECT_BUDGET:-2000}"
