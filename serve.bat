@echo off
rem ===========================================================================
rem  workbuddy2api 静默启动器
rem  给「开机自启」用：不开新窗口、前台运行 server.exe、日志落 logs\server.log。
rem  与 wb2api.bat start 的区别：那边 start 一个 cmd 窗口托管进程，窗口一关服务就停；
rem  这里没有窗口，进程独立存活，适合登录时自动拉起。
rem  也可手动双击（此时不会有任何界面，日志请去 logs\server.log 看）。
rem ===========================================================================
setlocal
cd /d "%~dp0"

rem 端口必须与 wb2api.bat 的 PORT 一致（改监听端口时两处一起改）
set "PORT=7863"
set "LOGDIR=%~dp0logs"
set "LOGFILE=%LOGDIR%\server.log"

if not exist "%~dp0bin\server.exe" (
  echo [serve] 找不到 bin\server.exe，先执行 build.bat 或 redeploy.bat
  exit /b 1
)

rem 单实例：端口已在监听说明服务在跑，再起一个只会 bind 失败并往日志刷错误
call :port_busy
if defined BUSY (
  echo [serve] %PORT% 已在监听（PID %BUSY%），服务已在运行，跳过启动。
  exit /b 0
)

if not exist "%LOGDIR%" mkdir "%LOGDIR%" >nul 2>&1

rem 日志轮转：超过 10MB 留一代，长期自启不会把磁盘写满
if exist "%LOGFILE%" (
  for %%A in ("%LOGFILE%") do if %%~zA GTR 10485760 (
    if exist "%LOGFILE%.1" del /f /q "%LOGFILE%.1" >nul 2>&1
    move /y "%LOGFILE%" "%LOGFILE%.1" >nul 2>&1
  )
)

echo [serve] %DATE:~0,10% %TIME% starting bin\server.exe -config config.json >> "%LOGFILE%"
"%~dp0bin\server.exe" -config config.json >> "%LOGFILE%" 2>&1
echo [serve] %DATE:~0,10% %TIME% server exited with code %ERRORLEVEL% >> "%LOGFILE%"
exit /b 0

rem findstr 用 ":7863 " 带尾随空格精确匹配，避免误命中 17863 这类端口
:port_busy
set BUSY=
for /f "tokens=5" %%a in ('netstat -ano 2^>nul ^| findstr /c:":%PORT% " ^| findstr /i "LISTEN"') do set BUSY=%%a
goto :eof
