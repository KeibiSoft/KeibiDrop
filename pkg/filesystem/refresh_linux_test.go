//go:build linux && !android

// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2025 KeibiSoft S.R.L.

package filesystem

import (
	"bufio"
	"context"
	"net"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/KeibiSoft/KeibiDrop/internal/testkit"
	"github.com/godbus/dbus/v5"
)

// fakeFileManager answers Properties.Get the way Nautilus publishes its windows.
type fakeFileManager struct{ wins map[string][]string }

func (f *fakeFileManager) Get(iface, prop string) (dbus.Variant, *dbus.Error) {
	if iface == "org.freedesktop.FileManager1" && prop == "OpenWindowsWithLocations" {
		return dbus.MakeVariant(f.wins), nil
	}
	return dbus.Variant{}, dbus.NewError("org.freedesktop.DBus.Error.UnknownProperty", nil)
}

// fakeWindow records the GTK actions activated on it.
type fakeWindow struct {
	mu      sync.Mutex
	actions []string
}

func (w *fakeWindow) Activate(name string, _ []dbus.Variant, _ map[string]dbus.Variant) *dbus.Error {
	w.mu.Lock()
	w.actions = append(w.actions, name)
	w.mu.Unlock()
	return nil
}

func (w *fakeWindow) got() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]string(nil), w.actions...)
}

// privateBus starts a session bus of its own; no dbus-daemon skips the test.
func privateBus(t *testing.T) string {
	t.Helper()
	bin, err := exec.LookPath("dbus-daemon")
	if err != nil {
		t.Skip("dbus-daemon not installed")
	}
	cmd := exec.Command(bin, "--session", "--nofork", "--print-address=1")
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Skipf("dbus-daemon did not start: %v", err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	addr, err := bufio.NewReader(out).ReadString('\n')
	if err != nil {
		t.Fatalf("no bus address: %v", err)
	}
	return strings.TrimSpace(addr)
}

func TestReloadNautilusReloadsOnlyWindowsOnAChangedFolder(t *testing.T) {
	addr := privateBus(t)
	svc, err := dbus.Connect(addr)
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()
	w1, w2 := &fakeWindow{}, &fakeWindow{}
	fm := &fakeFileManager{wins: map[string][]string{
		"/org/gnome/Nautilus/window/1": {"file:///home/me/Pictures", "file:///home/me/KeibiDrop/My%20Docs"},
		"/org/gnome/Nautilus/window/2": {"file:///home/me/KeibiDrop"},
	}}
	for path, obj := range map[dbus.ObjectPath]struct {
		v     any
		iface string
	}{
		"/org/freedesktop/FileManager1": {fm, "org.freedesktop.DBus.Properties"},
		"/org/gnome/Nautilus/window/1":  {w1, "org.gtk.Actions"},
		"/org/gnome/Nautilus/window/2":  {w2, "org.gtk.Actions"},
	} {
		if err := svc.Export(obj.v, path, obj.iface); err != nil {
			t.Fatal(err)
		}
	}
	if reply, err := svc.RequestName("org.freedesktop.FileManager1", dbus.NameFlagDoNotQueue); err != nil || reply != dbus.RequestNameReplyPrimaryOwner {
		t.Fatalf("RequestName = %v, %v", reply, err)
	}

	client, err := dbus.Connect(addr)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	reloadNautilus(ctx, client, changedDirs("/home/me/KeibiDrop", map[string]PeerChange{"/My Docs/a.txt": PeerAdded}))

	if got := w1.got(); len(got) != 1 || got[0] != "reload" {
		t.Fatalf("window 1 (shows the changed folder) got %v, want [reload]", got)
	}
	if got := w2.got(); len(got) != 0 {
		t.Fatalf("window 2 (shows another folder) got %v, want nothing", got)
	}
}

// No file manager on the bus: nothing is called, and none is started.
func TestReloadNautilusWithoutAFileManager(t *testing.T) {
	client, err := dbus.Connect(privateBus(t))
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	reloadNautilus(ctx, client, []string{"/home/me/KeibiDrop"})
	var has bool
	if err := client.BusObject().Call("org.freedesktop.DBus.NameHasOwner", 0, "org.freedesktop.FileManager1").Store(&has); err != nil || has {
		t.Fatalf("NameHasOwner(FileManager1) = %v, %v; want false", has, err)
	}
}

// dbusGoroutines counts the goroutines running godbus code.
func dbusGoroutines() int {
	buf := make([]byte, 1<<20)
	n := 0
	for _, g := range strings.Split(string(buf[:runtime.Stack(buf, true)]), "\n\n") {
		if strings.Contains(g, "github.com/godbus/dbus") {
			n++
		}
	}
	return n
}

// A bus that accepts the connection and never answers the handshake must not
// hold the refresher goroutine past the flush deadline, and must leave no
// D-Bus goroutine behind.
func TestRefreshAgainstABusThatNeverAnswers(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "bus")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var held []net.Conn // accepted and never answered
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			held = append(held, c)
			mu.Unlock()
		}
	}()
	t.Cleanup(func() {
		_ = ln.Close()
		mu.Lock()
		for _, c := range held {
			_ = c.Close()
		}
		mu.Unlock()
	})
	t.Setenv("DBUS_SESSION_BUS_ADDRESS", "unix:path="+sock)

	before := dbusGoroutines()
	flush := newTestFS().platformRefresh(t.TempDir())
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	if err := testkit.Within(3*time.Second, "a flush against a silent bus", func() error {
		flush(ctx, map[string]PeerChange{"/a.txt": PeerAdded})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if took := time.Since(start); took > 2*time.Second {
		t.Fatalf("flush took %s against a silent bus", took)
	}
	testkit.Eventually(t, 2*time.Second, 20*time.Millisecond, func() bool {
		return dbusGoroutines() <= before
	}, "the D-Bus goroutines to exit")
}
