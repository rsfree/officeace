# officeace2api 测试报告

**日期** 2026-11-21 · **被测版本** v0.0.6 · **环境** macOS 本地 + ai-prod（首尔 210.121.44.245）

---

## 摘要

| 维度 | 结论 |
|---|---|
| 接口功能 | 全部通过：chat（流式/非流式/多轮）、Anthropic Messages、图像生成、鉴权、错误码 |
| 能力支持面 | tools / JSON Schema / 视觉输入 / 前缀缓存 / 推理内容均可用；`n>1` 与 `logprobs` 被上游静默忽略 |
| 图片生成 | 质量达一线水准（中文渲染零错笔），但 `model` 字段无效、尺寸有像素下限 |
| 溯源真实性 | **密码学验证通过**：C2PA 签名 `Valid`，生成器 `gpt-image-2`，双通道（OpenAI 官方 / Azure） |
| 配额与限流 | API 直连**不扣积分**；并发 10 持续 12 分钟 **零 429**，代价是 p99 长尾 108s |
| 已修复缺陷 | 3 个（均在上线过程中发现并修复，见 §5） |
| 剩余风险 | 凭证 3.9 天后硬过期、上游模型池动态漂移、计费口径可能调整 |

---

## 1. 部署与可用性

| 项 | 值 |
|---|---|
| 线上入口 | `https://officeace.1task.cn`（nginx + Let's Encrypt，证书至 2027-02-18） |
| 容器 | `officeace2api:0.0.6`，宿主 `127.0.0.1:39055` → 容器 `8787` |
| 本地实例 | `127.0.0.1:8787` |
| 运行状态 | 容器 `Up 7 hours (healthy)`，账号池 1/1 健康 |

**上线验证矩阵（7 项全绿）**

| 检查项 | 期望 | 实测 |
|---|---|---|
| `:80 → :443` | 301 | 301 |
| `:443 /healthz` | 200 | 200 |
| 面板 `/` | 200 | 200 |
| `/v1/models` 无 key | 401 | 401 |
| `/v1/models` 带 key | 200 | 200 |
| 证书链校验 | `ssl_verify=0` | 0 |
| 未登记 Host | 404 | 404 |

---

## 2. 接口功能

| 用例 | 结果 | 关键数据 |
|---|---|---|
| Chat 非流式 | ✅ 200 | 3.08s，33/29 tokens，内容正确 |
| Chat 流式 | ✅ 200 | TTFB 2.26s / 总 2.78s，16 chunk，`[DONE]` 完整 |
| 流式 + `include_usage` | ✅ 200 | usage 正确透传（未请求时不下发） |
| 多轮上下文 | ✅ 200 | 正确复述上文数字，41/6 tokens |
| Anthropic `/v1/messages` | ✅ 200 | `stop_reason: tool_use`，`tool_use` block 结构正确 |
| OpenAI `tools` | ✅ 200 | `finish_reason: tool_calls`，`arguments` 为合法 JSON |
| 图像生成 | ✅ 200 | 1024×1024 PNG，见 §4 |
| 无效 key | ✅ 401 | `invalid api key` |
| 未知端点 | ✅ 404 | `unknown_endpoint`（见 §5.3） |

**流式不缓冲的证据**：经 nginx 时 TTFB 2.26s、总时长 2.78s —— 若被缓冲，首字节会等到结束才返回。

---

## 3. 能力支持面（逐项实测）

> 判定标准：不看 HTTP 200，看**输出里是否真的体现了该参数**。

| 参数 / 能力 | 结论 | 证据 |
|---|---|---|
| `tools` / `tool_choice` | ✅ 生效 | 真返回 `tool_calls` |
| `response_format: json_object` | ✅ 生效 | 输出合法 JSON |
| `response_format: json_schema`（strict） | ✅ 生效 | 产出 `{"age":34,"name":"林知遥"}` 符合 schema |
| 视觉输入（`image_url`） | ✅ 生效 | 434KB 图片准确描述出内容 |
| `stream` + `stream_options` | ✅ 生效 | 分片下发，usage 可控 |
| `reasoning_content` | ✅ 生效 | 推理过程独立返回 |
| `max_tokens` | ✅ 生效 | 精确截断 |
| `temperature` / `top_p` / `stop` / `seed` / penalties | ⚠️ 接受但未证生效 | 均 200，输出无可见变化（推理模型常忽略） |
| `n > 1` | ❌ 被忽略 | 请求 `n=2` 只返回 1 个 choice |
| `logprobs` | ❌ 被忽略 | message 中无 `logprobs` 字段 |
| `/v1/embeddings`、`/v1/responses`、`/v1/completions` | ❌ 不存在 | 404 |

