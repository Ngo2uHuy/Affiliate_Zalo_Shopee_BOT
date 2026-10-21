@echo off
cd /d "%~dp0"
if not exist .env (
  copy .env.example .env >nul
  echo Da tao file .env - hay mo bang Notepad, dien ZALO_BOT_TOKEN va ZALO_WEBHOOK_SECRET roi chay lai.
  notepad .env
  pause
  exit /b 1
)
go run . -tunnel
pause
