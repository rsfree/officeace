#!/usr/bin/env bash
# 守株待兔：盯着客户端登录，一出现新 token 就自动推线上 + 跑多形态测试
#
#   ./scripts/watch-login.sh              # 盯 30 分钟
#   ./scripts/watch-login.sh 3600         # 盯 1 小时
#   ./scripts/watch-login.sh 1800 --test  # 登录后自动跑多形态测试
#
# 背景：token 是 30 天定期的 IAM JWT，客户端不保存 refresh_token，
# 只能靠人工重登。重登后 auth.json 会被重写，本脚本轮询到就接管后续动作。
set -uo pipefail

PROJECT_DIR="$(cd "$(dirname "$0")/.." && pwd)"
AUTH="${AUTH_JSON:-$HOME/Library/Application Support/office-raccoon/agent/config/auth.json}"
LIMIT="${1:-1800}"
AUTO_TEST=0
[[ "${2:-}" == "--test" || "${1:-}" == "--test" ]] && AUTO_TEST=1
[[ "${1:-}" =~ ^[0-9]+$ ]] || LIMIT=1800

echo "守候 $AUTH"
echo "上限 ${LIMIT}s，每 3s 检查一次"
echo "→ 现在去「商汤小浣熊」客户端完成登录，剩下的交给我"
echo

DEADLINE=$(( $(date +%s) + LIMIT ))
while [ "$(date +%s)" -lt "$DEADLINE" ]; do
  if [ -s "$AUTH" ]; then
    if "$PROJECT_DIR/scripts/push-account.sh" --check >/dev/null 2>&1; then
      echo
      echo "✓ 检测到有效 token，开始推送"
      echo
      "$PROJECT_DIR/scripts/push-account.sh"
      rc=$?
      if [ $rc -eq 0 ] && [ "$AUTO_TEST" = "1" ]; then
        echo
        echo "=== 多形态测试 ==="
        python3 "$PROJECT_DIR/scripts/multiform-probe.py"
      fi
      exit $rc
    fi
  fi
  sleep 3
done

echo "✗ ${LIMIT}s 内未检测到新凭据，已退出"
exit 1
