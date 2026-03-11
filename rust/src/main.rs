mod bindings;

use copypasta::{ClipboardContext, ClipboardProvider};

use env;
use std::collections::HashMap;
use std::ffi::{CStr, CString};
use std::path::Path;
use std::process::Command;
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::{Arc, Mutex};

use slint::winit_030::WinitWindowAccessor;
#[allow(deprecated)]
use slint::winit_030::WinitWindowEventResult;

slint::include_modules!(); // this loads ui.slint as MainWindow

/// Returns true if a filename should be hidden from the UI.
fn is_hidden_file(name: &str) -> bool {
    name.starts_with('.') || name == ".fsuuid" || name == ".DS_Store" || name == "Thumbs.db"
}

/// Per-file download state tracked on the Rust side.
struct DownloadInfo {
    local_path: String,
    expected_size: i64,
    downloading: bool,
    saved: bool,
}

/// Determine file type category from filename extension.
fn file_type_from_name(name: &str) -> &'static str {
    let lower = name.to_lowercase();
    if let Some(ext) = lower.rsplit('.').next() {
        match ext {
            "pdf" => "pdf",
            "png" | "jpg" | "jpeg" | "gif" | "bmp" | "webp" | "svg" | "ico" | "tiff" => "image",
            "mp4" | "mov" | "avi" | "mkv" | "wmv" | "flv" | "webm" | "m4v" => "video",
            "mp3" | "wav" | "flac" | "aac" | "ogg" | "m4a" | "wma" => "audio",
            "rs" | "go" | "py" | "js" | "ts" | "c" | "cpp" | "h" | "java" | "rb" | "sh"
            | "toml" | "yaml" | "yml" | "json" | "xml" | "html" | "css" => "code",
            "zip" | "tar" | "gz" | "bz2" | "xz" | "7z" | "rar" | "dmg" | "iso" => "archive",
            "txt" | "md" | "log" | "csv" | "rtf" => "text",
            _ => "other",
        }
    } else {
        "other"
    }
}

