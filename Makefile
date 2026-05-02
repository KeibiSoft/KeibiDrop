COMMIT  := $(shell git rev-parse --short HEAD 2>/dev/null || echo "unknown")
VERSION := $(shell git describe --tags --abbrev=0 2>/dev/null || echo "0.1.0-beta")
LDFLAGS := -X github.com/KeibiSoft/KeibiDrop/pkg/logic/common.Version=$(VERSION) \
           -X github.com/KeibiSoft/KeibiDrop/pkg/logic/common.CommitHash=$(COMMIT)
DIST    := dist
GOARCH  := $(shell go env GOARCH)
GOOS    := $(shell go env GOOS)

# ── Windows: CGO env for WinFSP (cgofuse requires its headers + lib) ──────────
# Uses the 8.3 short path to avoid spaces breaking the linker -L flag.
# LIBCLANG_PATH is needed by bindgen (Rust build.rs) to locate libclang.dll.
ifeq ($(GOOS),windows)
WINFSP_DIR := C:/PROGRA~2/WinFsp
export CPATH := $(WINFSP_DIR)/inc/fuse
export CGO_LDFLAGS := -L$(WINFSP_DIR)/lib
export LIBCLANG_PATH := C:/Program Files/LLVM/bin
EXE := .exe
endif

# ── Build ──────────────────────────────────────────────────

build-cli:
	go build -ldflags="$(LDFLAGS)" -o keibidrop-cli$(EXE) cmd/cli/keibidrop-cli.go

build-kd:
	go build -ldflags="$(LDFLAGS)" -o kd$(EXE) ./cmd/kd

build-static-rust-bridge:
	go build -buildmode=c-archive -o libkeibidrop.a ./rustbridge

build-rust: protoc build-static-rust-bridge
	cd rust && cargo build --release

build-all: build-rust build-cli build-kd

# Cross-compile for macOS arm64 (from Intel) or x86_64 (from arm64)
# Usage: make cross-macos CROSS_ARCH=arm64   (from Intel Mac, target M1/M2)
#        make cross-macos CROSS_ARCH=amd64   (from M1/M2, target Intel)
CROSS_ARCH ?= arm64
CROSS_RUST_TARGET_arm64 := aarch64-apple-darwin
CROSS_RUST_TARGET_amd64 := x86_64-apple-darwin
CROSS_RUST_TARGET := $(CROSS_RUST_TARGET_$(CROSS_ARCH))

cross-macos:
	@echo "Cross-compiling for macOS $(CROSS_ARCH)..."
	# Go static lib for Rust FFI
	CGO_ENABLED=1 GOARCH=$(CROSS_ARCH) go build -buildmode=c-archive -o libkeibidrop.a ./rustbridge
	# Rust UI
	rustup target add $(CROSS_RUST_TARGET) 2>/dev/null || true
	cd rust && cargo build --release --target $(CROSS_RUST_TARGET)
	# Go CLI binaries
	CGO_ENABLED=1 CGO_CFLAGS="-arch $(if $(filter arm64,$(CROSS_ARCH)),arm64,x86_64)" \
	  CGO_LDFLAGS="-arch $(if $(filter arm64,$(CROSS_ARCH)),arm64,x86_64)" \
	  GOARCH=$(CROSS_ARCH) go build -ldflags="$(LDFLAGS)" -o keibidrop-cli-$(CROSS_ARCH) cmd/cli/keibidrop-cli.go
	CGO_ENABLED=1 CGO_CFLAGS="-arch $(if $(filter arm64,$(CROSS_ARCH)),arm64,x86_64)" \
	  CGO_LDFLAGS="-arch $(if $(filter arm64,$(CROSS_ARCH)),arm64,x86_64)" \
	  GOARCH=$(CROSS_ARCH) go build -ldflags="$(LDFLAGS)" -o kd-$(CROSS_ARCH) ./cmd/kd
	@echo "Done. Binaries: keibidrop-cli-$(CROSS_ARCH), kd-$(CROSS_ARCH)"
	@echo "Rust UI: rust/target/$(CROSS_RUST_TARGET)/release/keibidrop-rust"

