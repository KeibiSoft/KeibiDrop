// Platform-specific bindgen configuration for Linux.
// Included by build.rs via include!() — not a standalone file.

fn configure_bindgen_for_platform(builder: bindgen::Builder) -> bindgen::Builder {
    // Clang may not find stddef.h on some Linux distros — add GCC's include path.
    if let Ok(output) = std::process::Command::new("gcc")
        .arg("-print-file-name=include")
        .output()
    {
        if output.status.success() {
            let path = String::from_utf8_lossy(&output.stdout).trim().to_string();
            return builder.clang_arg(format!("-I{path}"));
        }
    }
    builder
}
