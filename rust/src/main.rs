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

fn walkdir(dir: &Path) -> Vec<std::path::PathBuf> {
    let mut files = Vec::new();
    if let Ok(entries) = std::fs::read_dir(dir) {
        for entry in entries.flatten() {
            let p = entry.path();
            if p.is_dir() {
                files.extend(walkdir(&p));
            } else {
                files.push(p);
            }
        }
    }
    files
}

/// Returns true if a filename should be hidden from the UI.
fn is_hidden_file(name: &str) -> bool {
    name.starts_with('.')
        || name == ".fsuuid"
        || name == ".DS_Store"
        || name == "Thumbs.db"
        || name.contains(".fuse_hidden")
        || name.contains("/.fseventsd")
}

/// Per-file download state tracked on the Rust side.
struct DownloadInfo {
    local_path: String,
    expected_size: i64,
    downloading: bool,
    saved: bool,
    paused: bool,
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
    current_folder: Arc<Mutex<String>>,
) {
    std::thread::spawn(move || {
        // FileWatcher running
        while running.load(Ordering::Relaxed) {
            std::thread::sleep(std::time::Duration::from_millis(500));

            // Event polling is handled by a dedicated Slint Timer (see main).
            // This thread only updates the file list.
            unsafe {
                let count = bindings::KD_GetFileCount();
                let folder = current_folder.lock().unwrap().clone();

                // Collect all remote file names
                let mut all_names: Vec<(String, i64)> = Vec::new();
                for i in 0..count {
                    let name_ptr = bindings::KD_GetFileName(i);
                    if name_ptr.is_null() {
                        continue;
                    }
                    let raw_name = CStr::from_ptr(name_ptr).to_string_lossy().to_string();
                    let name = raw_name.trim_start_matches('/').to_string();
                    if is_hidden_file(&name) {
                        continue;
                    }
                    let size = bindings::KD_GetFileSize(i) as i64;
                    all_names.push((name, size));
                }

                // Group by current folder: show items at this level,
                // collapse subdirectories into folder cards
                let mut files: Vec<FileInfo> = Vec::new();
                let mut seen_folders: std::collections::HashSet<String> = std::collections::HashSet::new();
                let dl = downloads.lock().unwrap();

                for (name, size) in all_names.iter() {
                    let name = name.clone();
                    let size = *size;
                    let relative = if folder.is_empty() {
                        name.clone()
                    } else if let Some(stripped) = name.strip_prefix(&format!("{}/", folder)) {
                        stripped.to_string()
                    } else {
                        continue; // not in current folder
                    };

                    if let Some(slash_pos) = relative.find('/') {
                        // This file is in a subfolder. Show the subfolder as a folder card.
                        let subfolder = &relative[..slash_pos];
                        let full_folder = if folder.is_empty() {
                            subfolder.to_string()
                        } else {
                            format!("{}/{}", folder, subfolder)
                        };
                        if seen_folders.insert(full_folder.clone()) {
                            // Count files in this subfolder
                            let child_count = all_names.iter()
                                .filter(|(n, _)| n.starts_with(&format!("{}/", full_folder)))
                                .count();
                            files.push(FileInfo {
                                name: slint::SharedString::from(subfolder),
                                size_bytes: child_count as i32,
                                downloading: false,
                                uploading: false,
                                progress: 0.0,
                                saved: false,
                                paused: false,
                                file_type: slint::SharedString::from("folder"),
                                is_local: false,
                            });
                        }
                        continue;
                    }

                    // Regular file at this level
                    let ftype = file_type_from_name(&relative);

                    // Check download state
                    let (downloading, progress, saved, paused) = if let Some(info) = dl.get(&name) {
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
                            (true, prog as f32, false, false)
                        } else if info.paused {
                            // Paused: show progress from bitmap via FFI
                            let c_name = CString::new(name.clone()).unwrap();
                            let prog = bindings::KD_GetDownloadProgress(c_name.into_raw());
                            let p = if prog >= 0 { prog as f32 / 100.0 } else { 0.0 };
                            (false, p, false, true)
                        } else {
                            (false, if info.saved { 1.0 } else { 0.0 }, info.saved, false)
                        }
                    } else {
                        // Check if file already exists in save path
                        let local = format!("{}/{}", save_path, name);
                        let already_saved = Path::new(&local).exists();
                        (false, if already_saved { 1.0 } else { 0.0 }, already_saved, false)
                    };

                    files.push(FileInfo {
                        name: slint::SharedString::from(&name),
                        size_bytes: size as i32,
                        downloading,
                        uploading: false, // TODO: wire from Go events
                        progress,
                        saved,
                        paused,
                        file_type: slint::SharedString::from(ftype),
                        is_local: false,
                    });
                }

                // Also include local files (files I shared) — show as already saved
                let local_count = bindings::KD_GetLocalFileCount();
                let mut local_names: Vec<String> = Vec::new();
                for i in 0..local_count {
                    let name_ptr = bindings::KD_GetLocalFileName(i);
                    if name_ptr.is_null() {
                        continue;
                    }
                    let raw_name = CStr::from_ptr(name_ptr).to_string_lossy().to_string();
                    let name = raw_name.trim_start_matches('/').to_string();
                    if !is_hidden_file(&name) {
                        local_names.push(name);
                    }
                }

                for lname in &local_names {
                    let relative = if folder.is_empty() {
                        lname.clone()
                    } else if let Some(stripped) = lname.strip_prefix(&format!("{}/", folder)) {
                        stripped.to_string()
                    } else {
                        continue;
                    };

                    if let Some(slash_pos) = relative.find('/') {
                        let subfolder = &relative[..slash_pos];
                        let full_folder = if folder.is_empty() {
                            subfolder.to_string()
                        } else {
                            format!("{}/{}", folder, subfolder)
                        };
                        if seen_folders.insert(full_folder.clone()) {
                            let child_count = local_names.iter()
                                .filter(|n| n.starts_with(&format!("{}/", full_folder)))
                                .count();
                            if !files.iter().any(|f| f.name.as_str() == subfolder) {
                                files.push(FileInfo {
                                    name: slint::SharedString::from(subfolder),
                                    size_bytes: child_count as i32,
                                    downloading: false,
                                    uploading: false,
                                    progress: 0.0,
                                    saved: true,
                                    paused: false,
                                    file_type: slint::SharedString::from("folder"),
                                    is_local: true,
                                });
                            }
                        }
                        continue;
                    }

                    // Skip if already in remote list
                    if files.iter().any(|f| f.name.as_str() == relative) {
                        continue;
                    }
                    let ftype = file_type_from_name(&relative);
                    files.push(FileInfo {
                        name: slint::SharedString::from(&relative),
                        size_bytes: 0,
                        downloading: false,
                        uploading: false,
                        progress: 1.0,
                        saved: true,
                        paused: false,
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
        // FileWatcher stopped
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
        // WinFSP may not register in System32 — check the install dir too.
        Path::new(r"C:\Windows\System32\winfsp-x64.dll").exists()
            || Path::new(r"C:\Program Files (x86)\WinFsp\bin\winfsp-x64.dll").exists()
            || Path::new(r"C:\Program Files\WinFsp\bin\winfsp-x64.dll").exists()
            || Path::new(r"C:\Program Files (x86)\WinFsp\bin\winfsp-a64.dll").exists()
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
            let raw = CStr::from_ptr(ptr).to_string_lossy().to_string();
            humanize_error(&raw)
        }
    }
}

fn show_toast(weak: &slint::Weak<MainWindow>, msg: &str) {
    let w = weak.clone();
    let m = msg.to_string();
    let _ = slint::invoke_from_event_loop(move || {
        if let Some(app) = w.upgrade() {
            app.set_toast_message(slint::SharedString::from(&m));
        }
    });
    let w2 = weak.clone();
    std::thread::spawn(move || {
        std::thread::sleep(std::time::Duration::from_secs(2));
        let _ = slint::invoke_from_event_loop(move || {
            if let Some(app) = w2.upgrade() {
                app.set_toast_message(slint::SharedString::default());
            }
        });
    });
}

fn humanize_error(msg: &str) -> String {
    let lower = msg.to_lowercase();
    if lower.contains("timeout") || lower.contains("timed out") {
        return "Peer didn't respond in time. Check that they pressed Connect.".into();
    }
    if lower.contains("relay at full capacity") || lower.contains("relay at maximum capacity") {
        return "Relay is busy. Try again in a minute, or switch to Local Network mode.".into();
    }
    if lower.contains("rate limit") {
        return "Too many attempts. Wait a few minutes and try again.".into();
    }
    if lower.contains("session not established") {
        return "Not connected yet. Exchange codes and press Connect first.".into();
    }
    if lower.contains("not found") && lower.contains("relay") {
        return "Peer not found on relay. Check the code and try again.".into();
    }
    if lower.contains("not found") {
        return "Peer not found. Check that they are online and try again.".into();
    }
    if lower.contains("fingerprint mismatch") {
        return "Security check failed. The peer's identity doesn't match. Try exchanging codes again.".into();
    }
    if lower.contains("identical fingerprint") {
        return "You entered your own code. Paste your peer's code, not yours.".into();
    }
    if lower.contains("nil pointer") || lower.contains("nil filesystem") {
        return "Something went wrong internally. Try restarting the app.".into();
    }
    if lower.contains("already running") || lower.contains("already mounted") {
        return "Already connected. Disconnect first before starting a new session.".into();
    }
    if lower.contains("invalid session") {
        return "Session expired. Reconnect to continue.".into();
    }
    if lower.contains("connection refused") || lower.contains("no route") {
        return "Can't reach peer. Check your internet connection or try Local Network mode.".into();
    }
    if lower.contains("invalid fingerprint") || lower.contains("invalid length") {
        return "Invalid code format. Check that you copied the full code.".into();
    }
    if lower.contains("context canceled") || lower.contains("canceled") {
        return "Connection cancelled.".into();
    }
    msg.to_string()
}

/// Shared logic for Create Room / Join Room buttons.
fn connect_room(
    weak: slint::Weak<MainWindow>,
    running: Arc<AtomicBool>,
    downloads: Arc<Mutex<HashMap<String, DownloadInfo>>>,
    save_path: String,
    current_folder: Arc<Mutex<String>>,
    target_screen: i32,
    create: bool,
) {
    let room_action_val = if create { 1 } else { 2 };

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

        // Wire health/reconnect events into the Go event channel.
        bindings::KD_SetupEventCallbacks();

        // Start file watcher
        running.store(true, Ordering::Relaxed);
        start_file_watcher(running.clone(), weak.clone(), downloads, save_path, current_folder.clone());

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

/// Connect using deterministic fingerprint tiebreaker (WAN mode).
fn connect_room_auto(
    weak: slint::Weak<MainWindow>,
    running: Arc<AtomicBool>,
    downloads: Arc<Mutex<HashMap<String, DownloadInfo>>>,
    save_path: String,
    current_folder: Arc<Mutex<String>>,
    target_screen: i32,
) {
    println!("Connecting (auto role)...");

    let weak_pre = weak.clone();
    let _ = slint::invoke_from_event_loop(move || {
        if let Some(app) = weak_pre.upgrade() {
            app.set_room_action(1);
            app.set_error_message(slint::SharedString::default());
            app.set_status_message(slint::SharedString::from("Connecting..."));
            app.set_connect_status(slint::SharedString::default());
        }
    });

    let countdown_weak = weak.clone();
    let countdown_running = Arc::new(AtomicBool::new(true));
    let countdown_flag = countdown_running.clone();
    std::thread::spawn(move || {
        let total_secs: u64 = 595;
        for elapsed in 0..total_secs {
            if !countdown_flag.load(Ordering::Relaxed) {
                break;
            }
            std::thread::sleep(std::time::Duration::from_secs(1));
            let remaining = total_secs - elapsed;
            let mins = remaining / 60;
            let secs = remaining % 60;
            let msg = format!("Waiting for peer... ({}:{:02} remaining)", mins, secs);
            let w = countdown_weak.clone();
            let _ = slint::invoke_from_event_loop(move || {
                if let Some(app) = w.upgrade() {
                    if app.get_room_action() != 0 {
                        app.set_connect_status(slint::SharedString::from(msg));
                    }
                }
            });
        }
    });

    std::thread::spawn(move || unsafe {
        let res = bindings::KD_Connect();
        countdown_running.store(false, Ordering::Relaxed);

        if res != 0 {
            let err = get_last_error();
            eprintln!("Connect failed: {}", err);
            let weak_err = weak.clone();
            let _ = slint::invoke_from_event_loop(move || {
                if let Some(app) = weak_err.upgrade() {
                    app.set_room_action(0);
                    app.set_status_message(slint::SharedString::default());
                    app.set_connect_status(slint::SharedString::default());
                    app.set_error_message(slint::SharedString::from(err));
                }
            });
            return;
        }

        println!("Connected successfully");
        bindings::KD_SetupEventCallbacks();
        running.store(true, Ordering::Relaxed);
        start_file_watcher(running.clone(), weak.clone(), downloads, save_path, current_folder.clone());

        let _ = slint::invoke_from_event_loop(move || {
            if let Some(app) = weak.upgrade() {
                app.set_room_action(0);
                app.set_status_message(slint::SharedString::default());
                app.set_connect_status(slint::SharedString::default());
                app.set_error_message(slint::SharedString::default());
                app.set_current_screen(target_screen);
            }
        });
    });
}

fn main() {
    #[cfg(unix)]
    unsafe {
        libc::signal(libc::SIGPIPE, libc::SIG_IGN);
    }

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

    // Determine if FUSE should be used (mirrors Go CLI logic).
    // ON by default if FUSE is present, OFF if NO_FUSE env is set.
    let no_fuse_env = env::var("NO_FUSE").map(|v| !v.is_empty()).unwrap_or(false);
    let fuse_present = is_fuse_present();
    let use_fuse = fuse_present && !no_fuse_env;
    println!(
        "FUSE present: {}, NO_FUSE env: {}, using FUSE: {}",
        fuse_present, no_fuse_env, use_fuse
    );

    // Local mode: direct LAN connection, skip relay
    let local_mode_env = env::var("KEIBIDROP_LOCAL")
        .map(|v| !v.is_empty())
        .unwrap_or(false);
    println!("Local mode: {}", local_mode_env);

    // Collab sync: auto-enabled with FUSE, can be overridden via env vars.
    let prefetch_on_open = env::var("KEIBIDROP_PREFETCH_ON_OPEN")
        .map(|v| !v.is_empty())
        .unwrap_or(use_fuse);
    let push_on_write = env::var("KEIBIDROP_PUSH_ON_WRITE")
        .map(|v| !v.is_empty())
        .unwrap_or(use_fuse);
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

    // Folder navigation state (shared with file watcher and callbacks)
    let current_folder: Arc<Mutex<String>> = Arc::new(Mutex::new(String::new()));

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

        // Set window icon from embedded PNG
        app.window().with_winit_window(|winit_win| {
            let icon_bytes = include_bytes!("../assets/icon-256.png");
            if let Ok(img) = image::load_from_memory(icon_bytes) {
                let rgba = img.to_rgba8();
                let (w, h) = rgba.dimensions();
                if let Ok(icon) = slint::winit_030::winit::window::Icon::from_rgba(rgba.into_raw(), w, h) {
                    winit_win.set_window_icon(Some(icon));
                }
            }
        });

        app.set_my_code(slint::SharedString::from(my_fp.clone()));
        app.set_mount_path(slint::SharedString::from(to_mount.clone()));

        // Version display
        let version_ptr = bindings::KD_GetVersion();
        if !version_ptr.is_null() {
            let version_str = CStr::from_ptr(version_ptr).to_string_lossy().to_string();
            app.set_version_text(slint::SharedString::from(version_str));
        }

        // Settings: populate from Go config
        let config_ptr = bindings::KD_GetConfig();
        if !config_ptr.is_null() {
            let config_str = CStr::from_ptr(config_ptr).to_string_lossy().to_string();
            for line in config_str.lines() {
                if let Some((key, val)) = line.split_once('=') {
                    match key.trim() {
                        "relay" => app.set_cfg_relay(slint::SharedString::from(val.trim())),
                        "save_path" => app.set_cfg_save_path(slint::SharedString::from(val.trim())),
                        "mount_path" => app.set_cfg_mount_path(slint::SharedString::from(val.trim())),
                        "log_file" => app.set_cfg_log_file(slint::SharedString::from(val.trim())),
                        _ => {}
                    }
                }
            }
        }
        let cfg_path_ptr = bindings::KD_GetConfigPath();
        if !cfg_path_ptr.is_null() {
            app.set_cfg_config_path(slint::SharedString::from(
                CStr::from_ptr(cfg_path_ptr).to_string_lossy().as_ref(),
            ));
        }

        // FUSE mode toggle
        app.set_fuse_available(fuse_present);
        app.set_fuse_mode(use_fuse);
        if !fuse_present {
            let hint = if cfg!(target_os = "macos") {
                "Install macFUSE: macfuse.github.io"
            } else if cfg!(target_os = "windows") {
                "Install WinFsp: winfsp.dev"
            } else {
                "Install fuse3: sudo apt install fuse3"
            };
            app.set_fuse_install_hint(hint.into());
        }
        app.on_fuse_mode_toggled(move |enabled| {
            println!("FUSE mode toggled: {}", enabled);
            bindings::KD_SetFUSEMode(if enabled { 1 } else { 0 });
        });

        // Local mode toggle
        let local_addr = {
            let ptr = bindings::KD_GetLinkLocalAddress();
            if ptr.is_null() {
                String::new()
            } else {
                CStr::from_ptr(ptr).to_string_lossy().to_string()
            }
        };
        println!("Link-local address: {}", local_addr);
        app.set_local_mode(local_mode_env);
        app.set_local_address(slint::SharedString::from(&local_addr));
        if local_mode_env {
            bindings::KD_SetLocalMode(1);
        }
        app.on_local_mode_toggled(move |enabled| {
            println!("Local mode toggled: {}", enabled);
            bindings::KD_SetLocalMode(if enabled { 1 } else { 0 });
            if enabled {
                bindings::KD_StartDiscovery();
                let name_ptr = bindings::KD_GetDiscoveryName();
                if !name_ptr.is_null() {
                    let name = CStr::from_ptr(name_ptr).to_string_lossy().to_string();
                    println!("Discovery name: {}", name);
                }
                // Don't auto-CreateRoom — wait until user taps a peer,
                // then tiebreaker decides who creates and who joins.
            } else {
                bindings::KD_StopDiscovery();
            }
        });

        // Poll discovered peers when in local mode
        let weak_disc = app.as_weak();
        std::thread::spawn(move || {
            loop {
                std::thread::sleep(std::time::Duration::from_secs(2));
                let weak = weak_disc.clone();
                let _ = slint::invoke_from_event_loop(move || {
                    if let Some(app) = weak.upgrade() {
                        if !app.get_local_mode() {
                            app.set_discovery_name(slint::SharedString::from(""));
                            let empty: Vec<DiscoveredPeer> = vec![];
                            app.set_discovered_peers(slint::ModelRc::new(slint::VecModel::from(empty)));
                            return;
                        }
                        let name_ptr = bindings::KD_GetDiscoveryName();
                        if !name_ptr.is_null() {
                            let name = CStr::from_ptr(name_ptr).to_string_lossy().to_string();
                            app.set_discovery_name(slint::SharedString::from(name));
                        }
                        let count = bindings::KD_GetDiscoveredPeerCount();
                        let mut peers: Vec<DiscoveredPeer> = Vec::new();
                        for i in 0..count {
                            let n = bindings::KD_GetDiscoveredPeerName(i);
                            let a = bindings::KD_GetDiscoveredPeerAddr(i);
                            if !n.is_null() && !a.is_null() {
                                peers.push(DiscoveredPeer {
                                    name: slint::SharedString::from(CStr::from_ptr(n).to_string_lossy().as_ref()),
                                    addr: slint::SharedString::from(CStr::from_ptr(a).to_string_lossy().as_ref()),
                                });
                            }
                        }
                        app.set_discovered_peers(slint::ModelRc::new(slint::VecModel::from(peers)));
                    }
                });
            }
        });

        // Handle discovered peer selection
        let weak_peer_sel = app.as_weak();
        app.on_discovery_peer_selected(move |idx| {
            let addr_ptr = bindings::KD_GetDiscoveredPeerAddr(idx);
            let name_ptr = bindings::KD_GetDiscoveredPeerName(idx);
            let my_name_ptr = bindings::KD_GetDiscoveryName();
            if !addr_ptr.is_null() && !name_ptr.is_null() && !my_name_ptr.is_null() {
                let addr = CStr::from_ptr(addr_ptr).to_string_lossy().to_string();
                let peer_name = CStr::from_ptr(name_ptr).to_string_lossy().to_string();
                let my_name = CStr::from_ptr(my_name_ptr).to_string_lossy().to_string();
                println!("Selected peer: {} ({})", peer_name, addr);

                // Deterministic tiebreaker: lower name = creator (listener), higher = joiner
                let i_am_creator = my_name < peer_name;

                let addr_c = CString::new(addr.clone()).unwrap();
                bindings::KD_SetPeerDirectAddress(addr_c.into_raw());

                if let Some(app) = weak_peer_sel.upgrade() {
                    app.set_peer_code(slint::SharedString::from(&addr));
                    if i_am_creator {
                        println!("I'm creator (listener) — invoking CreateRoom for {}", peer_name);
                        app.invoke_create_room_pressed();
                    } else {
                        println!("I'm joiner — invoking JoinRoom to {}", peer_name);
                        app.invoke_join_room_pressed();
                    }
                }
            }
        });

        // Handle Add: register peer fingerprint or direct address (local mode)
        let weak = app.as_weak();
        app.on_add_peer_code(move || {
            if let Some(app) = weak.upgrade() {
                let peer_code_shared = app.get_peer_code();
                let peer_code = peer_code_shared.as_str();
                let is_local = app.get_local_mode();
                println!("Peer code entered: {} (local={})", peer_code, is_local);

                let result = if is_local {
                    let c_addr = CString::new(peer_code).expect("CString::new failed");
                    bindings::KD_SetPeerDirectAddress(c_addr.as_ptr() as *mut i8)
                } else {
                    let c_fp = CString::new(peer_code).expect("CString::new failed");
                    bindings::KD_AddPeerFingerprint(c_fp.as_ptr() as *mut i8)
                };

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

        // Handle Copy: copy fingerprint or local address to clipboard
        let weak_copy = app.as_weak();
        let weak_toast_copy = app.as_weak();
        let local_addr_copy = local_addr.clone();
        app.on_copy_my_code(move || {
            let text = if let Some(app) = weak_copy.upgrade() {
                if app.get_local_mode() {
                    local_addr_copy.clone()
                } else {
                    app.get_my_code().to_string()
                }
            } else {
                my_fp.clone()
            };
            ctx.set_contents(text)
                .expect("My operating system hates me");
            show_toast(&weak_toast_copy, "Copied to clipboard");
        });

        // Create/Join Room setup
        let watcher_running = Arc::new(AtomicBool::new(false));
        let disconnecting = Arc::new(AtomicBool::new(false));
        let watcher_running_disconnect = watcher_running.clone();
        let disconnecting_disconnect = disconnecting.clone();

        // Create Room handler
        let weak_create = app.as_weak();
        let watcher_running_create = watcher_running.clone();
        let downloads_create = downloads.clone();
        let save_path_create = to_save.clone();
        let current_folder_create = current_folder.clone();
        app.on_create_room_pressed(move || {
            // Read current FUSE mode at press time (user may have toggled)
            let screen = if let Some(app) = weak_create.upgrade() {
                if app.get_fuse_mode() { 2 } else { 1 }
            } else { 1 };
            connect_room(
                weak_create.clone(),
                watcher_running_create.clone(),
                downloads_create.clone(),
                save_path_create.clone(),
                current_folder_create.clone(),
                screen,
                true,
            );
        });

        // Join Room handler
        let weak_join = app.as_weak();
        let watcher_running_join = watcher_running.clone();
        let downloads_join = downloads.clone();
        let save_path_join = to_save.clone();
        let current_folder_join = current_folder.clone();
        app.on_join_room_pressed(move || {
            let screen = if let Some(app) = weak_join.upgrade() {
                if app.get_fuse_mode() { 2 } else { 1 }
            } else { 1 };
            connect_room(
                weak_join.clone(),
                watcher_running_join.clone(),
                downloads_join.clone(),
                save_path_join.clone(),
                current_folder_join.clone(),
                screen,
                false,
            );
        });

        // Connect handler (WAN mode -- auto role via fingerprint tiebreaker)
        let weak_connect = app.as_weak();
        let current_folder_connect = current_folder.clone();
        let watcher_running_connect = watcher_running.clone();
        let downloads_connect = downloads.clone();
        let save_path_connect = to_save.clone();
        app.on_connect_pressed(move || {
            let screen = if let Some(app) = weak_connect.upgrade() {
                if app.get_fuse_mode() { 2 } else { 1 }
            } else { 1 };
            connect_room_auto(
                weak_connect.clone(),
                watcher_running_connect.clone(),
                downloads_connect.clone(),
                save_path_connect.clone(),
                current_folder_connect.clone(),
                screen,
            );
        });

        // Handle Cancel (abort room creation/join)
        let weak_cancel = app.as_weak();
        app.on_cancel_connect_pressed(move || {
            
            bindings::KD_UnmountFilesystem();
            bindings::KD_Disconnect();
            if let Some(app) = weak_cancel.upgrade() {
                app.set_room_action(0);
                app.set_status_message(slint::SharedString::default());
                app.set_error_message(slint::SharedString::default());
            }
        });

        // Handle Disconnect — warn if download in progress, then disconnect
        let weak_disconnect = app.as_weak();
        let disconnect_confirmed = Arc::new(AtomicBool::new(false));
        let disconnect_confirmed_inner = disconnect_confirmed.clone();
        app.on_export_logs_pressed(move || {
            
            std::thread::spawn(move || {
                if let Some(dest) = rfd::FileDialog::new()
                    .set_file_name("keibidrop-sanitized.log")
                    .save_file()
                {
                    let c_dest = CString::new(dest.to_string_lossy().to_string()).unwrap();
                    let res = bindings::KD_SanitizeLogs(c_dest.into_raw());
                    if res == 0 {
                        println!("Sanitized logs saved to: {}", dest.display());
                    } else {
                        let err = get_last_error();
                        eprintln!("Failed to export logs: {}", err);
                    }
                }
            });
        });

        app.on_disconnect_pressed(move || {
            // If downloads in progress and not yet confirmed, show warning instead
            if let Some(app) = weak_disconnect.upgrade() {
                if app.get_any_downloading() && !disconnect_confirmed_inner.swap(false, Ordering::Relaxed) {
                    app.set_status_message(slint::SharedString::from(
                        "Download in progress. Press Disconnect again to confirm."
                    ));
                    disconnect_confirmed_inner.store(true, Ordering::Relaxed);
                    return;
                }
            }
            disconnect_confirmed_inner.store(false, Ordering::Relaxed);

            println!("Disconnecting...");
            // Prevent double-disconnect (event timer + button click race)
            if disconnecting_disconnect.swap(true, Ordering::Relaxed) {
                return;
            }
            watcher_running_disconnect.store(false, Ordering::Relaxed);

            // Update UI immediately (we're on the UI thread — no blocking)
            if let Some(app) = weak_disconnect.upgrade() {
                app.set_peer_code(slint::SharedString::default());
                app.set_room_action(3); // "disconnecting" — disables Create/Join buttons
                app.set_status_message(slint::SharedString::from("Disconnecting..."));
                app.set_error_message(slint::SharedString::default());
                app.set_peer_code_added(false);
                app.set_peer_code_error(false);
                app.set_current_screen(0);
            }

            // FFI cleanup in background thread (KD_Disconnect blocks up to 2s+)
            let weak_bg = weak_disconnect.clone();
            let disc_done = disconnecting_disconnect.clone();
            std::thread::spawn(move || {
                bindings::KD_UnmountFilesystem();
                bindings::KD_Disconnect();
                let _ = slint::invoke_from_event_loop(move || {
                    disc_done.store(false, Ordering::Relaxed);
                    if let Some(app) = weak_bg.upgrade() {
                        let ptr = bindings::KD_Fingerprint();
                        if !ptr.is_null() {
                            let new_fp = CStr::from_ptr(ptr).to_string_lossy().to_string();
                            app.set_my_code(slint::SharedString::from(new_fp));
                        }
                        app.set_room_action(0);
                        app.set_status_message(slint::SharedString::default());
                    }
                });
            });
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

        {
            let folder = current_folder.clone();
            let weak = app.as_weak();
            app.on_navigate_folder(move |name| {
                let mut f = folder.lock().unwrap();
                if f.is_empty() {
                    *f = name.to_string();
                } else {
                    *f = format!("{}/{}", *f, name);
                }
                if let Some(app) = weak.upgrade() {
                    app.set_current_folder(slint::SharedString::from(f.as_str()));
                }
            });
        }

        {
            let folder = current_folder.clone();
            let weak = app.as_weak();
            app.on_back_folder(move || {
                let mut f = folder.lock().unwrap();
                if let Some(pos) = f.rfind('/') {
                    *f = f[..pos].to_string();
                } else {
                    *f = String::new();
                }
                if let Some(app) = weak.upgrade() {
                    app.set_current_folder(slint::SharedString::from(f.as_str()));
                }
            });
        }

        // Handle Add File: native file picker (multi-select) → copy to save folder → KD_AddFile
        let save_path_add = to_save.clone();
        let weak_toast_add = app.as_weak();
        app.on_add_file_pressed(move || {
            let save_path = save_path_add.clone();
            let weak = weak_toast_add.clone();
            std::thread::spawn(move || {
                if let Some(paths) = rfd::FileDialog::new().pick_files() {
                    let count = paths.len();
                    for path in paths {
                        let path_str = path.to_string_lossy().to_string();
                        let _ = std::fs::create_dir_all(&save_path);
                        let filename = path.file_name().unwrap_or_default().to_string_lossy().to_string();
                        let dest = format!("{}/{}", save_path, filename);
                        if let Err(e) = std::fs::copy(&path_str, &dest) {
                            eprintln!("Failed to copy file to save folder: {}", e);
                        }
                        let c_path = CString::new(path_str).unwrap();
                        let res = bindings::KD_AddFile(c_path.into_raw());
                        if res != 0 {
                            let err = get_last_error();
                            eprintln!("Failed to add file: {}", err);
                        }
                    }
                    if count == 1 {
                        show_toast(&weak, "File shared with peer");
                    } else if count > 1 {
                        show_toast(&weak, &format!("{} files shared with peer", count));
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
            let size = bindings::KD_GetFileSizeByName(c_name.as_ptr() as *mut i8) as i64;

            let local_path = format!("{}/{}", save_path, name);
            // Create parent directories (handles nested paths like screenshots/foo.png)
            if let Some(parent) = Path::new(&local_path).parent() {
                if let Err(e) = std::fs::create_dir_all(parent) {
                    eprintln!("Failed to create directory {}: {}", parent.display(), e);
                    return;
                }
            }

            

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
                        paused: false,
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
                    } else {
                        let err = get_last_error();
                        eprintln!("Download failed for {}: {}", name, err);
                    }
                }
            });
        });

        // Handle Pause/Resume: cancel active download or re-trigger save
        let downloads_pause = downloads.clone();
        let save_path_pause = to_save.clone();
        app.on_pause_file(move |filename| {
            let name = filename.to_string();
            let mut dl = downloads_pause.lock().unwrap();
            if let Some(info) = dl.get_mut(&name) {
                if info.downloading {
                    // Pause: cancel the active download
                    info.downloading = false;
                    info.paused = true;
                    let c_name = CString::new(name.clone()).unwrap();
                    bindings::KD_CancelDownload(c_name.into_raw());
                    println!("Paused download: {}", name);
                } else if info.paused {
                    // Resume: re-trigger download (PullFile resumes from bitmap)
                    info.downloading = true;
                    info.paused = false;
                    let local_path = format!("{}/{}", save_path_pause, name);
                    let dl_clone = downloads_pause.clone();
                    let name_clone = name.clone();
                    std::thread::spawn(move || {
                        let c_name = CString::new(name_clone.clone()).unwrap();
                        let c_path = CString::new(local_path).unwrap();
                        let res = bindings::KD_SaveFileByName(
                            c_name.into_raw(),
                            c_path.into_raw(),
                        );
                        let mut dl = dl_clone.lock().unwrap();
                        if let Some(info) = dl.get_mut(&name_clone) {
                            info.downloading = false;
                            info.paused = false;
                            if res == 0 {
                                info.saved = true;
                                
                            } else {
                                let err = get_last_error();
                                eprintln!("Download failed for {}: {}", name_clone, err);
                            }
                        }
                    });
                    println!("Resumed download: {}", name);
                }
            }
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
            if !std::path::Path::new(&local_path).exists() {
                println!("Open file: path does not exist: {}", local_path);
                return;
            }
            println!("Opening file: {}", local_path);

            #[cfg(target_os = "macos")]
            let _ = Command::new("open").arg(&local_path).spawn();
            #[cfg(target_os = "linux")]
            let _ = Command::new("xdg-open").arg(&local_path).spawn();
            // On Windows, `explorer file.txt` opens the folder, not the file.
            // `cmd /c start "" "path"` opens the file with its default handler.
            #[cfg(target_os = "windows")]
            let _ = Command::new("cmd").args(["/c", "start", "", &local_path]).spawn();
        });

        // Handle Save All: save every remote unsaved file
        let downloads_all = downloads.clone();
        let save_path_all = to_save.clone();
        let weak_toast_all = app.as_weak();
        app.on_save_all_pressed(move || {
            let file_count = bindings::KD_GetFileCount();
            let mut to_save_names: Vec<String> = Vec::new();
            for i in 0..file_count {
                let name_ptr = bindings::KD_GetFileName(i);
                if name_ptr.is_null() {
                    continue;
                }
                let raw = CStr::from_ptr(name_ptr).to_string_lossy().to_string();
                let name = raw.trim_start_matches('/').to_string();
                let dl = downloads_all.lock().unwrap();
                let dominated = dl.get(&name).map_or(false, |d| d.saved || d.downloading);
                drop(dl);
                if !dominated && !is_hidden_file(&name) {
                    to_save_names.push(name);
                }
            }
            let total = to_save_names.len();
            if total == 0 {
                show_toast(&weak_toast_all, "All files already saved");
                return;
            }
            show_toast(&weak_toast_all, &format!("Downloading {} files...", total));

            let downloads = downloads_all.clone();
            let base = save_path_all.clone();
            for name in to_save_names {
                let c_name = CString::new(name.clone()).unwrap();
                let size = bindings::KD_GetFileSizeByName(c_name.as_ptr() as *mut i8) as i64;
                let local_path = format!("{}/{}", base, name);
                // Create parent directories (handles nested paths like screenshots/foo.png)
                if let Some(parent) = Path::new(&local_path).parent() {
                    if let Err(e) = std::fs::create_dir_all(parent) {
                        eprintln!("Failed to create directory {}: {}", parent.display(), e);
                        continue;
                    }
                }
                {
                    let mut dl = downloads.lock().unwrap();
                    dl.insert(
                        name.clone(),
                        DownloadInfo {
                            local_path: local_path.clone(),
                            expected_size: size,
                            downloading: true,
                            saved: false,
                            paused: false,
                        },
                    );
                }
                let dl_clone = downloads.clone();
                let n = name.clone();
                std::thread::spawn(move || {
                    let c_n = CString::new(n.clone()).unwrap();
                    let c_p = CString::new(local_path.clone()).unwrap();
                    let res = bindings::KD_SaveFileByName(c_n.into_raw(), c_p.into_raw());
                    let mut dl = dl_clone.lock().unwrap();
                    if let Some(info) = dl.get_mut(&n) {
                        info.downloading = false;
                        if res == 0 {
                            info.saved = true;
                            
                        } else {
                            let err = get_last_error();
                            eprintln!("Download failed for {}: {}", n, err);
                        }
                    }
                });
            }
        });

        // Handle Exit: notify peer + clean shutdown
        let weak_exit = app.as_weak();
        app.on_exit_pressed(move || {
            println!("Exit pressed, shutting down...");
            bindings::KD_UnmountFilesystem();
            bindings::KD_Disconnect();
            bindings::KD_Stop();
            if let Some(app) = weak_exit.upgrade() {
                let _ = app.hide();
            }
        });

        app.on_open_fuse_install(|| {
            #[cfg(target_os = "macos")]
            let _ = Command::new("open").arg("https://macfuse.github.io/").spawn();
            #[cfg(target_os = "linux")]
            let _ = Command::new("xdg-open").arg("https://github.com/libfuse/libfuse").spawn();
            #[cfg(target_os = "windows")]
            let _ = Command::new("explorer").arg("https://winfsp.dev/").spawn();
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
                    let save = save_path_dnd.clone();
                    let path = path.to_path_buf();
                    std::thread::spawn(move || {
                        let _ = std::fs::create_dir_all(&save);
                        if path.is_dir() {
                            let dir_name = path.file_name()
                                .map(|n| n.to_string_lossy().to_string())
                                .unwrap_or_default();
                            for entry in walkdir(&path) {
                                if entry.is_file() {
                                    let rel = entry.strip_prefix(&path).unwrap_or(&entry);
                                    let remote_name = format!("{}/{}", dir_name, rel.to_string_lossy());
                                    let dest = Path::new(&save).join(&dir_name).join(rel);
                                    if let Some(parent) = dest.parent() {
                                        let _ = std::fs::create_dir_all(parent);
                                    }
                                    let _ = std::fs::copy(&entry, &dest);
                                    let c_local = CString::new(dest.to_string_lossy().to_string()).unwrap();
                                    let c_remote = CString::new(remote_name.clone()).unwrap();
                                    let res = bindings::KD_AddFileAs(c_local.into_raw(), c_remote.into_raw());
                                    if res != 0 {
                                        eprintln!("Failed to add: {}", remote_name);
                                    }
                                }
                            }
                            println!("Directory added: {}", path_str);
                        } else {
                            if let Some(fname) = path.file_name() {
                                let dest = format!("{}/{}", save, fname.to_string_lossy());
                                if let Err(e) = std::fs::copy(&path, &dest) {
                                    eprintln!("Failed to copy dropped file: {}", e);
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
                        }
                    });
                    WinitWindowEventResult::Propagate
                }
                _ => WinitWindowEventResult::Propagate,
            }
        });

        // Event polling timer — runs on UI thread, independent of file watcher.
        // This ensures disconnect/health events are always processed even when
        // the file watcher thread has stopped or hasn't started yet.
        let arrived_files: Arc<Mutex<Vec<String>>> = Arc::new(Mutex::new(Vec::new()));
        let _event_timer = {
            let timer = slint::Timer::default();
            let weak_evt = app.as_weak();
            let disconnecting_evt = disconnecting.clone();
            let watcher_running_evt = watcher_running.clone();
            let arrived_files = arrived_files.clone();
            timer.start(
                slint::TimerMode::Repeated,
                std::time::Duration::from_millis(200),
                move || {
                    loop {
                        let evt_ptr = bindings::KD_PollEvent();
                        if evt_ptr.is_null() {
                            break;
                        }
                        let evt = CStr::from_ptr(evt_ptr).to_string_lossy().to_string();
                        libc::free(evt_ptr as *mut libc::c_void);
                        println!("[Event] {}", evt);

                        // Connection mode events
                        if evt.starts_with("connection_mode:") {
                            let mode = evt.trim_start_matches("connection_mode:");
                            if let Some(app) = weak_evt.upgrade() {
                                app.set_connection_mode(slint::SharedString::from(mode));
                            }
                        }

                        // Connect status events
                        if evt.starts_with("connect_status:") {
                            let status = evt.trim_start_matches("connect_status:");
                            let display = match status {
                                "peer_not_ready" => "Peer not found yet. Make sure your peer pressed Connect.",
                                s => s,
                            };
                            if let Some(app) = weak_evt.upgrade() {
                                app.set_connect_status(slint::SharedString::from(display));
                            }
                        }

                        // File arrival notifications
                        if evt.starts_with("file_arrived:") {
                            let parts: Vec<&str> = evt.splitn(3, ':').collect();
                            if parts.len() >= 2 {
                                let name = parts[1];
                                if !is_hidden_file(name) {
                                    arrived_files.lock().unwrap().push(name.to_string());
                                }
                            }
                        }

                        let is_disconnect = evt.starts_with("peer_disconnected:")
                            || evt.starts_with("gave_up:");
                        if is_disconnect {
                            println!(
                                "[Event] Disconnect detected ({}), cleaning up...",
                                evt
                            );
                            // Prevent double-disconnect
                            if disconnecting_evt.swap(true, Ordering::Relaxed) {
                                break;
                            }
                            watcher_running_evt.store(false, Ordering::Relaxed);

                            let msg = if evt.starts_with("peer_disconnected:") {
                                "Peer disconnected"
                            } else {
                                "Connection lost"
                            };

                            // Update UI immediately (we're on the UI thread)
                            if let Some(app) = weak_evt.upgrade() {
                                app.set_peer_code(slint::SharedString::default());
                                app.set_room_action(3);
                                app.set_status_message(slint::SharedString::from(msg));
                                app.set_error_message(slint::SharedString::default());
                                app.set_peer_code_added(false);
                                app.set_peer_code_error(false);
                                app.set_current_screen(0);
                            }

                            // FFI cleanup in background thread
                            let weak_bg = weak_evt.clone();
                            let disc_done = disconnecting_evt.clone();
                            std::thread::spawn(move || {
                                bindings::KD_UnmountFilesystem();
                                bindings::KD_Disconnect();
                                let _ = slint::invoke_from_event_loop(move || {
                                    disc_done.store(false, Ordering::Relaxed);
                                    if let Some(app) = weak_bg.upgrade() {
                                        let ptr = bindings::KD_Fingerprint();
                                        if !ptr.is_null() {
                                            let fp = CStr::from_ptr(ptr)
                                                .to_string_lossy()
                                                .to_string();
                                            app.set_my_code(slint::SharedString::from(fp));
                                        }
                                        app.set_room_action(0);
                                        app.set_status_message(
                                            slint::SharedString::default(),
                                        );
                                    }
                                });
                            });
                            break;
                        }
                    }

                    // Send batched notification for files that arrived this tick
                    let mut files = arrived_files.lock().unwrap();
                    if !files.is_empty() {
                        let count = files.len();
                        let body = if count == 1 {
                            format!("{}", files[0])
                        } else if count <= 3 {
                            files.join(", ")
                        } else {
                            format!("{} files received", count)
                        };
                        files.clear();
                        drop(files);

                        std::thread::spawn(move || {
                            let _ = std::panic::catch_unwind(|| {
                                #[cfg(target_os = "macos")]
                                {
                                    let script = format!(
                                        "display notification \"{}\" with title \"KeibiDrop\"",
                                        body.replace('\"', "\\\"")
                                    );
                                    let _ = Command::new("osascript")
                                        .arg("-e")
                                        .arg(&script)
                                        .output();
                                }
                                #[cfg(not(target_os = "macos"))]
                                {
                                    let _ = notify_rust::Notification::new()
                                        .summary("KeibiDrop")
                                        .body(&body)
                                        .timeout(notify_rust::Timeout::Milliseconds(4000))
                                        .show();
                                }
                            });
                        });
                    }
                },
            );
            timer
        };

        // Run UI loop
        app.run().unwrap();

        // Cleanup
        bindings::KD_Stop();
        println!("KeibiDrop stopped.");
    }
}
