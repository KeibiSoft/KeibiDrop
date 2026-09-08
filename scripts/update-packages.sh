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
  for sha in "$SHA_DARWIN_ARM64" "$SHA_DARWIN_AMD64" "$SHA_LINUX_AMD64"; do
    if [ "$sha" = "NOT_FOUND" ] || [ -z "$sha" ]; then
      echo "  ABORT: a checksum is missing from SHA256SUMS; formula left untouched"
      exit 1
    fi
  done
  # Each url line names its platform; the sha256 on the line after it belongs
  # to that url. The old placeholder rewrite only worked on the first fill and
  # left every later release with the previous hashes (v0.4.5, set by hand).
  awk -v arm="$SHA_DARWIN_ARM64" -v amd="$SHA_DARWIN_AMD64" -v lin="$SHA_LINUX_AMD64" -v ver="$VERSION" '
    /^[[:space:]]*version "/ { sub(/version "[^"]*"/, "version \"" ver "\"") }
    /darwin-arm64\.tar\.gz/ { want = arm }
    /darwin-amd64\.tar\.gz/ { want = amd }
    /linux-amd64\.tar\.gz/  { want = lin }
    /^[[:space:]]*sha256 "/ && want != "" { sub(/sha256 "[^"]*"/, "sha256 \"" want "\""); want = "" }
    { print }
  ' "$BREW_FORMULA" > "$BREW_FORMULA.tmp" && mv "$BREW_FORMULA.tmp" "$BREW_FORMULA"
  echo "  Updated $BREW_FORMULA"
  echo "  Remember to: cd ../homebrew-keibidrop && git commit -am 'Update to $VERSION' && git push"
else
  echo "  SKIP: $BREW_FORMULA not found"
fi

# ── Chocolatey ────────────────────────────────────────────
echo "==> Updating Chocolatey package..."
mkdir -p dist/choco/tools
VERSION="$VERSION" envsubst '$VERSION' < choco/keibidrop.nuspec.tmpl > dist/choco/keibidrop.nuspec
TAG="$TAG" SEMVER="$VERSION" SHA256="$SHA_WINDOWS_AMD64" envsubst '$TAG$SEMVER$SHA256' < choco/tools/chocolateyinstall.ps1.tmpl > dist/choco/tools/chocolateyinstall.ps1
cp choco/tools/chocolateyuninstall.ps1 dist/choco/tools/
echo "  Generated dist/choco/"
echo "  To publish: gh workflow run chocolatey.yml -f tag=$TAG   (or: cd dist/choco && choco pack && choco push keibidrop.$VERSION.nupkg --source https://push.chocolatey.org/)"

# ── Snap ──────────────────────────────────────────────────
echo "==> Updating Snap..."
sed -i '' "s/version: '.*'/version: '$VERSION'/" snap/snapcraft.yaml 2>/dev/null || \
sed -i "s/version: '.*'/version: '$VERSION'/" snap/snapcraft.yaml
echo "  Updated snap/snapcraft.yaml"
echo "  To publish: snapcraft && snapcraft upload keibidrop_*.snap --release=edge"

# ── keibidrop.com and README ──────────────────────────────
# The download buttons carry versioned asset URLs (no stable aliases yet), so
# every page with buttons, the README and latest-version.txt move together.
# latest-version.txt drives the in-app update notice: it changes only here,
# after the release assets exist.
SITE="${SITE_DIR:-../../KeibiSoft/keibidrop.com}"
if [ -f "$SITE/latest-version.txt" ]; then
  PREV=$(tr -d '[:space:]' < "$SITE/latest-version.txt")
  echo "==> Updating keibidrop.com and README from $PREV to $VERSION..."
  sedi() { local expr="$1"; shift; sed -i '' "$expr" "$@" 2>/dev/null || sed -i "$expr" "$@"; }
  for f in "$SITE"/index.html "$SITE"/install.html "$SITE"/how-to-use.html "$SITE"/guides/*.html README.md; do
    sedi "s#/v$PREV/#/v$VERSION/#g; s#keibidrop-$PREV-#keibidrop-$VERSION-#g; s#keibidrop_${PREV}_#keibidrop_${VERSION}_#g" "$f"
  done
  sedi "s/\"softwareVersion\": \"$PREV\"/\"softwareVersion\": \"$VERSION\"/" "$SITE/index.html"
  if grep -q "darwin-amd64.dmg" /tmp/SHA256SUMS; then
    sedi "s/darwin-amd64\.tar\.gz/darwin-amd64.dmg/g" "$SITE/install.html"
    echo "  Intel Mac line now points at the DMG"
  fi
  printf '%s\n' "$VERSION" > "$SITE/latest-version.txt"
  echo "  Updated buttons on 7 pages, README, softwareVersion, latest-version.txt"
  echo "  By hand: the $VERSION entry on $SITE/docs/releases.html (and its meta description), then cd $SITE/.. && make push-kd && make indexnow SINCE=$(date +%F)"
else
  echo "  SKIP: site not found at $SITE (set SITE_DIR)"
fi

echo ""
echo "==> Done. Review changes, then commit and publish."
echo "  Still by hand: apt (reprepro), Chocolatey (choco push), MCP registry (server.json version + publish), Homebrew push."
