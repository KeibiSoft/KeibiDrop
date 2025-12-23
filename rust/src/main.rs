mod bindings;

use copypasta::{ClipboardContext, ClipboardProvider};

use env;
use std::ffi::{CStr, CString};
use std::path::Path;
use std::process::Command;

slint::include_modules!(); // this loads ui.slint as MainWindow

/// Check if FUSE is present on the system (mirrors Go's checkfuse.IsFUSEPresent)
fn is_fuse_present() -> bool {
    #[cfg(target_os = "macos")]
    {
        Path::new("/usr/local/lib/libfuse.dylib").exists()
            || Path::new("/Library/Filesystems/macfuse.fs").exists()
    }
    #[cfg(target_os = "linux")]
    {
        Path::new("/lib/x86_64-linux-gnu/libfuse.so.2").exists()
            || Path::new("/usr/lib/libfuse.so").exists()
            || Path::new("/usr/lib/x86_64-linux-gnu/libfuse3.so").exists()
    }
    #[cfg(target_os = "windows")]
    {
        Path::new(r"C:\Windows\System32\winfsp-x64.dll").exists()
    }
    #[cfg(not(any(target_os = "macos", target_os = "linux", target_os = "windows")))]
    {
        false
    }
}

fn main() {
    let mut ctx = ClipboardContext::new().unwrap();

    let log_file = env::var("LOG_FILE").unwrap_or_default();
    let to_save = env::var("TO_SAVE_PATH").unwrap_or_default();
    let to_mount = env::var("TO_MOUNT_PATH").unwrap_or_default();
    let relay = env::var("KEIBIDROP_RELAY").unwrap_or("http://0.0.0.0:54321".to_string());
    let inbound: i32 = env::var("INBOUND_PORT")
        .ok()
        .and_then(|v| v.parse().ok())
        .unwrap_or(26001);
    let outbound: i32 = env::var("OUTBOUND_PORT")
        .ok()
        .and_then(|v| v.parse().ok())
        .unwrap_or(26002);

    // Determine if FUSE should be used (mirrors Go CLI logic)
    let no_fuse_env = env::var("NO_FUSE").is_ok();
    let fuse_present = is_fuse_present();
    let use_fuse = fuse_present && !no_fuse_env;
    println!(
        "FUSE present: {}, NO_FUSE env: {}, using FUSE: {}",
        fuse_present, no_fuse_env, use_fuse
    );

    // Convert to CString
    let relay_c = CString::new(relay).unwrap();
    let to_mount_c = CString::new(to_mount).unwrap();
    let to_save_c = CString::new(to_save).unwrap();

    unsafe {
        let result = bindings::KD_Initialize(
            relay_c.into_raw(),
            inbound,
            outbound,
            to_mount_c.into_raw(),
            to_save_c.into_raw(),
            if use_fuse { 1 } else { 0 },
        );

        if result != 0 {
            eprintln!("Failed to initialize KeibiDrop, error code: {}", result);
        }
        // 3. Retrieve our fingerprint
        let my_fp = {
            let ptr = bindings::KD_Fingerprint();
            if ptr.is_null() {
                "unknown".to_string()
            } else {
                CStr::from_ptr(ptr).to_string_lossy().to_string()
            }
        };
        println!("Our fingerprint: {}", my_fp);

        // 4. Build UI
        let app = MainWindow::new().expect("Failed to create MainWindow");

        // set our fingerprint (property defined in ui.slint)
        app.set_my_code(slint::SharedString::from(my_fp.clone()));
        // or simpler, since Slint auto-generates setter:
        // app.set_my_code(SharedString::from(my_fp.clone()));

        // 5. Handle Add: register peer fingerprint

        let weak = app.as_weak(); // get a weak reference
        app.on_add_peer_code(move || {
            if let Some(app) = weak.upgrade() {
                // Fetch the latest peer code from the UI at button press time
                let peer_code_shared = app.get_peer_code();
                let peer_code = peer_code_shared.as_str();
                println!("Peer code entered: {}", peer_code);

                let c_peer_code = CString::new(peer_code).expect("CString::new failed");
                let result = bindings::KD_AddPeerFingerprint(c_peer_code.as_ptr() as *mut i8);
                if result != 0 {
                    println!("Received error code: {}", result);
                }
            }
        });

        // 6. Handle Copy: just log (you can add clipboard later)
        app.on_copy_my_code(move || {
            let my_fp = my_fp.clone();
            println!("Copy pressed: {}", my_fp);
            ctx.set_contents(my_fp)
                .expect("My operating system hates me");
        });

        // 7. Handle Next: create room and transition to connected screen
        // Screen 1 = no-fuse (file list), Screen 2 = fuse (mounted folder)
        let weak_next = app.as_weak();
        let target_screen = if use_fuse { 2 } else { 1 };
        app.on_next_pressed(move || {
            println!("Creating room...");
            let weak = weak_next.clone();
            std::thread::spawn(move || {
                let res = bindings::KD_CreateRoom();
                if res != 0 {
                    eprintln!("Failed to create room (code {}), trying to join...", res);
                    // If create fails, try to join (peer might have already created)
                    let join_res = bindings::KD_JoinRoom();
                    if join_res != 0 {
                        eprintln!("Failed to join room (code {}).", join_res);
                        return;
                    }
                    eprintln!("Room joined successfully");
                } else {
                    eprintln!("Room created successfully");
                }

                // Transition to appropriate screen based on FUSE availability
                let _ = slint::invoke_from_event_loop(move || {
                    if let Some(app) = weak.upgrade() {
                        app.set_current_screen(target_screen);
                    }
                });
            });
        });

        // 8. Handle Disconnect: unmount filesystem, stop, and return to screen 0
        let weak_disconnect = app.as_weak();
        app.on_disconnect_pressed(move || {
            println!("Disconnecting...");
            bindings::KD_UnmountFilesystem();
            bindings::KD_Stop();
            if let Some(app) = weak_disconnect.upgrade() {
                app.set_current_screen(0);
            }
        });

        // 9. Handle Open Folder: open mounted folder in system file manager
        let mount_path = env::var("TO_MOUNT_PATH").unwrap_or_else(|_| ".".to_string());
        app.on_open_folder_pressed(move || {
            println!("Opening folder: {}", mount_path);
            #[cfg(target_os = "macos")]
            let _ = Command::new("open").arg(&mount_path).spawn();
            #[cfg(target_os = "linux")]
            let _ = Command::new("xdg-open").arg(&mount_path).spawn();
            #[cfg(target_os = "windows")]
            let _ = Command::new("explorer").arg(&mount_path).spawn();
        });

        // 10. Run UI loop
        app.run().unwrap();

        // 11. Cleanup
        bindings::KD_Stop();
        println!("KeibiDrop stopped.");
    }
}
