# Render 部署指南

本项目已支持 Render 的 Docker Web Service 和 Blueprint 部署。

## 一、部署前准备

请先准备两个不要提交到 GitHub 的密码：

- `VPROXY_ADMIN_PASSWORD`：管理后台密码，建议使用 16 位以上的随机字符。
- `VPROXY_API_KEY`：客户端调用 API 时使用的非空密钥，无需以 `sk-` 开头。

`render.yaml` 已内置本版本的使用规则哈希：

```text
36800adeec862126
```

该值表示你已阅读并同意项目根目录 `cmd/vproxy/rules.txt` 中的使用规则，不需要在创建 Blueprint 时再次填写。规则更新后哈希会变化，需要同步更新 `render.yaml`。

代理池订阅为可选配置：

- `VPROXY_PROXY_SUBSCRIPTION_URL`：纯文本代理列表 URL；留空即关闭环境变量托管代理池。
- `VPROXY_PROXY_SUBSCRIPTION_TYPE`：无协议前缀代理行所使用的类型，默认 `http`；可设为 `https`、`socks4`、`socks4a`、`socks5`、`socks5h` 或 `auto`。
- `VPROXY_PROXY_SUBSCRIPTION_INTERVAL_MINUTES`：自动拉取间隔，默认 `60` 分钟，范围为 1–10080。

订阅中已经带有 `http://`、`socks5://` 等前缀的行会使用各自的协议，不受默认类型影响。

`render.yaml` 还为代理接力和后台健康巡检提供以下默认值：

| 环境变量 | 默认值 | 说明 |
|---|---:|---|
| `VPROXY_PROXY_FAILOVER_MAX_ATTEMPTS` | `30` | 单次请求最多尝试的代理节点数 |
| `VPROXY_PROXY_HEALTH_CHECK_ENABLED` | `true` | 启用后台自动健康巡检 |
| `VPROXY_ALLOW_PRIVATE_SUBSCRIPTION_URLS` | `false` | 是否允许订阅 URL 和订阅代理指向私网/本机；仅可信内网自托管场景才应开启 |
| `VPROXY_ALLOW_DOMAIN_SUBSCRIPTION_PROXIES` | `false` | 是否允许远程订阅中的代理端点使用域名；默认仅接受公网 IP 字面量以阻断 DNS 重绑定 |
| `VPROXY_PROXY_SUBSCRIPTION_ALLOW_PROXY_FALLBACK` | `false` | 直连拉取失败时是否允许经当前代理重试；仅在信任该代理时开启 |
| `VPROXY_PROXY_HEALTH_CHECK_INTERVAL_MINUTES` | `15` | 巡检调度间隔（分钟） |
| `VPROXY_PROXY_HEALTH_CHECK_BATCH_SIZE` | `50` | 每轮最多巡检的节点数 |
| `VPROXY_PROXY_HEALTH_CHECK_CONCURRENCY` | `5` | 每轮巡检的最大并发数 |
| `VPROXY_PROXY_HEALTH_CHECK_TIMEOUT_SECONDS` | `8` | 单个代理巡检超时（秒） |

代理池本身默认最多同时运行 `5` 个候选节点、每次请求最多接力尝试 `30` 个节点，并以 `1000` 毫秒的间隔启动后备节点。前三项可在管理面板的「设置」页分别通过 `parallel_pool_size`、`proxy_failover_max_attempts` 和 `parallel_pool_delay_ms` 调整。

远程订阅直连拉取会校验初始地址、每次重定向和实际拨号 IP。订阅产生的代理端点默认只接受公网 IP 字面量，并过滤回环、私网、链路本地和保留地址，从而避免代理域名在导入后发生 DNS 重绑定。示例 jsDelivr 地址中的代理行自带协议且使用 IP，可直接使用。

只有完全信任订阅提供方时，才设置 `VPROXY_ALLOW_DOMAIN_SUBSCRIPTION_PROXIES=true` 允许域名代理。只有确实需要从可信局域网拉取订阅或使用内网代理时，才设置 `VPROXY_ALLOW_PRIVATE_SUBSCRIPTION_URLS=true`；后者会显著放宽 SSRF 防护。经代理拉取订阅默认关闭，因为代理端会独立解析目标域名；如确有需要，可在信任当前代理的前提下设置 `VPROXY_PROXY_SUBSCRIPTION_ALLOW_PROXY_FALLBACK=true`。

服务默认将单次请求体限制为 `64 MiB`。可在管理面板「设置」页修改 `max_request_mb`（1–1024 MiB），修改后立即生效；超限请求返回 HTTP `413`。

## 二、上传到 GitHub

在项目根目录打开 PowerShell：

```powershell
git init
git add .
git commit -m "Adapt project for Render deployment"
git branch -M main
git remote add origin https://github.com/<你的用户名>/<你的仓库名>.git
git push -u origin main
```

