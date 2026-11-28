#!/usr/bin/env bash
# 把小浣熊客户端的新 token 推到线上网关
#
#   ./scripts/push-account.sh               # 读本机 auth.json，校验后推到线上
#   ./scripts/push-account.sh --check       # 只校验，不推送
#
# 背景：token 是 xiaohuanxiong.com/iam 签发的 JWT，有效期固定 30 天，
# 且因为 isIamToken 为真，客户端不会保存 refresh_token —— 到期只能重新登录，
# 没有静默续期通道。线上网关 sync_local=false，所以每次重登后必须手动推一次。
set -euo pipefail

PROJECT_DIR="$(cd "$(dirname "$0")/.." && pwd)"
GATEWAY="${GATEWAY:-https://officeace.1task.cn}"
AUTH_JSON="${AUTH_JSON:-$HOME/Library/Application Support/office-raccoon/agent/config/auth.json}"
PANEL_KEY="${PANEL_KEY:-$(python3 -c "import json,sys;print(json.load(open(sys.argv[1]))['panel_key'])" "$PROJECT_DIR/data/config.json")}"

CHECK_ONLY=0
[ "${1:-}" = "--check" ] && CHECK_ONLY=1

if [ ! -f "$AUTH_JSON" ]; then
  echo "✗ 找不到凭据文件: $AUTH_JSON"
  echo "  先在「商汤小浣熊」客户端里重新登录"
  exit 1
fi

read -r TOKEN EXP SID <<EOF
$(python3 - "$AUTH_JSON" <<'PY'
import json, base64, sys, datetime
d = json.load(open(sys.argv[1]))
tok = d.get("access_token") or ""
if not tok:
    print("", 0, "")
    raise SystemExit(0)
p = tok.split(".")
try:
    pl = json.loads(base64.urlsafe_b64decode(p[1] + "=" * (-len(p[1]) % 4)))
except Exception:
    print(tok, 0, "")
    raise SystemExit(0)
print(tok, pl.get("exp", 0), pl.get("sid", ""))
PY
)
EOF

if [ -z "$TOKEN" ]; then
  echo "✗ auth.json 里没有 access_token"
  exit 1
fi

REMAIN=$(python3 -c "
import sys,time
exp=int(sys.argv[1])
left=exp-time.time()
print(f'{left/3600:.1f}')
" "$EXP")

if python3 -c "import sys;sys.exit(0 if int(sys.argv[1])>__import__('time').time() else 1)" "$EXP"; then
  echo "✓ token 有效  剩余 ${REMAIN} 小时  sid=${SID:0:20}…"
else
  echo "✗ token 已过期 ${REMAIN#-} 小时（绝对值）  exp=$(python3 -c "import datetime,sys;print(datetime.datetime.fromtimestamp(int(sys.argv[1])))" "$EXP")"
  echo "  → 去「商汤小浣熊」客户端重新登录，auth.json 会自动刷新"
  exit 1
fi

if [ "$CHECK_ONLY" = "1" ]; then
  exit 0
fi

echo "→ 推送到 $GATEWAY"
BODY=$(python3 -c "
import json,sys
print(json.dumps({'token': sys.argv[1], 'note': '小浣熊客户端重登', 'identity': 'personal'}))
" "$TOKEN")

curl -s --noproxy '*' -m 30 -X POST "$GATEWAY/panel/api/accounts" \
  -H "Authorization: Bearer $PANEL_KEY" \
  -H "Content-Type: application/json" \
  -d "$BODY"
echo

echo "→ 复查健康状态"
curl -s --noproxy '*' -m 20 "$GATEWAY/healthz" | python3 -c "
import json,sys
d=json.load(sys.stdin)
a=d.get('accounts') or {}
print(f\"  version={d.get('version')} ok={d.get('ok')} healthy={a.get('healthy')} expired={a.get('expired')} total={a.get('total')}\")
"
