// Without the `bundled` feature, opusic-sys links `-lopus` but only adds a
// library search path when OPUS_LIB_DIR is set. Find the system libopus with
// pkg-config so a plain `cargo build` works (Homebrew, apt libopus-dev, ...).
fn main() {
    println!("cargo:rerun-if-changed=build.rs");
    if std::env::var_os("CARGO_FEATURE_BUNDLED").is_some() {
        return;
    }
    match pkg_config::Config::new()
        .cargo_metadata(false)
        .probe("opus")
    {
        Ok(lib) => {
            for dir in lib.link_paths {
                println!("cargo:rustc-link-search=native={}", dir.display());
            }
        }
        Err(e) => println!(
            "cargo:warning=libopus not found via pkg-config ({e}); install it \
             (brew install opus / apt install libopus-dev) or build with --features bundled"
        ),
    }
}
