#!/usr/bin/env bash
# First-time VPS setup for the simcast signalling server. Run as root from a checkout
# of this repo's deploy/ directory: sudo ./deploy/bootstrap.sh
#
# Installs coturn, lays down the systemd units + updater + Caddyfile, creates the
# simcast user and /etc/simcast/signal.env from the template (if absent), pulls the
# first simcast-signal binary, and enables the broker + auto-update timer.
# Idempotent: re-running is safe.
set -euo pipefail

if [ "$(id -u)" -ne 0 ]; then echo "run as root (sudo)"; exit 1; fi
here="$(cd "$(dirname "$0")" && pwd)"

echo "==> installing coturn"
apt-get update -y
apt-get install -y coturn curl

echo "==> creating simcast user + state dir"
id -u simcast >/dev/null 2>&1 || useradd --system --home /var/lib/simcast --shell /usr/sbin/nologin simcast
install -d -o simcast -g simcast -m 0750 /var/lib/simcast

echo "==> installing systemd units + updater"
install -m 0644 "${here}/systemd/simcast-signal.service" /etc/systemd/system/
install -m 0644 "${here}/systemd/simcast-signal-update.service" /etc/systemd/system/
install -m 0644 "${here}/systemd/simcast-signal-update.timer" /etc/systemd/system/
install -m 0755 "${here}/simcast-signal-update.sh" /usr/local/bin/simcast-signal-update.sh

echo "==> env file"
install -d -m 0750 /etc/simcast
if [ ! -f /etc/simcast/signal.env ]; then
  install -m 0600 "${here}/signal.env.example" /etc/simcast/signal.env
  echo "    created /etc/simcast/signal.env from template — EDIT IT before the service is useful"
fi

echo "==> first binary pull"
/usr/local/bin/simcast-signal-update.sh || echo "    (no release yet? re-run after the first GitHub release)"

echo "==> enabling services"
systemctl daemon-reload
systemctl enable --now simcast-signal.service || echo "    broker failed to start — likely needs /etc/simcast/signal.env edited"
systemctl enable --now simcast-signal-update.timer

cat <<'NOTE'

Next steps (manual):
  1. Edit /etc/simcast/signal.env (app secret, domain, --turn-secret).
  2. Set coturn static-auth-secret == --turn-secret in /etc/turnserver.conf, set
     external-ip and realm, then: systemctl enable --now coturn
  3. Install Caddy and point deploy/Caddyfile at your domain, then reload Caddy.
  4. systemctl restart simcast-signal
NOTE