**前缀缓存**

| 项 | 结果 |
|---|---|
| OpenAI 路径 | 透传 `usage.prompt_tokens_details.cached_tokens` |
| Anthropic 路径 | 映射为 `cache_read_input_tokens`，`input_tokens` 只计未缓存部分 |
| 生效前提 | **前 1–2 次不命中**（异步写入），第 3 次起命中 |
| 实测 | 7.3k prompt：首次 `cached=0` → 第 3 次 `cached=7296`，耗时 4.6s → 2.0s |

---

## 4. 图片生成深度测试

### 4.1 参数支持面

| 参数 | 结论 |
|---|---|
| `size` | ✅ 生效，但需 **≥ 约 1M 像素**；`1024x1024` / `1254x1254` / `1792x1024` / `1024x1792` 可用，`512x512` 被拒（`below the current minimum pixel budget`） |
| `quality` | ✅ 生效：`low`（20s）/ `medium`（30s，默认）/ `high`（**84s**） |
| `n` | ✅ 多图，`n=2` 真返回 2 张 |
| `model` | ❌ **被上游忽略**：传 `sn-nonexistent-model` 同样 200 并出图 |
| 画幅 | 不传 `size` 时上游**按 prompt 语义自动选**（海报→竖版，场景→横版） |

### 4.2 画质评估（四个难度场景）

| 场景 | 表现 |
|---|---|
| 中文文字海报 | 「晨光咖啡」四字 + 「OPEN 7:00」**笔画全对、零错字、零乱码**，排版留白干净 |
| 写实人像 | 毛孔、胡茬、布料褶皱、侧逆光均为专业摄影质感，无肢体畸形 |
| 复杂城市夜景 | 招牌日文可读且拼写正确（サロンパス / DHC / UNIQLO 等），雨伞结构、积水倒影符合物理 |
| 精确计数（5 个苹果） | 数量准确，但**过于一致**（像复制粘贴）、俯拍视角果蒂位置不合理 |

**结论**：可达商用水准，尤以文字渲染见长；弱点是缺乏自然随机性。

### 4.3 溯源验证（C2PA）

使用官方 `c2patool v0.27.23` 做密码学校验（验签覆盖整个文件字节，非字符串匹配）：

| 图 | `validation_state` | 签名主体 | 生成器 |
|---|---|---|---|
| 文字海报 | **Valid** | OpenAI Media Service | `gpt-image-2` |
| 苹果计数 | **Valid** | Microsoft | `Azure OpenAI ImageGen` |
| 圆形图标 | **Valid** | Microsoft | `Azure OpenAI ImageGen` |
| 抽象图案（抽验） | **Valid** | Microsoft | `Azure OpenAI ImageGen` |

- 证书链：`O="OpenAI OpCo, LLC"` ← `Trufo C2PA Claim Signing CA`（Trufo 是 OpenAI 的 C2PA 签发服务商）
- `c2pa.actions` 含 `c2pa.created` + `c2pa.converted` + **`c2pa.watermarked.unbound`**（SynthID 水印）
- `ingredients: 0` — 原生生成，无二次编辑痕迹

**双通道发现**：4 次抽验中 3 次落在 **Azure OpenAI**、1 次落在 **OpenAI 官方**，说明上游 LiteLLM 配置了双通道负载/故障转移。这解释了非标准尺寸（如 `1254x1254`，更可能是 Azure 侧档位）。

> 未验证项：SynthID 水印本身（检测器不公开，需在 `openai.com/verify` 手动上传）。

### 4.4 积分消耗

| 操作 | 余额变化 |
|---|---|
| 基线 | 260 |
| high 生图后 | 260（**未变**） |
| low 生图后 | 260（**未变**） |

