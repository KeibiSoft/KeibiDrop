// Platform-specific bindgen configuration for Windows.
// Included by build.rs via include!() — not a standalone file.

fn configure_bindgen_for_platform(builder: bindgen::Builder) -> bindgen::Builder {
    // Undefine _MSC_VER so the CGO-generated libkeibidrop.h uses the
    // `float _Complex` path instead of MSVC's <complex.h>, which isn't
    // present in LLVM's bundled clang headers.
    let mut builder = builder.clang_arg("-U_MSC_VER");

    // Clang bundled headers (stddef.h etc.) live inside the LLVM install.
    // Probe common install locations and add the first one found.
    let candidates = [
        r"C:\Program Files\LLVM\lib\clang\22\include",
        r"C:\Program Files\LLVM\lib\clang\21\include",
        r"C:\Program Files\LLVM\lib\clang\20\include",
        r"C:\Program Files\LLVM\lib\clang\19\include",
        r"C:\Program Files\LLVM\lib\clang\18\include",
        r"C:\Program Files\LLVM\lib\clang\17\include",
    ];
    for candidate in &candidates {
        if std::path::Path::new(candidate).join("stddef.h").exists() {
            builder = builder.clang_arg(format!("-I{candidate}"));
            break;
        }
    }
    builder
}
