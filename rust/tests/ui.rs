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
const WIN_H: u32 = 729; // Figma frames are 1144x729

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
fn menu_opens_settings() {
    let app = app();
    assert!(!app.get_settings_visible());
    one(&app, "Menu").invoke_accessible_default_action();
    one(&app, "Open settings").invoke_accessible_default_action();
    assert!(app.get_settings_visible(), "menu did not open settings");
    assert!(!app.get_menu_open(), "menu stayed open over settings");
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
    toggle(&app, "Teleport Folder is read only").invoke_accessible_default_action();
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
    one(&app, "Question?").invoke_accessible_default_action();
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
    app.on_send_feedback(move |m, c, _rating, _logs| {
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
    app.on_send_feedback(move |_, _, _, _| f.set(true));
    one(&app, "Send").invoke_accessible_default_action();
    assert!(!fired.get(), "send fired with an empty message");
}

// Logs go with a report only when the person switches them on.
#[test]
fn include_logs_goes_only_when_switched_on() {
    let app = app();
    app.set_feedback_visible(true);
    app.set_feedback_message("it hangs".into());
    let got = Rc::new(RefCell::new(Vec::<bool>::new()));
    let g = got.clone();
    app.on_send_feedback(move |_, _, _, logs| g.borrow_mut().push(logs));
    one(&app, "Send").invoke_accessible_default_action();
    toggle(&app, "Include logs").invoke_accessible_default_action();
    one(&app, "Send").invoke_accessible_default_action();
    assert_eq!(got.borrow().as_slice(), [false, true]);
}

// Save log is gone from the menus: logs ride with Report problem instead.
#[test]
fn no_menu_offers_save_log() {
    let app = app();
    for screen in 0..4 {
        app.set_current_screen(screen);
        app.set_menu_open(true);
        assert_eq!(ElementHandle::find_by_accessible_label(&app, "Save log").count(), 0, "screen {}", screen);
    }
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

#[test]
fn invite_link_button_fires_and_fits_the_window() {
    let app = app();
    let fired = Rc::new(Cell::new(false));
    let f = fired.clone();
    app.on_copy_invite_link(move || f.set(true));
    let btn = one(&app, "Copy invite link");
    assert_on_screen(&btn, "invite link button");
    btn.invoke_accessible_default_action();
    assert!(fired.get(), "invite button did not fire copy_invite_link");
}

// The connect screen is absolutely positioned, so a new element can land on
// top of an existing one and no test would notice. The first placement of this
// button covered the contacts panel, which starts at y=636 (Figma Screen 12).
#[test]
fn invite_link_button_clears_the_contacts_panel() {
    const CONTACTS_TOP: f32 = 636.0;
    let app = app();
    let btn = one(&app, "Copy invite link");
    let bottom = btn.absolute_position().y + btn.size().height;
    assert!(
        bottom <= CONTACTS_TOP,
        "invite button ends at y={} and covers the contacts panel at y={}",
        bottom,
        CONTACTS_TOP
    );

    let copy = one(&app, "Copy my code");
    let a = (btn.absolute_position(), btn.size());
    let b = (copy.absolute_position(), copy.size());
    let overlap = a.0.x < b.0.x + b.1.width
        && b.0.x < a.0.x + a.1.width
        && a.0.y < b.0.y + b.1.height
        && b.0.y < a.0.y + a.1.height;
    assert!(!overlap, "the two copy actions overlap each other");
}

// Caught in the cold install test of 2026-09-15: the button stopped short of
// the input above it, which reads as a misaligned control. Card 2's content
// row runs to x=796 in Figma Screen 12, the right edge of the Copy button.
#[test]
fn invite_button_ends_at_the_card_content_edge() {
    const CONTENT_RIGHT: f32 = 796.0;
    let app = app();
    let btn = one(&app, "Copy invite link");
    let right = btn.absolute_position().x + btn.size().width;
    assert!(
        (right - CONTENT_RIGHT).abs() < 1.0,
        "invite button ends at x={}, the card content edge is x={}",
        right,
        CONTENT_RIGHT
    );
}

#[test]
fn invite_link_button_hidden_in_local_mode() {
    let app = app();
    app.set_local_mode(true);
    assert_eq!(
        ElementHandle::find_by_accessible_label(&app, "Copy invite link").count(),
        0,
        "invite link offered on the LAN path, where no code is needed"
    );
}

#[test]
fn connect_takes_a_typed_code_without_add() {
    let app = app();
    let fired = Rc::new(Cell::new(false));
    let f = fired.clone();
    app.on_connect_pressed(move || f.set(true));

    // Nothing typed and nothing added: Connect stays inert.
    one(&app, "Connect").invoke_accessible_default_action();
    assert!(!fired.get(), "Connect fired with no code at all");

    // Typed but not added through the Add button: Connect still works.
    app.set_peer_code("a-code-from-a-chat".into());
    one(&app, "Connect").invoke_accessible_default_action();
    assert!(fired.get(), "Connect ignored a typed code");
}

#[test]
fn waiting_line_names_what_is_missing() {
    let app = app();
    let waiting = "Waiting for the other side. They need your code too.";
    assert_eq!(
        ElementHandle::find_by_accessible_label(&app, waiting).count(),
        0,
        "waiting line shown before connecting"
    );
    app.set_room_action(1);
    assert_eq!(
        ElementHandle::find_by_accessible_label(&app, waiting).count(),
        1,
        "no waiting line while connecting"
    );
}

// ---------------------------------------------------------------------------
// Direct transfer (no FUSE): two peers connect for the first time, one file
// from the peer is on the card, the person clicks Save. The card's button has
// no accessible label, so it is driven the way a mouse drives it: pointer
// press and release at its centre. save_file is the boundary the Rust side
// owns (main.rs on_save_file marks the download and calls the engine in a
// thread). The card itself changes only when the file watcher thread rebuilds
// the list model, which it does every 500 ms (main.rs start_file_watcher).

use slint::platform::{PointerEventButton, WindowEvent};

fn remote_file(name: &str) -> keibidrop_rust::FileInfo {
    keibidrop_rust::FileInfo {
        name: name.into(),
        label: name.rsplit('/').next().unwrap_or(name).into(),
        size_bytes: 160_423,
        downloading: false,
        uploading: false,
        progress: 0.0,
        saved: false,
        paused: false,
        file_type: "image".into(),
        is_local: false,
        viewable: false,
    }
}

// What the watcher thread does on every tick (main.rs start_file_watcher).
fn set_files(app: &MainWindow, names: &[&str]) {
    let rows: Vec<keibidrop_rust::FileInfo> = names.iter().map(|n| remote_file(n)).collect();
    keibidrop_rust::sync_file_list(app, rows);
}

fn connected_no_fuse(app: &MainWindow) {
    app.set_fuse_mode(false);
    app.set_current_screen(1);
    set_files(app, &["signal.jpeg"]);
}

// The card's action button (Save, Open, the percent while it downloads): 110px,
// or 80px beside X. X is a SmallOutlineButton.
fn save_button(app: &MainWindow) -> ElementHandle {
    let card = ElementHandle::find_by_element_type_name(app, "FileCard")
        .next()
        .expect("no FileCard on the connected screen");
    card.query_descendants()
        .match_type_name("OutlineButton")
        .find_all()
        .into_iter()
        .find(|b| b.size().width > 70.0)
        .expect("no Save button in the card")
}

fn button_text(el: &ElementHandle) -> String {
    el.query_descendants()
        .match_inherits("Text")
        .find_first()
        .and_then(|t| t.accessible_label())
        .map(|s| s.to_string())
        .unwrap_or_default()
}

fn centre(el: &ElementHandle) -> slint::LogicalPosition {
    let p = el.absolute_position();
    let s = el.size();
    slint::LogicalPosition::new(p.x + s.width / 2.0, p.y + s.height / 2.0)
}

fn press(app: &MainWindow, at: slint::LogicalPosition) {
    app.window().dispatch_event(WindowEvent::PointerMoved { position: at });
    app.window().dispatch_event(WindowEvent::PointerPressed {
        position: at,
        button: PointerEventButton::Left,
    });
}

fn release(app: &MainWindow, at: slint::LogicalPosition) {
    app.window().dispatch_event(WindowEvent::PointerReleased {
        position: at,
        button: PointerEventButton::Left,
    });
}

fn record_saves(app: &MainWindow) -> Rc<RefCell<Vec<String>>> {
    let saves = Rc::new(RefCell::new(Vec::<String>::new()));
    let seen = saves.clone();
    app.on_save_file(move |name| seen.borrow_mut().push(name.to_string()));
    saves
}

#[test]
fn first_save_click_fires_once_and_leaves_the_card_to_the_handler() {
    let app = app();
    connected_no_fuse(&app);
    let saves = record_saves(&app);

    let btn = save_button(&app);
    assert_eq!(button_text(&btn), "Save");
    let at = centre(&btn);
    press(&app, at);
    release(&app, at);

    assert_eq!(saves.borrow().as_slice(), ["signal.jpeg"], "one click, one save");
    // The .slint layer changes nothing by itself; the cue is the handler's job
    // (main.rs on_save_file calls mark_row_downloading), pinned below.
    assert_eq!(button_text(&save_button(&app)), "Save", "the card waits for the handler");
}

#[test]
fn marking_a_row_downloading_shows_the_cue_at_once() {
    let app = app();
    connected_no_fuse(&app);
    assert_eq!(button_text(&save_button(&app)), "Save");

    assert!(keibidrop_rust::mark_row_downloading(&app.get_file_list(), "signal.jpeg"));
    assert_eq!(button_text(&save_button(&app)), "0%", "progress shows on the click itself");
    let card = ElementHandle::find_by_element_type_name(&app, "FileCard").next().unwrap();
    assert!(card.query_descendants().match_type_name("ProgressRing").find_first().is_some(), "progress ring on the card");

    assert!(!keibidrop_rust::mark_row_downloading(&app.get_file_list(), "missing.bin"), "unknown name touches nothing");
}

#[test]
fn save_click_across_a_list_rebuild() {
    let app = app();
    connected_no_fuse(&app);
    let saves = record_saves(&app);

    let at = centre(&save_button(&app));
    press(&app, at);
    // The watcher tick lands between press and release.
    set_files(&app, &["signal.jpeg"]);
    release(&app, at);

    eprintln!("RECORD save_file calls with a rebuild mid-click: {:?}", saves.borrow());
    assert_eq!(saves.borrow().as_slice(), ["signal.jpeg"], "a click that spans a list rebuild");
}

#[test]
fn save_click_after_a_list_rebuild() {
    let app = app();
    connected_no_fuse(&app);
    let saves = record_saves(&app);

    // A tick before the click: the card is a new instance, the click is whole.
    set_files(&app, &["signal.jpeg"]);
    let at = centre(&save_button(&app));
    press(&app, at);
    release(&app, at);
    assert_eq!(saves.borrow().as_slice(), ["signal.jpeg"]);
}

// The logo and the "..." pill sit at one place and size on every screen, and
// a wider window centres the Figma column around them rather than leaving
// them at the left.
#[test]
fn logo_and_menu_pill_keep_their_place_on_every_screen_and_size() {
    let app = app();
    let header = |app: &MainWindow| {
        let (menu, logo) = (one(app, "Menu"), one(app, "KEIBIDROP"));
        (menu.absolute_position(), menu.size(), logo.absolute_position(), logo.size())
    };
    for (w, h) in [(WIN_W, WIN_H), (1600, 1000)] {
        app.window().set_size(slint::PhysicalSize::new(w, h));
        let at: Vec<_> = (0..4)
            .map(|screen| {
                app.set_current_screen(screen);
                header(&app)
            })
            .collect();
        assert!(at.iter().all(|p| *p == at[0]), "{}x{}: the header moves between screens: {:?}", w, h, at);
        if w != WIN_W {
            app.window().set_size(slint::PhysicalSize::new(WIN_W, WIN_H));
            app.set_current_screen(0);
            let base = header(&app);
            let shift = (w - WIN_W) as f32 / 2.0;
            assert_eq!(at[0].0.x - base.0.x, shift, "the pill's column is not centred");
            assert_eq!(at[0].2.x - base.2.x, shift, "the logo's column is not centred");
            assert_eq!((at[0].0.y, at[0].2.y), (base.0.y, base.2.y), "the header moved down");
        }
    }
}

// Her Teleport card (Figma Screen 14): its X ends the session.
#[test]
fn the_teleport_card_x_disconnects() {
    let app = app();
    app.set_current_screen(2);
    let fired = Rc::new(Cell::new(false));
    let f = fired.clone();
    app.on_disconnect_pressed(move || f.set(true));
    one(&app, "Disconnect").invoke_accessible_default_action();
    assert!(fired.get(), "the card's X did not disconnect");
}

#[test]
fn how_it_works_opens_help_on_the_teleport_screen() {
    let app = app();
    app.set_current_screen(2);
    one(&app, "How it works?").invoke_accessible_default_action();
    assert!(app.get_help_visible(), "help did not open");
}

// The Teleport switch keeps its job; while it is off it names her mode.
#[test]
fn the_teleport_switch_says_direct_transfer_while_off() {
    let app = app();
    app.set_fuse_available(true);
    app.set_fuse_mode(false);
    let count = |label: &str| ElementHandle::find_by_accessible_label(&app, label).count();
    assert_eq!((count("Direct transfer"), count("Select files to send")), (1, 1));
    toggle(&app, "Teleport").invoke_accessible_default_action();
    assert!(app.get_fuse_mode(), "the switch did not turn Teleport on");
    assert_eq!(count("Direct transfer"), 0, "Direct transfer still shown with Teleport on");
    assert_eq!(count("Instantly open shared files"), 1);
}

#[test]
fn add_ring_opens_the_picker() {
    let app = app();
    connected_no_fuse(&app);
    let picked = Rc::new(Cell::new(0));
    let p = picked.clone();
    app.on_add_file_pressed(move || p.set(p.get() + 1));
    one(&app, "Add files").invoke_accessible_default_action();
    assert_eq!(picked.get(), 1);
}

// While a file downloads its action shows the percent; a click pauses it.
#[test]
fn a_download_pauses_from_its_card() {
    let app = app();
    connected_no_fuse(&app);
    let paused = Rc::new(RefCell::new(Vec::<String>::new()));
    let seen = paused.clone();
    app.on_pause_file(move |name| seen.borrow_mut().push(name.to_string()));
    assert!(keibidrop_rust::mark_row_downloading(&app.get_file_list(), "signal.jpeg"));

    let at = centre(&save_button(&app));
    press(&app, at);
    release(&app, at);
    assert_eq!(paused.borrow().as_slice(), ["signal.jpeg"]);
}

// The grid makes cards for the rows in view only (a folder of 12,789 files
// froze it) and hands them to other files as it scrolls: each file must still
// land in its own row and column.
#[test]
fn a_scrolled_grid_shows_each_file_in_its_place() {
    let app = app();
    connected_no_fuse(&app);
    let names: Vec<String> = (0..1000).map(|i| format!("f{i:04}.txt")).collect();
    set_files(&app, &names.iter().map(String::as_str).collect::<Vec<_>>());
    let at = |name: &str| ElementHandle::find_by_accessible_label(&app, name).next().map(|e| e.absolute_position());

    let (top_left, top_fifth) = (at("f0000.txt").expect("first file"), at("f0004.txt").expect("fifth file"));
    assert!(at("f0040.txt").is_none(), "row 10 is out of view");

    // Ten rows down: the 41st file takes the first file's place.
    app.window().dispatch_event(WindowEvent::PointerScrolled {
        position: slint::LogicalPosition::new(500.0, 500.0),
        delta_x: 0.0,
        delta_y: -1830.0,
    });
    assert!(at("f0000.txt").is_none(), "row 0 still in view");
    assert_eq!(at("f0040.txt"), Some(top_left), "41st file not at the top left");
    assert_eq!(at("f0044.txt"), Some(top_fifth), "45th file not under it");
}

#[test]
fn save_all_is_in_the_menu_on_the_no_fuse_screen() {
    let app = app();
    connected_no_fuse(&app);
    let fired = Rc::new(Cell::new(false));
    let f = fired.clone();
    app.on_save_all_pressed(move || f.set(true));
    one(&app, "Menu").invoke_accessible_default_action();
    // Her order: Disconnect first.
    let row = |label: &str| one(&app, label).absolute_position().y;
    assert!(row("Disconnect") < row("Save all"), "Disconnect is not the first row");
    one(&app, "Save all").invoke_accessible_default_action();
    assert!(fired.get(), "Save all did not fire save_all_pressed");
    assert!(!app.get_menu_open(), "the menu stayed open");
}

// A connected screen's help has no line about fingerprints, and its card
// closes up under the steps: no gap where the connect screen explains contacts.
#[test]
fn connected_help_drops_the_fingerprint_line_and_the_gap() {
    let app = app();
    app.set_help_visible(true);
    let line = "Exchange fingerprints to establish a secure connection";
    let count = |app: &MainWindow| ElementHandle::find_by_accessible_label(app, line).count();
    assert_eq!(count(&app), 1, "the connect screen's help lost its line");
    let on_connect = one(&app, "Question?").absolute_position().y;
    app.set_current_screen(2);
    assert_eq!(count(&app), 0, "a connected screen's help still talks about fingerprints");
    let on_teleport = one(&app, "Question?").absolute_position().y;
    assert_eq!(on_connect - on_teleport, 74.0, "the card did not close up");
}

// The first-connect offer: Save keeps the friend, No thanks tells
// Rust not to offer them again; both close it.
#[test]
fn save_contact_offer_saves_or_declines() {
    let app = app();
    app.set_current_screen(2);
    let declined = Rc::new(Cell::new(0));
    let d = declined.clone();
    app.on_save_contact_declined(move || d.set(d.get() + 1));
    app.set_save_contact_visible(true);
    one(&app, "No thanks").invoke_accessible_default_action();
    assert_eq!(declined.get(), 1, "No thanks did not reach Rust");
    assert!(!app.get_save_contact_visible(), "No thanks left the offer open");

    let saved = Rc::new(RefCell::new(Vec::<String>::new()));
    let s = saved.clone();
    app.on_save_peer_as_contact(move |n| s.borrow_mut().push(n.to_string()));
    app.set_save_contact_visible(true);
    app.set_save_contact_name("Ana".into());
    one(&app, "Save").invoke_accessible_default_action();
    assert_eq!(saved.borrow().as_slice(), ["Ana"]);
    assert!(!app.get_save_contact_visible(), "Save left the offer open");
    assert_eq!(declined.get(), 1, "Save counted as a decline");
}

// Her folder and its title open the Teleport Folder, as Open Folder does.
#[test]
fn the_teleport_folder_and_title_open_the_folder() {
    let app = app();
    app.set_current_screen(2);
    let opened = Rc::new(Cell::new(0));
    let o = opened.clone();
    app.on_open_folder_pressed(move || o.set(o.get() + 1));
    one(&app, "Open Teleport Folder").invoke_accessible_default_action();
    one(&app, "Open Folder").invoke_accessible_default_action();
    assert_eq!(opened.get(), 2);
}
