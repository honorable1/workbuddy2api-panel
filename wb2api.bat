@echo off
setlocal
cd /d %~dp0

set PORT=7863
set CONFIG=config.json
set GOBIN=D:\Tools\go\bin
set PATH=%GOBIN%;%PATH%
set CGO_ENABLED=0
set GOPROXY=https://goproxy.cn,direct
set GOSUMDB=off

if "%1"=="" goto :start
if /i "%1"=="start"   goto :start
if /i "%1"=="stop"    goto :stop
if /i "%1"=="restart" goto :restart
if /i "%1"=="status"  goto :status
if /i "%1"=="build"   goto :build
goto :usage

:start
call :findpid
if defined PID (
  echo Already running ^(PID %PID%^), skip.
  goto :eof
)
if not exist auths mkdir auths
if not exist data mkdir data
if exist bin\server.exe (
  echo Starting prebuilt bin\server.exe ... ^(live log window will open^)
  start "" cmd /c "cd /d %~dp0 && bin\server.exe -config %CONFIG%"
) else (
  echo Starting via 'go run ./cmd/server' ... ^(live log window will open^)
  start "" cmd /c "cd /d %~dp0 && %GOBIN%\go.exe run ./cmd/server -config %CONFIG%"
)
set WAIT=40
:waitup
call :findpid
if defined PID (echo Started, PID %PID% & echo Tip: close the server window or run stop.bat to stop. & goto :eof)
set /a WAIT-=1
if %WAIT% leq 0 (echo Start timeout, check the server window for errors & goto :eof)
timeout /t 1 >nul
goto :waitup

:stop
call :findpid
if not defined PID (
  echo No process listening on %PORT%, already stopped.
  goto :eof
)
echo Stopping PID %PID% ...
taskkill /PID %PID% /T /F >nul 2>&1
timeout /t 1 >nul
call :findpid
if defined PID (echo Still running, leftover PID %PID%, run: taskkill /PID %PID% /F) else (echo Stopped)
goto :eof

:restart
call :stop
timeout /t 2 >nul
call :start
goto :eof

:status
call :findpid
if not defined PID goto :status_down
echo Running ^(PID %PID%^)
curl.exe -s --noproxy 127.0.0.1 -o nul -w "healthz -> HTTP %%{http_code}\n" --max-time 3 http://127.0.0.1:%PORT%/healthz
echo.
goto :eof
:status_down
echo Not running
goto :eof

:build
if not exist bin mkdir bin
echo go build -o bin\server.exe ./cmd/server
"%GOBIN%\go.exe" build -o bin\server.exe ./cmd/server && echo Build OK: bin\server.exe || echo Build FAILED
goto :eof

:findpid
set PID=
for /f "tokens=5" %%a in ('netstat -ano 2^>nul ^| findstr ":%PORT%" ^| findstr "LISTEN"') do set PID=%%a
goto :eof

:usage
echo Usage: %~nx0 {start^|stop^|restart^|status^|build}
goto :eof
