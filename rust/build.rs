// Platform-specific bindgen helpers — one per OS, mirroring Go's _windows.go convention.
#[cfg(target_os = "linux")]
include!("build_platform_linux.rs");
#[cfg(target_os = "windows")]
include!("build_platform_windows.rs");
#[cfg(not(any(target_os = "linux", target_os = "windows")))]
include!("build_platform_other.rs");

fn main() {
    slint_build::compile("src/ui.slint").unwrap();

    // Embed app icon in Windows executable
    #[cfg(target_os = "windows")]
    {
        let mut res = winresource::WindowsResource::new();
        res.set_icon("assets/keibidrop.ico");
        res.compile().expect("Failed to compile Windows resources");
    }

    // Tell cargo to look for shared libraries in the project root
    println!("cargo:rustc-link-search=native=..");
    println!("cargo:rustc-link-lib=static=keibidrop");
    println!("cargo:rustc-link-lib=static=keibidrop");
    if std::env::var("CARGO_CFG_TARGET_OS").as_deref() == Ok("macos") {
        println!("cargo:rustc-link-lib=framework=CoreFoundation");
        println!("cargo:rustc-link-lib=framework=Security");
        println!("cargo:rustc-link-lib=resolv");
    }
    // Re-run if the header changes
    println!("cargo:rerun-if-changed=../libkeibidrop.h");

    // Use bindgen to generate bindings
    let builder = bindgen::Builder::default()
        .header("../libkeibidrop.h")
        .raw_line("#![allow(non_upper_case_globals)]")
        .raw_line("#![allow(non_camel_case_types)]")
        .raw_line("#![allow(non_snake_case)]")
        .raw_line("#![allow(dead_code)]");

    let builder = configure_bindgen_for_platform(builder);

    let bindings = builder.generate().expect("Unable to generate bindings");

    bindings
        .write_to_file("src/bindings.rs")
        .expect("Couldn't write bindings!");
}