# Package cross-compiled DMG
cross-macos-dmg: cross-macos $(DIST)
	@echo "Packaging cross-compiled macOS .dmg for $(CROSS_ARCH)..."
	mkdir -p $(DIST)/dmg-cross/KeibiDrop.app/Contents/MacOS $(DIST)/dmg-cross/KeibiDrop.app/Contents/Resources
	cp rust/target/$(CROSS_RUST_TARGET)/release/keibidrop-rust $(DIST)/dmg-cross/KeibiDrop.app/Contents/MacOS/keibidrop
	cp keibidrop-cli-$(CROSS_ARCH) $(DIST)/dmg-cross/KeibiDrop.app/Contents/MacOS/keibidrop-cli
	cp kd-$(CROSS_ARCH) $(DIST)/dmg-cross/KeibiDrop.app/Contents/MacOS/kd
	cp assets/icons/keibidrop.icns $(DIST)/dmg-cross/KeibiDrop.app/Contents/Resources/keibidrop.icns
	sed 's/VERSION_PLACEHOLDER/$(VERSION)/g' assets/Info.plist.tmpl > $(DIST)/dmg-cross/KeibiDrop.app/Contents/Info.plist
	ln -s /Applications $(DIST)/dmg-cross/Applications
	hdiutil create -volname "KeibiDrop $(VERSION)" \
	  -srcfolder $(DIST)/dmg-cross -ov -format UDZO \
	  $(DIST)/keibidrop-$(VERSION)-darwin-$(CROSS_ARCH).dmg
	rm -rf $(DIST)/dmg-cross
	@echo "Created $(DIST)/keibidrop-$(VERSION)-darwin-$(CROSS_ARCH).dmg"

# ── Test & Lint ────────────────────────────────────────────

test:
	go test -v -count=1 -timeout 180s ./tests/...

lint:
	golangci-lint run ./...

sec:
	gosec -exclude-generated -conf .gosec.json ./...

# ── Protobuf ──────────────────────────────────────────────

install-proto:
	go install google.golang.org/protobuf/cmd/protoc-gen-go@latest
	go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@latest

protoc:
	PATH="$$PATH:$$(go env GOPATH)/bin" protoc \
	       --go_opt=module=github.com/KeibiSoft/KeibiDrop \
	       --go-grpc_opt=module=github.com/KeibiSoft/KeibiDrop \
	       --go_out=. --go-grpc_out=. keibidrop.proto

# ── Rust helpers ──────────────────────────────────────────

rust-bindings: build-static-rust-bridge
	bindgen libkeibidrop.h -o rust/src/bindings.rs

slint-preview:
	slint-viewer rust/src/ui.slint

# ── Packaging ─────────────────────────────────────────────

$(DIST):
	mkdir -p $(DIST)

# macOS .dmg — .app bundle with Applications symlink (drag-to-install)
package-macos: $(DIST)
	@echo "Packaging macOS .dmg for $(GOARCH)..."
	mkdir -p $(DIST)/dmg-staging
	# Create .app bundle
	mkdir -p $(DIST)/dmg-staging/KeibiDrop.app/Contents/MacOS
	mkdir -p $(DIST)/dmg-staging/KeibiDrop.app/Contents/Resources
	cp rust/target/release/keibidrop-rust $(DIST)/dmg-staging/KeibiDrop.app/Contents/MacOS/keibidrop
	cp keibidrop-cli $(DIST)/dmg-staging/KeibiDrop.app/Contents/MacOS/
	cp kd $(DIST)/dmg-staging/KeibiDrop.app/Contents/MacOS/
	cp assets/icons/keibidrop.icns $(DIST)/dmg-staging/KeibiDrop.app/Contents/Resources/keibidrop.icns
	sed 's/VERSION_PLACEHOLDER/$(VERSION)/g' assets/Info.plist.tmpl > $(DIST)/dmg-staging/KeibiDrop.app/Contents/Info.plist
	# Applications symlink for drag-to-install
	ln -s /Applications $(DIST)/dmg-staging/Applications
	hdiutil create -volname "KeibiDrop $(VERSION)" \
	  -srcfolder $(DIST)/dmg-staging \
	  -ov -format UDZO \
	  $(DIST)/keibidrop-$(VERSION)-darwin-$(GOARCH).dmg
	rm -rf $(DIST)/dmg-staging
	@echo "Created $(DIST)/keibidrop-$(VERSION)-darwin-$(GOARCH).dmg"

