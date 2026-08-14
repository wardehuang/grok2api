#!/usr/bin/env bash
set -euo pipefail

BASE_URL="http://127.0.0.1:18453"
DEPLOY_DIR="/home/ubuntu/grok2api"
IMPORT_DIR="/home/ubuntu/grok2api-import"
IMPORT_JSON="${IMPORT_DIR}/console_import_intersection.json"
NEW_PASS="$(openssl rand -base64 18 | tr -d '/+=' | head -c 24)"
COOKIE_JAR="/tmp/grok2api-admin.cookies"

echo "==> reset admin via bootstrap reseed"
# Stop container, clear admins table, rewrite bootstrap password, restart
cd "${DEPLOY_DIR}"
docker compose --env-file .env stop grok2api

# Update config bootstrap password
python3 - <<PY
from pathlib import Path
import re
path = Path("${DEPLOY_DIR}/config.yaml")
text = path.read_text(encoding="utf-8")
# replace bootstrapAdmin password line only
new_pass = """${NEW_PASS}"""
pattern = re.compile(r"(bootstrapAdmin:\n(?:.*\n)*?\s+password:\s*)\".*?\"", re.M)
updated, count = pattern.subn(r'\1"' + new_pass + '"', text, count=1)
if count != 1:
    # fallback simple: find password under bootstrapAdmin section
    lines = text.splitlines()
    in_section = False
    out = []
    replaced = False
    for line in lines:
        if line.startswith("bootstrapAdmin:"):
            in_section = True
            out.append(line)
            continue
        if in_section and line.startswith("  password:"):
            out.append(f'  password: "{new_pass}"')
            replaced = True
            in_section = False
            continue
        if in_section and line and not line.startswith(" ") and not line.startswith("\t"):
            in_section = False
        out.append(line)
    if not replaced:
        raise SystemExit("failed to rewrite bootstrapAdmin.password")
    updated = "\n".join(out) + ("\n" if text.endswith("\n") else "")
path.write_text(updated, encoding="utf-8")
print("config bootstrap password updated")
PY

printf '%s\n' "${NEW_PASS}" > "${DEPLOY_DIR}/.bootstrap-admin-password"
chmod 600 "${DEPLOY_DIR}/.bootstrap-admin-password" "${DEPLOY_DIR}/config.yaml"

# Delete admins so Bootstrap recreates with new password
# Volume data path
VOL_DATA="$(docker volume inspect grok2api_grok2api-data --format '{{.Mountpoint}}')"
echo "volume=$VOL_DATA"
sudo python3 - <<PY
import sqlite3
from pathlib import Path
db = Path("${VOL_DATA}") / "backend.db"
print("db", db, "exists", db.exists())
conn = sqlite3.connect(str(db))
cur = conn.cursor()
for table in ("admin_sessions", "admins"):
    try:
        cur.execute(f"DELETE FROM {table}")
        print(table, "deleted", cur.rowcount)
    except Exception as exc:
        print(table, "skip", exc)
conn.commit()
conn.close()
PY

docker compose --env-file .env start grok2api
for i in $(seq 1 30); do
  status="$(docker inspect --format '{{if .State.Health}}{{.State.Health.Status}}{{else}}{{.State.Status}}{{end}}' grok2api 2>/dev/null || echo missing)"
  echo "health $i $status"
  if [ "$status" = "healthy" ]; then
    break
  fi
  sleep 2
done

echo "==> login"
curl -fsS -c "${COOKIE_JAR}" -H "Content-Type: application/json" \
  -d "{\"username\":\"admin\",\"password\":\"${NEW_PASS}\"}" \
  "${BASE_URL}/api/admin/v1/auth/login" > /tmp/grok2api-login.json
python3 - <<'PY'
import json
data=json.load(open("/tmp/grok2api-login.json",encoding="utf-8"))
# Success wrapper: {data:{admin,tokens}} or similar
print(json.dumps(data, ensure_ascii=False)[:400])
tokens = None
if isinstance(data, dict):
    if "data" in data and isinstance(data["data"], dict):
        tokens = data["data"].get("tokens")
    tokens = tokens or data.get("tokens")
if not tokens or not tokens.get("accessToken"):
    raise SystemExit("login response missing accessToken")
open("/tmp/grok2api-access.token","w",encoding="utf-8").write(tokens["accessToken"])
print("access_token_saved")
PY

ACCESS="$(cat /tmp/grok2api-access.token)"
ACCOUNT_COUNT="$(python3 -c 'import json; print(len(json.load(open("'"${IMPORT_JSON}"'",encoding="utf-8"))["accounts"]))')"
echo "==> import console accounts count=${ACCOUNT_COUNT}"

# Prefer Authorization bearer; also send cookies
curl -sS -N \
  -b "${COOKIE_JAR}" \
  -H "Authorization: Bearer ${ACCESS}" \
  -F "files=@${IMPORT_JSON};type=application/json" \
  "${BASE_URL}/api/admin/v1/accounts/console/import" | tee /tmp/grok2api-console-import.sse

echo
python3 - <<'PY'
from pathlib import Path
import json
text = Path("/tmp/grok2api-console-import.sse").read_text(encoding="utf-8", errors="replace")
print("--- last 40 lines ---")
print("\n".join(text.splitlines()[-40:]))
event=None
for line in text.splitlines():
    if line.startswith("event:"):
        event=line[6:].strip()
    elif line.startswith("data:") and event in ("complete","error"):
        raw=line[5:].strip()
        print(event, raw[:500])
        try:
            print(event, "parsed", json.loads(raw))
        except Exception:
            pass
PY

echo "ADMIN_PASSWORD=${NEW_PASS}"
echo "CPA_ONLY_FILE=${IMPORT_DIR}/cpa_only_not_in_sso.txt"
wc -l "${IMPORT_DIR}/cpa_only_not_in_sso.txt"
