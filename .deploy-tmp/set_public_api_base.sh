#!/usr/bin/env bash
set -euo pipefail

BASE_URL="${BASE_URL:-http://127.0.0.1:18453}"
PUBLIC_API_BASE_URL="${PUBLIC_API_BASE_URL:-http://163.192.9.157:18453}"
PASS="$(tr -d '\r\n' < /home/ubuntu/grok2api/.bootstrap-admin-password)"

LOGIN_JSON="$(curl -fsS -H 'Content-Type: application/json' \
  -d "{\"username\":\"admin\",\"password\":\"${PASS}\"}" \
  "${BASE_URL}/api/admin/v1/auth/login")"
TOKEN="$(python3 -c 'import json,sys; print(json.load(sys.stdin)["data"]["tokens"]["accessToken"])' <<<"${LOGIN_JSON}")"
echo "login_ok"

echo "=== before system ==="
curl -fsS -H "Authorization: Bearer ${TOKEN}" "${BASE_URL}/api/admin/v1/system"
echo

SETTINGS_JSON="$(curl -fsS -H "Authorization: Bearer ${TOKEN}" "${BASE_URL}/api/admin/v1/settings")"
export SETTINGS_JSON PUBLIC_API_BASE_URL
python3 <<'PY'
import json
import os

settings = json.loads(os.environ["SETTINGS_JSON"])
public_base = os.environ["PUBLIC_API_BASE_URL"].rstrip("/")
data = settings["data"]
config = data["config"]
revision = data["revision"]
print("revision", revision)
print("frontend_before", config.get("frontend"))
config.setdefault("frontend", {})["publicApiBaseURL"] = public_base
payload = {"revision": str(revision), "config": config}
with open("/tmp/g2-settings-put.json", "w", encoding="utf-8") as handle:
    json.dump(payload, handle)
print("wrote /tmp/g2-settings-put.json publicApiBaseURL=", public_base)
PY

echo "=== put settings ==="
RESULT="$(curl -fsS -X PUT \
  -H "Authorization: Bearer ${TOKEN}" \
  -H "Content-Type: application/json" \
  --data-binary @/tmp/g2-settings-put.json \
  "${BASE_URL}/api/admin/v1/settings")"
python3 -c 'import json,sys; d=json.load(sys.stdin); print("frontend_after", d["data"]["config"].get("frontend")); print("revision", d["data"]["revision"])' <<<"${RESULT}"

echo "=== after system ==="
curl -fsS -H "Authorization: Bearer ${TOKEN}" "${BASE_URL}/api/admin/v1/system"
echo

echo "=== public ip ==="
curl -fsS https://api.ipify.org || true
echo
