# Vertex AI Proxy

免费使用 Google Gemini 模型的代理工具。将 **OpenAI 兼容的 API 请求**无缝转换为对 Google 匿名端点的调用——让你的客户端以为在调用 OpenAI，实际上使用的是免费的 Gemini 服务。也支持原始Gemini格式

**免安装、解压即用的绿色软件。** 全面支持 Windows、Linux、macOS 以及 Android 手机等平台。

## ✨ 核心特性

- **完整兼容 OpenAI 接口**：支持聊天（流式/非流式）、工具调用（Function Calling）、多模态输入（图片/文件）。
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
| `proxy_url` | 空 | 出站代理地址 (如 `http://127.0.0.1:7890`) |
| `parallel_pool_enabled` | true | 是否开启并发竞速节点池 |
| `parallel_pool_size` | 5 | 同时运行的候选代理上限 |
| `proxy_failover_max_attempts` | 30 | 单次请求最多尝试的代理数 |
| `parallel_pool_delay_ms` | 1000 | 后备代理启动间隔（毫秒） |
| `proxy_health_check_enabled` | true | 是否启用后台代理健康巡检 |
| `proxy_health_check_interval_minutes` | 15 | 健康巡检间隔（分钟） |
| `proxy_health_check_batch_size` | 50 | 每轮健康巡检最多测试的代理数 |
| `proxy_health_check_concurrency` | 5 | 健康巡检并发数 |
| `proxy_health_check_timeout_seconds` | 8 | 单个代理巡检超时（秒） |

> **提示**：在模型名（如 `gemini-3.5-flash`）前加上 `fake-` 或 `假流式-` 前缀，可将非流式模型伪装成流式输出。

详细配置说明请参阅 [部署指南](部署指南.md#配置怎么改)。

## ☁️ 部署到 Render

仓库根目录已提供 `render.yaml`，可在上传 GitHub 后直接创建 Render Blueprint。部署时只需填写管理面板密码和 API Key；还可选填纯文本代理订阅 URL，服务启动后会立即拉取并默认每 60 分钟更新。远程订阅 URL 默认只能指向公网，订阅代理默认只接受公网 IP 字面量，避免误访问 Render 内部网络和 DNS 重绑定。Blueprint 默认同时运行最多 5 个候选代理、单次请求最多接力尝试 30 个节点、每 1000 毫秒启动一个后备节点，并每 15 分钟分批巡检代理健康。详细配置、免费方案限制和持久化说明见 [Render 部署指南](RENDER_DEPLOY.md)。
