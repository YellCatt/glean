#!/bin/sh

# ============ 配置区 ============
PLUGIN_DIR="/plugins/data/glean"
BINARY_NAME="glean"
TMP_NAME="glean.tmp"
LOG_FILE="$PLUGIN_DIR/glean.log"
PID_FILE="$PLUGIN_DIR/glean.pid"
DOWNLOAD_URL="https://github.com/YellCatt/glean/releases/download/dev-latest/default.glean_linux_mipsle"

MAX_RETRY=20
RESTART_DELAY=5
MAX_RESTART_DELAY=300
UPDATE_INTERVAL=14400         # 14400 秒 = 4 小时
GRACEFUL_SHUTDOWN_TIMEOUT=10

# 下载超时配置
CONNECT_TIMEOUT=120
MAX_DOWNLOAD_TIME=1200

# 预创建目录，确保早期日志能写入
mkdir -p "$PLUGIN_DIR" 2>/dev/null

# ============ 全局状态 ============
CHILD_PID=""
NEED_UPDATE=0
RUNNING=1
CURRENT_DELAY=$RESTART_DELAY

# ============ 日志函数 ============
log() {
    echo "[$(date '+%Y-%m-%d %H:%M:%S')] $1" >> "$LOG_FILE"
}

log_info()  { log "【信息】$1"; }
log_ok()    { log "【成功】✓ $1"; }
log_warn()  { log "【警告】⚠ $1"; }
log_error() { log "【错误】✗ $1"; }
log_step()  { log "【步骤】$1"; }

# ============ 清理函数 ============
cleanup() {
    log_info "收到退出信号，开始清理..."
    RUNNING=0

    if [ -n "$CHILD_PID" ]; then
        kill "$CHILD_PID" 2>/dev/null
        sleep 1
        kill -9 "$CHILD_PID" 2>/dev/null
        wait "$CHILD_PID" 2>/dev/null
    fi

    [ -f "$PID_FILE" ] && rm -f "$PID_FILE"
    [ -f "$PLUGIN_DIR/$TMP_NAME" ] && rm -f "$PLUGIN_DIR/$TMP_NAME"
    log_ok "清理完成，脚本退出"
    exit 0
}

trap 'cleanup' INT TERM

# ============ 启动前强制清理残留进程 ============
killall -9 glean 2>/dev/null
rm -f "$PID_FILE"
log_info "已清理可能残留的 glean 进程和 PID 文件"

# ============ 防重复启动 ============
log "========================================"
log_info "glean 守护脚本启动"
log_info "当前工作目录: $(pwd)"
log_info "插件目录: $PLUGIN_DIR"
log_info "下载地址: $DOWNLOAD_URL"
log_info "最大下载重试: $MAX_RETRY 次"
log_info "更新检查间隔: ${UPDATE_INTERVAL} 秒"
log_info "连接超时: ${CONNECT_TIMEOUT} 秒"
log_info "单次下载最大耗时: ${MAX_DOWNLOAD_TIME} 秒"

if [ -f "$PID_FILE" ]; then
    OLD_PID=$(cat "$PID_FILE" 2>/dev/null)
    if [ -n "$OLD_PID" ] && kill -0 "$OLD_PID" 2>/dev/null; then
        log_error "检测到已有实例在运行 (PID: $OLD_PID)，请勿重复启动"
        exit 1
    else
        log_warn "发现残留 PID 文件，但对应进程已不存在，继续启动"
        rm -f "$PID_FILE"
    fi
fi

echo $$ > "$PID_FILE"
log_ok "PID 文件已写入: $PID_FILE (当前 PID: $$)"

# ============ 等待网络就绪 ============
# glean 需要请求 https://api.insigmind.com，必须等网络与 DNS 可用后再启动
log_step "等待网络就绪..."
NETWORK_WAIT=0
while true; do
    if ping -c 1 -W 3 8.8.8.8 > /dev/null 2>&1; then
        log_ok "网络已就绪 (累计等待 ${NETWORK_WAIT} 秒)"
        break
    fi

    NETWORK_WAIT=$((NETWORK_WAIT + 5))
    if [ $((NETWORK_WAIT % 15)) -eq 0 ]; then
        log_warn "网络未就绪，已等待 ${NETWORK_WAIT} 秒，继续等待..."
    fi
    sleep 5
done
log "========================================"

# ============ 检查并创建插件目录 ============
log_step "检查插件目录..."
if [ ! -d "$PLUGIN_DIR" ]; then
    log_info "目录不存在，正在创建: $PLUGIN_DIR"
    if mkdir -p "$PLUGIN_DIR"; then
        log_ok "目录创建成功"
    else
        log_error "目录创建失败，退出"
        exit 1
    fi
