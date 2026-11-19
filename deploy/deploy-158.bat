@echo off
chcp 65001 >nul
powershell -NoProfile -ExecutionPolicy Bypass -File "%~dp0deploy-158.ps1"
echo.
echo 输出也保存在 deploy-158.log
pause
