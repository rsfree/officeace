#!/bin/bash
# officeace2api 服务管理（launchd 用户级）
set -uo pipefail

LABEL="cn.1task.officeace2api"
PLIST="$HOME/Library/LaunchAgents/$LABEL.plist"
UID_NUM="$(id -u)"
DOMAIN="gui/$UID_NUM"
LOG="/Users/betterme/Desktop/officeace/data/service.log"
ERR="/Users/betterme/Desktop/officeace/data/service.err.log"

cmd="${1:-status}"

case "$cmd" in
  start)
    if launchctl print "$DOMAIN/$LABEL" >/dev/null 2>&1; then
      echo "已在运行"
    else
      launchctl bootstrap "$DOMAIN" "$PLIST" && echo "已启动"
    fi
    ;;
  stop)
    launchctl bootout "$DOMAIN/$LABEL" 2>/dev/null && echo "已停止" || echo "未在运行"
    ;;
  restart)
    launchctl bootout "$DOMAIN/$LABEL" 2>/dev/null
    sleep 1
    launchctl bootstrap "$DOMAIN" "$PLIST" && echo "已重启"
    ;;
  status)
    if launchctl print "$DOMAIN/$LABEL" >/dev/null 2>&1; then
      echo "● running"
      launchctl print "$DOMAIN/$LABEL" | /usr/bin/grep -E "state|pid|last exit|program =" | sed 's/^[[:space:]]*/  /'
      curl -s -m 3 --noproxy '*' http://127.0.0.1:8787/healthz 2>/dev/null | head -c 200
      echo
    else
      echo "○ stopped"
    fi
    ;;
  logs)
    tail -n "${2:-40}" "$LOG" 2>/dev/null || echo "无日志"
    ;;
  errors)
    tail -n "${2:-40}" "$ERR" 2>/dev/null || echo "无错误日志"
    ;;
  *)
    echo "用法: $0 {start|stop|restart|status|logs|errors}"
    exit 1
    ;;
esac