/// Start a background thread that polls Go for file list updates and pushes to Slint model.
fn start_file_watcher(
    running: Arc<AtomicBool>,
    weak: slint::Weak<MainWindow>,
    downloads: Arc<Mutex<HashMap<String, DownloadInfo>>>,
    save_path: String,
) {
    std::thread::spawn(move || {
        println!("[FileWatcher] Started");
        while running.load(Ordering::Relaxed) {
            std::thread::sleep(std::time::Duration::from_millis(500));

            unsafe {
                let count = bindings::KD_GetFileCount();

                // Build file list model
                let mut files: Vec<FileInfo> = Vec::new();
                let dl = downloads.lock().unwrap();

                for i in 0..count {
                    let name_ptr = bindings::KD_GetFileName(i);
                    if name_ptr.is_null() {
                        continue;
                    }
                    let raw_name = CStr::from_ptr(name_ptr).to_string_lossy().to_string();
                    // Remote file keys have leading "/" — strip it for display and paths
                    let name = raw_name.trim_start_matches('/').to_string();
                    // Skip hidden files
                    if is_hidden_file(&name) {
                        continue;
                    }
                    let size = bindings::KD_GetFileSize(i);
                    let ftype = file_type_from_name(&name);

                    // Check download state
                    let (downloading, progress, saved) = if let Some(info) = dl.get(&name) {
                        if info.downloading {
                            // Poll local file size for progress
                            let local_size = std::fs::metadata(&info.local_path)
                                .map(|m| m.len() as f64)
                                .unwrap_or(0.0);
                            let expected = if info.expected_size > 0 {
                                info.expected_size as f64
                            } else {
                                1.0
                            };
                            let prog = (local_size / expected).min(1.0);
                            (true, prog as f32, false)
                        } else {
                            (false, if info.saved { 1.0 } else { 0.0 }, info.saved)
                        }
                    } else {
                        // Check if file already exists in save path
                        let local = format!("{}/{}", save_path, name);
                        let already_saved = Path::new(&local).exists();
                        (false, if already_saved { 1.0 } else { 0.0 }, already_saved)
                    };

                    files.push(FileInfo {
                        name: slint::SharedString::from(&name),
                        size_bytes: size as i32,
                        downloading,
                        uploading: false, // TODO: wire from Go events
                        progress,
                        saved,
                        file_type: slint::SharedString::from(ftype),
                        is_local: false,
                    });
                }

                // Also include local files (files I shared) — show as already saved
                let local_count = bindings::KD_GetLocalFileCount();
                for i in 0..local_count {
                    let name_ptr = bindings::KD_GetLocalFileName(i);
                    if name_ptr.is_null() {
                        continue;
                    }
                    let raw_name = CStr::from_ptr(name_ptr).to_string_lossy().to_string();
                    // Key may be a full path (from PullFile) or bare name (from AddFile)
                    let display_name = Path::new(&raw_name)
                        .file_name()
                        .map(|f| f.to_string_lossy().to_string())
                        .unwrap_or(raw_name.clone());
                    // Skip hidden files
                    if is_hidden_file(&display_name) {
                        continue;
                    }
                    // Skip if already in remote list (peer has it too, or we downloaded it)
                    if files.iter().any(|f| f.name.as_str() == display_name) {
                        continue;
                    }
                    let ftype = file_type_from_name(&display_name);
                    files.push(FileInfo {
                        name: slint::SharedString::from(&display_name),
                        size_bytes: 0,
                        downloading: false,
                        uploading: false,
                        progress: 1.0,
                        saved: true, // local file = already on disk
                        file_type: slint::SharedString::from(ftype),
                        is_local: true,
                    });
                }

                let any_dl = files.iter().any(|f| f.downloading);
                drop(dl);

                // Push to UI (build VecModel on UI thread since Rc is not Send)
                let weak_clone = weak.clone();
                let _ = slint::invoke_from_event_loop(move || {
                    if let Some(app) = weak_clone.upgrade() {
                        let model = std::rc::Rc::new(slint::VecModel::from(files));
                        app.set_file_list(slint::ModelRc::from(model));
                        app.set_any_downloading(any_dl);
                    }
                });
            }
        }
        println!("[FileWatcher] Stopped");
    });
}

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

/// Retrieve and clear the last error from the Go FFI layer.
fn get_last_error() -> String {
    unsafe {
        let ptr = bindings::KD_GetLastErrorAndClear();
        if ptr.is_null() {
            "Unknown error".to_string()
        } else {
            CStr::from_ptr(ptr).to_string_lossy().to_string()
        }
    }
}

/// Shared logic for Create Room / Join Room buttons.
fn connect_room(
    weak: slint::Weak<MainWindow>,
    running: Arc<AtomicBool>,
    downloads: Arc<Mutex<HashMap<String, DownloadInfo>>>,
    save_path: String,
    target_screen: i32,
    create: bool,
) {
    let action = if create { "Creating" } else { "Joining" };
    let room_action_val = if create { 1 } else { 2 };
    println!("{} room...", action);

    // Set button state immediately on UI thread
    let weak_pre = weak.clone();
    let _ = slint::invoke_from_event_loop(move || {
        if let Some(app) = weak_pre.upgrade() {
            app.set_room_action(room_action_val);
            app.set_error_message(slint::SharedString::default());
            app.set_status_message(slint::SharedString::from(
                if create { "Creating room..." } else { "Joining room..." },
            ));
        }
    });

    std::thread::spawn(move || unsafe {
        let res = if create {
            bindings::KD_CreateRoom()
        } else {
            bindings::KD_JoinRoom()
        };

        if res != 0 {
            let err = get_last_error();
            eprintln!(
                "Failed to {} room: {}",
                if create { "create" } else { "join" },
                err
            );
            let weak_err = weak.clone();
            let _ = slint::invoke_from_event_loop(move || {
                if let Some(app) = weak_err.upgrade() {
                    app.set_room_action(0);
                    app.set_status_message(slint::SharedString::default());
                    app.set_error_message(slint::SharedString::from(err));
                }
            });
            return;
        }

        println!(
            "Room {} successfully",
            if create { "created" } else { "joined" }
        );

        // Start file watcher
        running.store(true, Ordering::Relaxed);
        start_file_watcher(running.clone(), weak.clone(), downloads, save_path);

        // Transition to connected screen
        let _ = slint::invoke_from_event_loop(move || {
            if let Some(app) = weak.upgrade() {
                app.set_room_action(0);
                app.set_status_message(slint::SharedString::default());
                app.set_error_message(slint::SharedString::default());
                app.set_current_screen(target_screen);
            }
        });
    });
}

