#!/usr/bin/env bash
set -euo pipefail

BASE_URL="http://127.0.0.1:18453"
ADMIN_USER="admin"
ADMIN_PASS="$(cat /home/ubuntu/grok2api/.bootstrap-admin-password)"
IMPORT_JSON="/home/ubuntu/grok2api-import/sso_console_import.json"
COOKIE_JAR="/tmp/grok2api-admin.cookies"
IMPORT_RESP="/tmp/grok2api-console-import-sso.sse"

ACCOUNT_COUNT="$(python3 -c 'import json; print(len(json.load(open("/home/ubuntu/grok2api-import/sso_console_import.json",encoding="utf-8"))["accounts"]))')"
echo "account_count=$ACCOUNT_COUNT"

echo "==> login"
curl -fsS -c "$COOKIE_JAR" -H "Content-Type: application/json" \
  -d "{\"username\":\"${ADMIN_USER}\",\"password\":\"${ADMIN_PASS}\"}" \
  "${BASE_URL}/api/admin/v1/auth/login" > /tmp/grok2api-login.json

python3 - <<'PY'
import json
data = json.load(open("/tmp/grok2api-login.json", encoding="utf-8"))
tokens = None
if isinstance(data, dict):
    if isinstance(data.get("data"), dict):
        tokens = data["data"].get("tokens")
    tokens = tokens or data.get("tokens")
if not tokens or not tokens.get("accessToken"):
    raise SystemExit("login missing accessToken: " + json.dumps(data, ensure_ascii=False)[:500])
open("/tmp/grok2api-access.token", "w", encoding="utf-8").write(tokens["accessToken"])
print("login_ok")
PY

ACCESS="$(cat /tmp/grok2api-access.token)"

echo "==> import console"
curl -sS -N \
  -b "$COOKIE_JAR" \
  -H "Authorization: Bearer ${ACCESS}" \
  -F "files=@${IMPORT_JSON};type=application/json" \
  "${BASE_URL}/api/admin/v1/accounts/console/import" | tee "$IMPORT_RESP"

echo
python3 - <<'PY'
from pathlib import Path
import json
text = Path("/tmp/grok2api-console-import-sso.sse").read_text(encoding="utf-8", errors="replace")
print("--- last 50 lines ---")
print("\n".join(text.splitlines()[-50:]))
event = None
for line in text.splitlines():
    if line.startswith("event:"):
        event = line[6:].strip()
    elif line.startswith("data:") and event in ("complete", "error"):
        raw = line[5:].strip()
        try:
            print(event, json.dumps(json.loads(raw), ensure_ascii=False))
        except Exception:
            print(event, raw[:800])
PY
