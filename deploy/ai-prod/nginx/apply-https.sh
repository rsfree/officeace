#!/usr/bin/env bash
# officeace2api · ai-prod 域名入口一键落地（幂等；DNS 未就绪会拒绝执行）
#
# 用法（在 ai-prod 上跑，root）：
#   bash /opt/officeace2api/nginx/apply-https.sh
#
# 流程：DNS 预检 → ACME 通道自检 → acme.sh 签发（HTTP-01 webroot）→ install-cert
#      → 投 :443 vhost → nginx -t + reload → 验证矩阵 + 终态断言
# 前置：:80 的 vhost 已投好（第 2 步会自检挑战路径可达）。
set -euo pipefail

HOST="${HOST:-officeace.1task.cn}"
IP_EXPECT="${IP_EXPECT:-210.121.44.245}"
VHOST_DIR=/www/server/panel/vhost/nginx
CERT_DIR="/www/server/panel/vhost/cert/$HOST"
ACME=/root/.acme.sh/acme.sh
NGINX=/www/server/nginx/sbin/nginx
SVC_DIR=/opt/officeace2api

echo "== 1) DNS 预检：$HOST 必须已指向 $IP_EXPECT =="
# 权威 NS 最准（无缓存），公共解析器兜底
GOT=""
for R in dns13.hichina.com dns14.hichina.com 8.8.8.8 1.1.1.1 223.5.5.5; do
  GOT=$(dig +short "$HOST" @"$R" 2>/dev/null | grep -E '^[0-9]+\.' | head -1 || true)
  [ "$GOT" = "$IP_EXPECT" ] && break
done
if [ "$GOT" != "$IP_EXPECT" ]; then
  echo "DNS 未就绪：$HOST -> '${GOT:-<空>}'（期望 $IP_EXPECT）—— 先加 A 记录再跑"
  exit 2
fi
echo "OK $HOST -> $GOT"

echo "== 2) ACME 通道自检 =="
mkdir -p /www/wwwroot/acme/.well-known/acme-challenge
echo "preflight-$$" > /www/wwwroot/acme/.well-known/acme-challenge/preflight
CODE=$(curl -s -o /dev/null -w '%{http_code}' -H "Host: $HOST" http://127.0.0.1/.well-known/acme-challenge/preflight)
[ "$CODE" = "200" ] || { echo "挑战路径返回 $CODE（应 200）—— 检查 $VHOST_DIR/$HOST.conf 的 ^~ location"; exit 3; }
echo "OK 挑战路径 200"

echo "== 3) 签发证书（已存在则跳过）=="
if [ -d "/root/.acme.sh/${HOST}_ecc" ]; then
  echo "· 证书已存在，跳过签发"
else
  "$ACME" --issue -d "$HOST" --webroot /www/wwwroot/acme --keylength ec-256
fi
mkdir -p "$CERT_DIR"
"$ACME" --install-cert -d "$HOST" --ecc \
  --key-file       "$CERT_DIR/privkey.pem" \
  --fullchain-file "$CERT_DIR/fullchain.pem" \
  --reloadcmd      "$NGINX -s reload"
echo "OK 证书就位：$CERT_DIR"

echo "== 4) 投 :443 vhost 并 reload（幂等）=="
SSL_CONF="$VHOST_DIR/$HOST-ssl.conf"
# 反代片段必须与 vhost 同目录（-ssl.conf 里 include 的是 vhost 目录下的 .inc）
INC_CONF="$VHOST_DIR/officeace-proxy.inc"
[ -f "$INC_CONF" ] || cp -f "$SVC_DIR/nginx/officeace-proxy.inc" "$INC_CONF"
if [ ! -f "$SSL_CONF" ]; then
  cp -f "$SVC_DIR/nginx/$HOST-ssl.conf" "$SSL_CONF"
  echo "· 已写入 $SSL_CONF"
else
  echo "· $SSL_CONF 已存在，跳过"
fi
# ⚠️ `-t && -s reload` 写成一行是错的：set -e 对 AND-OR 首命令不生效，-t 失败会被吞
"$NGINX" -t
"$NGINX" -s reload
echo "OK nginx reloaded"

echo "== 5) 验证矩阵 =="
R="--resolve $HOST:443:127.0.0.1"
# reload 是异步的：老 worker 会继续服务片刻
for _ in $(seq 1 20); do
  [ "$(curl -s -o /dev/null -w '%{http_code}' $R https://$HOST/healthz)" = "200" ] && break
  sleep 0.5
done
AK=$(sed -n 's/.*"api_key": "\([^"]*\)".*/\1/p' "$SVC_DIR/data/config.json")

printf '%-36s %s\n' ":80 → :443 301"        "$(curl -s -o /dev/null -w '%{http_code}' -H "Host: $HOST" http://127.0.0.1/healthz)"
printf '%-36s %s\n' ":443 /healthz (SNI)"   "$(curl -s -o /dev/null -w '%{http_code}' $R https://$HOST/healthz)"
printf '%-36s %s\n' ":443 / 面板"            "$(curl -s -o /dev/null -w '%{http_code}' $R https://$HOST/)"
printf '%-36s %s\n' ":443 /v1/models 无 key（应 401）" "$(curl -s -o /dev/null -w '%{http_code}' $R https://$HOST/v1/models)"
printf '%-36s %s\n' ":443 /v1/models 带 key（应 200）" "$(curl -s -o /dev/null -w '%{http_code}' $R -H "Authorization: Bearer $AK" https://$HOST/v1/models)"
printf '%-36s %s\n' "证书链校验（不加 -k）"  "$(curl -s -o /dev/null -w '%{http_code} ssl_verify=%{ssl_verify_result}' $R https://$HOST/healthz)"
printf '%-36s %s\n' "未登记 Host 应 404"    "$(curl -s -o /dev/null -w '%{http_code}' -H 'Host: nope.example.com' http://127.0.0.1/healthz)"
echo
echo "证书：$(openssl x509 -in "$CERT_DIR/fullchain.pem" -noout -subject -dates 2>/dev/null | tr '\n' ' ')"

# 终态断言：只打印不判定 = 看着像成功的假绿灯
HZ=$(curl -s -o /dev/null -w '%{http_code}' $R https://$HOST/healthz)
SV=$(curl -s -o /dev/null -w '%{ssl_verify_result}' $R https://$HOST/healthz)
MK=$(curl -s -o /dev/null -w '%{http_code}' $R https://$HOST/v1/models)
[ "$HZ" = "200" ] || { echo "✗ :443 /healthz 返回 $HZ（应 200）"; exit 4; }
[ "$SV" = "0" ]  || { echo "✗ 证书链校验失败 ssl_verify=$SV"; exit 5; }
[ "$MK" = "401" ] || { echo "✗ 无 key 访问 /v1/models 返回 $MK（应 401，鉴权未生效）"; exit 6; }
echo "✅ 完成：https://$HOST"
