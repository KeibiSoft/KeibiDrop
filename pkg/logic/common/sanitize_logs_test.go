// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2026 KeibiSoft S.R.L.

package common

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Lines shaped like the desktop log (slog text). The sanitized log goes out
// with a problem report, so a name that survives here leaves the machine.
func TestSanitizeLogContent(t *testing.T) {
	t.Setenv("HOME", "/Users/tester")
	fp := strings.Repeat("Ab3_-", 17) + "x"
	cases := []struct{ name, in, want string }{
		{"quoted path with spaces",
			`level=DEBUG msg="Local change queued" action=2 path="/Team Call 2025-01-02 at 10.00.00.mov"`,
			`level=DEBUG msg="Local change queued" action=2 path="/<redacted>.mov"`},
		{"bare path keeps safe folders and the extension",
			`msg=x path=/Users/tester/Documents/report.pdf`,
			`msg=x path=/<redacted>/<redacted>/Documents/<redacted>.pdf`},
		{"rename keys",
			`msg="Rename file" oldPath="/a b/c.txt" newPath=/a/d.txt`,
			`msg="Rename file" oldPath="/<redacted>/<redacted>.txt" newPath=/<redacted>/<redacted>.txt`},
		{"timestamps stay",
			`time=2026-09-16T07:41:23.733+03:00 level=INFO msg=ok`,
			`time=2026-09-16T07:41:23.733+03:00 level=INFO msg=ok`},
		{"states stay in from and to",
			`msg="Connection health changed" from=connected to=degraded dir=initiate`,
			`msg="Connection health changed" from=connected to=degraded dir=initiate`},
		{"paths in from and to",
			`msg="Renamed file on disk" from=/x/a.txt to="/x/b c.txt"`,
			`msg="Renamed file on disk" from=/<redacted>/<redacted>.txt to="/<redacted>/<redacted>.txt"`},
		{"addresses",
			`remote=198.51.100.7:26600 peer=[2001:db8::1]:26001 ll=fe80::1%en0 mac=aa:bb:cc:dd:ee:ff`,
			`remote=<ip-redacted> peer=<ip-redacted> ll=<ip-redacted> mac=<ip-redacted>`},
		{"device names and relay tokens",
			`msg="Discovery started" name="Tester's MacBook" port=26001 token=0a1b2c3d..4e5f6a7b hostname=testers-mbp.local`,
			`msg="Discovery started" name="<redacted>" port=26001 token=<redacted> hostname=<redacted>`},
		{"fingerprint",
			`fingerprint=` + fp + ` peer=Ab12Cd34`,
			`fingerprint=<fingerprint-redacted> peer=Ab12Cd34`},
		{"home folder and a path under it in free text",
			`configDir=/private/tmp/app-501/-Users-tester-work/x msg="saved to /Users/tester/Received/movie.mov"`,
			`configDir=/<redacted>/tmp/<redacted>/<redacted>/<redacted> msg="saved to <home>/Received/<redacted>.mov"`},
		{"account name outside the home folder",
			`msg="temp at /var/folders/-Users-tester-x"`,
			`msg="temp at /var/folders/-Users-<user>-x"`},
		{"a dot is not always an extension",
			`path="/Report v2.0 final"`,
			`path="/<redacted>"`},
		{"keys inside other keys",
			`volname=KeibiDrop remoteName=/x/y.pdf`,
			`volname=KeibiDrop remoteName=/<redacted>/<redacted>.pdf`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := SanitizeLogContent(c.in); got != c.want {
				t.Errorf("\n in   %s\n got  %s\n want %s", c.in, got, c.want)
			}
		})
	}
}

// The newest lines across the rotated file and the current one, sanitized.
func TestSanitizedLogTailReadsAcrossTheRotation(t *testing.T) {
	t.Setenv("HOME", "/Users/tester")
	dir := t.TempDir()
	log := filepath.Join(dir, "keibidrop.log")
	older := "time=1 msg=a\ntime=2 msg=b remote=10.0.0.7:26001\n"
	newer := "time=3 msg=c path=/x/secret.pdf\n"
	if err := os.WriteFile(log+".1", []byte(older), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(log, []byte(newer), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := SanitizedLogTail(log, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	want := "time=1 msg=a\ntime=2 msg=b remote=<ip-redacted>\ntime=3 msg=c path=/<redacted>/<redacted>.pdf\n"
	if got != want {
		t.Errorf("got  %q\nwant %q", got, want)
	}

	// A smaller cap keeps the newest whole lines only.
	got, err = SanitizedLogTail(log, len(newer)+20)
	if err != nil {
		t.Fatal(err)
	}
	if got != "time=3 msg=c path=/<redacted>/<redacted>.pdf\n" {
		t.Errorf("capped tail = %q", got)
	}
}
