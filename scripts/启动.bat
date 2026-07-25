@echo off
chcp 65001 >nul
cd /d "%~dp0"

if not exist config mkdir config
if not exist config\config.json if exist config\config.example.json copy config\config.example.json config\config.json >nul
if not exist config\api_keys.txt (
  if exist config\api_keys.example.txt copy config\api_keys.example.txt config\api_keys.txt >nul
  echo [!] 已生成 config\api_keys.txt —— 请用记事本编辑它，填入你的 sk- 密钥后重新启动。
  echo.
)

echo ════════════════════════════════════════════════════════
echo   ⚠️  本软件完全免费，如果你花钱购买了，你被骗了。
echo   获取正版请前往：https://discord.gg/odysseia
echo ════════════════════════════════════════════════════════
echo.
echo 首次启动需要同意使用规则，请仔细阅读。
echo 启动后管理面板: http://127.0.0.1:2156/admin/
echo --------------------------------------------------------------------
vertex-proxy.exe

echo.
echo 程序已退出。按任意键关闭窗口。
pause >nul
