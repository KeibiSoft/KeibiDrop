fn main() {
    slint_build::compile("src/ui.slint").unwrap();


    // Tell cargo to look for shared libraries in the project root
    println!("cargo:rustc-link-search=native=..");
    println!("cargo:rustc-link-lib=static=keibidrop");
    println!("cargo:rustc-link-lib=static=keibidrop");
    println!("cargo:rustc-link-lib=framework=CoreFoundation");
    println!("cargo:rustc-link-lib=framework=Security");
    println!("cargo:rustc-link-lib=resolv");
    // _=std::env::var("SLINT_INCLUDE_GENERATED");
    // Re-run if the header changes
    println!("cargo:rerun-if-changed=../libkeibidrop.h");

    // Use bindgen to generate bindings
    let bindings = bindgen::Builder::default()
        .header("../libkeibidrop.h")
        .generate()
        .expect("Unable to generate bindings");

    bindings
        .write_to_file("src/bindings.rs")
        .expect("Couldn't write bindings!");
}
