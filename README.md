# Vertex AI Proxy

免费使用 Google Gemini 模型的代理工具。将 **OpenAI、OpenAI Responses、Anthropic Claude 兼容请求**无缝转换为对 Google 匿名端点的调用，也支持 Gemini 原生格式。

**免安装、解压即用的绿色软件。** 全面支持 Windows、Linux、macOS 以及 Android 手机等平台。

## ✨ 核心特性

- **多协议兼容**：支持 OpenAI Chat Completions、OpenAI Responses、Anthropic Messages 和 Gemini 原生接口。
- **流式与工具调用**：四类接口均支持流式/非流式文本；兼容 Function Calling / Tool Use 及工具结果回传。
- **多模态输入**：兼容 OpenAI、Responses、Claude 和 Gemini 常见的图片输入格式。
- **丰富的多媒体支持**：支持文生图、图片编辑、语音合成（TTS）。
- **内置反爬突破**：内置 TLS 指纹伪装及 reCAPTCHA token 自动获取，轻松通过 Google 匿名端点校验。
- **单 API Key 直连**：切换到 `official` 模式即可改走官方接口（无需 reCAPTCHA），继续复用代理池做每请求 IP 轮换，适合有"每分钟每 IP 限额"的免费 Key；默认仍为无头 Vertex 匿名端点。
- **内置代理节点池**：支持 HTTP、HTTPS、SOCKS4、SOCKS4A、SOCKS5、SOCKS5H 以及 mihomo 节点；可订阅纯文本代理列表、定时差异更新、自动健康巡检、失败冷却与请求接力，并通过分页筛选管理大规模代理池。
- **可视化管理面板**：提供精美的 Web 后台，无需修改 JSON 文件，在浏览器中即可轻松管理 API 密钥、模型别名、代理节点和系统设置。
- **高级功能**：支持 Token 计数、Gemini 原生端点透传、假流式输出等。

## 🚀 三步上手

**1. 下载解压**：下载对应平台的压缩包并解压到任意位置。

**2. 一键启动**：
- **Windows**：双击运行 `启动.bat`
- **Linux/macOS**：终端执行 `sh start.sh`
- **Android (Termux)**：终端执行 `sh start.sh`

**3. 配置密钥**：
首次启动时控制台会输出**管理员密码**。使用浏览器访问 `http://127.0.0.1:2156/admin/` 登录管理面板。
进入左侧「密钥」菜单，添加一个非空且不含冒号的 API Key（如 `mykey123`），无需以 `sk-` 开头；也可点击“✨”按钮随机生成。

> **如何使用？**
> 在你的客户端（如 Cherry Studio、ChatBox 等）中，填入刚才设置的 API Key，API 地址填为 `http://127.0.0.1:2156/v1` 即可开始使用！

## 🔌 兼容接口

同一个本地 API Key 可用于以下协议：

| 协议 | 端点 | 鉴权 |
|------|------|------|
| OpenAI Chat Completions | `POST /v1/chat/completions` | `Authorization: Bearer <key>` |
| OpenAI Responses | `POST /v1/responses` | `Authorization: Bearer <key>` |
| Anthropic Messages | `POST /v1/messages` | `x-api-key: <key>` |
| Anthropic Token Count | `POST /v1/messages/count_tokens` | `x-api-key: <key>` |
| Gemini GenerateContent | `POST /v1beta/models/{model}:generateContent` | `x-goog-api-key: <key>` 或 `?key=<key>` |
| Gemini StreamGenerateContent | `POST /v1beta/models/{model}:streamGenerateContent?alt=sse` | `x-goog-api-key: <key>` 或 `?key=<key>` |

Responses API 示例：

```bash
curl http://127.0.0.1:2156/v1/responses \
  -H "Authorization: Bearer mykey123" \
  -H "Content-Type: application/json" \
  -d '{"model":"gemini-3.6-flash","input":"你好","stream":true}'
```

Anthropic Messages 示例：

