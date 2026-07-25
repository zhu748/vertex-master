# Render 部署指南

本项目已支持 Render 的 Docker Web Service 和 Blueprint 部署。

## 一、部署前准备

请先准备两个不要提交到 GitHub 的密码：

- `VPROXY_ADMIN_PASSWORD`：管理后台密码，建议使用 16 位以上的随机字符。
- `VPROXY_API_KEY`：客户端调用 API 时使用的非空密钥，无需以 `sk-` 开头。

本版本的使用规则哈希为：

```text
36800adeec862126
```

将该值填入 `VPROXY_RULES_HASH` 表示你已阅读并同意项目根目录 `cmd/vproxy/rules.txt` 中的使用规则。规则更新后哈希会变化，需要重新确认。

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
4. 按提示填入以下三个环境变量：
   - `VPROXY_ADMIN_PASSWORD`
   - `VPROXY_API_KEY`
   - `VPROXY_RULES_HASH`，值为 `36800adeec862126`
5. 创建 Blueprint 并等待构建完成。
6. 打开 `https://<你的服务名>.onrender.com/healthz`，返回 `"status":"healthy"` 即表示部署成功。

管理后台地址：

```text
https://<你的服务名>.onrender.com/admin/
```

OpenAI 兼容 API 地址：

```text
https://<你的服务名>.onrender.com/v1
```

## 四、免费方案与持久化

`render.yaml` 默认使用 Render Free Web Service。免费实例的文件系统是临时的，实例休眠、重启或重新部署后，通过后台修改的配置、节点、SQLite 数据和上传的背景图会丢失。但 Blueprint 中的管理密码和 API Key 来自 Render 环境变量，重启后仍然有效。

如果需要保留所有后台修改：

1. 将 Render 服务升级为付费实例。
2. 在服务的 **Disks** 页面添加持久磁盘。
3. 将挂载路径设为 `/app/storage`。

只有该挂载路径下的文件会被持久化；项目已将配置、API Key、模型、SQLite、日志和自定义资源统一放在该目录下。

## 五、常见问题

- 日志显示规则未同意并且容器退出：检查 `VPROXY_RULES_HASH` 是否与上文完全一致。
- Render 报告没有监听端口：不要删除 Render 自动注入的 `PORT`；程序会自动监听该端口。
- API 返回 401：检查请求头是否为 `Authorization: Bearer <VPROXY_API_KEY>`，且密钥是否与 Render 环境变量完全一致。
- 修改 Render 中的密码或密钥后，需要重新部署服务才能生效。

## 六、Render 官方资料

- [Blueprint YAML 参考](https://render.com/docs/blueprint-spec)
- [Web Service 端口绑定](https://render.com/docs/web-services#port-binding)
- [免费实例限制](https://render.com/docs/free)
- [持久磁盘](https://render.com/docs/disks)
