#!/bin/sh
# Vertex AI Proxy 启动脚本（Linux / Termux / ARM 设备）
# 缺配置时从示例初始化，然后启动程序。
# 首次启动时会显示使用规则，请仔细阅读并输入 yes 同意后方可使用。
set -e
cd "$(dirname "$0")"

mkdir -p config

if [ ! -f config/config.json ] && [ -f config/config.example.json ]; then
  cp config/config.example.json config/config.json
  echo "[init] 已生成 config/config.json"
fi

if [ ! -f config/api_keys.txt ]; then
  [ -f config/api_keys.example.txt ] && cp config/api_keys.example.txt config/api_keys.txt
  echo "[!] 已生成 config/api_keys.txt —— 请编辑它，填入你的 sk- 密钥后再用客户端访问。"
fi

# Termux SSL 证书修复
if [ -d "/data/data/com.termux" ]; then
  export SSL_CERT_FILE="${PREFIX}/etc/tls/cert.pem"
  [ ! -f "$SSL_CERT_FILE" ] && export SSL_CERT_FILE="${PREFIX}/etc/ssl/certs/ca-certificates.crt"
fi

chmod +x ./vertex-proxy 2>/dev/null || true
echo "[*] 首次启动需要同意使用规则，请仔细阅读。"
echo "[*] 启动后管理面板: http://127.0.0.1:2156/admin/"
exec ./vertex-proxy
