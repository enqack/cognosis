#!/usr/bin/env bash
# Opt-in git commit capture. Install per repo, only where wanted:
#   cp /path/to/cognosis/hooks/post-commit.sh .git/hooks/post-commit
#   chmod +x .git/hooks/post-commit
#
# Marker-gated: `cognosis hook post-commit` exits 0 silently in repos without
# a .cognosis-project marker, and never fails the commit — a broken capture
# prints a warning and moves on.
exec cognosis hook post-commit
