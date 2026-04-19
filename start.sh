#!/bin/bash
# Console Manager 启动/停止/状态管理脚本

APP_NAME="console-manager"
APP_DIR="$(cd "$(dirname "$0")" && pwd)"
APP_BIN="$APP_DIR/$APP_NAME"
APP_BIN_ARM64="$APP_DIR/${APP_NAME}-linux-arm64"
SOURCE_DIR="$APP_DIR/backend"
PID_FILE="$APP_DIR/$APP_NAME.pid"
LOG_FILE="$APP_DIR/$APP_NAME.log"
PORT="${PORT:-8080}"

Red='\033[0;31m'
Green='\033[0;32m'
Yellow='\033[0;33m'
NC='\033[0m'

usage() {
    echo "用法: $0 {start|stop|restart|status|log}"
    echo ""
    echo "  start    启动服务（后台运行，断开SSH不影响）"
    echo "  stop     停止服务"
    echo "  restart  重启服务"
    echo "  status   查看运行状态"
    echo "  log      查看实时日志"
}

is_running() {
    if [ -f "$PID_FILE" ]; then
        local pid
        pid=$(cat "$PID_FILE")
        if kill -0 "$pid" 2>/dev/null; then
            return 0
        fi
        rm -f "$PID_FILE"
    fi
    return 1
}

build_binary() {
    if ! command -v go >/dev/null 2>&1; then
        echo -e "${Red}[✗] 未找到 Go 环境，无法自动编译${NC}"
        echo "    请先安装 Go，或手动上传已编译的可执行文件"
        return 1
    fi

    if [ ! -f "$SOURCE_DIR/go.mod" ]; then
        echo -e "${Red}[✗] 未找到源码目录: $SOURCE_DIR${NC}"
        echo "    请确保 backend 源码存在，或手动上传已编译的可执行文件"
        return 1
    fi

    echo -e "${Yellow}[*] 未检测到可执行文件，开始自动编译...${NC}"
    if (cd "$SOURCE_DIR" && CGO_ENABLED=0 go build -o "$APP_BIN" -ldflags="-s -w" .); then
        chmod +x "$APP_BIN"
        echo -e "${Green}[✓] 编译完成: $APP_BIN${NC}"
        return 0
    fi

    echo -e "${Red}[✗] 自动编译失败${NC}"
    return 1
}

do_start() {
    if is_running; then
        echo -e "${Yellow}[!] $APP_NAME 已在运行 (PID: $(cat "$PID_FILE"))${NC}"
        return 1
    fi

    if [ -x "$APP_BIN" ]; then
        :
    elif [ -x "$APP_BIN_ARM64" ]; then
        APP_BIN="$APP_BIN_ARM64"
    else
        if ! build_binary; then
            exit 1
        fi
    fi

    echo -n "启动 $APP_NAME ... "
    PORT="$PORT" nohup "$APP_BIN" >> "$LOG_FILE" 2>&1 &
    local pid=$!
    echo "$pid" > "$PID_FILE"

    sleep 1
    if is_running; then
        echo -e "${Green}[✓] 已启动 (PID: $pid)${NC}"
        echo -e "  访问地址: http://$(hostname -I 2>/dev/null | awk '{print $1}' || echo 'localhost'):$PORT"
        echo -e "  日志文件: $LOG_FILE"
    else
        echo -e "${Red}[✗] 启动失败，请检查日志: $LOG_FILE${NC}"
        rm -f "$PID_FILE"
        tail -5 "$LOG_FILE"
        return 1
    fi
}

do_stop() {
    if ! is_running; then
        echo -e "${Yellow}[!] $APP_NAME 未在运行${NC}"
        return 0
    fi

    local pid
    pid=$(cat "$PID_FILE")
    echo -n "停止 $APP_NAME (PID: $pid) ... "
    kill "$pid"

    local i=0
    while kill -0 "$pid" 2>/dev/null && [ $i -lt 10 ]; do
        sleep 1
        i=$((i + 1))
    done

    if kill -0 "$pid" 2>/dev/null; then
        kill -9 "$pid"
        sleep 1
    fi

    rm -f "$PID_FILE"
    echo -e "${Green}[✓] 已停止${NC}"
}

case "${1:-}" in
    start)   do_start ;;
    stop)    do_stop ;;
    restart) do_stop; sleep 1; do_start ;;
    status)
        if is_running; then
            echo -e "${Green}[✓] $APP_NAME 运行中 (PID: $(cat "$PID_FILE"))${NC}"
        else
            echo -e "${Red}[✗] $APP_NAME 未运行${NC}"
        fi
        ;;
    log)
        if [ -f "$LOG_FILE" ]; then
            tail -f "$LOG_FILE"
        else
            echo "日志文件不存在"
        fi
        ;;
    *)  usage ;;
esac
