#!/bin/bash
# 直接从本机 OfficeAce 客户端读取登录态并打上游，用于验证协议是否仍然可用。
# 不经过 officeace2api 网关，不消耗网关额度。
set -euo pipefail

AUTH="${1:-$HOME/Library/Application Support/office-raccoon/agent/config/auth.json}"
BASE="${BASE:-https://xiaohuanxiong.com/api/web/llm/v2}"
MODEL="${MODEL:-sn-kimi-k3}"

if [ ! -f "$AUTH" ]; then
  echo "找不到凭证文件: $AUTH" >&2
  echo "先启动并登录 OfficeAce 客户端。" >&2
  exit 1
fi

TOKEN=$(python3 -c "import json,sys;print(json.load(open(sys.argv[1]))['access_token'])" "$AUTH")

echo "凭证: $AUTH"
echo "指纹: $(printf '%s' "$TOKEN" | shasum -a 256 | cut -c1-12)"
python3 - "$TOKEN" <<'PY'
import base64, json, sys
p = sys.argv[1].split('.')[1]
p += '=' * (-len(p) % 4)
d = json.loads(base64.urlsafe_b64decode(p))
import datetime
print("用户:", d.get('sub'), "| 到期:", datetime.datetime.fromtimestamp(d.get('exp',0)).isoformat())
PY

echo
echo "==> POST $BASE/chat/completions"
curl -sS -m 60 --noproxy '*' -w "\nHTTP %{http_code} | %{time_total}s\n" \
  -X POST "$BASE/chat/completions" \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d "{\"model\":\"$MODEL\",\"messages\":[{\"role\":\"user\",\"content\":\"ping\"}],\"max_tokens\":8,\"stream\":false}"
