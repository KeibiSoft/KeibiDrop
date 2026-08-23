// Headless UI tests: real ui.slint, no engine, no window, no network.
// Elements are found by accessible label and driven by their default
// action, the same surface a screen reader uses.
// Run: make test-ui   (cargo test --test ui -- --test-threads=1)

use i_slint_backend_testing::ElementHandle;
use keibidrop_rust::MainWindow;
use slint::ComponentHandle;
use std::cell::{Cell, RefCell};
use std::rc::Rc;

const WIN_W: u32 = 1144;
const WIN_H: u32 = 760;

fn app() -> MainWindow {
    i_slint_backend_testing::init_no_event_loop();
    let app = MainWindow::new().unwrap();
    app.window()
        .set_size(slint::PhysicalSize::new(WIN_W, WIN_H));
    app
}

// The one element with this label. Panics when missing or ambiguous.
fn one(app: &MainWindow, label: &str) -> ElementHandle {
    let mut it = ElementHandle::find_by_accessible_label(app, label);
    let first = it
        .next()
        .unwrap_or_else(|| panic!("no element labeled {:?}", label));
    assert!(
        it.next().is_none(),
        "more than one element labeled {:?}",
        label
    );
    first
}

// The one checkbox with this label; skips the row's plain-text twin.
fn toggle(app: &MainWindow, label: &str) -> ElementHandle {
    ElementHandle::find_by_accessible_label(app, label)
        .find(|e| e.accessible_checked().is_some())
        .unwrap_or_else(|| panic!("no toggle labeled {:?}", label))
}

fn assert_on_screen(el: &ElementHandle, what: &str) {
    let pos = el.absolute_position();
    let size = el.size();
    assert!(size.height > 0.0, "{} has zero height", what);
    assert!(pos.y >= 0.0, "{} starts above the window: y={}", what, pos.y);
    assert!(
        pos.y + size.height <= WIN_H as f32,
        "{} ends below the window: y={} h={}",
        what,
        pos.y,
        size.height
    );
}

#[test]
fn gear_opens_settings() {
    let app = app();
    assert!(!app.get_settings_visible());
    one(&app, "Open settings").invoke_accessible_default_action();
    assert!(app.get_settings_visible(), "gear did not open settings");
}

#[test]
fn settings_bottom_rows_reachable_at_default_size() {
    let app = app();
    app.set_settings_visible(true);
    assert_on_screen(&one(&app, "Buy relay credit"), "buy button");
    assert_on_screen(&one(&app, "Edit config file"), "edit-config button");
}

#[test]
fn settings_toggles_fire_their_callbacks() {
    let app = app();
    app.set_settings_visible(true);

    let fired = Rc::new(Cell::new(None::<bool>));
    let f = fired.clone();
    app.on_share_read_only_toggled(move |v| f.set(Some(v)));
    toggle(&app, "Share is read only").invoke_accessible_default_action();
    assert_eq!(fired.get(), Some(true), "share toggle callback");
    assert!(app.get_cfg_share_read_only(), "share toggle state");

    let f = fired.clone();
    app.on_mount_read_only_toggled(move |v| f.set(Some(v)));
    fired.set(None);
    toggle(&app, "Virtual folder is read only").invoke_accessible_default_action();
    assert_eq!(fired.get(), Some(true), "mount toggle callback");
    assert!(app.get_cfg_mount_read_only(), "mount toggle state");

    let f = fired.clone();
    app.on_preserve_metadata_toggled(move |v| f.set(Some(v)));
    fired.set(None);
    toggle(&app, "Keep original timestamps").invoke_accessible_default_action();
    assert_eq!(fired.get(), Some(true), "metadata toggle callback");
    assert!(app.get_cfg_preserve_metadata(), "metadata toggle state");
}

#[test]
fn toggle_invoked_twice_round_trips() {
    let app = app();
    app.set_settings_visible(true);
    let el = toggle(&app, "Share is read only");
    el.invoke_accessible_default_action();
    el.invoke_accessible_default_action();
    assert!(!app.get_cfg_share_read_only(), "toggle did not round-trip");
}

#[test]
fn edit_config_button_fires_open_config() {
    let app = app();
    app.set_settings_visible(true);
    let fired = Rc::new(Cell::new(false));
    let f = fired.clone();
    app.on_open_config(move || f.set(true));
    one(&app, "Edit config file").invoke_accessible_default_action();
    assert!(fired.get(), "edit-config did not fire open_config");
}

#[test]
fn update_notice_only_when_a_version_is_set() {
    let app = app();
    assert_eq!(
        ElementHandle::find_by_accessible_label(&app, "Update notice").count(),
        0,
        "notice shown with no update"
    );
    app.set_update_available("9.9.9".into());
    let fired = Rc::new(Cell::new(false));
    let f = fired.clone();
    app.on_open_update_page(move || f.set(true));
    let notice = one(&app, "Update notice");
    assert_on_screen(&notice, "update notice");
    notice.invoke_accessible_default_action();
    assert!(fired.get(), "notice click did not fire open_update_page");
}

#[test]
fn help_report_button_opens_feedback() {
    let app = app();
    app.set_help_visible(true);
    one(&app, "Report a problem").invoke_accessible_default_action();
    assert!(app.get_feedback_visible(), "feedback overlay did not open");
    assert!(!app.get_help_visible(), "help panel stayed open");
}

#[test]
fn feedback_send_passes_message_and_contact() {
    let app = app();
    app.set_feedback_visible(true);
    app.set_feedback_message("the mount hangs".into());
    app.set_feedback_contact("a@b.co".into());
    let got = Rc::new(RefCell::new(None::<(String, String)>));
    let g = got.clone();
    app.on_send_feedback(move |m, c| {
        *g.borrow_mut() = Some((m.to_string(), c.to_string()));
    });
    one(&app, "Send").invoke_accessible_default_action();
    assert_eq!(
        got.borrow().clone(),
        Some(("the mount hangs".to_string(), "a@b.co".to_string())),
        "send callback did not get the typed values"
    );
}

#[test]
fn feedback_send_disabled_on_empty_message() {
    let app = app();
    app.set_feedback_visible(true);
    let fired = Rc::new(Cell::new(false));
    let f = fired.clone();
    app.on_send_feedback(move |_, _| f.set(true));
    one(&app, "Send").invoke_accessible_default_action();
    assert!(!fired.get(), "send fired with an empty message");
}

#[test]
fn feedback_cancel_closes() {
    let app = app();
    app.set_feedback_visible(true);
    one(&app, "Cancel").invoke_accessible_default_action();
    assert!(!app.get_feedback_visible(), "cancel did not close feedback");
}

#[test]
fn fuse_offer_buttons_fire_accept_and_decline() {
    let app = app();
    app.set_fuse_offer_visible(true);
    app.set_fuse_offer_action("Turn on".into());

    let declined = Rc::new(Cell::new(false));
    let d = declined.clone();
    app.on_fuse_offer_declined(move || d.set(true));
    one(&app, "Not now").invoke_accessible_default_action();
    assert!(declined.get(), "Not now did not fire declined");

    let accepted = Rc::new(Cell::new(false));
    let a = accepted.clone();
    app.on_fuse_offer_accepted(move || a.set(true));
    one(&app, "Turn on").invoke_accessible_default_action();
    assert!(accepted.get(), "action button did not fire accepted");
}
