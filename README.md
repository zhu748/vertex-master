# Vertex AI Proxy

免费使用 Google Gemini 模型的代理工具。将 **OpenAI、OpenAI Responses、Anthropic Claude 兼容请求**无缝转换为对 Google 匿名端点的调用，也支持 Gemini 原生格式。

**免安装、解压即用的绿色软件。** 全面支持 Windows、Linux、macOS 以及 Android 手机等平台。

## ✨ 核心特性

- **多协议兼容**：支持 OpenAI Chat Completions、OpenAI Responses、Anthropic Messages 和 Gemini 原生接口。
- **流式与工具调用**：四类接口均支持流式/非流式文本；兼容 Function Calling / Tool Use 及工具结果回传。
- **多模态输入**：兼容 OpenAI、Responses、Claude 和 Gemini 常见的图片输入格式。
- **丰富的多媒体支持**：支持文生图、图片编辑、语音合成（TTS）。
- **内置反爬突破**：内置 TLS 指纹伪装及 reCAPTCHA token 自动获取，轻松通过 Google 匿名端点校验。
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

> 两种 CLI 的本地工具均由 CLI 自己执行，本项目只负责模型协议转换。OpenAI/Anthropic 的服务端托管工具不在支持范围内。

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
| `max_retries` | 1 | 请求失败重试次数 |
| `max_request_mb` | 64 | 单次 HTTP 请求体上限（MiB，面板修改即时生效） |
| `drop_max_tokens` | false | 移除客户端附带的输出 token 上限，避免思考 token 挤占正文；默认严格遵守客户端上限 |
| `claude_prompt_injection_enabled` | false | 是否为 Claude Messages 请求前置或后置注入额外 system 提示词 |
| `claude_prompt_injection_position` | append | 注入位置，可选 `prepend` 或 `append` |
| `claude_prompt_injection_text` | 空 | 要注入的 system 提示词 |
| `claude_prompt_replacement_enabled` | false | 是否在发送上游前，对 Claude system 提示词执行有序字面量替换 |
| `claude_prompt_replacements` | `[]` | 最多 32 条 `{from,to,disabled,models}` 规则；`disabled` 和 `models` 可选，`models` 留空时适用于所有模型 |
| `proxy_url` | 空 | 出站代理地址 (如 `http://127.0.0.1:7890`) |
| `parallel_pool_enabled` | true | 是否开启并发竞速节点池 |
| `parallel_pool_retry_enabled` | true | 并发池节点遇到 429 等可重试错误时是否在节点内自动重试 |
| `parallel_pool_size` | 5 | 同时运行的候选代理上限 |
| `proxy_failover_max_attempts` | 30 | 单次请求最多尝试的代理数 |
| `parallel_pool_delay_ms` | 1000 | 后备代理启动间隔（毫秒） |
| `proxy_health_check_enabled` | true | 是否启用后台代理健康巡检 |
| `proxy_health_check_interval_minutes` | 15 | 健康巡检间隔（分钟） |
| `proxy_health_check_batch_size` | 50 | 每轮健康巡检最多测试的代理数 |
| `proxy_health_check_concurrency` | 5 | 健康巡检并发数 |
| `proxy_health_check_timeout_seconds` | 8 | 单个代理巡检超时（秒） |

> **提示**：默认模型清单已包含稳定版 `gemini-3.6-flash`。在模型名前加上 `fake-` 或 `假流式-` 前缀，可将非流式模型伪装成流式输出。

> **预填充兼容**：通过 OpenAI Chat/Responses、Claude Messages 或 Gemini 原生接口调用 `gemini-3.6-flash` 时，服务会保留末尾纯文本 `assistant`/`model` 预填充的角色与原文，再追加一条短续写提示，使请求合法地以 `user` 结束；流式或非流式回复都会移除模型可能重复输出的前缀。工具调用、媒体、思考块和其他模型的历史行为保持不变；旧客户端传入的 `NONE`/`thinkingBudget` 也会在出站时转换为 Gemini 3.6 支持的思考级别。

> **Claude system 处理**：管理面板「设置」页可以为 `/v1/messages` 和 `/v1/messages/count_tokens` 配置多条 system 精确替换规则。每条规则可单独停用或限定客户端/实际模型名；模型范围默认留空并适用于所有模型，只有主动填写时才会限制。规则区分大小写、空白与换行，按列表顺序逐条执行，随后再前置或后置注入额外提示词。生成与 Token 计数的最近记录分开保存在当前进程内，可使用尚未保存的页面设置进行后端预览；被截断的记录不会用于创建精确规则，也不会写入日志或配置文件。

详细配置说明请参阅 [部署指南](部署指南.md#配置怎么改)。

## ☁️ 部署到 Render

仓库根目录已提供 `render.yaml`，可在上传 GitHub 后直接创建 Render Blueprint。部署时只需填写管理面板密码和 API Key；还可选填纯文本代理订阅 URL，服务启动后会立即拉取并默认每 60 分钟更新。远程订阅 URL 默认只能指向公网，订阅代理默认只接受公网 IP 字面量，避免误访问 Render 内部网络和 DNS 重绑定。Blueprint 默认同时运行最多 5 个候选代理、单次请求最多接力尝试 30 个节点、全局最多处理 16 个上游请求，并每 15 分钟分批巡检代理健康。`/healthz` 用于进程存活检查，Render 使用 `/readyz` 同时验证 SQLite 和 API Key 已就绪。详细配置、免费方案限制和持久化说明见 [Render 部署指南](RENDER_DEPLOY.md)。
