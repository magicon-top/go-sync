@echo off
cd /d "%~dp0"
schtasks /create /tn "Go_SyncWatcher" /tr "powershell -WindowStyle Hidden -NonInteractive -ExecutionPolicy Bypass -Command \"Start-Process '%CD%\go-sync.exe' -WorkingDir '%CD%' -NoNewWindow\"" /sc ONLOGON /rl HIGHEST /f
schtasks /run /tn "Go_SyncWatcher"


::schtasks /delete /tn "GoSyncWatcher" /f & taskkill /f /im go-sync.exe