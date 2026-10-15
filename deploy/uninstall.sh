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

# A table is ours when every route in it is a default route over an lm-up-
# interface (none are left once those are deleted above) or it is empty.
owned_table() {
	routes=$(ip -4 route show table "$1" 2>/dev/null) || return 1
	[ -z "$routes" ] && return 0
	! printf '%s\n' "$routes" | grep -qv '^default dev lm-up-[^ ]* scope link *$'
}

# Delete only rules of exactly the shape Lotsman installs, pointing at an owned
# table in 1000-1099; anything else on priority 3000/4000/5000 is someone else's.
#   3000: from <peer> fwmark <table> lookup <table>   (mark equals the table)
#   4000: from <upstream addr> lookup <table>
#   5000: from <peer> lookup <table>
ip -4 rule show | awk '
	$1 ~ /^(4000|5000):$/ && NF == 5 && $2 == "from" && $3 ~ /^[0-9.]+$/ && $4 == "lookup" && $5 ~ /^10[0-9][0-9]$/ {
		print substr($1, 1, 4), $3, $5, "-"
	}
	$1 == "3000:" && NF == 7 && $2 == "from" && $3 ~ /^[0-9.]+$/ && $4 == "fwmark" && $6 == "lookup" && $7 ~ /^10[0-9][0-9]$/ && $5 == sprintf("0x%x", $7) {
		print 3000, $3, $7, $5
	}
' | while read -r prio src table mark; do
	owned_table "$table" || continue
	if [ "$mark" = - ]; then
		while ip rule del priority "$prio" from "$src" lookup "$table" 2>/dev/null; do :; done
	else
		while ip rule del priority "$prio" from "$src" fwmark "$mark" lookup "$table" 2>/dev/null; do :; done
	fi
done

# ip_forward as it was before the install; the value is recorded in the manifest.
if ipf=$(sed -n 's/^ip_forward_before=//p' "$manifest") && [ -n "$ipf" ]; then
	sysctl -qw net.ipv4.ip_forward="$ipf"
fi

# Every path the install created, deepest first.
sed -n 's/^path=//p' "$manifest" | awk '{print length, $0}' | sort -rn | cut -d' ' -f2- | while read -r p; do
	rm -rf "$p"
done
pkgs=$(sed -n 's/^package=//p' "$manifest" | tr '\n' ' ')
if [ -n "$pkgs" ]; then
	# shellcheck disable=SC2086
	DEBIAN_FRONTEND=noninteractive apt-get purge -y -q $pkgs >/dev/null
fi
systemctl daemon-reload
echo "lotsman removed"