如果 GitHub 仓库已经有 README 或其他提交，不要直接强制推送，应先拉取并合并远程历史。

## 三、使用 Blueprint 部署

1. 登录 Render，选择 **New > Blueprint**。
2. 连接刚刚上传的 GitHub 仓库。
3. Render 会读取根目录的 `render.yaml`。
4. 按提示填写环境变量：
   - `VPROXY_ADMIN_PASSWORD`
   - `VPROXY_API_KEY`
   - `VPROXY_PROXY_SUBSCRIPTION_URL`（可选，不使用代理池时留空）
5. 代理类型、订阅刷新、接力尝试和健康巡检已有默认值；如需覆盖，可在 `render.yaml` 或服务的 **Environment** 页面修改。
6. 创建 Blueprint 并等待构建完成。
7. 打开 `https://<你的服务名>.onrender.com/healthz`，返回 `"status":"healthy"` 即表示部署成功。

设置代理 URL 后，服务会在启动时创建“环境变量代理池”并立即拉取一次，此后默认每 60 分钟刷新。后台健康巡检默认每 15 分钟从待检测节点中最多选择 50 个，以不超过 5 个并发逐一检测；请求遇到不可用代理时，会在本次请求的尝试上限内自动接力到后备节点。在管理面板中可以查看状态、手动刷新和调整运行参数；修改、停用或移除环境变量托管订阅时，请在 Render 环境变量中操作并重新部署。

管理后台地址：

```text
https://<你的服务名>.onrender.com/admin/
```

OpenAI 兼容 API 地址：

```text
https://<你的服务名>.onrender.com/v1
```

## 四、免费方案与持久化

`render.yaml` 默认使用 Render Free Web Service，需要注意：

- 免费 Web Service 在约 15 分钟没有入站请求后会休眠，下一次请求需要等待实例重新启动；休眠期间，进程内的订阅刷新和健康巡检也不会运行。
- 免费实例使用临时文件系统。实例休眠、重启或重新部署后，通过后台修改的配置、手动代理池订阅、节点、SQLite 数据和上传的背景图都会丢失。
- Render 可能暂停产生异常大量外联流量的免费服务。大型公共代理池的订阅刷新和健康巡检会增加外联请求；如遇限制，请缩小巡检批次、延长间隔或关闭自动巡检。
- Blueprint 中的管理密码、API Key 和可选代理订阅来自 Render 环境变量，重启后仍会恢复；订阅节点会在服务启动后重新拉取。

需要可靠保存后台修改时，建议：

1. 将 Render 服务升级为支持 Persistent Disk 的付费实例。
2. 在服务的 **Disks** 页面添加持久磁盘，并将挂载路径设为 `/app/storage`。
3. 保持单实例运行 SQLite；只有该挂载路径下的文件会被持久化，项目已将配置、API Key、模型、SQLite、日志和自定义资源统一放在该目录下。

如果计划多实例运行、需要数据库级高可用或不希望依赖单机磁盘，建议迁移到外部托管数据库和对象存储。当前版本的业务状态以 SQLite 和本地文件为主，接入 PostgreSQL 等外部数据库前需要增加相应的存储适配，不能只设置数据库 URL 即自动切换。

## 五、常见问题

- 日志显示规则未同意并且容器退出：检查 `VPROXY_RULES_HASH` 是否与上文完全一致。
- Render 报告没有监听端口：不要删除 Render 自动注入的 `PORT`；程序会自动监听该端口。
- API 返回 401：检查请求头是否为 `Authorization: Bearer <VPROXY_API_KEY>`，且密钥是否与 Render 环境变量完全一致。
- 免费实例唤醒后代理列表暂时为空：等待环境变量订阅完成首次拉取，或进入管理面板手动刷新。
- 大型代理池巡检造成外联流量过高：调低 `VPROXY_PROXY_HEALTH_CHECK_BATCH_SIZE`、调大 `VPROXY_PROXY_HEALTH_CHECK_INTERVAL_MINUTES`，或将 `VPROXY_PROXY_HEALTH_CHECK_ENABLED` 设为 `false`。
- 私有订阅 URL 被拒绝：默认安全策略只允许公网目标；仅在订阅和代理来源完全可信时设置 `VPROXY_ALLOW_PRIVATE_SUBSCRIPTION_URLS=true`。
- 订阅中的域名代理被过滤：默认仅接受公网 IP 代理；仅在完全信任订阅提供方时设置 `VPROXY_ALLOW_DOMAIN_SUBSCRIPTION_PROXIES=true`。
- 修改 Render 中的密码、密钥或代理订阅配置后，需要重新部署服务才能生效。

## 六、Render 官方资料

- [Blueprint YAML 参考](https://render.com/docs/blueprint-spec)
- [Web Service 端口绑定](https://render.com/docs/web-services#port-binding)
- [免费实例限制](https://render.com/docs/free)
- [持久磁盘](https://render.com/docs/disks)
