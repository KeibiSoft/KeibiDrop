//go:build linux && !android

// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package filesystem

import (
	"context"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"github.com/godbus/dbus/v5"
)

// platformRefresh: inotify sees no change the daemon makes, so Nautilus runs
// its own reload (the F5 a person presses) on each window that shows a
// changed folder, over the session bus. It never starts a bus or Nautilus.
// Nautilus 46 and older export reload as a window action; from 47 it is a
// slot action that D-Bus cannot reach, and the call does nothing.
func (fs *FS) platformRefresh(mountPoint string) func(context.Context, map[string]PeerChange) {
	return func(mountCtx context.Context, batch map[string]PeerChange) {
		addr := sessionBusAddress()
		if addr == "" {
			return
		}
		ctx, cancel := context.WithTimeout(mountCtx, 5*time.Second)
		defer cancel()
		// The deadline covers the handshake too: a bus that never answers
		// must not hold the refresher goroutine.
		conn, err := dbus.Connect(addr, dbus.WithContext(ctx))
		if err != nil {
			return
		}
		defer conn.Close()
		reloadNautilus(ctx, conn, changedDirs(mountPoint, batch))
	}
}

// sessionBusAddress is the bus of the person's session, or "" (a NAS
// container, a service). It never autolaunches one.
func sessionBusAddress() string {
	if a := os.Getenv("DBUS_SESSION_BUS_ADDRESS"); a != "" {
		return a
	}
	if rt := os.Getenv("XDG_RUNTIME_DIR"); rt != "" {
		if _, err := os.Stat(filepath.Join(rt, "bus")); err == nil {
			return "unix:path=" + filepath.Join(rt, "bus")
		}
	}
	return ""
}

// reloadNautilus activates "reload" on every Nautilus window that shows one of
// dirs. Nautilus publishes its windows as OpenWindowsWithLocations (window
// object path -> location URIs) on org.freedesktop.FileManager1. The owner is
// asked for by name first: a call to the name itself would start Nautilus.
func reloadNautilus(ctx context.Context, conn *dbus.Conn, dirs []string) {
	var owner string
	if err := conn.BusObject().CallWithContext(ctx, "org.freedesktop.DBus.GetNameOwner", 0, "org.freedesktop.FileManager1").Store(&owner); err != nil {
		return
	}
	var v dbus.Variant
	if err := conn.Object(owner, "/org/freedesktop/FileManager1").CallWithContext(ctx, "org.freedesktop.DBus.Properties.Get", 0,
		"org.freedesktop.FileManager1", "OpenWindowsWithLocations").Store(&v); err != nil {
		return
	}
	var wins map[string][]string
	if err := v.Store(&wins); err != nil {
		return
	}
	want := make(map[string]struct{}, len(dirs))
	for _, d := range dirs {
		want[filepath.Clean(d)] = struct{}{}
	}
	for win, uris := range wins {
		if !dbus.ObjectPath(win).IsValid() {
			continue
		}
		for _, u := range uris {
			pu, err := url.Parse(u)
			if err != nil || pu.Scheme != "file" {
				continue
			}
			if _, ok := want[filepath.Clean(pu.Path)]; ok {
				conn.Object(owner, dbus.ObjectPath(win)).CallWithContext(ctx, "org.gtk.Actions.Activate", 0,
					"reload", []dbus.Variant{}, map[string]dbus.Variant{})
				break
			}
		}
	}
}
