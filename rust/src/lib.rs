// UI components generated from ui.slint, exposed as a library so the
// headless UI tests (tests/ui.rs) can instantiate them without the engine.
slint::include_modules!();

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
