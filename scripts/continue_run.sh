#!/usr/bin/env bash
set -euo pipefail
RUN_ID="${1:?run id}"
MESSAGE="${2:?message}"
curl -sS -X POST "http://127.0.0.1:8790/api/runs/${RUN_ID}/continue" \
  -H 'Content-Type: application/json' \
  --data "$(python3 -c "import json,sys; print(json.dumps({'message': sys.argv[1]}))" "$MESSAGE")"
