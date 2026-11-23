#!/usr/bin/env bash
# officeace2api 生图测试脚本
#
#   ./scripts/img.sh "一只戴眼镜的橘猫"                  走网关，默认 low / 1024x1024
#   ./scripts/img.sh -q high -s 1792x1024 "城市夜景"     指定质量与尺寸
#   ./scripts/img.sh -d -q low "测试"                    直连上游（跳过网关，调试用）
#
# 参数
#   -q  quality   low | medium | high        （默认 low）
#   -s  size      1024x1024 | 1536x1024 | 1024x1536 | 1792x1024 | 1024x1792
#                 注意：总像素需 ≥ 约 1M，512x512 会被上游 400 拒绝
#   -n  count     生成张数（默认 1）
#   -d            直连 xiaohuanxiong.com（用本机 auth.json 的 access_token）
#
# 环境变量
#   GATEWAY       网关地址，默认 https://officeace.1task.cn
#   C2PATOOL      c2patool 路径，存在则自动做 C2PA 签名校验
set -euo pipefail

PROJECT_DIR="$(cd "$(dirname "$0")/.." && pwd)"
GATEWAY="${GATEWAY:-https://officeace.1task.cn}"
AUTH_JSON="$HOME/Library/Application Support/office-raccoon/agent/config/auth.json"
C2PATOOL="${C2PATOOL:-/tmp/c2patool/c2patool/c2patool}"

QUALITY=low
SIZE=1024x1024
COUNT=1
DIRECT=0

usage() { sed -n '2,20p' "$0" | sed 's/^# \{0,1\}//'; }

while getopts "q:s:n:dh" opt; do
  case "$opt" in
    q) QUALITY="$OPTARG" ;;
    s) SIZE="$OPTARG" ;;
    n) COUNT="$OPTARG" ;;
    d) DIRECT=1 ;;
    h) usage; exit 0 ;;
    *) usage; exit 1 ;;
  esac
done
shift $((OPTIND - 1))
PROMPT="${1:-一枚蓝色圆形徽章，纯白背景}"

if [ "$DIRECT" = "1" ]; then
  URL="https://xiaohuanxiong.com/api/web/llm/v2/images/gen"
  KEY=$(python3 -c "import json,sys;print(json.load(open(sys.argv[1]))['access_token'])" "$AUTH_JSON")
  MODE="直连上游"
else
  URL="$GATEWAY/v1/images/generations"
  KEY=$(python3 -c "import json,sys;print(json.load(open(sys.argv[1]))['api_key'])" "$PROJECT_DIR/data/config.json")
  MODE="走网关"
fi

BODY=$(python3 - "$PROMPT" "$QUALITY" "$SIZE" "$COUNT" <<'PY'
import json, sys
prompt, quality, size, n = sys.argv[1:5]
print(json.dumps({
    "model": "sn-image",
    "prompt": prompt,
    "n": int(n),
    "quality": quality,
    "size": size,
}, ensure_ascii=False))
PY
)

mkdir -p "$PROJECT_DIR/out"
STAMP=$(date +%Y%m%d-%H%M%S)
OUT="$PROJECT_DIR/out/img-$QUALITY-$STAMP.png"
RESP="$(mktemp)"

echo "→ $MODE  quality=$QUALITY  size=$SIZE  n=$COUNT"

CODE=$(curl -s -m 300 --noproxy '*' -o "$RESP" -w '%{http_code} %{time_total}' \
  -X POST "$URL" \
  -H "Authorization: Bearer $KEY" \
  -H "Content-Type: application/json" \
  -d "$BODY")
HTTP="${CODE%% *}"
ELAPSED="${CODE##* }"

if [ "$HTTP" != "200" ]; then
  echo "✗ HTTP $HTTP"
  head -c 400 "$RESP"; echo
  rm -f "$RESP"
  exit 1
fi

python3 - "$RESP" "$OUT" "$ELAPSED" <<'PY'
import base64, json, sys
resp, out, elapsed = sys.argv[1:4]
d = json.load(open(resp))
u = d.get("usage") or {}
imgs = d.get("data") or []
if not imgs:
    print("✗ 响应里没有图片"); raise SystemExit(1)
for i, item in enumerate(imgs):
    path = out if len(imgs) == 1 else out.replace(".png", f"-{i}.png")
    raw = base64.b64decode(item["b64_json"])
    open(path, "wb").write(raw)
    print(f"✓ {path}  ({len(raw)//1024} KB)")
print(f"  耗时 {elapsed}s | size={d.get('size')} quality={d.get('quality')} "
      f"format={d.get('output_format')}")
print(f"  tokens: input={u.get('input_tokens')} output={u.get('output_tokens')}")
PY

if [ -x "$C2PATOOL" ]; then
  echo "  --- C2PA ---"
  "$C2PATOOL" "$OUT" --detailed 2>/dev/null | python3 -c '
import json, sys
d = json.load(sys.stdin)
m = list(d["manifests"].values())[0]
acts = m.get("assertion_store", {}).get("c2pa.actions.v2", {}).get("actions", [])
agent = [a.get("softwareAgent") for a in acts if a.get("softwareAgent")]
print("  state   :", d.get("validation_state") or m.get("validation_state"))
print("  signer  :", m.get("claim", {}).get("claim_generator_info", {}).get("name"))
print("  agent   :", agent)
' 2>/dev/null || echo "  （解析失败）"
else
  echo "  （未装 c2patool，跳过签名校验）"
fi

rm -f "$RESP"