# Linux .tar.gz — universal fallback
package-tar: $(DIST)
	@echo "Packaging .tar.gz for $(GOOS)-$(GOARCH)..."
	mkdir -p $(DIST)/tar-staging/keibidrop-$(VERSION)
	cp rust/target/release/keibidrop-rust $(DIST)/tar-staging/keibidrop-$(VERSION)/keibidrop
	cp keibidrop-cli $(DIST)/tar-staging/keibidrop-$(VERSION)/
	cp kd $(DIST)/tar-staging/keibidrop-$(VERSION)/
	cp README.md LICENSE $(DIST)/tar-staging/keibidrop-$(VERSION)/
	cd $(DIST)/tar-staging && tar czf ../keibidrop-$(VERSION)-$(GOOS)-$(GOARCH).tar.gz keibidrop-$(VERSION)/
	rm -rf $(DIST)/tar-staging
	@echo "Created $(DIST)/keibidrop-$(VERSION)-$(GOOS)-$(GOARCH).tar.gz"

# Linux .deb — requires nfpm (go install github.com/goreleaser/nfpm/v2/cmd/nfpm@latest)
package-deb: $(DIST)
	@echo "Packaging .deb for $(GOARCH)..."
	VERSION=$(VERSION) GOARCH=$(GOARCH) nfpm package -p deb -f nfpm.yaml -t $(DIST)/
	@echo "Created .deb in $(DIST)/"

# Windows .zip — contains kd.exe + keibidrop-cli.exe + keibidrop.exe + README + LICENSE
# WinFsp is NOT bundled — it's a Chocolatey dependency and installed separately.
package-windows: $(DIST)
	@echo "Packaging Windows zip for $(GOARCH)..."
	mkdir -p $(DIST)/win-staging
	cp kd.exe $(DIST)/win-staging/ 2>/dev/null || cp kd $(DIST)/win-staging/kd.exe
	cp keibidrop-cli.exe $(DIST)/win-staging/ 2>/dev/null || cp keibidrop-cli $(DIST)/win-staging/keibidrop-cli.exe
	cp rust/target/release/keibidrop-rust.exe $(DIST)/win-staging/keibidrop.exe 2>/dev/null || true
	cp README.md LICENSE DUAL-LICENSE.md $(DIST)/win-staging/
ifeq ($(GOOS),windows)
	powershell.exe -Command "Compress-Archive -Path '$(DIST)/win-staging/*' -DestinationPath '$(DIST)/keibidrop-$(CHOCO_VERSION)-windows-$(GOARCH).zip' -Force"
else
	cd $(DIST)/win-staging && zip -r ../keibidrop-$(CHOCO_VERSION)-windows-$(GOARCH).zip .
endif
	rm -rf $(DIST)/win-staging
	@echo "Created $(DIST)/keibidrop-$(CHOCO_VERSION)-windows-$(GOARCH).zip"

# Chocolatey .nupkg — requires choco pack + package-windows first
# Choco requires semver without 'v' prefix (e.g. 0.1.1, not v0.1.1).
CHOCO_VERSION := $(patsubst v%,%,$(VERSION))

package-choco: $(DIST)
	@echo "Packaging Chocolatey .nupkg ($(CHOCO_VERSION))..."
	$(eval WIN_ZIP := $(DIST)/keibidrop-$(CHOCO_VERSION)-windows-$(GOARCH).zip)
	$(eval SHA256 := $(shell shasum -a 256 $(WIN_ZIP) 2>/dev/null | cut -d' ' -f1))
	@test -n "$(SHA256)" || (echo "ERROR: $(WIN_ZIP) not found — run 'make package-windows' first" && exit 1)
	VERSION=$(CHOCO_VERSION) envsubst '$$VERSION' < choco/keibidrop.nuspec.tmpl > $(DIST)/keibidrop.nuspec
	mkdir -p $(DIST)/tools
	TAG=$(VERSION) SEMVER=$(CHOCO_VERSION) SHA256=$(SHA256) envsubst '$$TAG$$SEMVER$$SHA256' < choco/tools/chocolateyinstall.ps1.tmpl > $(DIST)/tools/chocolateyinstall.ps1
	cp choco/tools/chocolateyuninstall.ps1 $(DIST)/tools/
	cd $(DIST) && choco pack keibidrop.nuspec || echo "choco pack failed"
	@echo "Created .nupkg in $(DIST)/ (SHA256=$(SHA256))"

# Generate SHA256 checksums for all release artifacts
checksums: $(DIST)
	cd $(DIST) && shasum -a 256 keibidrop-* > SHA256SUMS
	@echo "Created $(DIST)/SHA256SUMS"

