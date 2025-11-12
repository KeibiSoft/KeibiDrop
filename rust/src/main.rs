mod bindings;

use copypasta::{ClipboardContext, ClipboardProvider};

use env;
use std::ffi::{CStr, CString};

slint::include_modules!(); // this loads ui.slint as MainWindow

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

        // 7. Handle Next: create room
        app.on_next_pressed(move || {
            println!("Creating room...");
            std::thread::spawn(|| {
                let res = bindings::KD_CreateRoom();
                if res != 0 {
                    eprintln!("Failed to create room (code {}).", res);
                }

                eprintln!("Room created")
            });
        });

        // 8. Run UI loop
        app.run().unwrap();

        // 9. Cleanup
        bindings::KD_Stop();
        println!("KeibiDrop stopped.");
    }
}
