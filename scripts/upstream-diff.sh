#!/usr/bin/env bash
#
# Compare internal/csconn with the upstream github.com/cli/cli tree it was
# copied from (internal/codespaces at the given tag) and print the differences.
# Every hunk must correspond to an edit logged in internal/csconn/UPSTREAM.md.
#
# Usage: scripts/upstream-diff.sh <tag>      e.g. scripts/upstream-diff.sh v2.100.0
#
# The script only reads the repository. It exits 0 when files differ (the
# output is a report) and non-zero only on errors.

set -euo pipefail

if [[ $# -ne 1 || -z "$1" ]]; then
	echo "usage: $0 <tag>" >&2
	exit 2
fi
tag="$1"

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
ours_dir="$root/internal/csconn"
if [[ ! -d "$ours_dir" ]]; then
	echo "error: $ours_dir not found" >&2
	exit 1
fi

# The tag and commit internal/csconn was copied from (UPSTREAM.md).
pinned_tag=v2.100.0
pinned_commit=45437bc7eeeb3359bbfddd1742f79de7652fd3e2

# Upstream files under internal/codespaces that are deliberately not copied
# (UPSTREAM.md, file map). Anything else that is not copied is reported.
excluded=(
	internal/codespaces/rpc/jupyter/jupyter_server_host_service.v1.pb.go
	internal/codespaces/rpc/jupyter/jupyter_server_host_service.v1.proto
	internal/codespaces/rpc/jupyter/jupyter_server_host_service.v1.proto.mock.go
	internal/codespaces/rpc/jupyter/jupyter_server_host_service.v1_grpc.pb.go
	internal/codespaces/ssh.go
	internal/codespaces/ssh_test.go
	internal/codespaces/states.go
)

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

echo "Fetching github.com/cli/cli $tag ..." >&2
git -c advice.detachedHead=false clone --quiet --depth 1 --branch "$tag" https://github.com/cli/cli "$work/cli"
commit="$(git -C "$work/cli" rev-parse HEAD)"
echo "Upstream commit: $commit"
if [[ "$tag" == "$pinned_tag" && "$commit" != "$pinned_commit" ]]; then
	echo "error: tag $tag is at $commit, but internal/csconn was copied from $pinned_commit" >&2
	exit 1
fi

# Files that exist only in internal/csconn and have no upstream counterpart.
ours_only=(
	UPSTREAM.md
	api/runspace_test.go
	rpc/export_test.go
	rpc/runspace_test.go
)

# upstream_path <path relative to internal/csconn> prints the matching path
# relative to the upstream repository root.
upstream_path() {
	case "$1" in
	LICENSE) echo "LICENSE" ;;
	internal/tunnelstest/tunnels_api_server_mock.go)
		echo "internal/codespaces/connection/tunnels_api_server_mock.go"
		;;
	*) echo "internal/codespaces/$1" ;;
	esac
}

is_ours_only() {
	local f
	for f in "${ours_only[@]}"; do
		[[ "$f" == "$1" ]] && return 0
	done
	return 1
}

differing=()
compared=0
mapped=()

while IFS= read -r rel; do
	if is_ours_only "$rel"; then
		continue
	fi
	up="$(upstream_path "$rel")"
	mapped+=("$up")
	if [[ ! -f "$work/cli/$up" ]]; then
		echo "=== $rel: no upstream file at $up"
		differing+=("$rel")
		continue
	fi
	compared=$((compared + 1))
	rc=0
	out="$(diff -u --label "upstream/$up" --label "ours/$rel" "$work/cli/$up" "$ours_dir/$rel")" || rc=$?
	if [[ $rc -gt 1 ]]; then
		echo "error: diff failed for $rel" >&2
		exit 1
	fi
	if [[ $rc -eq 1 ]]; then
		echo "=== $rel"
		echo "$out"
		differing+=("$rel")
	fi
done < <(cd "$ours_dir" && find . -type f | sed 's#^\./##' | LC_ALL=C sort)

# Upstream files that are neither copied nor deliberately excluded, such as a
# file added at a newer tag or a copied file deleted here.
unaccounted=()
while IFS= read -r up; do
	if ! printf '%s\n' "${mapped[@]}" "${excluded[@]}" | grep -qxF "$up"; then
		unaccounted+=("$up")
	fi
done < <(cd "$work/cli" && find internal/codespaces -type f | LC_ALL=C sort)

echo
echo "Compared $compared files against cli/cli $tag; ${#differing[@]} differ."
if [[ ${#differing[@]} -gt 0 ]]; then
	echo "Differing files:"
	printf '  %s\n' "${differing[@]}"
fi
echo "Files with no upstream counterpart (not compared): ${ours_only[*]}"
if [[ ${#unaccounted[@]} -gt 0 ]]; then
	echo "Upstream files neither copied nor excluded:"
	printf '  %s\n' "${unaccounted[@]}"
fi