# Local choco install/uninstall test — validates the package before pushing.
# Requires: make package-choco first. Runs elevated (UAC prompt).
test-choco: package-choco
	@echo "=== Testing Chocolatey package locally ==="
	@echo "Installing keibidrop from local nupkg..."
	powershell.exe -Command "Start-Process powershell.exe -Verb RunAs -Wait -ArgumentList \
	  '-NoProfile -Command \"choco install keibidrop -s C:\\Users\\milam\\Desktop\\code\\KeibiDrop\\dist -y --force 2>&1 | Tee-Object C:\\temp\\choco_test.log; \
	  Write-Host \\\"--- Verifying binaries ---\\\"; \
	  if (Get-Command keibidrop-cli.exe -ErrorAction SilentlyContinue) { keibidrop-cli.exe --version } else { Write-Host \\\"FAIL: keibidrop-cli.exe not in PATH\\\" }; \
	  Write-Host \\\"--- Uninstalling ---\\\"; \
	  choco uninstall keibidrop -y 2>&1 | Tee-Object -Append C:\\temp\\choco_test.log\"'"
	@echo "=== Test log at C:\\temp\\choco_test.log ==="
	@cat /c/temp/choco_test.log 2>/dev/null | tr -d '\0' || true

clean-dist:
	rm -rf $(DIST)

# ── Mobile ────────────────────────────────────────────────

MOBILE_REPO ?= ../KeibiDropMobile

build-ios:
	GOFLAGS="-mod=mod" gomobile bind -target=ios -o KeibiDrop.xcframework ./mobile
	rm -rf ios/KeibiDrop/KeibiDrop.xcframework
	cp -r KeibiDrop.xcframework ios/KeibiDrop/KeibiDrop.xcframework

ANDROID_HOME ?= /usr/local/share/android-commandlinetools
ANDROID_NDK_HOME ?= $(ANDROID_HOME)/ndk/27.0.12077973

build-android:
	ANDROID_HOME=$(ANDROID_HOME) ANDROID_NDK_HOME=$(ANDROID_NDK_HOME) \
	GOFLAGS="-mod=mod" gomobile bind -target=android -androidapi 28 -o keibidrop.aar ./mobile

# Sync mobile app source + frameworks to the private KeibiDropMobile repo.
# Edit in KeibiDrop/ios and KeibiDrop/android, run this, commit in KeibiDropMobile.
sync-mobile:
	@echo "Syncing to $(MOBILE_REPO)..."
	rsync -av --delete --exclude='build/' --exclude='.DS_Store' ios/ $(MOBILE_REPO)/ios/
	rsync -av --delete --exclude='build/' --exclude='.DS_Store' android/ $(MOBILE_REPO)/android/
	@if [ -d KeibiDrop.xcframework ]; then \
		rsync -av --delete KeibiDrop.xcframework/ $(MOBILE_REPO)/ios/KeibiDrop/KeibiDrop.xcframework/; \
	fi
	@if [ -f keibidrop.aar ]; then \
		cp keibidrop.aar $(MOBILE_REPO)/android/keibidrop.aar; \
	fi
	@echo "Done. cd $(MOBILE_REPO) && git add -A && git commit"

SIM_ID ?= C415CC66-0845-4AA9-96CB-178CBC42A44F

# Build + run on iOS Simulator
run-ios-sim: build-ios
	rm -rf ios/build
	xcodebuild -project ios/KeibiDrop.xcodeproj -scheme KeibiDrop \
		-sdk iphonesimulator -destination 'platform=iOS Simulator,id=$(SIM_ID)' \
		-derivedDataPath ios/build build
	xcrun simctl boot $(SIM_ID) 2>/dev/null || true
	open -a Simulator
	xcrun simctl install $(SIM_ID) ios/build/Build/Products/Debug-iphonesimulator/KeibiDrop.app
	xcrun simctl launch $(SIM_ID) com.keibisoft.keibidrop

# Build + run on Android emulator (requires Android SDK + emulator running)
run-android-emu: build-android
	cd android && ./gradlew assembleDebug
	adb install android/app/build/outputs/apk/debug/app-debug.apk
	adb shell am start -n com.keibisoft.keibidrop/.MainActivity

clean: clean-dist
	rm -f keibidrop-cli keibidrop-cli.exe kd kd.exe libkeibidrop.a libkeibidrop.h
	rm -rf KeibiDrop.xcframework keibidrop.aar

# ── Run (dev) ─────────────────────────────────────────────
# Alice: ports 26001/26002    Bob: ports 26003/26004
# Relay: http://localhost:54321 (start your own relay first)

RELAY   ?= http://localhost:54321
BRIDGE  ?= bridge.keibisoft.com:26600
SCRIPTS := scripts/dev

