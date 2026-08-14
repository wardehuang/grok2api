#!/usr/bin/env bash
set -euo pipefail

PASS='sk-Q0Moji4ag5Fell2STBTvzUd4XBicntqzTeNtkoiI'
BASE='http://127.0.0.1:18453'
OUT_DIR='/home/ubuntu/grok2api-import'
EMAIL_FILE="${OUT_DIR}/console_sync_failed_emails.txt"
ID_FILE="${OUT_DIR}/console_sync_failed_ids.txt"
SUMMARY_FILE="${OUT_DIR}/console_failed_cleanup_summary.json"

mkdir -p "${OUT_DIR}"

echo "== login =="
LOGIN_JSON=$(curl -fsS -H 'Content-Type: application/json' \
  -d "{\"username\":\"admin\",\"password\":\"${PASS}\"}" \
  "${BASE}/api/admin/v1/auth/login")
TOKEN=$(python3 -c 'import json,sys; d=json.load(sys.stdin); print(d["data"]["tokens"]["accessToken"])' <<<"${LOGIN_JSON}")

echo "== summary before =="
SUMMARY_BEFORE=$(curl -fsS -H "Authorization: Bearer ${TOKEN}" \
  "${BASE}/api/admin/v1/accounts/summary")
echo "${SUMMARY_BEFORE}" | python3 -m json.tool

echo "== export reauthRequired console emails via API =="
export TOKEN BASE EMAIL_FILE ID_FILE OUT_DIR
python3 - <<'PY'
import json
import os
import urllib.request

token = os.environ["TOKEN"]
base = os.environ["BASE"]
email_file = os.environ["EMAIL_FILE"]
id_file = os.environ["ID_FILE"]
out_dir = os.environ["OUT_DIR"]

def get(url: str):
    request = urllib.request.Request(url, headers={"Authorization": f"Bearer {token}"})
    with urllib.request.urlopen(request, timeout=120) as response:
        return json.load(response)

page = 1
page_size = 2000
emails = []
ids = []
total = None
while True:
    payload = get(
        f"{base}/api/admin/v1/accounts?provider=grok_console&status=reauthRequired&page={page}&pageSize={page_size}"
    )
    data = payload["data"]
    items = data["items"]
    total = data["total"]
    for item in items:
        email = (item.get("email") or item.get("name") or "").strip()
        account_id = str(item["id"])
        if email:
            emails.append(email)
            ids.append(account_id)
    print(f"page={page} got={len(items)} exported={len(emails)} total={total}", flush=True)
    if page * page_size >= total or not items:
        break
    page += 1

with open(email_file, "w", encoding="utf-8") as handle:
    handle.write("\n".join(emails) + ("\n" if emails else ""))
with open(id_file, "w", encoding="utf-8") as handle:
    handle.write("\n".join(ids) + ("\n" if ids else ""))

pre = {
    "failed_exported": len(emails),
    "list_total": total,
    "email_file": email_file,
    "id_file": id_file,
}
with open(f"{out_dir}/console_failed_pre_summary.json", "w", encoding="utf-8") as handle:
    json.dump(pre, handle, ensure_ascii=False, indent=2)
    handle.write("\n")
print(json.dumps(pre, ensure_ascii=False, indent=2))
PY

FAILED_COUNT=$(wc -l < "${EMAIL_FILE}" | tr -d ' ')
echo "failed_count=${FAILED_COUNT}"
head -n 5 "${EMAIL_FILE}" || true

if [[ "${FAILED_COUNT}" -eq 0 ]]; then
  echo "no failed accounts to clean"
  exit 0
fi

echo "== cleanup preview =="
curl -fsS -H "Authorization: Bearer ${TOKEN}" -H 'Content-Type: application/json' \
  -d '{"provider":"grok_console","statuses":["reauthRequired"]}' \
  "${BASE}/api/admin/v1/accounts/cleanup-preview" \
  | tee "${OUT_DIR}/cleanup_preview.json" | python3 -m json.tool

echo "== cleanup reauthRequired console =="
curl -fsS -H "Authorization: Bearer ${TOKEN}" -H 'Content-Type: application/json' \
  -d '{"provider":"grok_console","statuses":["reauthRequired"]}' \
  "${BASE}/api/admin/v1/accounts/cleanup" \
  | tee "${OUT_DIR}/cleanup_result.json" | python3 -m json.tool

echo "== summary after =="
curl -fsS -H "Authorization: Bearer ${TOKEN}" \
  "${BASE}/api/admin/v1/accounts/summary" \
  | tee "${OUT_DIR}/summary_after.json" | python3 -m json.tool

python3 - <<PY
import json
from pathlib import Path
out_dir = Path("${OUT_DIR}")
out = {
  "failed_exported": int("${FAILED_COUNT}"),
  "email_file": "${EMAIL_FILE}",
  "id_file": "${ID_FILE}",
  "pre": json.loads((out_dir / "console_failed_pre_summary.json").read_text(encoding="utf-8")),
  "preview": json.loads((out_dir / "cleanup_preview.json").read_text(encoding="utf-8")).get("data"),
  "cleanup": json.loads((out_dir / "cleanup_result.json").read_text(encoding="utf-8")).get("data"),
  "summary_before": json.loads('''${SUMMARY_BEFORE}''').get("data"),
  "summary_after": json.loads((out_dir / "summary_after.json").read_text(encoding="utf-8")).get("data"),
}
(out_dir / "console_failed_cleanup_summary.json").write_text(
    json.dumps(out, ensure_ascii=False, indent=2) + "\n", encoding="utf-8"
)
print(json.dumps(out, ensure_ascii=False, indent=2))
PY

echo "DONE email_file=${EMAIL_FILE}"
