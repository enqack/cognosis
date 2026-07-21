#!/usr/bin/env bash
# Feature: retrieval-quality measurement (internal/query/retrievaleval).
#
# Local-tier by design. These sweeps build multi-thousand-chunk corpora and
# produce recall/latency numbers that jitter with HNSW graph construction and
# machine load, so they are not part of CI: a threshold on a shared runner is
# either meaningless or flaky, and flaky assertions get muted rather than
# fixed. The assertions here are bounds and relations only; the numbers land in
# internal/query/retrievaleval/testdata/ for a human to read.
#
# Needs Postgres only -- no Ollama. The corpus uses the deterministic clustered
# Synth provider, not a live embedding server.
set -euo pipefail
source "$(dirname "$0")/_lib.sh"

require_env

# The Go tests key on COGNOSIS_TEST_DSN (schema isolation) and gate the sweeps
# on COGNOSIS_EVAL_DSN being set at all.
export COGNOSIS_TEST_DSN="${COGNOSIS_TEST_DSN:-$COGNOSIS_DSN}"
export COGNOSIS_EVAL_DSN="$COGNOSIS_TEST_DSN"

cd "$(dirname "$0")/../.."

echo "running retrieval evaluation sweeps (corpus size: ${COGNOSIS_EVAL_NOTES:-1600} notes)"
# TestKeywordORFallbackSweep is here and TestKeywordANDvsOR is not, deliberately.
# The AND/OR artifact is already recorded and its control only checks that the
# connective takes effect. The fallback sweep's control fails when the corpus
# stops starving the keyword leg, which a future change to query generation or
# the vocabulary could do silently -- text generation has quietly emptied a
# fixture property twice before.
go test ./internal/query/retrievaleval/ -v -timeout 30m \
  -run 'TestVectorLegCapacity|TestVectorLegRecallVsExact|TestFusedTopKUnderCorrectedScan|TestRecordExactProbePlan|TestKeywordORFallbackSweep|TestNoteLevelMembershipSweep|TestGraphWeightSweep|TestGraphLegContribution' \
  || fail "retrieval evaluation sweeps failed"

pass "capacity, recall-vs-exact, fused-overlap, ground-truth plan, OR-fallback, note-level-membership, graph-weight and graph-contribution sweeps recorded"

echo
echo "recorded artifacts:"
for f in internal/query/retrievaleval/testdata/*.txt; do
  [ -e "$f" ] || continue
  echo "  $f"
done

echo
echo "retrieval-eval check: all criteria pass"
echo "run 'mage bench' for the latency numbers (Q4); they are not asserted here"