# On Windows, WinFSP requires drive letters for FUSE mount points.
# On Linux/macOS a subdirectory is fine.
ifeq ($(GOOS),windows)
ALICE_MOUNT ?= K:
BOB_MOUNT   ?= L:
else
ALICE_MOUNT ?= MountAlice
BOB_MOUNT   ?= MountBob
endif

# Rust UI (FUSE toggle is in the UI, no need for NO_FUSE env)
run-alice:
	TO_MOUNT_PATH=$(ALICE_MOUNT) KEIBIDROP_RELAY=$(RELAY) INBOUND_PORT=26001 OUTBOUND_PORT=26002 bash $(SCRIPTS)/example_run_rust_ui_nofuse.sh
run-bob:
	TO_MOUNT_PATH=$(BOB_MOUNT) KEIBIDROP_RELAY=$(RELAY) INBOUND_PORT=26003 OUTBOUND_PORT=26004 bash $(SCRIPTS)/example_run_rust_ui.sh

# Go CLI
run-cli-alice:
	KEIBIDROP_RELAY=$(RELAY) INBOUND_PORT=26001 OUTBOUND_PORT=26002 bash $(SCRIPTS)/example_run_cli.sh
run-cli-bob:
	KEIBIDROP_RELAY=$(RELAY) INBOUND_PORT=26003 OUTBOUND_PORT=26004 bash $(SCRIPTS)/example_run_peer_cli.sh

# kd daemon (agent, always no-FUSE)
run-kd-alice:
	KD_NO_FUSE=1 KD_RELAY=$(RELAY) KD_INBOUND_PORT=26001 KD_OUTBOUND_PORT=26002 bash $(SCRIPTS)/example_run_kd_alice.sh
run-kd-bob:
	KD_NO_FUSE=1 KD_RELAY=$(RELAY) KD_INBOUND_PORT=26003 KD_OUTBOUND_PORT=26004 bash $(SCRIPTS)/example_run_kd_bob.sh

# ── Bridge mode (for peers behind firewalls / NAT) ───────
# Both peers connect outbound to the bridge relay. No inbound ports needed.
# Default bridge: 185.104.181.40:26600 (Timisoara)
# Override: make run-bridge-alice BRIDGE=your-server:26600

run-bridge-alice: build-kd
	KD_RELAY=https://keibidroprelay.keibisoft.com/ KD_BRIDGE=$(BRIDGE) \
	KD_NO_FUSE=1 KD_SAVE_PATH=SaveAlice KD_SOCKET=/tmp/kd-alice.sock \
	./kd start

run-bridge-bob: build-kd
	KD_RELAY=https://keibidroprelay.keibisoft.com/ KD_BRIDGE=$(BRIDGE) \
	KD_NO_FUSE=1 KD_SAVE_PATH=SaveBob KD_SOCKET=/tmp/kd-bob.sock \
	./kd start

run-bridge-alice-fuse: build-kd
	KD_RELAY=https://keibidroprelay.keibisoft.com/ KD_BRIDGE=$(BRIDGE) \
	KD_SAVE_PATH=SaveAlice KD_MOUNT_PATH=MountAlice KD_SOCKET=/tmp/kd-alice.sock \
	./kd start

run-bridge-bob-fuse: build-kd
	KD_RELAY=https://keibidroprelay.keibisoft.com/ KD_BRIDGE=$(BRIDGE) \
	KD_SAVE_PATH=SaveBob KD_MOUNT_PATH=MountBob KD_SOCKET=/tmp/kd-bob.sock \
	./kd start

# ── Help ──────────────────────────────────────────────────

