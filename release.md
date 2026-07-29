# Release 构建指南

本项目使用 **GitHub Actions** 自动构建多平台二进制并发布 Release。本文档描述发布流程、版本号约定、本地构建方式以及产物清单。

## 发布流程（推荐：自动构建）

### 1. 准备提交

确保所有改动已合并到 `main` 分支，并通过本地校验：

```bash
# 格式检查（必须无输出）
gofmt -l ./cmd ./internal

# 静态检查
go vet ./...

# 全量测试（含 race 检测）
go test ./... -count=1
go test -race ./... -count=1

# JS 语法检查（管理后台资源）
for f in internal/admin/assets/*.js; do node --check "$f"; done

# GitHub Actions 工作流检查
go run github.com/rhysd/actionlint/cmd/actionlint@v1.7.12 -shellcheck=
```

### 2. 决定版本号

遵循 [Semantic Versioning](https://semver.org/lang/zh-CN/)：

| 类型 | 触发条件 | 示例 |
|------|---------|------|
| **Major** (X.0.0) | 不兼容的 API 变更 | 协议端点路径变更、配置文件格式破坏性改动 |
| **Minor** (1.X.0) | 向后兼容的新功能 | 新增协议、新增模型、新增管理面板页面 |
| **Patch** (1.0.X) | 向后兼容的 bug 修复 | 修复流式中断、修复鉴权失败、修复崩溃 |

查看当前最新版本：

```bash
git tag -l --sort=-version:refname | head -5
```

### 3. 打标签并推送

```bash
# 假设当前最新版本是 v1.1.0，本次是 bug 修复
git tag -a v1.1.1 -m "fix(vertex): stop aborting streams on harmless promptFeedback.blockReason"
git push origin v1.1.1
```

### 4. 等待 Actions 完成

推送 `v*` 标签后，`.github/workflows/release.yml` 会自动触发：

1. **verify** job：跑 `actionlint` / `gofmt` / `go vet` / `go test` / `go test -race` / JS 语法检查
2. **build** job：交叉编译 9 个平台产物，生成 SHA256SUMS.txt，发布 GitHub Release

在 **Actions** 页面查看进度：`https://github.com/<user>/vertex-master/actions`

构建完成后，Release 会出现在 `https://github.com/<user>/vertex-master/releases`，包含所有产物附件。

### 5.（可选）手动触发试跑

不推标签也能在 Actions 页面手动触发 workflow：

- 进入 Actions → Release → Run workflow
- 输入版本号（如 `v1.2.0-rc1`）
- 勾选 `draft` 可发布为草稿（不直接公开）

## 本地构建（不通过 Actions）

如需在本地复现 release 产物：

```bash
# Linux / macOS
bash scripts/build-release.sh v1.1.1

# Windows PowerShell
powershell -File scripts/build-release.ps1 v1.1.1
```

产物输出到 `dist/` 目录。

### Android 产物（需要 NDK）

Android/arm64 产物需要 CGO + Android NDK。设置环境变量：

```bash
export ANDROID_NDK_HOME=/path/to/android-ndk
bash scripts/build-release.sh v1.1.1
```

未设置 `ANDROID_NDK_HOME` 时会自动跳过 Android 产物。

## 产物清单

每个 release 包含以下 9 个压缩包（Android 需 NDK 才会构建）：

| 文件名 | 平台 | 适用场景 |
|--------|------|---------|
| `vertex-proxy-windows-amd64.zip` | Windows x64 | 主流 Windows PC |
| `vertex-proxy-windows-386.zip` | Windows 32 位 | 老机器 |
| `vertex-proxy-linux-amd64.zip` | Linux x86_64 | 主流 Linux 服务器 |
| `vertex-proxy-linux-386.zip` | Linux 32 位 | 老机器 |
| `vertex-proxy-linux-arm64.zip` | Linux ARM64 | 树莓派 3/4/5（64 位系统） |
| `vertex-proxy-linux-arm32.zip` | Linux ARM32 | 树莓派 0/1/2/3（32 位系统） |
| `vertex-proxy-android-arm64.zip` | Android arm64 | Termux / 鸿蒙 4.x 老版本 |
| `vertex-proxy-darwin-amd64.zip` | macOS Intel | 老 Mac |
| `vertex-proxy-darwin-arm64.zip` | macOS Apple Silicon | M1/M2/M3 Mac |

每个压缩包内含：

- `vertex-proxy` / `vertex-proxy.exe` — 主程序二进制
- `config/config.example.json` — 配置文件示例
- `config/api_keys.example.txt` — API Key 文件示例
- `config/models.json` — 模型清单
- `部署指南.md` — 详细部署文档
- `rules.txt` — 用户协议（已嵌入二进制，此为可读副本）
- 平台对应的启动脚本（`start.sh` / `启动.bat` / `setup.sh` / `setup.bat` / `vertex-proxy.service`）

附件还包含 `SHA256SUMS.txt`，用于校验下载完整性：

```bash
sha256sum -c SHA256SUMS.txt
```

## Release Notes

GitHub Release 的 release notes 由 `softprops/action-gh-release` 的 `generate_release_notes: true` 自动生成，包含自上一个 release 以来的所有 commit 标题与 PR 链接。

如需手写 release notes，可在 release 发布后进入编辑页面手动修改。

## 版本号嵌入

`scripts/build-release.sh` 通过 `-ldflags` 把版本信息嵌入二进制：

```
-X main.version=v1.1.1
-X main.buildCommit=<git short hash>
-X main.buildTime=<UTC timestamp>
```

启动时控制台 banner 会显示这些信息，便于排查用户反馈的问题。
