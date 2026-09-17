#!/bin/sh
# Removes everything a Lotsman install put on this host and restores the
# sysctls it changed. Reads the manifest written at install time.
set -eu

manifest=/etc/lotsman/INSTALLED
if [ ! -f "$manifest" ]; then
	echo "no $manifest: nothing to uninstall" >&2
	exit 1
fi

systemctl disable --now lotsman 2>/dev/null || true

# Lotsman removes its own rules and interfaces on stop; clean up if it crashed.
nft delete table inet lotsman 2>/dev/null || true
for i in $(ip -o link show 2>/dev/null | awk -F': ' '/: lm(0|-up-)/{print $2}'); do
	ip link del "$i" 2>/dev/null || true
done
ip rule show | awk '/lookup 10[0-9][0-9]/ {print $1}' | tr -d : | while read -r prio; do
	while ip rule del priority "$prio" 2>/dev/null; do :; done
done

# ip_forward as it was before the install; the value is recorded in the manifest.
if ipf=$(sed -n 's/^ip_forward_before=//p' "$manifest") && [ -n "$ipf" ]; then
	sysctl -qw net.ipv4.ip_forward="$ipf"
fi

# Every path the install created, deepest first.
sed -n 's/^path=//p' "$manifest" | awk '{print length, $0}' | sort -rn | cut -d' ' -f2- | while read -r p; do
	rm -rf "$p"
done
systemctl daemon-reload
echo "lotsman removed"
