@echo off
cd /d "%~dp0"
schtasks /create /tn "Go_SyncWatcher" /tr "\"%CD%\go-sync.exe\"" /sc ONLOGON /ru "NT AUTHORITY\SYSTEM" /rl HIGHEST /f
schtasks /run /tn "Go_SyncWatcher"


::schtasks /delete /tn "GoSyncWatcher" /f & taskkill /f /im go-sync.exe