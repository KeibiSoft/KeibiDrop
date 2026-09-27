// UI components generated from ui.slint, exposed as a library so the
// headless UI tests (tests/ui.rs) can instantiate them without the engine.
slint::include_modules!();

/// Flip a card to its downloading state on the click itself. The watcher
/// thread rebuilds the list every 500 ms, so a small file finished before the
/// tick and the card went from Save straight to Open with no cue (2026-09-25).
/// Progress is left as it was: a Resume click arrives through the same path.
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
