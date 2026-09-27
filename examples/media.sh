#!/bin/sh
# media.sh generates one image, then a short video that uses that image as the first frame.
# A running agent-mock is required. Usage: ./examples/media.sh [base-url-without-/v1]
# A non-200 body, a missing url or request_id, or a failed video exits 1.
# The image URL is printed as IMAGE, the video URL as VIDEO.
set -eu
BASE="${1:-http://127.0.0.1:8787}"
HDR='Content-Type: application/json'

echo "== image =="
BODY=$(curl -sS "$BASE/v1/images/generations" -H "$HDR" \
  -d '{"prompt":"An orange cat sitting on a windowsill, watercolor","size":"1792x1024"}')
printf '%s\n' "$BODY"
URL=$(printf '%s' "$BODY" | sed -n 's/.*"url":"\([^"]*\)".*/\1/p')
if [ -z "$URL" ]; then
  echo "image response has no url" >&2
  exit 1
fi
echo "IMAGE $URL"

echo "== video =="
ACK=$(curl -sS "$BASE/v1/videos/generations" -H "$HDR" \
  -d "{\"prompt\":\"The camera slowly pushes in\",\"image\":{\"url\":\"$URL\"},\"duration\":6}")
printf '%s\n' "$ACK"
ID=$(printf '%s' "$ACK" | sed -n 's/.*"request_id":"\([^"]*\)".*/\1/p')
if [ -z "$ID" ]; then
  echo "video response has no request_id" >&2
  exit 1
fi

echo "== poll $ID =="
i=0
while [ "$i" -lt 120 ]; do
  ST=$(curl -sS "$BASE/v1/videos/$ID")
  printf '%s\n' "$ST"
  if printf '%s' "$ST" | grep -q '"status":"done"'; then
    VIDEO=$(printf '%s' "$ST" | sed -n 's/.*"url":"\([^"]*\)".*/\1/p')
    echo "VIDEO $VIDEO"
    exit 0
  fi
  if printf '%s' "$ST" | grep -q '"status":"failed"'; then
    exit 1
  fi
  i=$((i + 1))
  sleep 5
done
echo "video still pending after 10 minutes" >&2
exit 1
