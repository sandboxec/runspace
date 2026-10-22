#!/bin/sh
#
# Install a prebuilt runspace binary from the GitHub releases on Linux or
# macOS. It downloads the archive for this system, checks it against the
# release's SHA-256 checksums, and installs the binary. On Windows, use
# install.ps1.
#
# Usage: scripts/install.sh [-b <dir>] [<version>]
#
#   curl -fsSL https://get.sandbox.ec/runspace | sh
#   curl -fsSL https://get.sandbox.ec/runspace | sh -s -- -b ~/bin v0.1.1
#
# The version is a release tag such as v0.1.1 and defaults to
# RUNSPACE_VERSION, or else the latest release. The binary goes into dir,
# which defaults to RUNSPACE_INSTALL_DIR, or else ~/.local/bin. The script
# needs curl or wget, tar, and sha256sum or shasum.

set -eu

repo=sandboxec/runspace

usage() {
	echo "usage: install.sh [-b <dir>] [<version>]"
}

die() {
	echo "error: $*" >&2
	exit 1
}

have() {
	command -v "$1" >/dev/null 2>&1
}

# get writes the body of an HTTPS URL to a file, or to stdout for -. An
# optional third argument is the Accept header.
get() {
	if have curl; then
		curl -fsSL --proto '=https' --tlsv1.2 -H "Accept: ${3:-*/*}" -o "$2" "$1"
		return
	fi
	# GNU wget -q prints nothing when it fails, so say why from its exit
	# status. BusyBox wget prints its own errors and exits 1.
	wget -q --header "Accept: ${3:-*/*}" -O "$2" "$1" || {
		s=$?
		case $s in
		4) echo "wget: network failure" >&2 ;;
		5) echo "wget: TLS certificate verification failed" >&2 ;;
		8) echo "wget: the server returned an error" >&2 ;;
		esac
		return "$s"
	}
}

sha256() {
	if have sha256sum; then
		sha256sum "$1" | cut -d ' ' -f 1
	else
		shasum -a 256 "$1" | cut -d ' ' -f 1
	fi
}

# All the work happens in main, which runs only once the whole script has
# arrived, so a download through curl | sh that breaks off runs nothing.
main() {
	version=${RUNSPACE_VERSION:-}
	dir=${RUNSPACE_INSTALL_DIR:-}
	while [ $# -gt 0 ]; do
		case $1 in
		-b)
			[ $# -ge 2 ] || {
				usage >&2
				exit 2
			}
			dir=$2
			shift 2
			;;
		-h | --help)
			usage
			exit 0
			;;
		-*)
			usage >&2
			exit 2
			;;
		*)
			version=$1
			shift
			;;
		esac
	done
	dir=${dir:-$HOME/.local/bin}

	have curl || have wget || die "need curl or wget to download"
	have sha256sum || have shasum || die "need sha256sum or shasum to verify the download"
	# BusyBox wget without ssl_client accepts any certificate and says so.
	# The checksums come over the same connection as the archive, so they
	# cannot make up for that.
	if ! have curl && get "https://github.com/$repo/releases/latest" /dev/null application/json 2>&1 |
		grep -q 'validation not implemented'; then
		die "this wget does not check TLS certificates; install curl"
	fi

	case $(uname -s) in
	Linux) os=linux ;;
	Darwin) os=darwin ;;
	*) die "unsupported system $(uname -s); on Windows, use install.ps1" ;;
	esac
	case $(uname -m) in
	x86_64 | amd64) arch=amd64 ;;
	aarch64 | arm64) arch=arm64 ;;
	*) die "unsupported architecture $(uname -m)" ;;
	esac
	# A shell under Rosetta 2 reports x86_64 on Apple silicon, where the
	# native binary is the better choice.
	if [ "$os" = darwin ] && [ "$arch" = amd64 ] &&
		[ "$(sysctl -n hw.optional.arm64 2>/dev/null)" = 1 ]; then
		arch=arm64
	fi

	# The release page answers with JSON when asked to, which, unlike the
	# REST API, does not use up the rate limit for anonymous callers.
	if [ -z "$version" ]; then
		version=$(get "https://github.com/$repo/releases/latest" - application/json |
			sed -n 's/.*"tag_name":"\([^"]*\)".*/\1/p')
		[ -n "$version" ] || die "cannot look up the latest release"
	fi
	case $version in
	v*) ;;
	*) version=v$version ;;
	esac

	# Archive names carry the version without its v.
	asset=runspace_${version#v}_${os}_${arch}.tar.gz
	sums=runspace_${version#v}_checksums.txt
	base=https://github.com/$repo/releases/download/$version

	mkdir -p "$dir" 2>/dev/null && [ -w "$dir" ] ||
		die "cannot write to $dir; choose another directory with -b"

	work=$(mktemp -d)
	trap 'rm -rf "$work"' EXIT
	trap 'exit 1' HUP INT TERM

	echo "Downloading runspace $version for $os/$arch"
	get "$base/$asset" "$work/$asset" || die "cannot download $base/$asset; is $version a release?"
	get "$base/$sums" "$work/$sums" || die "cannot download $base/$sums"

	want=$(awk -v f="$asset" '$2 == f { print $1 }' "$work/$sums")
	[ -n "$want" ] || die "$sums has no checksum for $asset"
	got=$(sha256 "$work/$asset")
	[ "$got" = "$want" ] || die "checksum mismatch for $asset: got $got, want $want"

	tar -xzf "$work/$asset" -C "$work" runspace

	# Copy next to the target and rename, so the replacement is atomic and
	# works while an older runspace is running. The copy must run before it
	# replaces a binary that does.
	part=$(mktemp "$dir/.runspace.XXXXXX")
	trap 'rm -rf "$work" "$part"' EXIT
	cp "$work/runspace" "$part"
	chmod 0755 "$part"
	installed=$("$part" -V) || die "the $os/$arch binary does not run from $dir"
	mv -f "$part" "$dir/runspace"

	echo "Installed $installed to $dir/runspace"
	case :$PATH: in
	*:"$dir":*) ;;
	*)
		echo "$dir is not in your PATH. Add it in your shell profile:"
		echo "  export PATH=\"$dir:\$PATH\""
		;;
	esac
}

main "$@"
