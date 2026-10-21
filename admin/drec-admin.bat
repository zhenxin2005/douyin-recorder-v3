@echo off
chcp 65001 >nul
title douyin-recorder-v3 激活码管理
powershell -NoProfile -ExecutionPolicy Bypass -File "%~dp0drec-admin.ps1"
pause
