# Code signing policy

KeibiDrop release binaries are signed so you can confirm they come from us and
have not been altered.

## How releases are cut

Releases are built by
[`.github/workflows/release.yml`](.github/workflows/release.yml) when a `v*` tag
is pushed. Only maintainers with push access to this repository can create tags,
so only they can publish a release.

## What is signed

- Windows (`kd.exe`, `keibidrop-cli.exe`, `keibidrop.exe`): signed during the
  release build with the SignPath Foundation certificate. SignPath verifies that
  each request comes from this repository's release workflow (origin
  verification) before signing. The signing key lives in SignPath's HSM; we
  never hold it.
- macOS (`.dmg` and the binaries inside it): signed with our Apple Developer ID
  certificate and notarized by Apple.
- Linux (`.deb`, `.tar.gz`): not code-signed. Verify downloads against the
  `SHA256SUMS` published with each release.

Free code signing provided by [SignPath.io](https://signpath.io), certificate by
[SignPath Foundation](https://signpath.org).
