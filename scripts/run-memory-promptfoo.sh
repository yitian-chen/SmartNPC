#!/usr/bin/env bash
# Run Go exactly once, then import its immutable report into local Promptfoo.
set -euo pipefail
ROOT="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
TOOL="$ROOT/eval/memory-promptfoo"
export PROMPTFOO_DISABLE_TELEMETRY=1
export PROMPTFOO_CONFIG_DIR="$ROOT/tmp/memory-bench/promptfoo-state"
export PROMPTFOO_DISABLE_UPDATE=1
NODE="$TOOL/node_modules/node/bin/node"
CLI="$TOOL/node_modules/promptfoo/dist/src/entrypoint.js"
if [[ ! -x "$NODE" || ! -f "$CLI" ]]; then
 echo 'Install first: cd eval/memory-promptfoo && npm ci' >&2; exit 2
fi
export PATH="$TOOL/node_modules/node/bin:$PATH"
cd "$ROOT"
if [[ "${1:-}" = view ]];then
 shift
 exec "$NODE" --require "$TOOL/bind-local.cjs" "$CLI" view --port 15500 --no "$@"
fi
if [[ "${1:-}" = --help ]];then
 cat <<'EOF'
Usage: scripts/run-memory-promptfoo.sh [Go bench options]
       scripts/run-memory-promptfoo.sh --report DIR_OR_RESULTS_JSON
       scripts/run-memory-promptfoo.sh view
Default: run offline Go bench once, then import to Promptfoo.
--live explicitly enables model calls; all budgets remain enforced in Go.
--report imports existing results with zero new model calls (no other arguments).
view binds only 127.0.0.1:15500. Forward that port over SSH to view locally.
EOF
 exit 0
fi
stamp="$(date +%Y%m%d-%H%M%S)-$$"
IMPORT="$ROOT/tmp/memory-bench/promptfoo-import-$stamp"
if [[ "${1:-}" = --report ]];then
 [[ $# = 2 ]] || { echo '--report requires exactly one report path' >&2;exit 2; }
 REPORT="$2"; [[ "$REPORT" = /* ]] || REPORT="$ROOT/$REPORT"
 [[ ! -d "$REPORT" ]] || REPORT="$REPORT/results.json"
else
 # An owned output path guarantees we import this run, never a stale report.
 for arg in "$@";do
  [[ "$arg" != --output && "$arg" != --output=* ]] || { echo 'Use --report for existing output; new runs choose an isolated directory.' >&2;exit 2; }
 done
 REPORT="$ROOT/tmp/memory-bench/promptfoo-run-$stamp/results.json"
 set +e
 "$ROOT/scripts/run-memory-bench.sh" "$@" --output "$(dirname "$REPORT")"
 bench_status=$?
 set -e
 if [[ ! -f "$REPORT" ]];then
  echo 'Go bench did not produce a report' >&2
  if [[ "$bench_status" != 0 ]];then exit "$bench_status";fi
  exit 1
 fi
fi
"$NODE" "$TOOL/prepare.cjs" "$REPORT" "$IMPORT/config.json"
echo "Importing $REPORT (zero additional model calls)"
set +e
"$NODE" "$CLI" eval -c "$IMPORT/config.json" --no-cache --no-progress-bar --no-table -o "$IMPORT/promptfoo-results.json"
eval_status=$?
set -e
echo "Promptfoo results: $IMPORT/promptfoo-results.json"
echo 'View: scripts/run-memory-promptfoo.sh view'
if [[ "${bench_status:-0}" != 0 ]];then exit "$bench_status";fi
exit "$eval_status"