fn main() {
    let mut ctx = ClipboardContext::new().unwrap();

    let _log_file = env::var("LOG_FILE").unwrap_or_default();
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

    // Collab sync options (off by default)
    let prefetch_on_open = env::var("KEIBIDROP_PREFETCH_ON_OPEN").is_ok();
    let push_on_write = env::var("KEIBIDROP_PUSH_ON_WRITE").is_ok();
    println!(
        "Collab sync: prefetch_on_open={}, push_on_write={}",
        prefetch_on_open, push_on_write
    );

    // Convert to CString
    let relay_c = CString::new(relay).unwrap();
    let to_mount_c = CString::new(to_mount.clone()).unwrap();
    let to_save_c = CString::new(to_save.clone()).unwrap();

    // Shared download state
    let downloads: Arc<Mutex<HashMap<String, DownloadInfo>>> =
        Arc::new(Mutex::new(HashMap::new()));

    unsafe {
        let result = bindings::KD_Initialize(
            relay_c.into_raw(),
            inbound,
            outbound,
            to_mount_c.into_raw(),
            to_save_c.into_raw(),
            if use_fuse { 1 } else { 0 },
            if prefetch_on_open { 1 } else { 0 },
            if push_on_write { 1 } else { 0 },
        );

        if result != 0 {
            eprintln!("Failed to initialize KeibiDrop, error code: {}", result);
        }

        // Retrieve our fingerprint
        let my_fp = {
            let ptr = bindings::KD_Fingerprint();
            if ptr.is_null() {
                "unknown".to_string()
            } else {
                CStr::from_ptr(ptr).to_string_lossy().to_string()
            }
        };
        println!("Our fingerprint: {}", my_fp);

        // Build UI
        let app = MainWindow::new().expect("Failed to create MainWindow");
        app.set_my_code(slint::SharedString::from(my_fp.clone()));
        app.set_mount_path(slint::SharedString::from(to_mount.clone()));

        // Handle Add: register peer fingerprint
        let weak = app.as_weak();
        app.on_add_peer_code(move || {
            if let Some(app) = weak.upgrade() {
                let peer_code_shared = app.get_peer_code();
                let peer_code = peer_code_shared.as_str();
                println!("Peer code entered: {}", peer_code);

                let c_peer_code = CString::new(peer_code).expect("CString::new failed");
                let result = bindings::KD_AddPeerFingerprint(c_peer_code.as_ptr() as *mut i8);
                if result != 0 {
                    println!("Received error code: {}", result);
                    app.set_peer_code_added(false);
                    app.set_peer_code_error(true);
                } else {
                    app.set_peer_code_error(false);
                    app.set_peer_code_added(true);
                }
            }
        });

        // Handle Copy: copy fingerprint to clipboard
        app.on_copy_my_code(move || {
            let my_fp = my_fp.clone();
            println!("Copy pressed: {}", my_fp);
            ctx.set_contents(my_fp)
                .expect("My operating system hates me");
        });

        // Create/Join Room setup
        let target_screen = if use_fuse { 2 } else { 1 };
        let watcher_running = Arc::new(AtomicBool::new(false));
        let watcher_running_disconnect = watcher_running.clone();

        // Create Room handler
        let weak_create = app.as_weak();
        let watcher_running_create = watcher_running.clone();
        let downloads_create = downloads.clone();
        let save_path_create = to_save.clone();
        app.on_create_room_pressed(move || {
            connect_room(
                weak_create.clone(),
                watcher_running_create.clone(),
                downloads_create.clone(),
                save_path_create.clone(),
                target_screen,
                true,
            );
        });

        // Join Room handler
        let weak_join = app.as_weak();
        let watcher_running_join = watcher_running.clone();
        let downloads_join = downloads.clone();
        let save_path_join = to_save.clone();
        app.on_join_room_pressed(move || {
            connect_room(
                weak_join.clone(),
                watcher_running_join.clone(),
                downloads_join.clone(),
                save_path_join.clone(),
                target_screen,
                false,
            );
        });

        // Handle Disconnect
        let weak_disconnect = app.as_weak();
        app.on_disconnect_pressed(move || {
            println!("Disconnecting...");
            watcher_running_disconnect.store(false, Ordering::Relaxed);
            bindings::KD_UnmountFilesystem();
            bindings::KD_Stop();
            if let Some(app) = weak_disconnect.upgrade() {
                app.set_room_action(0);
                app.set_status_message(slint::SharedString::default());
                app.set_error_message(slint::SharedString::default());
                app.set_peer_code_added(false);
                app.set_peer_code_error(false);
                app.set_current_screen(0);
            }
        });

        // Handle Open Folder (FUSE mode)
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

        // Handle Add File: native file picker → copy to save folder → KD_AddFile
        let save_path_add = to_save.clone();
        app.on_add_file_pressed(move || {
            println!("Add file pressed");
            let save_path = save_path_add.clone();
            std::thread::spawn(move || {
                if let Some(path) = rfd::FileDialog::new().pick_file() {
                    let path_str = path.to_string_lossy().to_string();
                    println!("Selected file: {}", path_str);
                    // Copy to save folder for safekeeping
                    let _ = std::fs::create_dir_all(&save_path);
                    let filename = path.file_name().unwrap_or_default().to_string_lossy().to_string();
                    let dest = format!("{}/{}", save_path, filename);
                    if let Err(e) = std::fs::copy(&path_str, &dest) {
                        eprintln!("Failed to copy file to save folder: {}", e);
                    }
                    let c_path = CString::new(path_str.clone()).unwrap();
                    let res = bindings::KD_AddFile(c_path.into_raw());
                    if res != 0 {
                        let err = get_last_error();
                        eprintln!("Failed to add file: {}", err);
                    } else {
                        println!("File added: {}", path_str);
                    }
                }
            });
        });

        // Handle Save File: download file by name to save path
        let downloads_save = downloads.clone();
        let save_path_save = to_save.clone();
        app.on_save_file(move |filename| {
            let name = filename.to_string();
            let downloads = downloads_save.clone();
            let save_path = save_path_save.clone();

            // Get file size by name
            let c_name = CString::new(name.clone()).unwrap();
            let size = bindings::KD_GetFileSizeByName(c_name.as_ptr() as *mut i8);

            // Ensure save directory exists
            if let Err(e) = std::fs::create_dir_all(&save_path) {
                eprintln!("Failed to create save directory {}: {}", save_path, e);
                return;
            }

            let local_path = format!("{}/{}", save_path, name);

            println!("Saving file {} -> {}", name, local_path);

            // Mark as downloading
            {
                let mut dl = downloads.lock().unwrap();
                dl.insert(
                    name.clone(),
                    DownloadInfo {
                        local_path: local_path.clone(),
                        expected_size: size,
                        downloading: true,
                        saved: false,
                    },
                );
            }

            // Download in background thread
            std::thread::spawn(move || {
                let c_name = CString::new(name.clone()).unwrap();
                let c_path = CString::new(local_path.clone()).unwrap();
                let res = bindings::KD_SaveFileByName(
                    c_name.into_raw(),
                    c_path.into_raw(),
                );

                let mut dl = downloads.lock().unwrap();
                if let Some(info) = dl.get_mut(&name) {
                    info.downloading = false;
                    if res == 0 {
                        info.saved = true;
                        println!("Download complete: {}", name);
                    } else {
                        let err = get_last_error();
                        eprintln!("Download failed for {}: {}", name, err);
                    }
                }
            });
        });

        // Handle Open File: open saved file with system handler
        let save_path_open = to_save.clone();
        app.on_open_file(move |filename| {
            let name = filename.to_string();

            // Try to get real path for local files first
            let c_name = CString::new(name.clone()).unwrap();
            let real_ptr = bindings::KD_GetLocalFileRealPath(c_name.as_ptr() as *mut i8);
            let local_path = if !real_ptr.is_null() {
                CStr::from_ptr(real_ptr).to_string_lossy().to_string()
            } else {
                format!("{}/{}", save_path_open, name)
            };
            println!("Opening file: {}", local_path);

            #[cfg(target_os = "macos")]
            let _ = Command::new("open").arg(&local_path).spawn();
            #[cfg(target_os = "linux")]
            let _ = Command::new("xdg-open").arg(&local_path).spawn();
            #[cfg(target_os = "windows")]
            let _ = Command::new("explorer").arg(&local_path).spawn();
        });

        // Handle Exit: clean shutdown
        let weak_exit = app.as_weak();
        app.on_exit_pressed(move || {
            println!("Exit pressed, shutting down...");
            bindings::KD_UnmountFilesystem();
            bindings::KD_Stop();
            if let Some(app) = weak_exit.upgrade() {
                let _ = app.hide();
            }
        });

        // Drag-and-drop: intercept winit events for OS file drops
        let weak_dnd = app.as_weak();
        let save_path_dnd = to_save.clone();
        #[allow(deprecated)]
        app.window().on_winit_window_event(move |_slint_window, event| {
            use slint::winit_030::winit;
            match event {
                winit::event::WindowEvent::HoveredFile(_path) => {
                    if let Some(app) = weak_dnd.upgrade() {
                        app.set_drag_hovering(true);
                    }
                    WinitWindowEventResult::Propagate
                }
                winit::event::WindowEvent::HoveredFileCancelled => {
                    if let Some(app) = weak_dnd.upgrade() {
                        app.set_drag_hovering(false);
                    }
                    WinitWindowEventResult::Propagate
                }
                winit::event::WindowEvent::DroppedFile(path) => {
                    if let Some(app) = weak_dnd.upgrade() {
                        app.set_drag_hovering(false);
                    }
                    let path_str = path.to_string_lossy().to_string();
                    println!("File dropped: {}", path_str);
                    // Copy to save folder for safekeeping
                    let _ = std::fs::create_dir_all(&save_path_dnd);
                    if let Some(fname) = path.file_name() {
                        let dest = format!("{}/{}", save_path_dnd, fname.to_string_lossy());
                        if let Err(e) = std::fs::copy(&path, &dest) {
                            eprintln!("Failed to copy dropped file to save folder: {}", e);
                        }
                    }
                    let c_path = CString::new(path_str.clone()).unwrap();
                    let res = bindings::KD_AddFile(c_path.into_raw());
                    if res != 0 {
                        let err = get_last_error();
                        eprintln!("Failed to add dropped file: {}", err);
                    } else {
                        println!("Dropped file added: {}", path_str);
                    }
                    WinitWindowEventResult::Propagate
                }
                _ => WinitWindowEventResult::Propagate,
            }
        });

        // Run UI loop
        app.run().unwrap();

        // Cleanup
        bindings::KD_Stop();
        println!("KeibiDrop stopped.");
    }
}
