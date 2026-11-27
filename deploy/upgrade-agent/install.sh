#!/usr/bin/env bash
# Install or update the host-side grok2api upgrade agent systemd units.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_DIR="${PROJECT_DIR:-/home/ubuntu/grok2api}"
CONTROL_DIR="${CONTROL_DIR:-$PROJECT_DIR/upgrade-control}"
STATE_DIR="${STATE_DIR:-$PROJECT_DIR/.upgrade}"
IMAGE_REPO="${IMAGE_REPO:-ghcr.io/simooonn/grok2api}"
CONTAINER_UID="${CONTAINER_UID:-10001}"
CONTAINER_GID="${CONTAINER_GID:-10001}"

if [ "$(id -u)" -ne 0 ]; then
  echo "Please run as root (sudo)." >&2
  exit 1
fi

# Stop any legacy HTTP agent process if present.
systemctl stop grok2api-upgrade-agent.service 2>/dev/null || true
systemctl disable grok2api-upgrade-agent.service 2>/dev/null || true
rm -f /etc/grok2api-upgrade-agent.env

mkdir -p "$CONTROL_DIR/inbox" "$CONTROL_DIR/outbox" "$STATE_DIR/agent" "$STATE_DIR/backups"
chown "$CONTAINER_UID:$CONTAINER_GID" "$CONTROL_DIR/inbox"
chmod 0755 "$CONTROL_DIR" "$CONTROL_DIR/inbox" "$CONTROL_DIR/outbox"
chmod 0700 "$STATE_DIR" "$STATE_DIR/agent" "$STATE_DIR/backups"

install -o root -g root -m 0755 "$SCRIPT_DIR/grok2api-upgrade-agent.py" /usr/local/sbin/grok2api-upgrade-agent.py

render_unit() {
  local name="$1"
  sed \
    -e "s|@PROJECT_DIR@|$PROJECT_DIR|g" \
    -e "s|@CONTROL_DIR@|$CONTROL_DIR|g" \
    -e "s|@STATE_DIR@|$STATE_DIR|g" \
    -e "s|@IMAGE_REPO@|$IMAGE_REPO|g" \
    "$SCRIPT_DIR/$name" > "/etc/systemd/system/$name"
  chmod 0644 "/etc/systemd/system/$name"
}

render_unit grok2api-upgrade-agent.path
render_unit grok2api-upgrade-agent.service
render_unit grok2api-upgrade-agent-heartbeat.service
render_unit grok2api-upgrade-agent-heartbeat.timer

systemctl daemon-reload
systemctl enable --now grok2api-upgrade-agent.path
systemctl enable --now grok2api-upgrade-agent-heartbeat.timer
systemctl start grok2api-upgrade-agent-heartbeat.service

echo "Installed grok2api upgrade agent."
echo "  Control dir : $CONTROL_DIR"
echo "  Image repo  : $IMAGE_REPO"
echo "  Heartbeat   : $(cat "$CONTROL_DIR/outbox/agent.json")"
