#!/usr/bin/env bash
set -euo pipefail

BASE_URL="http://127.0.0.1:18453"
ADMIN_USER="admin"
ADMIN_PASS="$(cat /home/ubuntu/grok2api/.bootstrap-admin-password)"
IMPORT_JSON="/home/ubuntu/grok2api-import/console_import_intersection.json"
COOKIE_JAR="/tmp/grok2api-admin.cookies"
LOGIN_RESP="/tmp/grok2api-login.json"
IMPORT_RESP="/tmp/grok2api-console-import.sse"

echo "==> compute intersection"
python3 /home/ubuntu/grok2api-import/intersect_import_console.py

ACCOUNT_COUNT="$(python3 -c 'import json; d=json.load(open("'"$IMPORT_JSON"'",encoding="utf-8")); print(len(d["accounts"]))')"
echo "intersection_accounts=$ACCOUNT_COUNT"
if [ "$ACCOUNT_COUNT" -eq 0 ]; then
  echo "no intersection; skip import"
  exit 0
fi

echo "==> admin login"
curl -fsS -c "$COOKIE_JAR" -H "Content-Type: application/json" \
  -d "{\"username\":\"${ADMIN_USER}\",\"password\":\"${ADMIN_PASS}\"}" \
  "${BASE_URL}/api/admin/v1/auth/login" > "$LOGIN_RESP"
python3 - <<'PY'
import json
data = json.load(open("/tmp/grok2api-login.json", encoding="utf-8"))
# tolerate wrapped or bare success
print("login_keys", sorted(data.keys()) if isinstance(data, dict) else type(data).__name__)
print("login_ok")
PY

echo "==> console import ${ACCOUNT_COUNT} accounts"
# SSE stream; capture complete event
curl -sS -N -b "$COOKIE_JAR" \
  -F "files=@${IMPORT_JSON};type=application/json" \
  "${BASE_URL}/api/admin/v1/accounts/console/import" | tee "$IMPORT_RESP"

echo
echo "==> parse import result"
python3 - <<'PY'
from pathlib import Path
text = Path("/tmp/grok2api-console-import.sse").read_text(encoding="utf-8", errors="replace")
print("--- sse tail ---")
print("\n".join(text.splitlines()[-30:]))
# parse event: complete
event = None
data_line = None
for line in text.splitlines():
    if line.startswith("event:"):
        event = line[6:].strip()
    elif line.startswith("data:") and event == "complete":
        data_line = line[5:].strip()
    elif line.startswith("event:") is False and line.startswith("data:") and event in (None, "error", "complete"):
        if "created" in line or "authImportFailed" in line or '"code"' in line:
            print("data_candidate", line[:300])
if data_line:
    import json
    print("complete", data_line)
else:
    # maybe NDJSON-ish or single JSON body
    stripped = text.strip()
    if stripped.startswith("{"):
        print("body", stripped[:500])
PY

echo "==> done"
cat /home/ubuntu/grok2api-import/intersection_summary.json
