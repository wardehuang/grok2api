#!/usr/bin/env bash
set -euo pipefail
PASS="$(cat /home/ubuntu/grok2api/.bootstrap-admin-password | tr -d '\r\n')"
echo "PASS_LEN=${#PASS}"
curl -sS -w "\nHTTP=%{http_code}\n" -H "Content-Type: application/json" \
  -d "{\"username\":\"admin\",\"password\":\"${PASS}\"}" \
  "http://127.0.0.1:18453/api/admin/v1/auth/login" || true
echo
# Check if admin was bootstrapped; maybe password changed in UI
docker exec grok2api sh -c 'ls -la /app/data 2>/dev/null; ls -la /app/data/*.db 2>/dev/null || true'
