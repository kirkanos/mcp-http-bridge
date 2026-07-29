#!/usr/bin/env bash
# Cross-compiles release archives into dist/ and writes SHA256SUMS.
#
#   ./scripts/build-release.sh v1.2.3
#
# The version is baked into the binary via -ldflags and reported by
# `mcp-http-bridge -version`.
set -euo pipefail

version="${1:-dev}"
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
out="$root/dist"

targets=(
	darwin/arm64
	darwin/amd64
	linux/amd64
	linux/arm64
	windows/amd64
)

rm -rf "$out"
mkdir -p "$out"
cp "$root/LICENSE" "$root/README.md" "$out/"

for target in "${targets[@]}"; do
	goos="${target%%/*}"
	goarch="${target##*/}"
	binary="mcp-http-bridge"
	archive="mcp-http-bridge_${version}_${goos}_${goarch}"
	[ "$goos" = windows ] && binary="$binary.exe"

	echo "building $archive"
	CGO_ENABLED=0 GOOS="$goos" GOARCH="$goarch" go build \
		-trimpath \
		-ldflags "-s -w -X main.version=$version" \
		-o "$out/$binary" \
		"$root"

	if [ "$goos" = windows ]; then
		(cd "$out" && zip -q "$archive.zip" "$binary" LICENSE README.md)
	else
		(cd "$out" && tar -czf "$archive.tar.gz" "$binary" LICENSE README.md)
	fi
	rm "$out/$binary"
done

rm "$out/LICENSE" "$out/README.md"
(cd "$out" && shasum -a 256 ./* >SHA256SUMS)

echo
ls -lh "$out"
