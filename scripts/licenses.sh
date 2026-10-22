#!/usr/bin/env bash
#
# Check the licenses of every third-party package linked into runspace and
# save their texts in build/licenses, for every release target. goreleaser
# runs this before building; it fails the release on a forbidden,
# restricted, or unknown license.

set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$root"
out="$root/build/licenses"
bin="$root/build/bin"

GOBIN="$bin" go install github.com/google/go-licenses/v2@v2.0.1

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
rm -rf "$out"
mkdir -p "$out"

# The linked set differs by target (mousetrap is linked on Windows only), so
# check each one and save the union. runspace's own licenses ship separately.
for os in linux darwin windows; do
	for arch in amd64 arm64; do
		export GOOS="$os" GOARCH="$arch"
		"$bin/go-licenses" check . \
			--disallowed_types=forbidden,restricted,unknown \
			--ignore go.sandbox.ec/runspace
		"$bin/go-licenses" save . \
			--save_path "$work/$os-$arch" \
			--ignore go.sandbox.ec/runspace
		# go-licenses saves read-only files, which a later copy must
		# overwrite.
		chmod -R u+w "$work/$os-$arch"
		cp -R "$work/$os-$arch/." "$out/"
	done
done
unset GOOS GOARCH
echo "Saved third-party licenses for $(find "$out" -name 'LICENSE*' -o -name 'license*' | wc -l | tr -d ' ') files in $out"
