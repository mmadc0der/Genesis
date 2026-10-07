#!/usr/bin/env bash
set -euo pipefail
RUN_ID="${1:?run id}"
for _ in $(seq 1 40); do
  sleep 15
  line=$(curl -sS http://127.0.0.1:8790/api/runs | python3 -c "
import json, sys
for r in json.load(sys.stdin)['runs']:
    if r['run_id'] == sys.argv[1]:
        print(r['state'], r.get('finish_reason',''), r.get('error',''))
        break
" "$RUN_ID")
  echo "$(date -u +%H:%M:%S) $RUN_ID $line"
  case "$line" in
    completed*|failed*) exit 0 ;;
  esac
done
echo "timeout waiting for $RUN_ID"
exit 1
