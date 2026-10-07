#!/usr/bin/env bash
#
# 把"Windows 在 WSL 眼里的地址"写进 Prometheus 的 file_sd 目标文件。
#
# 什么时候需要跑：
#   - 第一次配置（现在）
#   - 每次重启 WSL 之后（地址可能变）
#   - Prometheus 的 Targets 页面显示 deeptalk = DOWN 的时候
#
# 为什么要有这个脚本：写死地址会在某天静默失效 —— Prometheus 显示 DOWN，
# 你会以为是后端挂了，其实是 WSL 换了网段。让脚本去问操作系统，
# 比让人去记一个 172.x.x.x 可靠。
#
# 用法（在 WSL 里，项目根目录）：
#   bash deploy/observability/refresh-target.sh
#
# 想让它在每次开终端时自动跑，把这行加到 ~/.bashrc：
#   [ -f ~/DeepTalk/deploy/observability/refresh-target.sh ] && \
#     bash ~/DeepTalk/deploy/observability/refresh-target.sh --quiet

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
TARGET_FILE="$SCRIPT_DIR/targets/deeptalk.yml"
BACKEND_PORT="${DEEPTALK_PORT:-9090}"

QUIET=0
[[ "${1:-}" == "--quiet" ]] && QUIET=1
say() { [[ $QUIET -eq 1 ]] || echo "$@"; }

# ---------- 1. 取 Windows 主机地址 ----------
#
# 在你的拓扑里（后端在 Windows、Docker 在 WSL），这条默认路由的网关
# 就是 Windows。容器发往它的包会经 docker0 → WSL 主机 → Windows 转发出去。
WINDOWS_IP="$(ip route show default | awk '{print $3; exit}')"

if [[ -z "$WINDOWS_IP" ]]; then
  echo "✗ 取不到默认网关，拿不到 Windows 地址" >&2
  exit 1
fi

say "Windows 地址（WSL 视角）: $WINDOWS_IP"

# ---------- 2. 先测通，再写文件 ----------
#
# 顺序很重要：先写文件再测的话，Prometheus 会先看到 DOWN 再变 UP，
# 而 DOWN 那一下最容易让人误判成"配置写错了"。
probe() {
  local url="http://$WINDOWS_IP:$BACKEND_PORT/metrics"
  if command -v curl >/dev/null 2>&1; then
    curl -sf -m 3 -o /dev/null "$url"
  elif command -v wget >/dev/null 2>&1; then
    wget -q -T 3 -O /dev/null "$url"
  else
    # 两个都没有时不能假装成功：返回一个特殊码，让下面给出正确的提示
    return 127
  fi
}

set +e
probe
PROBE=$?
set -e

if [[ $PROBE -eq 0 ]]; then
  say "✓ 后端可达：http://$WINDOWS_IP:$BACKEND_PORT/metrics"
elif [[ $PROBE -eq 127 ]]; then
  say "⚠ 没找到 curl 也没找到 wget，跳过后端可达性检查"
else
  echo "⚠ 连不上 http://$WINDOWS_IP:$BACKEND_PORT/metrics" >&2
  echo "" >&2
  echo "  按可能性排序，逐个排查：" >&2
  echo "  1) 后端没在跑 —— 在 Windows 上确认 go run ./cmd/server 还活着" >&2
  echo "  2) 后端只监听了 127.0.0.1 —— 它必须监听 0.0.0.0（本项目默认就是 0.0.0.0）" >&2
  echo "  3) Windows 防火墙拦了来自 WSL 网段的入站。在**管理员** PowerShell 里执行：" >&2
  echo "       New-NetFirewallRule -DisplayName 'DeepTalk metrics from WSL' \\" >&2
  echo "         -Direction Inbound -LocalPort $BACKEND_PORT -Protocol TCP -Action Allow -Profile Any" >&2
  echo "  4) WSL 换了网段 —— 重跑本脚本即可" >&2
  echo "" >&2
  echo "  仍然写入目标文件（Prometheus 会显示 DOWN，但至少地址是对的）。" >&2
fi

# ---------- 3. 写文件 ----------
#
# 先写临时文件再 mv：Prometheus 每 30 秒读一次这个文件，
# 直接覆盖有可能被它读到写了一半的内容（半截 YAML），
# 那一次抓取就会失败。mv 在同一文件系统内是原子的。
TMP="$(mktemp "$TARGET_FILE.XXXXXX")"
cat > "$TMP" <<EOF
# Prometheus 的 file_sd 目标文件。
#
# 由 refresh-target.sh 生成于 $(date '+%Y-%m-%d %H:%M:%S')，**不要手改**。
# 想看当前生效的地址，直接读这个文件。

- targets: ["$WINDOWS_IP:$BACKEND_PORT"]
  labels:
    service: deeptalk-backend
    env: local
    via: wsl-gateway
EOF

# 0644：Prometheus 容器里以 nobody 运行，读不到 0600 的文件。
# 这个坑的症状是 Targets 页面报 "permission denied"，
# 而文件内容看起来完全正确。
chmod 644 "$TMP"
mv "$TMP" "$TARGET_FILE"

say "✓ 已写入 $TARGET_FILE"
say ""
say "Prometheus 会在 30 秒内自动重载，不用重启容器。"
say "查看效果： http://localhost:9091/targets"
