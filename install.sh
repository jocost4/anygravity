#!/usr/bin/env bash
set -euo pipefail

echo "=========================================================="
echo "  Anygravity - Antigravity to Any AI Agent Pipe Setup"
echo "=========================================================="

REPO_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PORT="${PORT:-20130}"
HOST="${HOST:-127.0.0.1}"
IS_USER_MODE=0

for arg in "$@"; do
    case "$arg" in
        --user)
            IS_USER_MODE=1
            ;;
    esac
done

# Auto-detect rootless mode if not running as root and sudo is absent or avoided
if [ "$IS_USER_MODE" -eq 0 ] && [ "$(id -u)" -ne 0 ] && ! command -v sudo >/dev/null 2>&1; then
    IS_USER_MODE=1
fi

GO_CMD="go"
if ! command -v go >/dev/null 2>&1; then
    if [ -x "$HOME/.local/go/bin/go" ]; then
        GO_CMD="$HOME/.local/go/bin/go"
    elif [ -x "/usr/local/go/bin/go" ]; then
        GO_CMD="/usr/local/go/bin/go"
    else
        echo "[-] Go is not installed. Installing golang-go..."
        if [ "$IS_USER_MODE" -eq 0 ] && command -v apt-get >/dev/null 2>&1; then
            sudo apt-get update -qq && sudo apt-get install -y -qq golang-go
        else
            echo "[!] Please install Go manually: https://go.dev/dl/"
            exit 1
        fi
    fi
fi

echo "[+] Building anygravity static binary..."
cd "$REPO_DIR"
CGO_ENABLED=0 "$GO_CMD" build -ldflags="-s -w" -o anygravity main.go
ln -sf anygravity hermesgravity

if [ "$IS_USER_MODE" -eq 1 ]; then
    echo "[+] Installing in user mode (rootless)..."
    USER_BIN_DIR="$HOME/.local/bin"
    USER_SYSTEMD_DIR="$HOME/.config/systemd/user"
    mkdir -p "$USER_BIN_DIR" "$USER_SYSTEMD_DIR"

    cp -f anygravity "$USER_BIN_DIR/anygravity"
    chmod +x "$USER_BIN_DIR/anygravity"
    ln -sf "$USER_BIN_DIR/anygravity" "$USER_BIN_DIR/hermesgravity"

    cat <<EOF > "$USER_SYSTEMD_DIR/anygravity.service"
[Unit]
Description=Anygravity - Ultra-lightweight Antigravity Provider Pipe for Any AI Agent
After=network.target

[Service]
Type=exec
Environment=PATH=$HOME/.local/bin:/usr/local/bin:/usr/bin:/bin
Environment=PORT=${PORT}
WorkingDirectory=$HOME
ExecStart=$USER_BIN_DIR/anygravity
Restart=always
RestartSec=3
TimeoutStopSec=10
KillSignal=SIGTERM

LimitNOFILE=65536
MemoryMax=64M

[Install]
WantedBy=default.target
EOF

    systemctl --user daemon-reload
    systemctl --user enable anygravity
    systemctl --user restart anygravity
    SYSTEMCTL_CMD="systemctl --user status anygravity"
else
    BIN_PATH="/usr/local/bin/anygravity"
    LEGACY_BIN_PATH="/usr/local/bin/hermesgravity"
    SERVICE_PATH="/etc/systemd/system/anygravity.service"
    TEMPLATE_SERVICE_PATH="/etc/systemd/system/anygravity@.service"

    echo "[+] Installing binary to $BIN_PATH..."
    if systemctl is-active --quiet anygravity 2>/dev/null; then
        sudo systemctl stop anygravity
    fi
    if systemctl is-active --quiet hermesgravity 2>/dev/null; then
        sudo systemctl stop hermesgravity
    fi
    sudo cp -f anygravity "$BIN_PATH"
    sudo chmod +x "$BIN_PATH"
    sudo ln -sf "$BIN_PATH" "$LEGACY_BIN_PATH"

    echo "[+] Detecting install user and home directory..."
    INSTALL_USER="${SUDO_USER:-$(id -un)}"
    INSTALL_HOME=""
    if command -v getent >/dev/null 2>&1; then
        INSTALL_HOME=$(getent passwd "$INSTALL_USER" | cut -d: -f6 || true)
    fi
    if [ -z "$INSTALL_HOME" ] || [ ! -d "$INSTALL_HOME" ]; then
        INSTALL_HOME=$(eval echo "~$INSTALL_USER")
    fi

    echo "[+] Installing systemd service for user '$INSTALL_USER' (home: '$INSTALL_HOME', port: '$PORT')..."
    cat <<EOF | sudo tee "$SERVICE_PATH" > /dev/null
[Unit]
Description=Anygravity - Ultra-lightweight Antigravity Provider Pipe for Any AI Agent
After=network.target

[Service]
Type=exec
User=${INSTALL_USER}
Environment=HOME=${INSTALL_HOME}
Environment=PATH=${INSTALL_HOME}/.local/bin:/usr/local/bin:/usr/bin:/bin
Environment=PORT=${PORT}
WorkingDirectory=${INSTALL_HOME}
ExecStart=${BIN_PATH}
Restart=always
RestartSec=3
TimeoutStopSec=10
KillSignal=SIGTERM

# Hardened sandbox / resource constraints
LimitNOFILE=65536
MemoryMax=64M

[Install]
WantedBy=multi-user.target
EOF

    # Also install multi-user template unit so any user can run: sudo systemctl enable --now anygravity@<username>
    sudo cp -f "$REPO_DIR/systemd/anygravity@.service" "$TEMPLATE_SERVICE_PATH"

    sudo systemctl daemon-reload
    sudo systemctl enable anygravity
    sudo systemctl restart anygravity
    SYSTEMCTL_CMD="sudo systemctl status anygravity"
fi

echo "[+] Verifying health on http://${HOST}:${PORT}/health..."
sleep 1
if curl -sf "http://${HOST}:${PORT}/health" >/dev/null 2>&1; then
    echo "[✓] Anygravity is running on http://${HOST}:${PORT}!"
else
    echo "[!] Warning: Server did not respond immediately. Check: $SYSTEMCTL_CMD"
fi

echo ""
echo "Configuration for Hermes (~/.hermes/config.yaml):"
echo "----------------------------------------------------------"
echo "custom_providers:"
echo "  - name: Anygravity (localhost:${PORT})"
echo "    base_url: http://${HOST}:${PORT}/v1"
echo "    api_key: anygravity-local"
echo "----------------------------------------------------------"
echo "(Models are discovered automatically via GET /v1/models — no manual model list needed!)"
echo "All done!"