```bash
curl http://127.0.0.1:2156/v1/messages \
  -H "x-api-key: mykey123" \
  -H "anthropic-version: 2023-06-01" \
  -H "Content-Type: application/json" \
  -d '{"model":"gemini-3.6-flash","max_tokens":1024,"messages":[{"role":"user","content":"你好"}]}'
```

Responses 当前实现面向无状态生成：支持 `input`、`instructions`、图片、函数工具、结构化输出和 SSE；`previous_response_id`、Conversations 及 OpenAI 托管工具不会在本地持久化或执行。

## 🔑 上游通道：无头 Vertex 匿名 / 单 API Key 直连

本项目默认使用**无头 Vertex 匿名端点**（项目初始行为：模拟浏览器调用 Cloud Console 私有接口，网页公开 `anonAPIKey` + reCAPTCHA，无需任何密钥）。同时支持切换到**单 API Key 直连**：所有对话、绘图、TTS 与 Token 计数请求改走官方 Gemini API（`generativelanguage.googleapis.com`，`x-goog-api-key` 鉴权，无需 reCAPTCHA）。

**如何切换模式**（三选一，保存即时生效）：

1. **管理面板**：设置 → 🔑 上游通道 → 「上游通道模式」下拉框选 `anon`（匿名端点，默认）或 `official`（官方直连）；
2. **配置文件**：`config.json` 中 `"gemini_api_mode": "official"`（或 `"anon"`）；
3. **环境变量**：`VPROXY_GEMINI_API_MODE=official`（优先级最高）；仅设置 `VPROXY_GEMINI_API_KEY` 而未选模式时，视为要直连。

- **开箱即用**：项目内置一个免费代理池订阅（`https://youhua.onrender.com/api/latest/txt`，每小时自动拉取最新可用代理，`type://host:port` 行式列表按行识别 http/socks4/socks5），开箱自动启用并轮换 IP。直连模式的 Key 支持三种方式：面板粘贴一次（保存到本地 `config.json`，已被 `.gitignore` 排除，不会入库）、环境变量 `VPROXY_GEMINI_API_KEY`、或自建时构建期注入：`go build -ldflags "-X github.com/bsfdsagfadg/vertex/internal/config.builtinGeminiAPIKey=你的Key"`。注意：不要把真实 Key 硬编码进公开仓库源码——Google 会扫描公开仓库并自动吊销泄露的密钥。
- **代理池照常生效**：直连模式仍复用竞速节点池。上游按 IP 限速时，429 会让该节点进入 30 秒冷却并自动接力下一个节点，相当于每个请求自动换 IP，特别适合有"每分钟每 IP 请求上限"的免费 Key。建议同时开启「并发请求池」并导入多个代理节点。
- **地理封锁自动换节点**：官方 API 对部分地区 IP 返回 `User location is not supported`，该错误被识别为节点故障（可重试）而非请求错误，会自动换节点接力。
- **Key 与模式解耦**：Key 填好后切换 `anon/official` 不会丢失；anon 模式下 Key 仅保存不生效，随时一键切回直连。面板中的 Key 以脱敏形式展示，重新输入即更换；`official` 模式未配置 Key 时请求会收到明确的配置错误提示。
- **代理订阅**：默认代理池订阅可通过环境变量 `VPROXY_PROXY_SUBSCRIPTION_URL` 覆盖；显式设为空串可停用托管订阅，完全不影响手动导入的节点。
- **镜像/中转**：`gemini_api_base_url` 可填自定义 Base URL（需以 `http(s)://` 开头），留空使用官方地址。

> 注意：切换模式不改变客户端接入方式，四种兼容协议照常使用；仅有模型可用性取决于所用通道/Key 本身的权限。

## 🧑‍💻 Codex CLI / Claude Code

### Codex CLI

Codex CLI 应使用 Responses API。先把 API Key 放进环境变量：

```powershell
$env:VPROXY_API_KEY = "mykey123"
```

在 `%USERPROFILE%\.codex\config.toml` 中添加：