else
    log_ok "目录已存在: $PLUGIN_DIR"
fi

# ============ 进入插件目录 ============
# 必须 cd，glean 会把 stats_history.json / stats_log.csv / reports / logs 写在当前工作目录
cd "$PLUGIN_DIR" || {
    log_error "进入目录失败: $PLUGIN_DIR"
    exit 1
}

# ============ 下载函数 ============
download_binary() {
    log_step "尝试从 GitHub 下载最新版本..."

    [ -f "$TMP_NAME" ] && rm -f "$TMP_NAME"

    retry=0
    while [ "$retry" -lt "$MAX_RETRY" ]; do
        retry=$((retry + 1))
        log_info "第 $retry / $MAX_RETRY 次下载尝试 (连接超时 ${CONNECT_TIMEOUT}s, 最大耗时 ${MAX_DOWNLOAD_TIME}s)..."

        curl -L -k --connect-timeout "$CONNECT_TIMEOUT" --max-time "$MAX_DOWNLOAD_TIME" -o "$TMP_NAME" "$DOWNLOAD_URL"
        curl_exit=$?

        if [ "$curl_exit" -eq 0 ] && [ -f "$TMP_NAME" ] && [ -s "$TMP_NAME" ]; then
            size=$(ls -lh "$TMP_NAME" | awk '{print $5}')
            chmod +x "$TMP_NAME"
            log_ok "下载成功，文件大小: $size，已添加执行权限"
            return 0
        else
            log_error "下载失败 (curl 退出码: $curl_exit)"
            [ -f "$TMP_NAME" ] && rm -f "$TMP_NAME"

            if [ "$retry" -lt "$MAX_RETRY" ]; then
                log_info "等待 10 秒后重试..."
                sleep 10
            fi
        fi
    done

    log_error "已达到最大重试次数 ($MAX_RETRY)，下载失败"
    return 1
}

# ============ 程序控制函数 ============
start_program() {
    if [ ! -f "$BINARY_NAME" ]; then
        log_error "二进制文件不存在，无法启动"
        return 1
    fi

    "./$BINARY_NAME" >> "$LOG_FILE" 2>&1 &
    CHILD_PID=$!
    log_ok "程序已启动 (PID: $CHILD_PID)"
    return 0
}

stop_program() {
    if [ -z "$CHILD_PID" ] || [ ! -d "/proc/$CHILD_PID" ]; then
        CHILD_PID=""
        return 0
    fi

    log_info "正在停止程序 (PID: $CHILD_PID)..."
    kill "$CHILD_PID" 2>/dev/null

    count=0
    while [ -d "/proc/$CHILD_PID" ] && [ "$count" -lt "$GRACEFUL_SHUTDOWN_TIMEOUT" ]; do
        sleep 1
        count=$((count + 1))
    done

    if [ -d "/proc/$CHILD_PID" ]; then
        log_warn "程序未在 ${GRACEFUL_SHUTDOWN_TIMEOUT} 秒内退出，强制终止"
        kill -9 "$CHILD_PID" 2>/dev/null
        sleep 1
    fi

    wait "$CHILD_PID" 2>/dev/null
    stop_exit=$?
    CHILD_PID=""
    return $stop_exit
}

# ============ 将秒数转换为人类可读的时长 ============
format_duration() {
    local total=$1
    local days=$((total / 86400))
    local hours=$(((total % 86400) / 3600))
    local mins=$(((total % 3600) / 60))
    local secs=$((total % 60))
    local result=""

    [ $days -gt 0 ] && result="${result}${days}天"
    [ $hours -gt 0 ] && result="${result}${hours}小时"
    [ $mins -gt 0 ] && result="${result}${mins}分"
    [ $secs -gt 0 ] || [ -z "$result" ] && result="${result}${secs}秒"

    echo "$result"
}

# ============ 更新检查函数（定时直接下载，不调用 GitHub API） ============
check_and_update() {
    now=$(date +%s)

    last_check=0
    if [ -f "$PLUGIN_DIR/.last_update_check" ]; then
        last_check=$(cat "$PLUGIN_DIR/.last_update_check" | cut -d'|' -f1)
    fi

    elapsed=$((now - last_check))
    if [ "$elapsed" -lt "$UPDATE_INTERVAL" ]; then
        return 0
    fi

    log_step "距离上次更新已 $(format_duration $elapsed)（${elapsed}秒），开始下载最新版本..."
    echo "$now|$(date '+%Y-%m-%d %H:%M:%S %Z (UTC%z)')" > "$PLUGIN_DIR/.last_update_check"

    if download_binary; then
        # 如果当前有旧版本，比较文件内容是否相同
        if [ -f "$BINARY_NAME" ]; then
            if cmp -s "$TMP_NAME" "$BINARY_NAME"; then
                log_info "下载的文件与当前版本一致，无需替换"
                rm -f "$TMP_NAME"
                return 0
            fi
            log_info "下载的文件与当前版本不同，准备替换"
        else
            log_info "当前无旧版本，直接启用新版本"
        fi

        NEED_UPDATE=1
        return 0
    else
        log_warn "下载失败，继续使用当前版本"
        return 1
    fi
}

