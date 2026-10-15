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

# A failed stop can leave the daemon and downstream alive. Stop ingress before
# removing protection, including when ambiguous routing state must be retained.
if ! links=$(ip -o link show); then
	echo "cannot inspect downstream: preserving firewall and routing" >&2
	exit 1
fi
if printf '%s\n' "$links" | awk -F': ' '$2 == "lm0" || $2 ~ /^lm0@/ { found=1 } END { exit !found }'; then
	if ! ip link set dev lm0 down; then
		echo "cannot disable downstream lm0: preserving firewall and routing" >&2
		exit 1
	fi
fi

# Lotsman removes its own rules and interfaces on stop; clean up if it crashed.
nft delete table inet lotsman 2>/dev/null || true

# A table is ours when every route in it is a default route over an lm-up-
# interface or it is empty. A qualified probe must match that interface.
owned_table() {
	if ! routes=$(LC_ALL=C ip -4 route show table "$1" 2>&1); then
		# Linux does not create an empty FIB table until it gets a route.
		case "$routes" in
			*"FIB table does not exist."*) return 0 ;;
			*) return 1 ;;
		esac
	fi
	[ -z "$routes" ] && return 0
	printf '%s\n' "$routes" | awk -v dev="$2" '
		NF != 5 || $1 != "default" || $2 != "dev" || $4 != "scope" || $5 != "link" { exit 1 }
		dev == "-" && $3 !~ /^lm-up-/ { exit 1 }
		dev != "-" && $3 != dev { exit 1 }
	'
}

# Delete only rules of exactly the shape Lotsman installs, pointing at an owned
# table in 1000-1099; anything else on priority 3000/4000/5000 is someone else's.
#   3000: from <peer> fwmark <table> lookup <table>   (mark equals the table)
#   4000: from <upstream addr> oif lm-up-<name> lookup <table>
# Legacy source-only probe rules are also recognized.
#   5000: from <peer> lookup <table>
candidates=$(ip -4 rule show | awk '
	{
		line[NR] = $0; priority[NR] = substr($1, 1, length($1)-1); source[NR] = $3
		out[NR] = "-"
		for (i = 4; i < NF; i++) {
			if ($i == "lookup") table[NR] = $(i+1)
			if ($i == "oif") out[NR] = $(i+1)
		}
	}
	$1 ~ /^(4000|5000):$/ && NF == 5 && $2 == "from" && $3 ~ /^[0-9.]+$/ && $4 == "lookup" && $5 ~ /^10[0-9][0-9]$/ {
		candidate[NR] = 1; mark[NR] = "-"
	}
	$1 == "4000:" && $2 == "from" && $3 ~ /^[0-9.]+$/ && $4 == "oif" && $5 ~ /^lm-up-[a-zA-Z0-9_-]+$/ && ((NF == 7 && $6 == "lookup") || (NF == 8 && $6 == "[detached]" && $7 == "lookup")) && $NF ~ /^10[0-9][0-9]$/ {
		candidate[NR] = 1; mark[NR] = "-"
	}
	$1 == "3000:" && NF == 7 && $2 == "from" && $3 ~ /^[0-9.]+$/ && $4 == "fwmark" && $6 == "lookup" && $7 ~ /^10[0-9][0-9]$/ && $5 == sprintf("0x%x", $7) {
		candidate[NR] = 1; mark[NR] = $5
	}
	END {
		for (n = 1; n <= NR; n++) if (candidate[n]) {
			ambiguous = 0
			if (priority[n] == 4000) for (m = 1; m <= NR; m++) {
				if (priority[m] == priority[n] && source[m] == source[n] && table[m] == table[n] && (out[n] == "-" || out[m] == out[n]) && line[m] != line[n]) ambiguous = 1
			}
			print priority[n], source[n], table[n], mark[n], out[n], ambiguous
		}
	}
')
cleanup_incomplete=0
while read -r prio src table mark oif ambiguous; do
	[ -n "$prio" ] || continue
	owned_table "$table" "$oif" || continue
	if [ "$ambiguous" = 1 ]; then
		echo "ambiguous probe rule in table $table: manual cleanup required" >&2
		cleanup_incomplete=1
		continue
	fi
	if [ "$oif" != - ]; then
		while ip rule del priority "$prio" from "$src" oif "$oif" lookup "$table" 2>/dev/null; do :; done
	elif [ "$mark" = - ]; then
		while ip rule del priority "$prio" from "$src" lookup "$table" 2>/dev/null; do :; done
	else
		while ip rule del priority "$prio" from "$src" fwmark "$mark" lookup "$table" 2>/dev/null; do :; done
	fi
done <<EOF
$candidates
EOF

# Keep route ownership evidence until rules are checked. Preserve interfaces and
# their routes when an ambiguous legacy deletion needs an operator decision.
if [ "$cleanup_incomplete" = 0 ]; then
	for i in $(ip -o link show 2>/dev/null | awk -F': ' '/: lm(0|-up-)/{print $2}'); do
		ip link del "$i" 2>/dev/null || true
	done
fi

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
if [ "$cleanup_incomplete" != 0 ]; then
	echo "lotsman files removed; routing cleanup requires manual intervention" >&2
	exit 1
fi
echo "lotsman removed"