```toml
model = "gemini-3.6-flash"
model_provider = "vertex_proxy"
model_reasoning_summary = "none"
model_supports_reasoning_summaries = false

[model_providers.vertex_proxy]
name = "Vertex AI Proxy"
base_url = "http://127.0.0.1:2156/v1"
env_key = "VPROXY_API_KEY"
wire_api = "responses"
supports_websockets = false
```

然后直接运行 `codex`。当前兼容文本、图片、SSE、并行函数工具调用、工具结果回传和细分 token 统计。Responses reasoning summary、`reasoning.encrypted_content`、OpenAI 托管工具及 `previous_response_id` 服务端续接暂不实现；上面的配置会关闭 CLI 对 reasoning summary 的期待。

### Claude Code

按 Anthropic LLM gateway 方式设置环境变量，并显式选择代理中存在的模型：

```powershell
$env:ANTHROPIC_BASE_URL = "http://127.0.0.1:2156"
$env:ANTHROPIC_AUTH_TOKEN = "mykey123"
$env:DISABLE_PROMPT_CACHING = "1"
claude --model gemini-3.6-flash
```

当前兼容 Messages 流式/非流式、系统提示、多模态输入、并行 Tool Use / Tool Result、Extended Thinking 事件、ping 保活、token 统计和 `/v1/messages/count_tokens`。其中 token 数由匿名 Vertex `CountTokens` operation 精确计算，不使用本地启发式估算；Anthropic Prompt Caching 不会产生真实缓存收益，因此建议禁用。

