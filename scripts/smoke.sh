#!/usr/bin/env bash
# Smoke-test a running tagger over both transports.
#
#   ./scripts/smoke.sh                              # localhost defaults
#   HTTP_ADDR=host:8080 GRPC_ADDR=host:9090 ./scripts/smoke.sh
#
# Needs curl. grpcurl is optional; the gRPC checks are skipped without it.
set -euo pipefail

HTTP_ADDR="${HTTP_ADDR:-localhost:8080}"
GRPC_ADDR="${GRPC_ADDR:-localhost:9090}"
TEXT="${TEXT:-Kubernetes operators reconcile desired state by watching custom resources.}"

pass() { printf '  \033[32mok\033[0m   %s\n' "$1"; }
fail() { printf '  \033[31mFAIL\033[0m %s\n' "$1"; exit 1; }
skip() { printf '  \033[33mskip\033[0m %s\n' "$1"; }

echo "REST  http://${HTTP_ADDR}"

status=$(curl -fsS -o /tmp/tagger-health.$$ -w '%{http_code}' "http://${HTTP_ADDR}/health") \
  || fail "GET /health did not respond"
[ "$status" = "200" ] || fail "GET /health returned ${status}"
pass "GET /health -> $(cat /tmp/tagger-health.$$)"
rm -f /tmp/tagger-health.$$

body=$(jq -cn --arg t "$TEXT" '{text:$t}' 2>/dev/null || printf '{"text":%s}' "\"${TEXT}\"")
status=$(curl -sS -o /tmp/tagger-tag.$$ -w '%{http_code}' \
  -H 'Content-Type: application/json' -d "$body" "http://${HTTP_ADDR}/tag")
[ "$status" = "200" ] || fail "POST /tag returned ${status}: $(cat /tmp/tagger-tag.$$)"
pass "POST /tag -> $(cat /tmp/tagger-tag.$$)"
rm -f /tmp/tagger-tag.$$

# An empty text must be rejected before the request reaches the model.
status=$(curl -sS -o /dev/null -w '%{http_code}' \
  -H 'Content-Type: application/json' -d '{"text":""}' "http://${HTTP_ADDR}/tag")
[ "$status" = "400" ] || fail "POST /tag with empty text returned ${status}, want 400"
pass "POST /tag (empty text) -> 400"

echo
echo "gRPC  ${GRPC_ADDR}"

if ! command -v grpcurl >/dev/null 2>&1; then
  skip "grpcurl not installed (brew install grpcurl)"
  exit 0
fi

# Server reflection is enabled, so no local .proto copy is needed.
out=$(grpcurl -plaintext -d "$body" "${GRPC_ADDR}" tagger.v1.Tagger/Tag) \
  || fail "tagger.v1.Tagger/Tag failed"
pass "Tagger/Tag -> $(echo "$out" | tr -d '\n ')"

out=$(grpcurl -plaintext -d '{"service":"tagger.v1.Tagger"}' \
  "${GRPC_ADDR}" grpc.health.v1.Health/Check) || fail "health check failed"
echo "$out" | grep -q SERVING || fail "health check is not SERVING: ${out}"
pass "Health/Check -> SERVING"

echo
echo "All smoke checks passed."
