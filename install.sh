#!/usr/bin/env bash
set -euo pipefail

echo "=========================================================="
echo "  Anygravity - Antigravity to Hermes Agent Pipe Setup"
echo "=========================================================="

REPO_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
BIN_PATH="/usr/local/bin/anygravity"
LEGACY_BIN_PATH="/usr/local/bin/hermesgravity"
SERVICE_PATH="/etc/systemd/system/anygravity.service"
PORT="${PORT:-20130}"
HOST="${HOST:-127.0.0.1}"

GO_CMD="go"
if ! command -v go >/dev/null 2>&1; then
    if [ -x "$HOME/.local/go/bin/go" ]; then
        GO_CMD="$HOME/.local/go/bin/go"
    elif [ -x "/usr/local/go/bin/go" ]; then
        GO_CMD="/usr/local/go/bin/go"
    else
        echo "[-] Go is not installed. Installing golang-go..."
        if command -v apt-get >/dev/null 2>&1; then
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
sed -e "s|^User=.*|User=${INSTALL_USER}|g" \
    -e "s|^Environment=HOME=.*|Environment=HOME=${INSTALL_HOME}|g" \
    -e "s|^Environment=PORT=.*|Environment=PORT=${PORT}|g" \
    -e "s|^WorkingDirectory=.*|WorkingDirectory=${INSTALL_HOME}|g" \
    -e "s|/home/ubuntu|${INSTALL_HOME}|g" \
    "$REPO_DIR/systemd/anygravity.service" | sudo tee "$SERVICE_PATH" > /dev/null

sudo systemctl daemon-reload
sudo systemctl enable anygravity
sudo systemctl restart anygravity

echo "[+] Verifying health on http://${HOST}:${PORT}/health..."
sleep 1
if curl -sf "http://${HOST}:${PORT}/health" >/dev/null 2>&1; then
    echo "[✓] Anygravity is running on http://${HOST}:${PORT}!"
else
    echo "[!] Warning: Server did not respond immediately. Check: journalctl -u anygravity -f"
fi

echo ""
echo "Configuration for Hermes (~/.hermes/config.yaml):"
echo "----------------------------------------------------------"
echo "model:"
echo "  provider: custom"
echo "  default: gemini-3.8-flash-high"
echo "  custom_providers:"
echo "    antigravity:"
echo "      base_url: http://${HOST}:${PORT}/v1"
echo "      api_key: agy-local"
echo "----------------------------------------------------------"
echo "All done!"
