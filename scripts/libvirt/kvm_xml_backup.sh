#!/usr/bin/env bash
# Arcatum script definition — libvirt/KVM configuration backup.
#
# Dumps everything needed to re-define the guests of a KVM host on a rebuilt machine:
# the persistent XML of every domain (running or not), the virtual networks and storage
# pools those domains reference, and the autostart flags — which live as symlinks under
# /etc/libvirt/qemu/autostart and appear in no XML at all, so a dump of the domains alone
# quietly loses them.
#
# What is NOT here: disk images. This is a configuration backup; the volumes belong in a
# files-backup instance or a storage-level snapshot.
#
# Non-secret parameters arrive as ARCATUM_<PARAM> environment variables
# (docs/script-development.md §3). The archive is written to stdout so nothing stays on
# the hypervisor (capture = "stream").
set -euo pipefail

URI="${ARCATUM_CONNECT_URI:-qemu:///system}"

# bool parameters are stored as the operator typed them — the server only checks that Go's
# strconv.ParseBool accepts them, so "1", "True" and "on" all have to work here too.
is_true() {
	case "${1:-}" in
	1 | [tT] | [tT][rR][uU][eE] | [yY] | [yY][eE][sS] | [oO][nN]) return 0 ;;
	*) return 1 ;;
	esac
}

WANT_NETWORKS=$(is_true "${ARCATUM_INCLUDE_NETWORKS:-true}" && echo 1 || echo 0)
WANT_POOLS=$(is_true "${ARCATUM_INCLUDE_POOLS:-true}" && echo 1 || echo 0)
WANT_SECRETS=$(is_true "${ARCATUM_INCLUDE_SECRETS:-false}" && echo 1 || echo 0)
WANT_NVRAM=$(is_true "${ARCATUM_INCLUDE_NVRAM:-false}" && echo 1 || echo 0)

# --security-info keeps graphics passwords in the XML. Without them the dump restores into
# a guest whose VNC/SPICE console is configured differently from the original.
SEC_FLAG=()
is_true "${ARCATUM_SECURITY_INFO:-true}" && SEC_FLAG=(--security-info)

command -v virsh >/dev/null 2>&1 || {
	echo "virsh not found in PATH (${PATH}) — install libvirt-clients on this host" >&2
	exit 1
}

# The runner is a systemd service with a lean environment; everything below goes through
# this one wrapper so the URI is never forgotten on a call.
v() { virsh --connect "$URI" --quiet "$@"; }

# Fail on a dead or unreachable libvirtd here, with the URI in the message, rather than
# letting every dump below fail one by one with the same error.
if ! HYPERVISOR_HOST=$(v hostname 2>&1); then
	echo "cannot connect to libvirt at ${URI}: ${HYPERVISOR_HOST}" >&2
	echo "(the runner needs root or membership of the libvirt group)" >&2
	exit 1
fi
HYPERVISOR_HOST="${HYPERVISOR_HOST//[$'\r\n']/}"

# Built inside the run's working directory, which the runner deletes after the run — the
# trap is what keeps a by-hand run from leaving it behind. Deliberately no `exec tar` at
# the end: exec would drop this trap.
tmp=$(mktemp -d "${PWD}/kvm-xml.XXXXXX")
trap 'rm -rf "$tmp"' EXIT
mkdir -p "$tmp/domains" "$tmp/host"

manifest="$tmp/MANIFEST.txt"
{
	echo "# arcatum kvm-xml-backup"
	echo "generated_utc=$(date -u +%Y-%m-%dT%H:%M:%SZ)"
	echo "runner_host=$(hostname -f 2>/dev/null || hostname)"
	echo "hypervisor_host=${HYPERVISOR_HOST}"
	echo "connect_uri=${URI}"
	echo "virsh_version=$(virsh --version 2>/dev/null || echo unknown)"
} >"$manifest"

