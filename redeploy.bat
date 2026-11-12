@echo off
chcp 65001 >nul
setlocal
cd /d %~dp0

echo ============================================
echo   workbuddy2api 一键部署（停服 - 换版 - 启动）
echo ============================================
echo.

echo [1/4] 检查运行状态
call :findpid
if defined PID (
  echo       7863 正在监听（PID %PID%），先停止...
  taskkill /PID %PID% /T /F >nul 2>&1
  timeout /t 2 >nul
  call :findpid
  if defined PID (echo       停止失败，仍有 PID %PID%，请手动结束后重跑本脚本 & pause & exit /b 1)
  echo       已停止。
) else (
  echo       当前未运行。
)

echo [2/4] 备份旧二进制
if exist "bin\server.exe" (
  copy /y "bin\server.exe" "bin\server.exe~" >nul
  echo       旧版已备份为 bin\server.exe~
) else (
  echo       没有 bin\server.exe，跳过备份。
)

echo [3/4] 替换二进制
if exist "bin\server.new.exe" (
  move /y "bin\server.new.exe" "bin\server.exe" >nul
  echo       已用 bin\server.new.exe 替换 bin\server.exe
) else (
  echo       没有 server.new.exe，改为现场编译（需要 Go 工具链）...
  call "%~dp0wb2api.bat" build
)

echo [4/4] 启动服务
call "%~dp0wb2api.bat" start
echo.
curl.exe -s --noproxy 127.0.0.1 -o nul -w "       healthz -^> HTTP %%{http_code}\n" --max-time 5 http://127.0.0.1:7863/healthz
echo.
echo 完成。面板：http://127.0.0.1:7863/panel/  记得 Ctrl+F5 强刷
echo 回滚：先 stop.bat，再把 bin\server.exe~ 改名回 bin\server.exe，最后 start.bat
echo.
pause
goto :eof

:findpid
set PID=
for /f "tokens=5" %%a in ('netstat -ano 2^>nul ^| findstr ":7863" ^| findstr "LISTEN"') do set PID=%%a
goto :eof