**结论：API 直连不扣客户端积分。** 原因：积分由客户端 `officeace-aom-usage-plugin` 上报 token 用量至华为云 `/v1/aom-report` 后扣减，直连 LLM API 不经过该链路。

> 风险：若上游改为服务端计费，按 1 积分 ≈ 25 tokens 反推，**一张 high（7024 tokens）≈ 280 积分**，而免费版日额度仅 300。

### 4.5 频控（两阶段压测）

| 测试 | 并发 | 时长 | 请求 | 429 | 吞吐 | p50 | p99 |
|---|---|---|---|---|---|---|---|
| 阶梯（串行 25→15→8s） | 1 | 41min | 59 | 0 | 1.3→3.3/min | 24.6s | 33.4s |
| 冲刺（无间隔） | **10** | 12min | **258** | **0** | **21.6/min** | 26.4s | **108.3s** |

- 每分钟完成数稳定：`25 18 22 21 21 22 22 25 21 22 20 19`
- 单秒最大发起 10 个，全部 200
- **无速率限流、无并发限制**；压力表现为**排队延迟**而非拒绝
- `max` 耗时 115.4s

**对网关配置的直接影响**：超时必须 **≥ 180s**（覆盖 p99 108s），否则高并发下长尾会被误判超时。

> 未探明：更高阈值（并发 30+）、日配额、账号级隐性风控。

---

## 5. 测试中发现并修复的缺陷

| # | 问题 | 影响 | 修复版本 |
|---|---|---|---|
| 5.1 | 图片 usage 字段名与 chat 不同（`input_tokens`/`output_tokens`），按 chat 解析导致**每张图记 0 token** | 用量统计失真 | v0.0.4 |
| 5.2 | Anthropic 路径未映射上游 `cached_tokens` | 客户端把缓存部分按全价计费 | v0.0.5 |
| 5.3 | 未注册端点返回 **405** 而非 404（mux 路径匹配但方法不符） | 客户端误判端点存在 | v0.0.6 |

---

## 6. 风险清单

| 级别 | 风险 | 说明 | 建议 |
|---|---|---|---|
| 🔴 高 | **凭证硬过期** | token 无 refresh 机制，`exp=2026-11-25 09:49`，剩余 **3 天 23 小时**；`auth.json` 25 天未被改写过 | 到期前重开客户端登录；加到期提醒 |
| 🟠 中 | **模型池漂移** | `sn-kimi-k3` 已于本日下线（本地与境外同时 400），当前仅 `deepseek` + `sn-image` | 面板保留可切换，监控失效 |
| 🟠 中 | **计费口径可能变** | 当前不扣积分依赖客户端上报链路；上游改为服务端计费后 high 一张≈280 积分 | 生产用 low/medium |
| 🟡 低 | **长尾延迟** | 10 并发下 p99 108s | 超时 ≥180s |
| 🟡 低 | **签名算法** | JWT 用 HS256（对称），密钥泄露即可伪造任意用户 | 属上游设计，无法本地缓解 |

---

## 7. 附录

**测试脚本**（均在仓库 `scripts/`）

| 脚本 | 用途 |
|---|---|
| `img.sh` | 单次生图 + 自动 C2PA 校验 |
| `rate-probe.py` | 阶梯式频控探测（滚动统计 1m/5m/15m/1h） |
| `burst-probe.py` | 高压冲刺（并发 10，测 429 拐点） |
| `probe.sh` | 绕过网关直连上游验证协议 |

**数据归档**（ai-prod `/opt/officeace2api/probe/`）

| 文件 | 内容 |
|---|---|
| `rate-phase1.jsonl` | 59 条阶梯测试记录 |
| `burst-phase2.jsonl` | 258 条冲刺测试记录 |

**测试环境版本**

- 客户端：OfficeAce.app v1.8.0（华为云签名，内含商汤小浣熊 1.2.0）
- 上游：`xiaohuanxiong.com/api/web/llm/v2`（LiteLLM 聚合）
- 实际模型：DeepSeek V4（文本）/ gpt-image-2（图片，OpenAI / Azure 双通道）
