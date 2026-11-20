# officeace2api @ ai-prod

## 机器事实

| 项 | 值 |
|---|---|
| 主机 | `210.121.44.245`（首尔，x86_64） |
| 目录 | `/opt/officeace2api/`（`src/` 源码、`data/` 状态、`nginx/` 配置） |
| 端口 | 容器 `127.0.0.1:39055` → 容器内 `8787`（只绑回环） |
| 入口 | `https://officeace.1task.cn`（nginx + Let's Encrypt，acme.sh 自动续期） |
| 容器 | `officeace2api`，`restart: unless-stopped`，healthcheck 走 `/healthz` |

## 这台机器上的特殊约束

**服务器上没有 OfficeAce 客户端**，所以：

- `sync_local` 已关闭（`data/config.json`），不会有本机凭证自动同步；
- 账号池里的 token 是**从开发机手动搬过来的**，`data/accounts.json` 权限 600；
- token 由上游签发、约 30 天有效，**过期后必须手动更新**（见下）。

## 更新凭证

在开发机（OfficeAce 已登录）执行：

```bash
scp -i ~/.ssh/id_ed25519_ops \
  ~/Desktop/officeace/data/accounts.json \
  root@210.121.44.245:/opt/officeace2api/data/accounts.json
ssh -i ~/.ssh/id_ed25519_ops root@210.121.44.245 \
  'chmod 600 /opt/officeace2api/data/accounts.json && cd /opt/officeace2api && docker compose restart'
```

或直接在面板「账号池 → 手动添加」粘贴新的 `access_token`（热生效，不用重启）。

## 更新代码

```bash
# 开发机
tar --exclude=./data --exclude=./.git --exclude=./officeace2api --exclude=./docs \
  -czf /tmp/oa_src.tgz -C ~/Desktop/officeace .
scp -i ~/.ssh/id_ed25519_ops /tmp/oa_src.tgz root@210.121.44.245:/opt/officeace2api/

# 服务器
ssh -i ~/.ssh/id_ed25519_ops root@210.121.44.245 '
  cd /opt/officeace2api/src && tar -xzf ../oa_src.tgz && rm -f ../oa_src.tgz
  docker build --build-arg VERSION=<版本> -t officeace2api:<版本> . 
  cd /opt/officeace2api && sed -i "s/image: officeace2api:.*/image: officeace2api:<版本>/" docker-compose.yml
  docker compose up -d'
```

## 模型会漂移（重要）

上游的模型池是动态的。**2026-11-21 实测 `sn-kimi-k3` 已下线**（返回 `400 param model unsupport`，
开发机与服务器同时复现，不是地域问题），当前可用：`sn-deepseek-v4-1-flash-1017`、`sn-image`。

排查顺序：

1. 先在开发机直连上游试同一模型（`scripts/probe.sh`）——若也 400，是上游下线，不是部署问题；
2. 面板「模型」页把失效模型关掉，换可用的；
3. `sn-deepseek-v4-1-flash-1017` 是**推理模型**，`max_tokens` 太小会把额度全用在 `reasoning_content` 上、
   导致 `content` 为空。客户端建议 ≥ 400。

## 验证

```bash
curl -s https://officeace.1task.cn/healthz
# 无 key 应 401
curl -s -o /dev/null -w '%{http_code}\n' https://officeace.1task.cn/v1/models
# 带 key 应 200
curl -s -H "Authorization: Bearer <API Key>" https://officeace.1task.cn/v1/models
```

## nginx 要点

- `officeace-proxy.inc` 里 **`proxy_buffering off`** 是 SSE 的命门，删了流式会整段攒到最后；
- `proxy_read_timeout 3600s` 覆盖长对话与图片生成（实测 10–30s，留足余量）；
- `client_max_body_size 32m` 给图像 edits 的 base64 body；
- 重载：`/www/server/nginx/sbin/nginx -t` 与 `-s reload` **必须分两行**（`-t && -s reload` 会吞掉失败）。
