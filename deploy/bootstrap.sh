#!/usr/bin/env bash
# First-time VPS setup for the simbeam signalling server. Run as root from a checkout
# of this repo's deploy/ directory: sudo ./deploy/bootstrap.sh
#
# Installs coturn, lays down the systemd units + updater + Caddyfile, creates the
# simbeam user and /etc/simbeam/signal.env from the template (if absent), pulls the
# first simbeam-signal binary, and enables the broker + auto-update timer.
# Idempotent: re-running is safe.
set -euo pipefail

if [ "$(id -u)" -ne 0 ]; then echo "run as root (sudo)"; exit 1; fi
here="$(cd "$(dirname "$0")" && pwd)"

echo "==> installing coturn"
apt-get update -y
apt-get install -y coturn curl

echo "==> creating simbeam user + state dir"
id -u simbeam >/dev/null 2>&1 || useradd --system --home /var/lib/simbeam --shell /usr/sbin/nologin simbeam
install -d -o simbeam -g simbeam -m 0750 /var/lib/simbeam

echo "==> installing systemd units + updater"
install -m 0644 "${here}/systemd/simbeam-signal.service" /etc/systemd/system/
install -m 0644 "${here}/systemd/simbeam-signal-update.service" /etc/systemd/system/
install -m 0644 "${here}/systemd/simbeam-signal-update.timer" /etc/systemd/system/
install -m 0755 "${here}/simbeam-signal-update.sh" /usr/local/bin/simbeam-signal-update.sh

echo "==> env file"
install -d -m 0750 /etc/simbeam
if [ ! -f /etc/simbeam/signal.env ]; then
  install -m 0600 "${here}/signal.env.example" /etc/simbeam/signal.env
  echo "    created /etc/simbeam/signal.env from template — EDIT IT before the service is useful"
fi

echo "==> first binary pull"
/usr/local/bin/simbeam-signal-update.sh || echo "    (no release yet? re-run after the first GitHub release)"

echo "==> enabling services"
systemctl daemon-reload
systemctl enable --now simbeam-signal.service || echo "    broker failed to start — likely needs /etc/simbeam/signal.env edited"
systemctl enable --now simbeam-signal-update.timer

cat <<'NOTE'

Next steps (manual):
  1. Edit /etc/simbeam/signal.env (app secret, domain, --turn-secret).
  2. Set coturn static-auth-secret == --turn-secret in /etc/turnserver.conf, set
     external-ip and realm, then: systemctl enable --now coturn
  3. Install Caddy and point deploy/Caddyfile at your domain, then reload Caddy.
  4. systemctl restart simbeam-signal
NOTE
