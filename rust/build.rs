fn main() {
    slint_build::compile("src/ui.slint").unwrap();


    // Tell cargo to look for shared libraries in the project root
    println!("cargo:rustc-link-search=native=..");
    println!("cargo:rustc-link-lib=static=keibidrop");
    println!("cargo:rustc-link-lib=static=keibidrop");
    if std::env::var("CARGO_CFG_TARGET_OS").as_deref() == Ok("macos") {
        println!("cargo:rustc-link-lib=framework=CoreFoundation");
        println!("cargo:rustc-link-lib=framework=Security");
        println!("cargo:rustc-link-lib=resolv");
    }
    // _=std::env::var("SLINT_INCLUDE_GENERATED");
    // Re-run if the header changes
    println!("cargo:rerun-if-changed=../libkeibidrop.h");

    // Use bindgen to generate bindings
    let mut builder = bindgen::Builder::default()
        .header("../libkeibidrop.h")
        .raw_line("#![allow(non_upper_case_globals)]")
        .raw_line("#![allow(non_camel_case_types)]")
        .raw_line("#![allow(non_snake_case)]")
        .raw_line("#![allow(dead_code)]");

    // On Linux, clang may not find stddef.h — add GCC's include path
    if cfg!(target_os = "linux") {
        if let Ok(output) = std::process::Command::new("gcc")
            .arg("-print-file-name=include")
            .output()
        {
            if output.status.success() {
                let path = String::from_utf8_lossy(&output.stdout).trim().to_string();
                builder = builder.clang_arg(format!("-I{path}"));
            }
        }
    }

    let bindings = builder.generate().expect("Unable to generate bindings");

    bindings
        .write_to_file("src/bindings.rs")
        .expect("Couldn't write bindings!");
}
