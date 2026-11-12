#!/bin/bash
# WorkBuddy2API Panel 启动/停止管理脚本
# 适用：Git Bash / WSL（Windows 上的 POSIX shell）
# 用法： ./wb2api.sh {start|stop|restart|status|build}
#
# 说明：
#   - 默认采用「方式三：源码运行（开发调试）」即 go run ./cmd/server
#   - 启动前若 bin/server.exe 不存在，会先 go build 一次（仅首次/改源码后）
#   - 端口检测同时覆盖 IPv4(0.0.0.0:7863) 与 IPv6([::]:7863)

set -u
cd "$(dirname "$0")"

PORT=7863
CONFIG=config.json
BIN=bin/server.exe
GOBIN=/d/Tools/go/bin

# ---- 环境 ----
export PATH="$GOBIN:$PATH"
export CGO_ENABLED=0
export GOPROXY="https://goproxy.cn,direct"
export GOSUMDB="off"

pid_on_port() {
  # 返回监听 PORT 的进程 PID（Windows netstat -ano，兼容 IPv4/IPv6）
  netstat -ano 2>/dev/null \
    | grep LISTEN \
    | grep -E "[:.]${PORT}([^0-9]|$)" \
    | awk '{print $5}' | head -1
}

case "${1:-}" in
  build)
    echo "==> go build -o $BIN ./cmd/server"
    mkdir -p bin
    go build -o "$BIN" ./cmd/server && echo "构建完成: $BIN" || echo "构建失败"
    ;;

  start)
    p=$(pid_on_port)
    if [ -n "$p" ]; then
      echo "已在运行 (PID $p)，无需重复启动"
      exit 0
    fi
    mkdir -p auths data
    # 优先用预编译二进制（启动快）；不存在则回退 go run
    if [ -x "$BIN" ]; then
      echo "==> 启动预编译二进制 $BIN"
      nohup "$BIN" -config "$CONFIG" > server.log 2>&1 &
    else
      echo "==> 启动 (go run ./cmd/server -config $CONFIG)"
      nohup go run ./cmd/server -config "$CONFIG" > server.log 2>&1 &
    fi
    sleep 2
    p=$(pid_on_port)
    if [ -n "$p" ]; then
      echo "已启动 (PID $p)，日志: server.log"
      echo "面板: http://127.0.0.1:${PORT}/panel/  |  健康检查: http://127.0.0.1:${PORT}/healthz"
    else
      echo "未能确认是否启动成功，请查看 server.log"
    fi
    ;;

  stop)
    p=$(pid_on_port)
    if [ -z "$p" ]; then
      echo "未发现监听 ${PORT} 的进程（可能已停止）"
      exit 0
    fi
    echo "==> 停止监听 ${PORT} 的进程 PID $p"
    taskkill.exe //PID "$p" //T //F >/dev/null 2>&1
    sleep 1
    pp=$(pid_on_port)
    if [ -z "$pp" ]; then
      echo "已停止"
    else
      echo "仍未停止，残留 PID $pp，可手动执行: taskkill.exe //PID $pp //F"
    fi
    ;;

  restart)
    "$0" stop
    sleep 2
    "$0" start
    ;;

  status)
    p=$(pid_on_port)
    if [ -n "$p" ]; then
      echo "运行中 (PID $p)"
      curl -s -o /dev/null -w "healthz -> HTTP %{http_code}\n" --max-time 3 "http://127.0.0.1:${PORT}/healthz"
    else
      echo "未运行"
    fi
    ;;

  *)
    echo "用法: $0 {start|stop|restart|status|build}"
    exit 1
    ;;
esac
