#!/bin/bash
set -e

# 支持将配置指向 Render 持久磁盘或其他自定义目录
mkdir -p "$(dirname "$VPROXY_CONFIG")"
mkdir -p "$(dirname "$VPROXY_API_KEYS")"
mkdir -p "$(dirname "$VPROXY_MODELS")"

# 若未检测到用户的配置文件，则从系统备用区初始化一份默认配置文件
if [ ! -f "$VPROXY_CONFIG" ]; then
    echo "[Entrypoint] 未检测到 config.json，正在初始化默认配置..."
    cp /app/config.example.json "$VPROXY_CONFIG"
fi

if [ ! -f "$VPROXY_API_KEYS" ]; then
    echo "[Entrypoint] 未检测到 api_keys.txt，正在初始化密钥文件..."
    cp /app/api_keys.example.txt "$VPROXY_API_KEYS"
fi

if [ ! -f "$VPROXY_MODELS" ]; then
    echo "[Entrypoint] 未检测到 models.json，正在初始化模型清单..."
    cp /app/models.json "$VPROXY_MODELS"
fi

# 使用 exec 确保系统信号能够直接透传给 Go 程序，支持 SIGHUP 配置热重载
echo "[Entrypoint] 启动 Vertex AI Proxy 服务..."
exec /app/vproxy