# ============ 主守护循环 ============
main_loop() {
    # ========== 启动时：优先启动本地版本，避免下载阻塞 ==========
    if ! start_program; then
        log_warn "本地版本不存在，尝试下载..."
        if download_binary; then
            mv "$TMP_NAME" "$BINARY_NAME"
            if ! start_program; then
                log_error "程序启动失败，守护循环终止"
                return 1
            fi
        else
            log_error "下载失败且无本地版本，无法启动"
            return 1
        fi
    fi

    CURRENT_DELAY=$RESTART_DELAY
    echo "$(date +%s)|$(date '+%Y-%m-%d %H:%M:%S %Z (UTC%z)')" > "$PLUGIN_DIR/.last_update_check"

    # 标记首次检查：程序启动后立即后台下载对比
    FIRST_CHECK=1

    while [ "$RUNNING" -eq 1 ]; do
        if [ -n "$CHILD_PID" ] && [ -d "/proc/$CHILD_PID" ]; then
            # 程序正常运行中 —— 检查更新
            if [ "$FIRST_CHECK" -eq 1 ]; then
                log_step "程序已启动，立即后台检查新版本..."
                FIRST_CHECK=0
                if download_binary; then
                    if [ -f "$BINARY_NAME" ] && cmp -s "$TMP_NAME" "$BINARY_NAME"; then
                        log_info "当前已是最新版本，无需替换"
                        rm -f "$TMP_NAME"
                    else
                        log_ok "发现新版本，准备热更新"
                        NEED_UPDATE=1
                    fi
                else
                    log_warn "启动后更新检查失败，继续使用当前版本"
                fi
            else
                check_and_update
            fi

            if [ "$NEED_UPDATE" -eq 1 ]; then
                log_step "执行热更新..."
                stop_program

                [ -f "$BINARY_NAME" ] && rm -f "$BINARY_NAME"
                mv "$TMP_NAME" "$BINARY_NAME"
                log_ok "已替换为新版本"
                NEED_UPDATE=0

                if ! start_program; then
                    log_error "热更新后启动失败，守护循环终止"
                    break
                fi
                CURRENT_DELAY=$RESTART_DELAY
            else
                sleep 10
            fi
        else
            # 程序已退出（异常或正常）
            if [ -n "$CHILD_PID" ]; then
                wait "$CHILD_PID" 2>/dev/null
                EXIT_CODE=$?

                log "========================================"
                log_info "程序已退出，退出码: $EXIT_CODE"
                if [ "$EXIT_CODE" -eq 0 ]; then
                    log_info "状态: 正常退出"
                elif [ "$EXIT_CODE" -eq 143 ] || [ "$EXIT_CODE" -eq 130 ]; then
                    # 143 = SIGTERM(守护脚本主动停止/热更新)，130 = SIGINT
                    log_info "状态: 被信号终止（守护脚本主动停止或热更新，属正常）"
                else
                    log_error "状态: 异常退出"
                fi
                CHILD_PID=""
            fi

            # 退出后先尝试更新
            check_and_update

            if [ "$NEED_UPDATE" -eq 1 ]; then
                [ -f "$BINARY_NAME" ] && rm -f "$BINARY_NAME"
                mv "$TMP_NAME" "$BINARY_NAME"
                log_ok "已更新到新版本"
                NEED_UPDATE=0
            fi

            # 指数退避重启
            log_info "等待 ${CURRENT_DELAY} 秒后重启..."
            sleep "$CURRENT_DELAY"

            CURRENT_DELAY=$((CURRENT_DELAY * 2))
            if [ "$CURRENT_DELAY" -gt "$MAX_RESTART_DELAY" ]; then
                CURRENT_DELAY=$MAX_RESTART_DELAY
            fi

            if ! start_program; then
                log_error "重启失败，守护循环终止"
                break
            fi
            CURRENT_DELAY=$RESTART_DELAY
        fi
    done
}

# ============ 启动 ============
main_loop
cleanup