运行中按 Enter 发送的内容属于 Claude Code 的排队/实时引导：它不能修改已经发出的 HTTP/SSE 请求，通常会在下一次工具结果回传时进入新的 `/v1/messages` 请求。Claude Code 可能把这类内容放进 `messages[]` 的中途 `system` turn；由于 Gemini 只支持请求级 `systemInstruction`，本项目会在应用完 Claude system 提示词策略后，把中途 turn 保留在原位置并作为最新用户引导发送。`tool_result` 后直接追加的文本也会按原顺序保留。若要立即终止当前操作并改派任务，请按 Ctrl+C 后再发送。若排队消息长期未生效，先运行 `claude --version` 并更新 Claude Code；官方曾修复过工作中忽略用户消息的问题。参考 [Claude Code Streaming Input](https://code.claude.com/docs/en/agent-sdk/streaming-vs-single-mode)、[Claude Code Changelog](https://code.claude.com/docs/en/changelog) 和 [Mid-conversation messages](https://platform.claude.com/docs/en/build-with-claude/mid-conversation-system-messages)。

> 两种 CLI 的本地工具均由 CLI 自己执行，本项目只负责模型协议转换。OpenAI/Anthropic 的服务端托管工具不在支持范围内。

### 思考强度转换

代理会在模型别名解析后，按实际 Gemini 型号转换思考参数。Codex/OpenAI Responses 的 `reasoning.effort`（以及 Chat Completions 的 `reasoning_effort`）支持 `none`、`minimal`、`low`、`medium`、`high`、`xhigh`、`max`；Claude Messages 同时兼容当前的 `thinking: {"type":"adaptive"}` + `output_config.effort`，以及旧版 `thinking: {"type":"enabled","budget_tokens":N}`。Claude 显式启用思考时会按 `display` 请求或隐藏 Gemini thought summaries；手动模式要求整数预算且至少为 `1024`。

按 Google GenerateContent API 的官方能力，当前适配矩阵如下：

| Gemini 模型 | 官方默认 | 官方可用控制 | 兼容协议转换 |
|---|---|---|---|
| 3.8 / 3.7 Flash（官方已发布） | `medium` | `low/medium/high`（**MINIMAL 会返回验证错误**） | `none/minimal` 降为 `low`，`xhigh/max` 收敛到 `high` |
| 3.6 / 3.5 Flash | `medium` | `minimal/low/medium/high` | `none` 降为 `minimal`，`xhigh/max` 收敛到 `high` |
| 3.5 / 3.1 Flash-Lite | `minimal` | `minimal/low/medium/high` | 同上 |
| 3.1 Pro | `high` | `low/medium/high` | `none/minimal` 降为 `low` |
| 3.1 Flash Image / Flash-Lite Image | `minimal` | `minimal/high` | `none/minimal/low` → `minimal`，其余 → `high` |
| 3 Flash | `high` | `minimal/low/medium/high` | `none` 降为 `minimal`，`xhigh/max` 收敛到 `high` |
| 2.5 Pro | 动态 | `thinkingBudget=128..32768` 或 `-1` | `none` → `128`；`minimal/low/medium/high+` → `1024/1024/8192/24576` |
| 2.5 Flash | 动态 | `0..24576` 或 `-1` | `none/minimal/low/medium/high+` → `0/1024/1024/8192/24576` |
| 2.5 Flash-Lite | 关闭 | `0`、`512..24576` 或 `-1` | 与 2.5 Flash 相同；正数预算至少为 `512` |
| 3 Pro Image | 固定开启 | 不开放强度控制 | 移除无效强度字段，保留模型官方行为 |
| 2.5 Flash Image | 不支持 Thinking | 无 | 移除无效思考配置 |

未显式传入强度时不会强行覆盖 Gemini 的模型默认值。Claude 旧版数值预算在 Gemini 2.5 上会尽量原值保留并按官方范围截断；发往 Gemini 3 时，`≤1024`、`≤8192`、更高预算依次映射为 `low`、`medium`、`high`，再按具体模型支持的等级收敛。

> **Gemini 3.7 / 3.8 Flash 说明（官方已发布）**：Google 官方模型卡与 Thinking 文档已公布二者的独立等级表：3.7 Flash（2026-08-13 GA）与 3.8 Flash（2026-09-02 GA）均仅支持 `LOW/MEDIUM/HIGH` 三档（默认 `medium`），`thinking_level: MINIMAL` 会返回 API 验证错误。因此本项目把 `NONE/MINIMAL` 请求自动折叠为 `LOW`；3.6 Flash 仍保持官方四档 `MINIMAL/LOW/MEDIUM/HIGH`。如要在当前网络和匿名上游配额下复测 3.7 三档，可运行：`$env:VERTEX_LIVE_THINKING_TEST = '1'; go test ./internal/vertex -run '^TestLiveGemini37FlashThinkingLevels$' -v -count=1`（3.8 模型卡标注 Live API 暂不支持，故未提供对应的 Live 复测）。若上游明确返回 `INVALID_ARGUMENT`，请以其实际错误和最新官方表为准。

参考 [Google Gemini Thinking](https://ai.google.dev/gemini-api/docs/generate-content/thinking)、[Google OpenAI 兼容映射](https://ai.google.dev/gemini-api/docs/openai#thinking)、[Google 3.6+ 模型迁移说明](https://ai.google.dev/gemini-api/docs/latest-model)、[Anthropic Effort](https://platform.claude.com/docs/en/build-with-claude/effort) 和 [Anthropic Extended Thinking](https://platform.claude.com/docs/en/build-with-claude/extended-thinking)。

**完整的分平台部署教程**（包括开机自启、代理配置、手机部署、常见问题解答）见 **[部署指南](部署指南.md)**。

## 🛠 自己编译（可选）

如果你想从源码自行编译：

```bash
go build -o vertex-proxy ./cmd/vproxy
go build -o vertex-proxy.exe ./cmd/vproxy
```

交叉编译示例（例如在 macOS/Windows 上编译 Linux 适用版本）：
```bash
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o vertex-proxy ./cmd/vproxy
```

## ⚙️ 配置说明

强烈建议直接使用**管理面板**的「设置」页进行配置修改，所有修改即时生效，无需重启。
如果需要手动修改，配置文件路径为 `config/config.json`：

| 选项 | 默认值 | 说明 |
|------|------|------|
| `port_api` | 2156 | 服务监听端口 |
| `admin_password` | 自动生成 | 管理面板登录密码 |
| `max_retries` | 10 | 请求失败重试次数 |
| `max_request_mb` | 64 | 单次 HTTP 请求体上限（MiB，面板修改即时生效） |
| `drop_max_tokens` | false | 移除客户端附带的输出 token 上限，避免思考 token 挤占正文；默认严格遵守客户端上限 |
| `claude_prompt_injection_enabled` | false | 是否为 Claude Messages 请求前置或后置注入额外 system 提示词 |
| `claude_prompt_injection_position` | append | 注入位置，可选 `prepend` 或 `append` |
| `claude_prompt_injection_text` | 空 | 要注入的 system 提示词 |
| `claude_prompt_strip_claude_code_promotions` | true | 精确移除 Claude Code 注入的模型、产品入口及 Fast mode 推广片段 |
| `claude_prompt_replace_security_preamble` | true | 精确替换 Claude/Codex CLI 注入的 `IMPORTANT: Assist with authorized security testing...` 安全测试说明 |
| `claude_prompt_promotion_targets` | `null` | 默认维护的 Claude Code 推广精确匹配列表；填入非空数组可覆盖，`null` 恢复内置列表 |
| `claude_prompt_security_preamble_from` | 空 | 默认维护的安全测试提示原文；与下一项同时填写时覆盖内置替换 |
| `claude_prompt_security_preamble_to` | 空 | 对应的安全测试提示替换文本；留空且上一项留空时恢复内置替换 |
| `claude_prompt_replacement_enabled` | false | 是否在发送上游前，对 Claude 真正的 system/developer 提示词执行有序字面量替换 |
| `claude_prompt_replacements` | `[]` | 最多 32 条 `{from,to,disabled,models}` 规则；面板可明确选择替换或删除，删除仍兼容保存为 `to: ""` |
| `proxy_url` | 空 | 出站代理地址 (如 `http://127.0.0.1:7890`) |
| `parallel_pool_enabled` | true | 是否开启并发竞速节点池 |
| `parallel_pool_retry_enabled` | true | 并发池节点遇到 429 等可重试错误时是否在节点内自动重试 |
| `parallel_pool_size` | 10 | 同时运行的候选代理上限 |
| `proxy_failover_max_attempts` | 30 | 单次请求最多尝试的代理数 |
| `parallel_pool_delay_ms` | 1000 | 后备代理启动间隔（毫秒） |
| `proxy_health_check_enabled` | true | 是否启用后台代理健康巡检 |
| `proxy_health_check_interval_minutes` | 15 | 健康巡检间隔（分钟） |
| `proxy_health_check_batch_size` | 50 | 每轮健康巡检最多测试的代理数 |
| `proxy_health_check_concurrency` | 5 | 健康巡检并发数 |
| `proxy_health_check_timeout_seconds` | 8 | 单个代理巡检超时（秒） |
| `keepalive_url` | 空 | 部署保活的 HTTP(S) 地址；留空关闭，保存后立即热重载 |
| `keepalive_interval` | 60 | 保活 GET 请求间隔（秒），范围 5–86400 |
| `gemini_api_mode` | `anon` | 上游通道：`anon`＝无头 Vertex 匿名端点（默认）；`official`＝官方 Gemini API 直连。支持环境变量 `VPROXY_GEMINI_API_MODE` |
| `gemini_api_key` | 空 | 直连模式密钥，仅 `official` 模式生效；支持面板粘贴 / `VPROXY_GEMINI_API_KEY`（单独设置时视为要直连）/ 构建期注入内置密钥 |
| `gemini_api_base_url` | 空 | 官方 API 基地址覆盖（镜像/中转）；留空使用 `https://generativelanguage.googleapis.com/v1beta` |

自建 HTTPS 反向代理默认不会信任客户端可伪造的 `X-Forwarded-*` 请求头。只有当 Vertex 仅接收来自可信反向代理的流量时，才设置环境变量 `VPROXY_TRUST_PROXY_HEADERS=true`，以便管理会话正确识别 HTTPS 和客户端地址；Render 环境会自动启用该行为。

保活也可完全通过环境变量配置。推荐使用 `VPROXY_KEEPALIVE_URL` 与 `VPROXY_KEEPALIVE_INTERVAL_SECONDS`；为兼容 gcli2api，项目同时接受 `KEEPALIVE_URL` 与 `KEEPALIVE_INTERVAL`（以及 `VPROXY_KEEPALIVE_INTERVAL`）。同一项同时设置时，`VPROXY_*` 形式优先；URL 环境变量存在时，管理面板会将对应字段标为环境托管。可将 URL 设为当前实例的 `/keepalive`，例如 `https://example.onrender.com/keepalive`。

> **提示**：默认模型清单已包含 `gemini-3.6-flash`、官方已发布的 `gemini-3.7-flash` 与 `gemini-3.8-flash`（2026-09-02 GA）。在模型名前加上 `fake-` 或 `假流式-` 前缀，可将非流式模型伪装成流式输出。假流式变体同样会经过完整的思考强度归一化（fake prefix 在内部被防御性剥离）。3.7/3.8 均为官方三档思考规格，详见下方思考强度说明。

> **预填充兼容**：通过 OpenAI Chat/Responses、Claude Messages 或 Gemini 原生接口调用 `gemini-3.6-flash`、`gemini-3.7-flash`、`gemini-3.8-flash`（以及后续 3.6+ 系列）时，服务会保留末尾纯文本 `assistant`/`model` 预填充的角色与原文，再追加一条短续写提示，使请求合法地以 `user` 结束；流式或非流式回复都会移除模型可能重复输出的前缀。工具调用、媒体、思考块和其他模型的历史行为保持不变；旧客户端传入的 `NONE`/`thinkingBudget` 也会在出站时转换为该模型支持的思考级别。

> **🧠 思考强度（Thinking Budget）兼容**：本项目会自动把客户端的各种"思考强度"参数统一映射到 Gemini 的 `thinkingConfig`，按模型官方上限做 clamp，并按目标模型支持的字段形态（budget vs level）归一化。具体行为见下表：

| 协议路径 | 客户端字段 | 映射规则 |
|---|---|---|
| OpenAI Chat / Responses | `reasoning_effort` | `none/minimal/low/medium/high/max/xhigh` → 对应 `thinkingLevel`；`auto/default` → 不设置 thinkingConfig，让模型用默认行为 |
| Anthropic Messages（Claude Code） | `thinking.budget_tokens` | 2.5 系列保留 budget 并 clamp；3.x 系列按 budget 大小映射到 LOW/MEDIUM/HIGH；`-1`（动态）→ HIGH |
| Anthropic Messages（新 API） | `output_config.effort` | `low/medium/high/xhigh/max/auto/default` 全部接受 |
| Gemini 原生 | `thinkingConfig.thinkingBudget` / `thinkingLevel` | 出站前按模型 kind 归一化（见下） |

各 Gemini 模型的思考控制形态（出站会自动归一化到模型支持的字段）：

| 模型 family | 支持字段 | 范围 / 档位 | 默认行为 |
|---|---|---|---|
| `gemini-2.5-pro` | thinkingBudget | 128 – 32768（无法关闭，最小 128） | 动态思考 |
| `gemini-2.5-flash` | thinkingBudget | 0 – 24576 | 动态思考 |
| `gemini-2.5-flash-lite` | thinkingBudget | 512 – 24576（正数下限 512） | 关闭 |
| `gemini-2.5-flash-image` | 不支持 | 删除整个 thinkingConfig | 固定 |
| `gemini-3.8-flash` / `3.7-flash`（官方已发布） | thinkingLevel | LOW/MEDIUM/HIGH（**不支持 MINIMAL，发送会报验证错误**） | 默认 medium；NONE/MINIMAL 自动折叠为 LOW |
| `3.6-flash` / `3.5-flash` / `3.1-flash-lite` / `3-flash` | thinkingLevel | MINIMAL/LOW/MEDIUM/HIGH | 3.6 默认 medium |
| `gemini-3.1-pro` | thinkingLevel | LOW/MEDIUM/HIGH（不支持 MINIMAL） | 默认 high |
| `gemini-3.1-flash-image` / `3.1-flash-lite-image` | thinkingLevel | MINIMAL/HIGH 两档 | — |
| `gemini-3-pro-preview` | thinkingLevel | LOW/HIGH 两档 | — |
| `gemini-3-pro-image` | 不支持 | 删除整个 thinkingConfig | 固定 |

> **Claude Code "max" 档**：Claude Code 走 Anthropic 协议时，"max" 档会发送较大的 `thinking.budget_tokens`。本项目会自动按 budget 大小映射到 HIGH（>8192 → HIGH），从而真正开启 Gemini 的最大思考强度。Gemini 3.6 系列官方明确支持 MINIMAL/LOW/MEDIUM/HIGH 四档（HIGH 即最大档）；官方已发布的 3.7 Flash（2026-08-13）与 3.8 Flash（2026-09-02）均仅支持 LOW/MEDIUM/HIGH 三档，`thinkingLevel: MINIMAL` 会返回 API 验证错误，因此本项目会把 NONE/MINIMAL 请求自动折叠为 LOW。

> **聊天软件 "auto" 档**：Cherry Studio / ChatBox 等聊天软件的"自动"选项会发送 `reasoning_effort: "auto"` 或 `output_config.effort: "auto"`。本项目会跳过设置 thinkingConfig，让 Gemini 使用其官方默认思考行为（2.5 系列动态思考、3.x 系列 HIGH），不会被报错也不会被钉死在固定档位。

> **Claude system 处理**：默认先精确移除 Claude Code 注入的 Claude 模型推荐、产品入口及 Fast mode 推广三行，再精确替换 Claude/Codex CLI 注入的 `IMPORTANT: Assist with authorized security testing...` 安全测试说明，二者都可在管理面板关闭。设置页的“内置规则内容”会展示每一段精确原文及替换结果，可单独编辑、恢复维护的默认值或关闭规则；Claude 请求后，最近记录会在设置页打开期间最多每 5 秒更新，并显示每条推广片段和安全提示的实际命中数。随后可以为 `/v1/messages` 和 `/v1/messages/count_tokens` 配置多条 system 精确替换或删除规则。每条规则可单独停用或限定客户端/实际模型名；模型范围默认留空并适用于所有模型，只有主动填写时才会限制。规则区分大小写、空白与换行，按列表顺序逐条执行，最后再以前置或后置的独立 system 片段注入额外提示词。顶层和中途 system 会保持先后顺序与独立片段，只有规则确实跨片段匹配时才兼容性合并。`role: user` 内的 `<system-reminder>` 仍是用户内容，不会被 system 规则改写。生成与 Token 计数的最近记录分开保存在当前进程内，可使用尚未保存的页面设置进行后端预览；被截断的记录不会用于创建精确规则，也不会写入日志或配置文件。

详细配置说明请参阅 [部署指南](部署指南.md#配置怎么改)。

## ☁️ 部署到 Render

仓库根目录已提供 `render.yaml`，可在上传 GitHub 后直接创建 Render Blueprint。部署时只需填写管理面板密码和 API Key；还可选填纯文本代理订阅 URL，服务启动后会立即拉取并默认每 60 分钟更新。远程订阅 URL 默认只能指向公网，订阅代理默认只接受公网 IP 字面量，避免误访问 Render 内部网络和 DNS 重绑定。Blueprint 默认同时运行最多 10 个候选代理、每个候选节点最多重试 10 次、单次请求最多接力尝试 30 个节点、全局最多处理 16 个上游请求，并每 15 分钟分批巡检代理健康。`/healthz` 用于进程存活检查，Render 使用 `/readyz` 同时验证 SQLite 和 API Key 已就绪。详细配置、免费方案限制和持久化说明见 [Render 部署指南](RENDER_DEPLOY.md)。
