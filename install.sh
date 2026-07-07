#!/usr/bin/env bash
#
# install.sh — install wayland-gestures as a system service.
#
#   sudo ./install.sh              install + enable + start at boot
#   sudo ./install.sh --uninstall  remove everything this script created
#
# What it sets up:
#   1) /usr/local/bin/wayland-gestures            (the binary)
#   2) input access to /dev/input/* and /dev/uinput, WITHOUT exposing your
#      login user to global keylogging/injection:
#        - loads the 'uinput' kernel module at boot
#        - a udev rule giving /dev/uinput to group 'input' (mode 0660)
#        - a dedicated, no-login system user 'wayland-gestures' in the 'input'
#          group; your own user is NOT added to 'input'
#   3) a systemd service that runs the daemon as that user on every boot
#
set -euo pipefail

SVC_USER=wayland-gestures
BIN_DEST=/usr/local/bin/wayland-gestures
UNIT=/etc/systemd/system/wayland-gestures.service
UDEV_RULE=/etc/udev/rules.d/99-wayland-gestures-uinput.rules
MODLOAD=/etc/modules-load.d/wayland-gestures.conf
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

need_root() {
	if [ "$(id -u)" -ne 0 ]; then
		echo "error: run with sudo (needs root to install the service)." >&2
		exit 1
	fi
}

# Create (or reuse) a locked-down system user for the daemon. It has no shell,
# no home and no password — its only privilege is membership in 'input', so
# input access is confined to this single-purpose process instead of every app
# your login user runs.
ensure_service_user() {
	if ! id -u "$SVC_USER" >/dev/null 2>&1; then
		local nologin; nologin="$(command -v nologin || echo /usr/sbin/nologin)"
		useradd --system --no-create-home --shell "$nologin" "$SVC_USER"
	fi
	usermod -aG input "$SVC_USER"
}

uninstall() {
	need_root
	echo "==> stopping and disabling service"
	systemctl disable --now wayland-gestures.service 2>/dev/null || true
	rm -f "$UNIT" "$UDEV_RULE" "$MODLOAD" "$BIN_DEST"
	systemctl daemon-reload
	udevadm control --reload 2>/dev/null || true
	if id -u "$SVC_USER" >/dev/null 2>&1; then
		echo "==> removing system user '$SVC_USER'"
		userdel "$SVC_USER" 2>/dev/null || true
	fi
	echo "==> removed."
}

install() {
	need_root
	echo "==> installing wayland-gestures (service user: $SVC_USER)"

	# 1) binary -------------------------------------------------------------
	if command -v go >/dev/null 2>&1; then
		echo "==> building binary with go"
		( cd "$SCRIPT_DIR" && go build -o "$BIN_DEST" . )
	elif [ -x "$SCRIPT_DIR/wayland-gestures" ]; then
		echo "==> go not found; installing prebuilt binary from repo"
		install -m 0755 "$SCRIPT_DIR/wayland-gestures" "$BIN_DEST"
	else
		echo "error: 'go' is not installed and no prebuilt ./wayland-gestures exists." >&2
		exit 1
	fi
	chmod 0755 "$BIN_DEST"

	# 2) input-group access -------------------------------------------------
	echo "==> ensuring 'uinput' module loads at boot"
	printf 'uinput\n' > "$MODLOAD"
	modprobe uinput || true

	echo "==> udev rule: /dev/uinput -> group 'input', mode 0660"
	printf 'KERNEL=="uinput", GROUP="input", MODE="0660", OPTIONS+="static_node=uinput"\n' > "$UDEV_RULE"
	udevadm control --reload
	udevadm trigger --name-match=uinput || true

	echo "==> creating dedicated service user '$SVC_USER' (no login, in 'input')"
	ensure_service_user

	# 3) systemd service ----------------------------------------------------
	echo "==> writing $UNIT"
	cat > "$UNIT" <<EOF
[Unit]
Description=wayland-gestures (global touchpad/mouse gesture daemon)
After=systemd-user-sessions.service

[Service]
Type=simple
ExecStart=$BIN_DEST
User=$SVC_USER
SupplementaryGroups=input
Restart=always
RestartSec=2
# Hardening: the daemon only needs raw input devices, nothing else.
NoNewPrivileges=true
ProtectSystem=strict
ProtectHome=true
PrivateTmp=true
PrivateNetwork=true
RestrictAddressFamilies=AF_UNIX
ProtectKernelModules=true
ProtectControlGroups=true
RestrictNamespaces=true
SystemCallFilter=@system-service

[Install]
WantedBy=multi-user.target
EOF

	echo "==> enabling + starting service"
	systemctl daemon-reload
	systemctl enable --now wayland-gestures.service

	echo
	echo "Done. The daemon is running and will start on every boot."
	echo "  status: systemctl status wayland-gestures.service"
	echo "  logs:   journalctl -u wayland-gestures.service -f"
	echo "  debug:  edit $UNIT, add -debug to ExecStart, then:"
	echo "          sudo systemctl daemon-reload && sudo systemctl restart wayland-gestures.service"
}

case "${1:-}" in
	--uninstall|-u) uninstall ;;
	"" ) install ;;
	* ) echo "usage: sudo $0 [--uninstall]" >&2; exit 1 ;;
esac
