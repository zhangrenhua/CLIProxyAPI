#!/bin/bash
#
# CLIProxyAPI 启动脚本（同时支持 macOS 与 Linux，参考 claude_start.sh）
#
# 进程匹配的跨平台处理：
#   - Linux : 用 pidof（按 exe basename 完整匹配，不会像 pgrep -x 那样把 comm 截断到 15 字符，
#             也不会误命中命令行里含二进制名的 watchdog bash）
#   - macOS : 没有 pidof，用 pgrep -x 按进程名精确匹配（同样不会误伤 watchdog bash）
#
# 停止全部（先杀 watchdog，否则它会在 10s 内把 app 重新拉起来）：
#   pkill -9 -f cli-proxy-watchdog
#   Linux : kill "$(pidof CLIProxyAPI-linux-amd64)"
#   macOS : pkill -x cli-proxy-api

set -u

OS="$(uname -s)"

# 可执行文件名：按平台选择，可用环境变量 CPA_BIN 覆盖（自定义构建名时）
if [ -n "${CPA_BIN:-}" ]; then
    BIN="$CPA_BIN"
elif [ "$OS" = "Linux" ]; then
    BIN="CLIProxyAPI-linux-amd64"     # 仓库预置的 linux 二进制；自建请用 CPA_BIN 覆盖
else
    BIN="cli-proxy-api"               # 本机构建的 macOS 二进制
fi

CONFIG="config.yaml"                  # 配置文件（与二进制同目录）
LOG="cli-proxy-api.log"               # 运行日志（追加写入，保留崩溃/重启历史）
WATCHDOG_TAG="cli-proxy-watchdog"     # watchdog 命令行里的标记，便于精确 pkill

# 切到脚本所在目录（防止不同 cwd 启动时 config.yaml / 日志路径漂移）
cd "$(dirname "$0")" || exit 1

# 返回 app 的 pid 列表（每行一个），没有则为空
app_pids() {
    if [ "$OS" = "Linux" ]; then
        pidof "$BIN" 2>/dev/null | tr ' ' '\n'
    else
        pgrep -x "$BIN" 2>/dev/null
    fi
}

# 给 app 发信号（$1 = TERM / KILL），跨平台
app_signal() {
    local pids
    pids="$(app_pids)"
    [ -n "$pids" ] && echo "$pids" | xargs kill -s "$1" 2>/dev/null
}

# 停止已有进程
# app 用 SIGTERM 走 graceful shutdown（main.go 里有 30s 优雅关闭，保住在飞请求 / 日志缓冲）
# watchdog 用 SIGKILL 直接干掉，bash sleeper 没什么需要清理的
app_signal TERM
pkill -9 -f "$WATCHDOG_TAG" 2>/dev/null

# 等旧 app 真正退出（优雅关闭最多 30s），最多等 35s
# 不等的话 sleep 1 后旧实例可能还在占端口，新实例启动失败
for _ in $(seq 1 35); do
    [ -z "$(app_pids)" ] && break
    sleep 1
done

# 兜底：35s 还没退出说明 graceful shutdown 卡死了，强杀避免新实例端口冲突
if [ -n "$(app_pids)" ]; then
    app_signal KILL
    sleep 1
fi

chmod +x "$BIN"

# 启动主进程
echo "===== $(date '+%Y-%m-%d %H:%M:%S') start =====" >> "$LOG"
nohup ./"$BIN" --config "$CONFIG" >> "$LOG" 2>&1 &
echo "$BIN 已启动，pid: $!"

# 管理面板地址提示（端口取自 config.yaml，缺省 3001）
PORT="$(grep -E '^port:' "$CONFIG" 2>/dev/null | head -1 | awk '{print $2}')"
echo "管理面板: http://localhost:${PORT:-3001}/management.html"
echo "查看日志: tail -f $LOG"

# 启动 watchdog：10s 检查一次，app 不在了就重新执行本脚本
# 命令行里带 "$WATCHDOG_TAG" 标记，便于下次启动时 pkill 干掉
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
SCRIPT_NAME="$(basename "$0")"
SCRIPT_PATH="$SCRIPT_DIR/$SCRIPT_NAME"
nohup bash -c "
    # $WATCHDOG_TAG
    cd '$SCRIPT_DIR'
    OS='$OS'
    BIN='$BIN'
    # 与主脚本同样的跨平台进程判定：Linux 用 pidof，macOS 用 pgrep -x
    running() {
        if [ \"\$OS\" = Linux ]; then pidof \"\$BIN\" > /dev/null 2>&1
        else pgrep -x \"\$BIN\" > /dev/null 2>&1; fi
    }
    while true; do
        sleep 10
        if ! running; then
            echo \"[\$(date '+%Y-%m-%d %H:%M:%S')] \$BIN 已停止，重新执行 $SCRIPT_NAME\" >> cli-proxy-watchdog.log
            exec bash '$SCRIPT_PATH'
        fi
    done
" > /dev/null 2>&1 &
echo "守护进程 (watchdog) 已启动，pid: $!"
