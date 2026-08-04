// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

// testpeer is a scriptable KeibiDrop peer for multi-process integration tests.
// It reads line-based commands from stdin and writes responses to stdout.
// Configuration is via environment variables.
package main

import (
	"bufio"
	"context"
	"crypto/md5" // #nosec G501 -- convergence checks, not security
	crand "crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/KeibiSoft/KeibiDrop/pkg/logic/common"
)

func main() {
	relayURL := os.Getenv("RELAY_URL")
	inbound, _ := strconv.Atoi(os.Getenv("INBOUND_PORT"))
	outbound, _ := strconv.Atoi(os.Getenv("OUTBOUND_PORT"))
	mountDir := os.Getenv("MOUNT_DIR")
	saveDir := os.Getenv("SAVE_DIR")
	useFUSE := os.Getenv("USE_FUSE") == "1"
	logFile := os.Getenv("LOG_FILE")

	wr := os.Stderr
	if logFile != "" {
		f, err := os.OpenFile(filepath.Clean(logFile),
			os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err == nil {
			wr = f
			defer f.Close()
		}
	}
	handler := slog.NewTextHandler(wr, &slog.HandlerOptions{Level: slog.LevelDebug})
	logger := slog.New(handler).With("component", "testpeer",
		"fuse", useFUSE, "in", inbound, "out", outbound)

	parsed, err := url.Parse(relayURL)
	if err != nil {
		fmt.Println("ERR:invalid relay URL: " + err.Error())
		os.Exit(1) //nolint:gocritic
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// prefetchOnOpen honors KEIBIDROP_PREFETCH_ON_OPEN (default off = on-demand) so
	// benchmarks can compare the on-demand vs push/prefetch read paths.
	prefetchOnOpen := os.Getenv("KEIBIDROP_PREFETCH_ON_OPEN") == "1"
	kd, err := common.NewKeibiDropWithIP(ctx, logger, useFUSE, parsed,
		inbound, outbound, mountDir, saveDir, prefetchOnOpen, false, "::1")
	if err != nil {
		fmt.Println("ERR:init failed: " + err.Error())
		os.Exit(1)
	}
	// live_collab toggle: when set, enable macFUSE auto_cache so a peer's
	// same-size in-place edit is seen live (at the cost of mmap-write integrity).
	kd.AutoCache = os.Getenv("KEIBIDROP_LIVE_COLLAB") == "1"

	go kd.Run()
	fmt.Println("READY")

	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		args := strings.Fields(line)
		if len(args) == 0 {
			continue
		}

		switch args[0] {
		case "fingerprint":
			fp, err := kd.ExportFingerprint()
			if err != nil {
				fmt.Println("ERR:" + err.Error())
			} else {
				fmt.Println("FP:" + fp)
			}

		case "register":
			if len(args) < 2 {
				fmt.Println("ERR:usage: register <fingerprint>")
				continue
			}
			if err := kd.AddPeerFingerprint(args[1]); err != nil {
				fmt.Println("ERR:" + err.Error())
			} else {
				fmt.Println("OK")
			}

		case "create":
			if err := kd.CreateRoom(); err != nil {
				fmt.Println("ERR:" + err.Error())
			} else {
				fmt.Println("CONNECTED")
			}

		case "join":
			if err := kd.JoinRoom(); err != nil {
				fmt.Println("ERR:" + err.Error())
			} else {
				fmt.Println("CONNECTED")
			}

		case "add":
			if len(args) < 2 {
				fmt.Println("ERR:usage: add <path>")
				continue
			}
			if err := kd.AddFile(strings.Join(args[1:], " ")); err != nil {
				fmt.Println("ERR:" + err.Error())
			} else {
				fmt.Println("OK")
			}

		case "list":
			remote, local := kd.ListFiles()
			for _, f := range remote {
				fmt.Println("REMOTE:" + f)
			}
			for _, f := range local {
				fmt.Println("LOCAL:" + f)
			}
			fmt.Println("END")

		case "wait_file":
			if len(args) < 3 {
				fmt.Println("ERR:usage: wait_file <name> <timeout_sec>")
				continue
			}
			timeout, _ := strconv.Atoi(args[2])
			if timeout <= 0 {
				timeout = 15
			}
			deadline := time.Now().Add(time.Duration(timeout) * time.Second)
			found := false
			for time.Now().Before(deadline) {
				p := filepath.Join(mountDir, args[1])
				if _, err := os.Stat(p); err == nil { // #nosec G703
					found = true
					break
				}
				time.Sleep(200 * time.Millisecond)
			}
			if found {
				fmt.Println("OK")
			} else {
				fmt.Println("ERR:timeout waiting for " + args[1])
			}

		case "read_file":
			if len(args) < 2 {
				fmt.Println("ERR:usage: read_file <name>")
				continue
			}
			p := filepath.Join(mountDir, args[1])
			data, err := os.ReadFile(p) // #nosec G304
			if err != nil {
				fmt.Println("ERR:" + err.Error())
			} else {
				fmt.Printf("DATA:%d:%s\n", len(data), string(data))
			}

		case "read_at":
			// read_at <name> <offset> <size> — ReadAt on the mounted file,
			// returns the bytes hex-encoded (binary-safe). For seek/scrub tests.
			if len(args) < 4 {
				fmt.Println("ERR:usage: read_at <name> <offset> <size>")
				continue
			}
			off, _ := strconv.ParseInt(args[2], 10, 64)
			sz, _ := strconv.Atoi(args[3])
			rf, oerr := os.Open(filepath.Join(mountDir, args[1])) // #nosec G304
			if oerr != nil {
				fmt.Println("ERR:" + oerr.Error())
				continue
			}
			rbuf := make([]byte, sz)
			rn, rerr := rf.ReadAt(rbuf, off)
			rf.Close()
			if rerr != nil && rerr != io.EOF {
				fmt.Println("ERR:" + rerr.Error())
				continue
			}
			fmt.Printf("HEX:%d:%s\n", rn, hex.EncodeToString(rbuf[:rn]))

		case "write_file":
			// write_file <name> <content>
			if len(args) < 3 {
				fmt.Println("ERR:usage: write_file <name> <content>")
				continue
			}
			p := filepath.Join(mountDir, args[1])
			content := strings.Join(args[2:], " ")
			if err := os.WriteFile(p, []byte(content), 0644); err != nil { //nolint:gosec // G306: shared file needs read access
				fmt.Println("ERR:" + err.Error())
			} else {
				fmt.Println("OK")
			}

		case "pull":
			if len(args) < 3 {
				fmt.Println("ERR:usage: pull <remote> <local>")
				continue
			}
			if err := kd.PullFile(args[1], args[2]); err != nil {
				fmt.Println("ERR:" + err.Error())
			} else {
				fmt.Println("OK")
			}

		case "mkdir_p":
			// mkdir_p <relative-path> — create directory tree on the mount (recursive)
			if len(args) < 2 {
				fmt.Println("ERR:usage: mkdir_p <path>")
				continue
			}
			p := filepath.Join(mountDir, strings.Join(args[1:], " "))
			if err := os.MkdirAll(p, 0o755); err != nil {
				fmt.Println("ERR:" + err.Error())
			} else {
				fmt.Println("OK")
			}

		case "list_dir":
			// list_dir <relative-path> — list direct children of a directory on the mount.
			// Prints ENTRY:<name> per child, then END.
			if len(args) < 2 {
				fmt.Println("ERR:usage: list_dir <path>")
				continue
			}
			p := filepath.Join(mountDir, strings.Join(args[1:], " "))
			entries, err := os.ReadDir(p)
			if err != nil {
				fmt.Println("ERR:" + err.Error())
			} else {
				for _, e := range entries {
					fmt.Println("ENTRY:" + e.Name())
				}
				fmt.Println("END")
			}

		case "exec":
			// exec <dir-relative-to-mount> <command> [args...]
			// Runs a shell command inside the FUSE mount. Returns EXEC:<exit_code>:<output>
			if len(args) < 3 {
				fmt.Println("ERR:usage: exec <dir> <command> [args...]")
				continue
			}
			dir := filepath.Join(mountDir, args[1])
			// cd AFTER execve, never via cmd.Dir: Go chdirs in the vfork
			// child BEFORE exec, inside OUR OWN mount. The vfork parent
			// keeps its P with preemption off; when a GC stop-the-world
			// is in flight the chdir's FUSE request is never served and
			// the whole peer freezes (core.4785 thread 16).
			var cmd *exec.Cmd
			if runtime.GOOS == "windows" {
				cmd = exec.Command(args[2], args[3:]...) // #nosec G204 -- test harness, args from stdin
				cmd.Dir = dir
			} else {
				shArgs := append([]string{"-c", `cd "$0" && exec "$@"`, dir}, args[2:]...)
				cmd = exec.Command("sh", shArgs...) // #nosec G204 G702 -- test harness, args from stdin
			}
			out, err := cmd.CombinedOutput()
			exitCode := 0
			if err != nil {
				if exitErr, ok := err.(*exec.ExitError); ok {
					exitCode = exitErr.ExitCode()
				} else {
					exitCode = -1
				}
			}
			// Replace newlines with \n literal for single-line protocol
			escaped := strings.ReplaceAll(strings.TrimRight(string(out), "\n"), "\n", "\\n")
			fmt.Printf("EXEC:%d:%s\n", exitCode, escaped)

		case "dlbytes":
			// dlbytes <rel> — bytes fetched from the peer for a tracked file.
			// Reads Download.BytesDownloaded; 0 when untracked.
			if len(args) < 2 {
				fmt.Println("ERR:usage: dlbytes <rel>")
				continue
			}
			rel := "/" + strings.TrimPrefix(args[1], "/")
			var n uint64
			if kd.FS != nil && kd.FS.Root != nil {
				kd.FS.Root.RemoteFilesLock.RLock()
				if f, ok := kd.FS.Root.RemoteFiles[rel]; ok {
					n = f.Download.BytesDownloaded.Load()
				}
				kd.FS.Root.RemoteFilesLock.RUnlock()
			}
			fmt.Printf("DLBYTES:%d\n", n)

		case "md5":
			// md5 <rel> — hash the file through our own mount (Windows has
			// no md5sum; verbs drive the same FUSE surface as the shell).
			if len(args) < 2 {
				fmt.Println("ERR:usage: md5 <rel>")
				continue
			}
			mf, merr := os.Open(filepath.Join(mountDir, args[1])) // #nosec G304
			if merr != nil {
				fmt.Println("ERR:" + merr.Error())
				continue
			}
			mh := md5.New() // #nosec G401 -- convergence check, not security
			_, merr = io.Copy(mh, mf)
			mf.Close()
			if merr != nil {
				fmt.Println("ERR:" + merr.Error())
				continue
			}
			fmt.Printf("MD5:%x\n", mh.Sum(nil))

		case "write_rand":
			// write_rand <rel> <bytes> — random content through the mount
			// (dd if=/dev/urandom replacement).
			if len(args) < 3 {
				fmt.Println("ERR:usage: write_rand <rel> <bytes>")
				continue
			}
			wn, werr := strconv.Atoi(args[2])
			if werr != nil || wn < 0 {
				fmt.Println("ERR:bad byte count")
				continue
			}
			wbuf := make([]byte, wn)
			_, _ = crand.Read(wbuf)
			if err := os.WriteFile(filepath.Join(mountDir, args[1]), wbuf, 0644); err != nil { //nolint:gosec // G306: shared file needs read access
				fmt.Println("ERR:" + err.Error())
			} else {
				fmt.Println("OK")
			}

		case "write_rand_at":
			// write_rand_at <rel> <offset> <bytes> — random overwrite at an
			// offset, size preserved (dd conv=notrunc replacement).
			if len(args) < 4 {
				fmt.Println("ERR:usage: write_rand_at <rel> <offset> <bytes>")
				continue
			}
			woff, _ := strconv.ParseInt(args[2], 10, 64)
			wan, waerr := strconv.Atoi(args[3])
			if waerr != nil || wan < 0 {
				fmt.Println("ERR:bad byte count")
				continue
			}
			wf, wferr := os.OpenFile(filepath.Join(mountDir, args[1]), os.O_WRONLY, 0644) // #nosec G304
			if wferr != nil {
				fmt.Println("ERR:" + wferr.Error())
				continue
			}
			wabuf := make([]byte, wan)
			_, _ = crand.Read(wabuf)
			_, wferr = wf.WriteAt(wabuf, woff)
			wf.Close()
			if wferr != nil {
				fmt.Println("ERR:" + wferr.Error())
			} else {
				fmt.Println("OK")
			}

		case "rename":
			// rename <old> <new> — rename inside the mount (mv -f replacement).
			if len(args) < 3 {
				fmt.Println("ERR:usage: rename <old> <new>")
				continue
			}
			if err := os.Rename(filepath.Join(mountDir, args[1]), filepath.Join(mountDir, args[2])); err != nil {
				fmt.Println("ERR:" + err.Error())
			} else {
				fmt.Println("OK")
			}

		case "copy":
			// copy <src> <dst> — byte copy through the mount (cp replacement).
			if len(args) < 3 {
				fmt.Println("ERR:usage: copy <src> <dst>")
				continue
			}
			cs, cerr := os.Open(filepath.Join(mountDir, args[1])) // #nosec G304
			if cerr != nil {
				fmt.Println("ERR:" + cerr.Error())
				continue
			}
			cd, cderr := os.Create(filepath.Join(mountDir, args[2])) // #nosec G304
			if cderr != nil {
				cs.Close()
				fmt.Println("ERR:" + cderr.Error())
				continue
			}
			_, cerr = io.Copy(cd, cs)
			cs.Close()
			if cclose := cd.Close(); cerr == nil {
				cerr = cclose
			}
			if cerr != nil {
				fmt.Println("ERR:" + cerr.Error())
			} else {
				fmt.Println("OK")
			}

		case "exec_sh":
			// exec_sh <dir-relative-to-mount> <shell-command-line>
			// The line runs through sh -c, so quotes and parens survive
			// (gimp script-fu batch lines break under space-splitting exec).
			if len(args) < 3 {
				fmt.Println("ERR:usage: exec_sh <dir> <command-line>")
				continue
			}
			dir := filepath.Join(mountDir, args[1])
			cmdline := strings.Join(args[2:], " ")
			// Same vfork hazard as "exec": cd inside the shell, not cmd.Dir.
			cmd := exec.Command("sh", "-c", fmt.Sprintf("cd %q && { %s; }", dir, cmdline)) // #nosec G204 G702 -- test harness, args from stdin
			out, err := cmd.CombinedOutput()
			exitCode := 0
			if err != nil {
				if exitErr, ok := err.(*exec.ExitError); ok {
					exitCode = exitErr.ExitCode()
				} else {
					exitCode = -1
				}
			}
			escaped := strings.ReplaceAll(strings.TrimRight(string(out), "\n"), "\n", "\\n")
			fmt.Printf("EXEC:%d:%s\n", exitCode, escaped)

		case "quit":
			_ = kd.UnmountFilesystem()
			cancel()
			fmt.Println("BYE")
			return

		default:
			fmt.Println("ERR:unknown command: " + args[0])
		}
	}
}
