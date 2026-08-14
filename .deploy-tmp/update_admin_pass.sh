#!/usr/bin/env bash
set -euo pipefail
PASS='sk-Q0Moji4ag5Fell2STBTvzUd4XBicntqzTeNtkoiI'
printf '%s\n' "$PASS" > /home/ubuntu/grok2api/.bootstrap-admin-password
chmod 600 /home/ubuntu/grok2api/.bootstrap-admin-password
# verify login works
curl -fsS -H 'Content-Type: application/json' \
  -d "{\"username\":\"admin\",\"password\":\"${PASS}\"}" \
  'http://127.0.0.1:18453/api/admin/v1/auth/login' \
  | python3 -c 'import sys,json; d=json.load(sys.stdin); assert "data" in d and "tokens" in d["data"], d; print("login_ok")'
