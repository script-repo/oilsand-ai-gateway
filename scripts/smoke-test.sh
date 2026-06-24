#!/usr/bin/env bash
set -euo pipefail

if [[ $# -ne 2 ]]; then
  echo "Usage: $0 <gateway-url> <client-api-key>" >&2
  exit 2
fi

GATEWAY_URL="${1%/}"
CLIENT_API_KEY="$2"

curl -fsS "$GATEWAY_URL/v1/models" \
  -H "Authorization: Bearer $CLIENT_API_KEY" | jq '.data[].id'

curl -fsS -X POST "$GATEWAY_URL/v1/chat/completions" \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer $CLIENT_API_KEY" \
  -d '{
    "model": "llama3.1",
    "messages": [{"role": "user", "content": "Say hello from local Ollama."}],
    "stream": false
  }' | jq .

curl -fsS -X POST "$GATEWAY_URL/v1/chat/completions" \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer $CLIENT_API_KEY" \
  -d '{
    "model": "or-deepseek-r1-free",
    "messages": [{"role": "user", "content": "Say hello from OpenRouter."}],
    "stream": false
  }' | jq .
