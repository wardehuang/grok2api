#!/usr/bin/env bash
set -euo pipefail
cd /home/ubuntu/grok2api
docker compose up -d grok2api
for i in 1 2 3 4 5 6 7 8 9 10; do
  if curl -fsS -o /dev/null -w "%{http_code}" http://127.0.0.1:18453/api/admin/v1/system >/dev/null 2>&1 || true; then
    status="$(curl -s -o /dev/null -w "%{http_code}" http://127.0.0.1:18453/readyz || true)"
    if [ "$status" = "200" ]; then
      break
    fi
  fi
  sleep 2
done
docker ps --filter name=^grok2api$ --format 'table {{.Names}}\t{{.Status}}\t{{.Image}}'
docker inspect grok2api --format 'Created={{.Created}} Image={{.Config.Image}}'
python3 <<'PY'
import json, urllib.request
passw = open('/home/ubuntu/grok2api/.bootstrap-admin-password').read().strip()
req = urllib.request.Request(
    'http://127.0.0.1:18453/api/admin/v1/auth/login',
    data=json.dumps({'username': 'admin', 'password': passw}).encode(),
    headers={'Content-Type': 'application/json'},
)
tok = json.load(urllib.request.urlopen(req))['data']['tokens']['accessToken']
req2 = urllib.request.Request(
    'http://127.0.0.1:18453/api/admin/v1/system',
    headers={'Authorization': 'Bearer ' + tok},
)
print('system', json.load(urllib.request.urlopen(req2)))
PY
# binary contains the media path constant used by videoResultURL
if docker exec grok2api sh -c "strings /app/grok2api | grep -F '/v1/media/videos/' | head -3"; then
  echo "binary_has_media_videos_path=yes"
else
  echo "binary_has_media_videos_path=no"
  exit 1
fi
if docker exec grok2api sh -c "strings /app/grok2api | grep -F '优先返回免鉴权本地媒体地址' | head -1"; then
  echo "binary_has_video_result_comment=yes"
else
  echo "binary_has_video_result_comment=no"
fi
