#!/bin/sh
# Record the mock harness's data from a live StarRocks.
#
#   sh tools/record_demo.sh
#
# The captures must be real: a screenshot of a chart drawn from a synthetic
# curve would be a picture of nothing. Point the config at a cluster under
# load, then run this.
set -eu

cd "$(dirname "$0")/../.."
OUT="tools/uimock/recorded"
mkdir -p "$OUT"

SECONDS_TO_SAMPLE="${1:-40}"

echo "recording the dashboard over ${SECONDS_TO_SAMPLE}s..."
sr-dash -json dashboard "$SECONDS_TO_SAMPLE" > "$OUT/dashboard.json"

echo "recording the session list..."
sr-dash -json sessions > "$OUT/sessions.json"

echo "done: $OUT"
