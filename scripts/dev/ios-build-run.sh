#!/usr/bin/env bash
# Build the iOS app and run it on the simulator.
# Usage: bash scripts/dev/ios-build-run.sh [simulator-name]
# Default simulator: iPhone 15 Pro
set -e

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
PROJECT="$ROOT/ios/KeibiDrop.xcodeproj"
SCHEME="KeibiDrop"
SIM_NAME="${1:-iPhone 15 Pro}"
DERIVED="$ROOT/ios/build"

echo "=== Building xcframework (if needed) ==="
if [ ! -d "$ROOT/KeibiDrop.xcframework" ]; then
    echo "xcframework not found, building..."
    cd "$ROOT" && make build-ios
fi

echo "=== Building iOS app for simulator ==="
xcodebuild \
    -project "$PROJECT" \
    -scheme "$SCHEME" \
    -sdk iphonesimulator \
    -destination "platform=iOS Simulator,name=$SIM_NAME" \
    -derivedDataPath "$DERIVED" \
    build 2>&1 | grep -E "^(Build |error:|warning:.*error|CompileSwift|\*\*)" || true

# Check if build succeeded
APP_PATH=$(find "$DERIVED" -name "KeibiDrop.app" -path "*/Debug-iphonesimulator/*" | head -1)
if [ -z "$APP_PATH" ]; then
    echo "BUILD FAILED - no .app found"
    # Show actual errors
    xcodebuild \
        -project "$PROJECT" \
        -scheme "$SCHEME" \
        -sdk iphonesimulator \
        -destination "platform=iOS Simulator,name=$SIM_NAME" \
        -derivedDataPath "$DERIVED" \
        build 2>&1 | grep "error:" | head -20
    exit 1
fi

echo "=== App built: $APP_PATH ==="

echo "=== Booting simulator: $SIM_NAME ==="
xcrun simctl boot "$SIM_NAME" 2>/dev/null || true

echo "=== Installing app ==="
xcrun simctl install booted "$APP_PATH"

echo "=== Launching app ==="
xcrun simctl launch booted com.keibisoft.keibidrop

echo "=== Done. App running on $SIM_NAME ==="
echo "    Logs: xcrun simctl spawn booted log stream --predicate 'subsystem == \"com.keibisoft.keibidrop\"'"
echo "    Stop: xcrun simctl terminate booted com.keibisoft.keibidrop"
