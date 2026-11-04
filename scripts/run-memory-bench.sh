#!/usr/bin/env bash
# No UE/process services are launched: this invokes one same-package Go test.
set -euo pipefail
ROOT="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"
live=false; cases=all; groups=memory,no-memory; repeats=2; repeat_set=false
concurrency=1; max_calls=30; config="${LLM_CONFIG:-}"; suite="$ROOT/agenttown-mcp/cmd/agenttown-mcp/testdata/memory_bench/suite.yaml"
output="$ROOT/tmp/memory-bench/$(date +%Y%m%d-%H%M%S)-$$"
usage(){ cat <<'EOF'
Usage: scripts/run-memory-bench.sh [--live] [--cases all|smoke|id,id]
       [--groups memory,no-memory] [--repeats N] [--concurrency N]
       [--llm-config FILE] [--suite FILE] [--output DIR] [--max-calls N]
Offline is the default (scripted protocol replay, not model capability scores).
Live defaults to smoke (5 cases), 1 repeat, 1 worker, 30 HTTP calls maximum.
EOF
}
case_set=false
while (($#)); do
 case "$1" in
 --live) live=true; shift;;
 --cases|--groups|--repeats|--concurrency|--llm-config|--suite|--output|--max-calls)
  (($#>=2)) || { usage >&2; exit 2; }
  case "$1" in
   --cases) cases="$2";case_set=true;; --groups) groups="$2";; --repeats) repeats="$2";repeat_set=true;;
   --concurrency) concurrency="$2";; --llm-config) config="$2";; --suite) suite="$2";; --output) output="$2";; --max-calls) max_calls="$2";;
  esac;shift 2;;
 --help|-h) usage;exit 0;; *) echo "Unknown argument: $1" >&2;usage >&2;exit 2;;
 esac
done
if $live;then
 $case_set || cases=smoke
 $repeat_set || repeats=1
fi
# Reuse project-local environment configuration without displaying credentials.
if $live && [[ -f "$ROOT/.env" ]];then set -a; source "$ROOT/.env"; set +a;fi
[[ -n "$config" ]] || config="${LLM_CONFIG:-}"
[[ "$suite" = /* ]] || suite="$ROOT/$suite"
[[ "$output" = /* ]] || output="$ROOT/$output"
if [[ -n "$config" && "$config" != /* ]];then config="$ROOT/$config";fi
mkdir -p "$output"
cd "$ROOT/agenttown-mcp"
GO="$(command -v go || true)";[[ -n "$GO" ]] || GO=/usr/local/go/bin/go
"$GO" test ./cmd/agenttown-mcp -run '^TestMemoryBench_Run$' -count=1 -timeout=60m -v -args \
 -memory-bench-run -memory-bench-live="$live" -memory-bench-suite="$suite" \
 -memory-bench-cases="$cases" -memory-bench-groups="$groups" -memory-bench-repeats="$repeats" \
 -memory-bench-concurrency="$concurrency" -memory-bench-max-calls="$max_calls" \
 -memory-bench-config="$config" -memory-bench-output="$output"
printf 'Report: %s/summary.md\n' "$output"
