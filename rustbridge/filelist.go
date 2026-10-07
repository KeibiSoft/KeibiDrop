// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026 KeibiSoft S.R.L.

package main

import "C"

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/KeibiSoft/KeibiDrop/pkg/logic/common"
	synctracker "github.com/KeibiSoft/KeibiDrop/pkg/sync-tracker"
)

// The app's file grid reads the lists through these. It polls KD_FileListStamp
// (no lock) and reads both lists with KD_ListAllFiles only when it moved. Before,
// each poll made two calls per file and each call sorted every name again:
// 12,789 shared files kept a core busy with nothing changing.

// pruneLocalAfter throttles PruneStaleLocalFiles, which checks every shared
// file on disk, to once per 10 s rather than once per poll.
var pruneLocalAfter atomic.Int64

func pruneLocalFilesThrottled(st *synctracker.SyncTracker) {
	now := time.Now().UnixNano()
	next := pruneLocalAfter.Load()
	if now < next || !pruneLocalAfter.CompareAndSwap(next, now+int64(10*time.Second)) {
		return
	}
	st.PruneStaleLocalFiles()
}

// KD_FileListStamp changes whenever either file list may have changed.
//
//export KD_FileListStamp
func KD_FileListStamp() C.ulonglong {
	if kd == nil || kd.SyncTracker == nil {
		return 0
	}
	pruneLocalFilesThrottled(kd.SyncTracker)
	return C.ulonglong(synctracker.ListWrites())
}

// KD_ListAllFiles returns both lists in one call, one file per line, sorted by
// name within each list: the peer's files "R\t<size>\t<name>", then ours
// "L\t<size>\t<name>". Names are escaped (listEscape). One sort per call,
// where the per-index getters sorted every name once per file. Free the
// result with free().
//
//export KD_ListAllFiles
func KD_ListAllFiles() *C.char {
	if kd == nil || kd.SyncTracker == nil {
		return C.CString("")
	}
	st := kd.SyncTracker
	// One read lock at a time, never one inside the other; names and sizes
	// are copied under each.
	st.RemoteFilesMu.RLock()
	remote := sortedSizes(st.RemoteFiles)
	st.RemoteFilesMu.RUnlock()
	st.LocalFilesMu.RLock()
	local := sortedSizes(st.LocalFiles)
	st.LocalFilesMu.RUnlock()

	var b strings.Builder
	for _, list := range []struct {
		tag   string
		files []nameSize
	}{{"R\t", remote}, {"L\t", local}} {
		for _, f := range list.files {
			b.WriteString(list.tag)
			b.WriteString(strconv.FormatUint(f.size, 10))
			b.WriteByte('\t')
			b.WriteString(listEscape(f.name))
			b.WriteByte('\n')
		}
	}
	return C.CString(b.String())
}

type nameSize struct {
	name string
	size uint64
}

// sortedSizes copies a file map's names and sizes, sorted by name. The caller
// holds the map's read lock.
func sortedSizes(files map[string]*synctracker.File) []nameSize {
	out := make([]nameSize, 0, len(files))
	for name, f := range files {
		out = append(out, nameSize{name, f.Size})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out
}

// KD_AddFilesAs shares many files in one call (kd.AddFilesAs): one line per
// file, "<local path>\t<name the peer sees>", both escaped (listEscape).
// Returns how many were announced, -1 when none were; the last error is set
// whenever one occurred.
//
//export KD_AddFilesAs
func KD_AddFilesAs(list *C.char) C.int {
	if kd == nil {
		setLastError(fmt.Errorf("not initialized"))
		return -1
	}
	lines := strings.Split(C.GoString(list), "\n")
	items := make([]common.LocalAs, 0, len(lines))
	for _, line := range lines {
		local, remote, ok := strings.Cut(line, "\t")
		if ok {
			items = append(items, common.LocalAs{Local: listUnescape(local), Remote: listUnescape(remote)})
		}
	}
	n, err := kd.AddFilesAs(items)
	if err != nil {
		setLastError(err)
		if n == 0 {
			return -1
		}
	}
	return C.int(n)
}

// listEscape keeps a name inside one tab-separated field of one line.
func listEscape(s string) string {
	if !strings.ContainsAny(s, "\\\t\n") {
		return s
	}
	return strings.NewReplacer(`\`, `\\`, "\t", `\t`, "\n", `\n`).Replace(s)
}

func listUnescape(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) {
			i++
			switch s[i] {
			case 't':
				b.WriteByte('\t')
			case 'n':
				b.WriteByte('\n')
			default:
				b.WriteByte(s[i])
			}
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}
