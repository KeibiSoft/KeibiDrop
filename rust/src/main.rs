mod bindings;
use std::ffi::{CStr,CString};
use env;
use slint::SharedString;

slint::include_modules!(); // this loads ui.slint as MainWindow

fn main() {


    let log_file = env::var("LOG_FILE").unwrap_or_default();
    let to_save = env::var("TO_SAVE_PATH").unwrap_or_default();
    let to_mount = env::var("TO_MOUNT_PATH").unwrap_or_default();
    let relay = env::var("KEIBIDROP_RELAY").unwrap_or_default();
    let inbound: i32 = env::var("INBOUND_PORT").ok().and_then(|v| v.parse().ok()).unwrap_or(0);
    let outbound: i32 = env::var("OUTBOUND_PORT").ok().and_then(|v| v.parse().ok()).unwrap_or(0);

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
        let my_fp = unsafe {
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
    app.on_add_peer_code(move |peer_fp| {
        let s = peer_fp.to_string();
        println!("Registering peer fingerprint: {}", s);

        let c = CString::new(s.clone()).unwrap();
        let res = unsafe { bindings::KD_AddPeerFingerprint(c.as_ptr() as *mut _) };
        if res == 0 {
            println!("Peer registered successfully!");
        } else {
            eprintln!("Failed to register peer (code {}).", res);
        }
    });

    // 6. Handle Copy: just log (you can add clipboard later)
    app.on_copy_my_code(move |code| {
        println!("Copy pressed: {}", code);
    });

    // 7. Handle Next: create room
    app.on_next_pressed(move || {
        println!("Creating room...");
        let res = unsafe { bindings::KD_CreateRoom() };
        if res == 0 {
            println!("Room created successfully!");
        } else {
            eprintln!("Failed to create room (code {}).", res);
        }
    });

    // 8. Run UI loop
    app.run().unwrap();

    // 9. Cleanup
    unsafe {
       bindings::KD_Stop();
    }
    println!("KeibiDrop stopped.");

    }
}
