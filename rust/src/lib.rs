// UI components generated from ui.slint, exposed as a library so the
// headless UI tests (tests/ui.rs) can instantiate them without the engine.
slint::include_modules!();

pub mod backdrop;

use slint::ComponentHandle;
use std::sync::atomic::{AtomicU64, Ordering};

static BACKDROP_GENERATION: AtomicU64 = AtomicU64::new(0);

/// Called when an overlay opens: snapshot the window with the header and the
/// overlays hidden, blur it on a worker thread, then give it to the overlay.
/// A later capture wins over a slower earlier one.
pub fn install_backdrop(app: &MainWindow) {
    app.set_capturing(true);
    let shot = app.window().take_snapshot();
    app.set_capturing(false);
    let Ok(shot) = shot else { return };

    let scale = app.window().scale_factor();
    let size = app.window().size().to_logical(scale);
    let generation = BACKDROP_GENERATION.fetch_add(1, Ordering::Relaxed) + 1;
    // The dim base shows until the new blur lands.
    app.global::<Backdrop>().set_blur(slint::Image::default());

    let weak = app.as_weak();
    std::thread::spawn(move || {
        let blur = backdrop::blur(&shot, scale);
        let _ = weak.upgrade_in_event_loop(move |app| {
            if BACKDROP_GENERATION.load(Ordering::Relaxed) != generation {
                return;
            }
            let bd = app.global::<Backdrop>();
            bd.set_width(size.width);
            bd.set_height(size.height);
            bd.set_blur(slint::Image::from_rgba8(blur));
        });
    });
}

/// The watcher's 500 ms tick: change only the rows that differ. A new model
/// makes Slint rebuild every card, and a click in progress on one is lost.
pub fn sync_file_list(app: &MainWindow, rows: Vec<FileInfo>) {
    use slint::Model;
    let list = app.get_file_list();
    let Some(shown) = list.as_any().downcast_ref::<slint::VecModel<FileInfo>>() else {
        app.set_file_list(slint::ModelRc::new(slint::VecModel::from(rows)));
        return;
    };
    let common = shown.row_count().min(rows.len());
    for (i, row) in rows.iter().enumerate().take(common) {
        if shown.row_data(i).as_ref() != Some(row) {
            shown.set_row_data(i, row.clone());
        }
    }
    while shown.row_count() > rows.len() {
        shown.remove(shown.row_count() - 1);
    }
    for row in rows.into_iter().skip(common) {
        shown.push(row);
    }
}

/// Flip a card to downloading on the click itself; the watcher's 500 ms rebuild can
/// land after a small file has finished. Progress is kept: Resume comes through here too.
pub fn mark_row_downloading(list: &slint::ModelRc<FileInfo>, name: &str) -> bool {
    use slint::Model;
    for i in 0..list.row_count() {
        if let Some(mut row) = list.row_data(i) {
            if row.name.as_str() == name {
                row.downloading = true;
                row.paused = false;
                list.set_row_data(i, row);
                return true;
            }
        }
    }
    false
}
