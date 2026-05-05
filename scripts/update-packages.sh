#!/usr/bin/env bash
# Update package manager formulas/configs after a GitHub release.
# Usage: ./scripts/update-packages.sh v0.2.0-beta.1
#
# Prerequisites:
#   - gh CLI authenticated
#   - SHA256SUMS file in the release
#   - homebrew-keibidrop repo cloned at ../homebrew-keibidrop

set -euo pipefail

TAG="${1:?Usage: $0 <tag> (e.g. v0.2.0-beta.1)}"
VERSION="${TAG#v}"

echo "==> Updating packages for $TAG (version $VERSION)"

# Download SHA256SUMS from the release
SUMS=$(gh release view "$TAG" --json assets -q '.assets[] | select(.name == "SHA256SUMS") | .url' 2>/dev/null || true)
if [ -z "$SUMS" ]; then
  echo "Downloading SHA256SUMS from release..."
  gh release download "$TAG" -p "SHA256SUMS" -D /tmp/
else
  curl -sL "$SUMS" -o /tmp/SHA256SUMS
fi

get_sha() {
  local pattern="$1"
  grep "$pattern" /tmp/SHA256SUMS | awk '{print $1}' || echo "NOT_FOUND"
}

SHA_DARWIN_ARM64=$(get_sha "darwin-arm64.tar.gz")
SHA_DARWIN_AMD64=$(get_sha "darwin-amd64.tar.gz")
SHA_LINUX_AMD64=$(get_sha "linux-amd64.tar.gz")
SHA_WINDOWS_AMD64=$(get_sha "windows-amd64.zip")

echo "  darwin-arm64:  $SHA_DARWIN_ARM64"
echo "  darwin-amd64:  $SHA_DARWIN_AMD64"
echo "  linux-amd64:   $SHA_LINUX_AMD64"
echo "  windows-amd64: $SHA_WINDOWS_AMD64"

# ── Homebrew ──────────────────────────────────────────────
BREW_FORMULA="../homebrew-keibidrop/Formula/keibidrop.rb"
if [ -f "$BREW_FORMULA" ]; then
  echo "==> Updating Homebrew formula..."
  sed -i '' \
    -e "s/version \".*\"/version \"$VERSION\"/" \
    -e "s/PLACEHOLDER_ARM64_SHA256/$SHA_DARWIN_ARM64/" \
    -e "s/PLACEHOLDER_AMD64_SHA256/$SHA_DARWIN_AMD64/" \
    -e "s/PLACEHOLDER_LINUX_AMD64_SHA256/$SHA_LINUX_AMD64/" \
    "$BREW_FORMULA"
  # Also update any previously filled SHA256s
  echo "  Updated $BREW_FORMULA"
  echo "  Remember to: cd ../homebrew-keibidrop && git commit -am 'Update to $VERSION' && git push"
else
  echo "  SKIP: $BREW_FORMULA not found"
fi

# ── Chocolatey ────────────────────────────────────────────
echo "==> Updating Chocolatey package..."
mkdir -p dist/choco/tools
VERSION="$VERSION" envsubst < choco/keibidrop.nuspec.tmpl > dist/choco/keibidrop.nuspec
VERSION="$VERSION" SHA256="$SHA_WINDOWS_AMD64" envsubst < choco/tools/chocolateyinstall.ps1.tmpl > dist/choco/tools/chocolateyinstall.ps1
cp choco/tools/chocolateyuninstall.ps1 dist/choco/tools/
echo "  Generated dist/choco/"
echo "  To publish: cd dist/choco && choco pack && choco push keibidrop.$VERSION.nupkg --source https://push.chocolatey.org/"

# ── Snap ──────────────────────────────────────────────────
echo "==> Updating Snap..."
sed -i '' "s/version: '.*'/version: '$VERSION'/" snap/snapcraft.yaml 2>/dev/null || \
sed -i "s/version: '.*'/version: '$VERSION'/" snap/snapcraft.yaml
echo "  Updated snap/snapcraft.yaml"
echo "  To publish: snapcraft && snapcraft upload keibidrop_*.snap --release=edge"

echo ""
echo "==> Done. Review changes, then commit and publish."
