// Platform-specific bindgen configuration for macOS and other Unix platforms.
// Included by build.rs via include!() — not a standalone file.

fn configure_bindgen_for_platform(builder: bindgen::Builder) -> bindgen::Builder {
    // No extra configuration needed on macOS/other — clang finds its own headers.
    builder
}
