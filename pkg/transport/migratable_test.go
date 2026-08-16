// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package transport

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/KeibiSoft/KeibiDrop/internal/fp"
	"github.com/KeibiSoft/KeibiDrop/internal/testkit"
)

// TestDialQUICMigratable checks that after Migrate the local address changes and the
// same stream keeps carrying bytes, with no re-dial.
func TestDialQUICMigratable(t *testing.T) {
	testkit.Run(t, func() error {
		ln := testkit.Must(QUIC().Listen("127.0.0.1:0"))
		defer ln.Close()

		go func() {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			defer c.Close()
			_, _ = io.Copy(c, c)
		}()

		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		mc := testkit.Must(DialQUICMigratable(ctx, ln.Addr().String()))
		defer mc.Close()

		echo := func(msg string) error {
			testkit.Must(mc.Write([]byte(msg)))
			buf := make([]byte, len(msg))
			testkit.Must(io.ReadFull(mc, buf))
			return fp.Equal("echo", string(buf), msg)
		}

		if err := echo("before-migration"); err != nil {
			return err
		}

		oldAddr, newAddr := testkit.Must2(mc.Migrate(ctx))
		if err := fp.NotEqual("local address after Migrate", newAddr.String(), oldAddr.String()); err != nil {
			return err
		}
		t.Logf("migrated %v -> %v", oldAddr, newAddr)

		return echo("after-migration") // same conn, same stream, new path
	})
}
