#!/usr/bin/env bash
set -Eeuo pipefail

# One-command installer for Xboard-Node binaries published in a pinned GitHub Release.
# Supports Linux x86_64/amd64 and aarch64/arm64.
# To publish a newer node build, update RELEASE_BASE and the four SHA256 values below.
# Keep this script's raw.githubusercontent.com URL stable; users should not need a version query.

MODE=""
PANEL_URL=""
TOKEN=""
NODE_ID=""
KERNEL="xray"

RELEASE_BASE="https://github.com/xiaobaifafa/Xboard-Node-Install/releases/download/install"

BIN="/usr/local/bin/xboard-node"
XBCTL="/usr/local/bin/xbctl"
CONF="/etc/xboard-node/config.yml"
ENV_FILE="/etc/xboard-node/node.env"
SERVICE_FILE="/etc/systemd/system/xboard-node.service"

fail() { echo "[ERROR] $*" >&2; exit 1; }
log() { echo "[STEP] $*"; }

while [[ $# -gt 0 ]]; do
  case "$1" in
    --mode)     [[ $# -ge 2 ]] || fail "--mode 缺少参数"; MODE="$2"; shift 2 ;;
    --panel)    [[ $# -ge 2 ]] || fail "--panel 缺少参数"; PANEL_URL="$2"; shift 2 ;;
    --token)    [[ $# -ge 2 ]] || fail "--token 缺少参数"; TOKEN="$2"; shift 2 ;;
    --node-id)  [[ $# -ge 2 ]] || fail "--node-id 缺少参数"; NODE_ID="$2"; shift 2 ;;
    --kernel)   [[ $# -ge 2 ]] || fail "--kernel 缺少参数"; KERNEL="$2"; shift 2 ;;
    -h|--help)
      cat <<'HELP'
用法:
  bash install-test-node.sh --mode node --panel PANEL_URL --token TOKEN --node-id NODE_ID [--kernel xray|singbox]

参数:
  --mode       当前只支持 node
  --panel      Xboard 面板根地址，例如 https://panel.example.com/
  --token      面板中该节点对应的通信 Token
  --node-id    面板中的节点 ID
  --kernel     xray（默认）或 singbox
HELP
      exit 0
      ;;
    *) fail "未知参数: $1" ;;
  esac
done

[[ $EUID -eq 0 ]] || fail "请使用 root 或 sudo 执行"
[[ "$MODE" == "node" ]] || fail "当前脚本只支持 --mode node"
[[ -n "$PANEL_URL" && -n "$TOKEN" && -n "$NODE_ID" ]] || fail "缺少参数：--mode node --panel URL --token TOKEN --node-id ID"
[[ "$PANEL_URL" =~ ^https?://[^/]+/?$ ]] || fail "--panel 请填写面板根地址，例如 https://panel.example.com/"
[[ "$NODE_ID" =~ ^[0-9]+$ ]] || fail "--node-id 必须是数字"
[[ "$KERNEL" == "xray" || "$KERNEL" == "singbox" ]] || fail "--kernel 只能是 xray 或 singbox"
[[ "$TOKEN" != *$'\n'* && "$TOKEN" != *$'\r'* ]] || fail "Token 不能包含换行符"

command -v systemctl >/dev/null 2>&1 || fail "系统不支持 systemd"
command -v curl >/dev/null 2>&1 || fail "缺少 curl，请先安装 curl"
command -v sha256sum >/dev/null 2>&1 || fail "缺少 sha256sum"

case "$(uname -m)" in
  x86_64|amd64) ARCH="amd64" ;;
  aarch64|arm64) ARCH="arm64" ;;
  *) fail "暂不支持当前 CPU 架构：$(uname -m)，仅支持 amd64 和 arm64" ;;
esac

case "$ARCH" in
  amd64)
    NODE_SHA256="f72c5ea3878f5d61b46e492d2a088f0da50c16f922abd883f178ef6c1694fc64"
    XBCTL_SHA256="db49ff8cbba2d3a7fa488f6e3ae6657e05efb929195190511e4a16531cf73a26"
    ;;
  arm64)
    NODE_SHA256="55f5dfcea8bfac6ee139b1907af7c059add4da1eb442d4f957757b4df17abd0a"
    XBCTL_SHA256="f50b7e5d9dfb951fad3a79fc70c687a18c501c81602b5d19c0519de1eb34f471"
    ;;
esac

TMP_DIR="$(mktemp -d)"
cleanup() { rm -rf "$TMP_DIR"; }
trap cleanup EXIT

log "检测到 CPU 架构：$ARCH"
log "从 GitHub Release install 下载程序"
curl -fL --retry 5 --retry-all-errors --retry-delay 2 \
  "$RELEASE_BASE/xboard-node-linux-$ARCH" -o "$TMP_DIR/xboard-node"
curl -fL --retry 5 --retry-all-errors --retry-delay 2 \
  "$RELEASE_BASE/xbctl-linux-$ARCH" -o "$TMP_DIR/xbctl"

log "校验下载文件 SHA256"
echo "$NODE_SHA256  $TMP_DIR/xboard-node" | sha256sum -c -
echo "$XBCTL_SHA256  $TMP_DIR/xbctl" | sha256sum -c -

log "安装二进制文件"
install -m 0755 "$TMP_DIR/xboard-node" "$BIN"
install -m 0755 "$TMP_DIR/xbctl" "$XBCTL"

mkdir -p /etc/xboard-node

log "生成节点配置"
"$XBCTL" config init \
  --mode node \
  --panel-url "$PANEL_URL" \
  --token "$TOKEN" \
  --node-id "$NODE_ID" \
  --output "$CONF"

INSTANCE_ID="$(awk '/^[[:space:]]*-[[:space:]]*id:/ {print $3; exit}' "$CONF")"
ENV_KEY="$(awk '/token_env:/ {print $2; exit}' "$CONF")"
[[ -n "$INSTANCE_ID" && -n "$ENV_KEY" ]] || fail "读取生成配置失败，请检查 xbctl config init 输出"
[[ "$ENV_KEY" =~ ^[A-Za-z_][A-Za-z0-9_]*$ ]] || fail "生成的 Token 环境变量名称不合法：$ENV_KEY"

sed -i -E "s/^([[:space:]]*type:).*/\1 \"$KERNEL\"/" "$CONF"
mkdir -p "/etc/xboard-node/instances/$INSTANCE_ID"

# Store the panel token in a root-readable environment file, not in the YAML.
umask 077
printf '%s=%s\n' "$ENV_KEY" "$TOKEN" > "$ENV_FILE"
chmod 600 "$ENV_FILE"

log "配置 systemd 服务"
systemctl stop xboard-node.service >/dev/null 2>&1 || true
cat > "$SERVICE_FILE" <<'UNIT'
[Unit]
Description=Xboard Node
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=root
WorkingDirectory=/etc/xboard-node
EnvironmentFile=/etc/xboard-node/node.env
ExecStart=/usr/local/bin/xboard-node -c /etc/xboard-node/config.yml
Restart=always
RestartSec=5
LimitNOFILE=1048576

[Install]
WantedBy=multi-user.target
UNIT

systemctl daemon-reload
systemctl enable xboard-node.service >/dev/null
systemctl reset-failed xboard-node.service >/dev/null 2>&1 || true
systemctl restart xboard-node.service
sleep 5

echo
echo "===== 服务状态 ====="
systemctl status xboard-node.service --no-pager -l || true
echo
echo "===== 最近 60 行日志 ====="
journalctl -u xboard-node.service -n 60 --no-pager || true

if ! systemctl is-active --quiet xboard-node.service; then
  fail "节点服务未保持运行，请根据上方日志排查"
fi

echo
echo "[OK] systemd 服务正在运行。"
echo "请确认日志中出现 ws connected、内核启动成功和节点配置更新等信息。"
echo "常用命令：systemctl status xboard-node；journalctl -u xboard-node -f；xbctl status"