help:
	@echo "Build:"
	@echo "  make build-cli              Build interactive CLI"
	@echo "  make build-kd               Build agent daemon"
	@echo "  make build-rust             Build Rust/Slint UI (includes protoc + Go static lib)"
	@echo "  make build-all              Build everything"
	@echo ""
	@echo "Run (dev):                    Alice=26001/26002  Bob=26003/26004"
	@echo "  make run-alice              Rust UI, Alice (FUSE toggle in UI)"
	@echo "  make run-bob                Rust UI, Bob   (FUSE toggle in UI)"
	@echo "  make run-cli-alice          Go CLI, Alice"
	@echo "  make run-cli-bob            Go CLI, Bob"
	@echo "  make run-kd-alice           kd daemon, Alice (local/direct)"
	@echo "  make run-kd-bob             kd daemon, Bob (local/direct)"
	@echo "  make run-bridge-alice       kd via bridge relay, Alice (no-FUSE)"
	@echo "  make run-bridge-bob         kd via bridge relay, Bob (no-FUSE)"
	@echo "  make run-bridge-alice-fuse  kd via bridge relay, Alice (FUSE)"
	@echo "  make run-bridge-bob-fuse    kd via bridge relay, Bob (FUSE)"
	@echo ""
	@echo "Test & Lint:"
	@echo "  make test                   Run all integration tests"
	@echo "  make lint                   Run golangci-lint"
	@echo "  make sec                    Run gosec security scanner"
	@echo ""
	@echo "Packaging:"
	@echo "  make package-macos          .dmg for macOS"
	@echo "  make package-tar            .tar.gz archive"
	@echo "  make package-deb            .deb package (needs nfpm)"
	@echo "  make package-windows        .zip for Windows"
	@echo "  make package-choco          Chocolatey .nupkg"
	@echo "  make checksums              SHA256SUMS for dist/"
	@echo ""
	@echo "Mobile:"
	@echo "  make build-ios              Build iOS framework (.xcframework)"
	@echo "  make build-android          Build Android library (.aar)"
	@echo "  make run-ios-sim            Build + run on iOS Simulator"
	@echo "  make run-android-emu        Build + run on Android emulator"
	@echo "  make sync-mobile            Sync ios/ android/ to KeibiDropMobile repo"
	@echo ""
	@echo "Other:"
	@echo "  make protoc                 Regenerate gRPC stubs"
	@echo "  make rust-bindings          Regenerate Rust FFI bindings"
	@echo "  make clean                  Remove build artifacts"

# ── WAN Testing (Mac <-> VPS) ─────────────────────────────────────────────────
wan-deploy: build-kd
	bash scripts/wan/deploy.sh

wan-clean:
	bash scripts/wan/clean.sh

wan-start:
	bash scripts/wan/start.sh

wan-benchmark:
	bash scripts/wan/benchmark.sh

wan-test: wan-clean wan-start wan-benchmark

# ── Android Dev Helpers ───────────────────────────────────────────────
# Push text from Mac clipboard to Android device clipboard + paste into focused field.
# Usage: make android-push-clip FP=<fingerprint>
# Tap peer code field on Android first, then run this.
android-push-clip:
	@if [ -z "$(FP)" ]; then echo "Usage: make android-push-clip FP=<fingerprint>"; exit 1; fi
	adb shell input text '$(FP)'

# Pull fingerprint from Android screen to Mac clipboard.
# Works by dumping the UI tree and finding the fingerprint-length text.
android-pull-clip:
	@adb shell uiautomator dump /sdcard/ui.xml >/dev/null 2>&1; \
	adb pull /sdcard/ui.xml /tmp/_kd_ui.xml >/dev/null 2>&1; \
	FP=$$(grep -o 'text="[^"]*"' /tmp/_kd_ui.xml | grep -E '[A-Za-z0-9_-]{60,}' | tail -1 | sed 's/text="//;s/"//'); \
	if [ -z "$$FP" ]; then echo "No fingerprint found on screen. Make sure the app is open."; exit 1; fi; \
	echo "$$FP" | tr -d '\n' | pbcopy; \
	echo "Pulled: $$FP"

# Deploy latest AAR + APK to connected Android device.
android-deploy: build-android
	cp keibidrop.aar ../KeibiDropMobile/keibidrop.aar
	JAVA_HOME=/usr/local/opt/openjdk@21/libexec/openjdk.jdk/Contents/Home \
	  cd ../KeibiDropMobile/android && ./gradlew assembleDebug
	adb install -r ../KeibiDropMobile/android/app/build/outputs/apk/debug/app-debug.apk
	adb shell am force-stop com.keibisoft.keibidrop
	adb shell am start -n com.keibisoft.keibidrop/.MainActivity

.PHONY: build-cli build-kd build-static-rust-bridge build-rust build-all \
        test lint sec install-proto protoc rust-bindings slint-preview \
        package-macos package-tar package-deb package-windows package-choco checksums clean-dist clean \
        run-alice run-bob \
        run-cli-alice run-cli-bob \
        run-kd-alice run-kd-bob \
        run-bridge-alice run-bridge-bob run-bridge-alice-fuse run-bridge-bob-fuse \
        build-ios build-android run-ios-sim run-android-emu sync-mobile \
        wan-deploy wan-clean wan-start wan-benchmark wan-test help \
        android-push-clip android-pull-clip android-deploy
