#!/bin/bash

# 停止已有进程
pkill -f CLIProxyAPI-linux-amd64 2>/dev/null && sleep 1

export DATABASE_DRIVER=sqlite
export DATABASE_PATH=./codex2api.db
export CACHE_DRIVER=memory
export ADMIN_SECRET=jun135420

chmod +x CLIProxyAPI-linux-amd64

nohup ./CLIProxyAPI-linux-amd64 > /dev/null 2>&1 &

echo "CLIProxyAPI-linux-amd64 started, pid: $!"
