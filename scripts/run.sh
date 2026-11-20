#!/bin/bash
# 前台启动（Ctrl+C 停止）。想长期常驻，把本行丢进 tmux/screen 或你的终端标签页。
set -euo pipefail
cd "$(dirname "$0")/.."
exec ./officeace2api -data data "$@"
