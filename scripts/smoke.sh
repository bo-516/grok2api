#!/bin/sh
# Curl smoke test for a running agent-mock. Local dev only.
# Usage: ./scripts/smoke.sh [--media] [base-url-without-/v1]
set -eu
MEDIA=0
if [ "${1:-}" = "--media" ]; then
  MEDIA=1
  shift
fi
BASE="${1:-http://127.0.0.1:8787}"
HDR='Content-Type: application/json'
if [ "$MEDIA" = 1 ]; then
  echo "== image =="
  BODY=$(curl -sS "$BASE/v1/images/generations" -H "$HDR" \
    -d '{"prompt":"A red apple on a white table","size":"1024x1024"}')
  echo "$BODY"
  URL=$(printf '%s' "$BODY" | sed -n 's/.*"url":"\([^"]*\)".*/\1/p')
  if [ -n "$URL" ]; then
    echo "== download =="
    curl -sS -D - -o /dev/null "$URL" | head -n 20
  fi
  exit 0
fi

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