# Host capabilities and node info: not needed to re-define a guest, but they are what you
# read when a restored domain refuses to start on different hardware (CPU model, NUMA).
v capabilities >"$tmp/host/capabilities.xml" 2>/dev/null || echo "warning: could not read host capabilities" >&2
v nodeinfo >"$tmp/host/nodeinfo.txt" 2>/dev/null || true

# Domain names may legally contain characters best kept out of an archive path. The real
# name always stays in MANIFEST.txt; the file name is the sanitised one, deduplicated so
# two names never collapse onto the same file.
declare -A used_names=()
safe_name() {
	local s="${1//[^A-Za-z0-9._-]/_}" candidate n=2
	[ -n "$s" ] || s="unnamed"
	candidate="$s"
	while [ -n "${used_names[$candidate]:-}" ]; do
		candidate="${s}_${n}"
		n=$((n + 1))
	done
	used_names[$candidate]=1
	printf '%s' "$candidate"
}

# Some virsh versions pad the output of `*-list --name` into columns, so a name arrives as
# "default             " and every later --pool/--network/--domain call fails on the trailing
# spaces. A libvirt object name cannot begin or end with whitespace, so trimming is safe.
trim() {
	local s="$1"
	s="${s#"${s%%[![:space:]]*}"}"
	printf '%s' "${s%"${s##*[![:space:]]}"}"
}

# "Autostart:      enable" → "enable"
info_field() {
	awk -F: -v key="$1" '$1 == key { sub(/^[ \t]+/, "", $2); sub(/[ \t]+$/, "", $2); print $2; exit }'
}

failed=0

# ---------------------------------------------------------------- domains
domains=()
while IFS= read -r dom; do
	dom=$(trim "$dom")
	[ -n "$dom" ] && domains+=("$dom")
done < <(v list --all --name)

echo "" >>"$manifest"
echo "# domains: name<TAB>file<TAB>state<TAB>autostart<TAB>persistent" >>"$manifest"

for dom in "${domains[@]:-}"; do
	[ -n "$dom" ] || continue
	file=$(safe_name "$dom")
	xml="$tmp/domains/${file}.xml"

	# --inactive is the persistent configuration: what `virsh define` needs, without the
	# runtime-only additions libvirt makes to a live domain. A transient domain has no
	# such configuration, so fall back to the live XML rather than losing it entirely.
	if ! v dumpxml --domain "$dom" --inactive "${SEC_FLAG[@]}" >"$xml" 2>/dev/null; then
		if v dumpxml --domain "$dom" "${SEC_FLAG[@]}" >"$xml" 2>/dev/null; then
			echo "warning: ${dom}: no persistent config, backed up the live XML" >&2
		else
			echo "error: ${dom}: dumpxml failed" >&2
			rm -f "$xml"
			failed=$((failed + 1))
			continue
		fi
	fi

	dominfo=$(v dominfo --domain "$dom" 2>/dev/null || true)
	state=$(printf '%s\n' "$dominfo" | info_field "State")
	autostart=$(printf '%s\n' "$dominfo" | info_field "Autostart")
	persistent=$(printf '%s\n' "$dominfo" | info_field "Persistent")
	printf '%s\t%s\t%s\t%s\t%s\n' "$dom" "domains/${file}.xml" "${state:-?}" "${autostart:-?}" "${persistent:-?}" >>"$manifest"

	if [ "$WANT_NVRAM" = 1 ]; then
		# libvirt writes <nvram>/path/VARS.fd</nvram> on one line; a UEFI-less guest has
		# no such element and simply yields nothing here.
		nvram_path=$(sed -n 's#.*<nvram[^>]*>\([^<]*\)</nvram>.*#\1#p' "$xml" | head -n1)
		if [ -n "$nvram_path" ] && [ -r "$nvram_path" ]; then
			mkdir -p "$tmp/nvram"
			cp -- "$nvram_path" "$tmp/nvram/${file}.fd"
		elif [ -n "$nvram_path" ]; then
			echo "warning: ${dom}: nvram ${nvram_path} not readable" >&2
		fi
	fi
done

[ "${#domains[@]}" -gt 0 ] || echo "warning: no domains defined at ${URI}" >&2

# ---------------------------------------------------------------- networks
if [ "$WANT_NETWORKS" = 1 ]; then
	mkdir -p "$tmp/networks"
	used_names=()
	echo "" >>"$manifest"
	echo "# networks: name<TAB>file<TAB>autostart" >>"$manifest"
	while IFS= read -r net; do
		net=$(trim "$net")
		[ -n "$net" ] || continue
		file=$(safe_name "$net")
		if ! v net-dumpxml --network "$net" --inactive >"$tmp/networks/${file}.xml" 2>/dev/null; then
			echo "error: network ${net}: net-dumpxml failed" >&2
			rm -f "$tmp/networks/${file}.xml"
			failed=$((failed + 1))
			continue
		fi
		autostart=$(v net-info --network "$net" 2>/dev/null | info_field "Autostart" || true)
		printf '%s\t%s\t%s\n' "$net" "networks/${file}.xml" "${autostart:-?}" >>"$manifest"
	done < <(v net-list --all --name)
fi

# ---------------------------------------------------------------- storage pools
if [ "$WANT_POOLS" = 1 ]; then
	mkdir -p "$tmp/pools"
	used_names=()
	echo "" >>"$manifest"
	echo "# pools: name<TAB>file<TAB>autostart" >>"$manifest"
	while IFS= read -r pool; do
		pool=$(trim "$pool")
		[ -n "$pool" ] || continue
		file=$(safe_name "$pool")
		if ! v pool-dumpxml --pool "$pool" >"$tmp/pools/${file}.xml" 2>/dev/null; then
			echo "error: pool ${pool}: pool-dumpxml failed" >&2
			rm -f "$tmp/pools/${file}.xml"
			failed=$((failed + 1))
			continue
		fi
		autostart=$(v pool-info --pool "$pool" 2>/dev/null | info_field "Autostart" || true)
		printf '%s\t%s\t%s\n' "$pool" "pools/${file}.xml" "${autostart:-?}" >>"$manifest"
	done < <(v pool-list --all --name)
fi

# ---------------------------------------------------------------- secrets (definitions only)
if [ "$WANT_SECRETS" = 1 ]; then
	mkdir -p "$tmp/secrets"
	used_names=()
	echo "" >>"$manifest"
	echo "# secrets (definitions only, no values): uuid<TAB>file" >>"$manifest"
	while IFS= read -r uuid; do
		[ -n "$uuid" ] || continue
		file=$(safe_name "$uuid")
		# secret-dumpxml is the definition (usage type, target). The value stays behind on
		# purpose — it would land in the payload in clear. Restore feeds it back with
		# `virsh secret-set-value`.
		if ! v secret-dumpxml --secret "$uuid" >"$tmp/secrets/${file}.xml" 2>/dev/null; then
			echo "error: secret ${uuid}: secret-dumpxml failed" >&2
			rm -f "$tmp/secrets/${file}.xml"
			failed=$((failed + 1))
			continue
		fi
		printf '%s\t%s\n' "$uuid" "secrets/${file}.xml" >>"$manifest"
		# The grep is not decoration: older virsh prints a table header even for --uuid,
		# so match what looks like a UUID instead of trusting the first column.
	done < <(v secret-list --uuid 2>/dev/null | grep -Eio '[0-9a-f]{8}(-[0-9a-f]{4}){3}-[0-9a-f]{12}')
fi

# ---------------------------------------------------------------- the payload
# A partial configuration backup that reports success is the failure mode worth avoiding:
# it looks fine in the history and is found wanting on the night of the restore.
if [ "$failed" -gt 0 ]; then
	echo "aborting: ${failed} object(s) could not be dumped" >&2
	exit 1
fi

echo "backed up ${#domains[@]} domain(s) from ${HYPERVISOR_HOST}" >&2
tar -czf - -C "$tmp" .
