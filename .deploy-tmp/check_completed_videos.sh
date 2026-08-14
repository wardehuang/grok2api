#!/usr/bin/env bash
set -euo pipefail
PASS="$(tr -d '\r\n' < /home/ubuntu/grok2api/.bootstrap-admin-password)"
LOGIN="$(curl -fsS -H 'Content-Type: application/json' -d "{\"username\":\"admin\",\"password\":\"${PASS}\"}" http://127.0.0.1:18453/api/admin/v1/auth/login)"
TOKEN="$(python3 -c 'import json,sys; print(json.load(sys.stdin)["data"]["tokens"]["accessToken"])' <<<"$LOGIN")"
# Find a completed video job with asset
curl -fsS -H "Authorization: Bearer ${TOKEN}" \
  "http://127.0.0.1:18453/api/admin/v1/media/videos?status=completed&page=1&pageSize=5" \
  | python3 -c '
import json,sys
d=json.load(sys.stdin)
items=d.get("data",{}).get("items") or []
print("completed_jobs", len(items))
for item in items[:5]:
    print(item.get("id"), "assetId=", item.get("assetId"), "status=", item.get("status"))
'
