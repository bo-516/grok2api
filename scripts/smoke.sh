#!/bin/sh
# Curl smoke test for a running agent-mock. Local dev only.
# Usage: ./scripts/smoke.sh [base-url-without-/v1]
set -eu
BASE="${1:-http://127.0.0.1:8787}"
HDR='Content-Type: application/json'

echo "== text =="
curl -sS "$BASE/v1/chat/completions" -H "$HDR" \
  -d '{"model":"gpt-4o-mini","messages":[{"role":"user","content":"Reply with exactly the word: pong"}]}'
echo

echo "== stream =="
curl -sS -N "$BASE/v1/chat/completions" -H "$HDR" \
  -d '{"model":"gpt-4o-mini","stream":true,"messages":[{"role":"user","content":"Count from 1 to 5"}]}'
echo

echo "== json_schema =="
curl -sS "$BASE/v1/chat/completions" -H "$HDR" \
  -d '{"model":"gpt-4o-mini","messages":[{"role":"user","content":"What is the capital of France?"}],"response_format":{"type":"json_schema","json_schema":{"name":"capital","schema":{"type":"object","additionalProperties":false,"required":["city"],"properties":{"city":{"type":"string"}}}}}}'
echo

echo "== tools =="
curl -sS "$BASE/v1/chat/completions" -H "$HDR" \
  -d '{"model":"gpt-4o-mini","messages":[{"role":"user","content":"北京今天天气怎么样？"}],"tools":[{"type":"function","function":{"name":"get_weather","description":"weather","parameters":{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}}}]}'
echo
