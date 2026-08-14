#!/usr/bin/env python3
"""Match CPA xAI emails with local SSO ledger, prepare Console import, report CPA-only."""

from __future__ import annotations

import json
import glob
import os
from pathlib import Path

CPA_DIR = Path("/home/ubuntu/.cli-proxy-api")
SSO_FILE = Path("/home/ubuntu/grok2api-import/total_accounts.txt")
OUT_DIR = Path("/home/ubuntu/grok2api-import")
IMPORT_JSON = OUT_DIR / "console_import_intersection.json"
CPA_ONLY_TXT = OUT_DIR / "cpa_only_not_in_sso.txt"
INTERSECTION_SUMMARY = OUT_DIR / "intersection_summary.json"


def load_sso_by_email(path: Path) -> dict[str, dict[str, str]]:
    mapping: dict[str, dict[str, str]] = {}
    duplicates = 0
    empty_sso = 0
    bad_lines = 0
    with path.open(encoding="utf-8", errors="replace") as handle:
        for line_number, raw in enumerate(handle, start=1):
            line = raw.strip()
            if not line:
                continue
            parts = line.split("----")
            if len(parts) < 3:
                bad_lines += 1
                continue
            email = parts[0].strip()
            password = parts[1].strip()
            sso = parts[2].strip()
            if not email:
                bad_lines += 1
                continue
            if not sso:
                empty_sso += 1
                continue
            key = email.lower()
            if key in mapping:
                duplicates += 1
            mapping[key] = {
                "email": email,
                "password": password,
                "sso": sso,
            }
    return mapping, {
        "sso_unique_emails": len(mapping),
        "sso_duplicate_email_lines_overwritten": duplicates,
        "sso_empty_token_lines": empty_sso,
        "sso_bad_lines": bad_lines,
    }


def load_cpa_accounts(cpa_dir: Path) -> list[dict]:
    accounts = []
    for path in sorted(cpa_dir.glob("xai-*.json")):
        with path.open(encoding="utf-8") as handle:
            item = json.load(handle)
        email = str(item.get("email") or "").strip()
        sub = str(item.get("sub") or "").strip()
        accounts.append(
            {
                "path": str(path),
                "filename": path.name,
                "email": email,
                "email_key": email.lower(),
                "sub": sub,
                "disabled": bool(item.get("disabled")),
            }
        )
    return accounts


def main() -> None:
    OUT_DIR.mkdir(parents=True, exist_ok=True)
    sso_map, sso_stats = load_sso_by_email(SSO_FILE)
    cpa_accounts = load_cpa_accounts(CPA_DIR)

    intersection = []
    cpa_only = []
    cpa_missing_email = 0
    seen_intersection_emails: set[str] = set()

    for account in cpa_accounts:
        email_key = account["email_key"]
        if not email_key:
            cpa_missing_email += 1
            cpa_only.append(account)
            continue
        sso_entry = sso_map.get(email_key)
        if sso_entry is None:
            cpa_only.append(account)
            continue
        if email_key in seen_intersection_emails:
            # same email multiple CPA files; still count as intersection once for import
            continue
        seen_intersection_emails.add(email_key)
        intersection.append(
            {
                "name": sso_entry["email"],
                "email": sso_entry["email"],
                "user_id": account["sub"],
                "sso_token": sso_entry["sso"],
                "cpa_file": account["filename"],
            }
        )

    import_document = {
        "provider": "grok_console",
        "accounts": [
            {
                "name": item["email"],
                "email": item["email"],
                "user_id": item["user_id"],
                "sso_token": item["sso_token"],
            }
            for item in intersection
        ],
    }
    IMPORT_JSON.write_text(
        json.dumps(import_document, ensure_ascii=False, indent=2) + "\n",
        encoding="utf-8",
    )
    os.chmod(IMPORT_JSON, 0o600)

    with CPA_ONLY_TXT.open("w", encoding="utf-8") as handle:
        handle.write("# email\tsub\tcpa_file\tdisabled\n")
        for account in cpa_only:
            handle.write(
                f"{account['email']}\t{account['sub']}\t{account['filename']}\t{account['disabled']}\n"
            )

    summary = {
        **sso_stats,
        "cpa_total_files": len(cpa_accounts),
        "cpa_missing_email": cpa_missing_email,
        "intersection_unique_emails": len(intersection),
        "cpa_only_count": len(cpa_only),
        "import_json": str(IMPORT_JSON),
        "cpa_only_txt": str(CPA_ONLY_TXT),
        "intersection_sample_emails": [item["email"] for item in intersection[:10]],
        "cpa_only_sample_emails": [item["email"] for item in cpa_only[:10]],
    }
    INTERSECTION_SUMMARY.write_text(
        json.dumps(summary, ensure_ascii=False, indent=2) + "\n",
        encoding="utf-8",
    )
    print(json.dumps(summary, ensure_ascii=False, indent=2))


if __name__ == "__main__":
    main()
