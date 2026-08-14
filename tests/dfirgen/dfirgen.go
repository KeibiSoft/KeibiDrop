// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2026 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

// Package dfirgen writes a deterministic synthetic Windows triage tree:
// event logs, registry hives, prefetch files, IIS logs and PowerShell
// history. Sizes scale with a total budget, content comes from a seeded
// RNG so runs are reproducible, and a known subset of text files carries
// the IOCMarker string so a grep pass has an exact expected hit count.
// Test data only; no file here is a real forensic artifact.
package dfirgen

import (
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
)

// IOCMarker is the string a triage grep hunts for. Clearly synthetic on
// purpose: the tests need an exact count, not realism.
const IOCMarker = "KD-DFIR-IOC"

// Summary reports what Generate wrote.
type Summary struct {
	Files    int
	Bytes    int64
	IOCFiles int
	// Extract lists the KAPE-style pull set as root-relative paths:
	// event logs, registry hives, prefetch, PowerShell history.
	Extract []string
}

type writer struct {
	root string
	rng  *rand.Rand
	sum  *Summary
}

func (w *writer) binary(rel string, magic string, size int) error {
	if size < 64 {
		size = 64
	}
	path := filepath.Join(w.root, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	f, err := os.Create(path) // #nosec G304 -- test fixture path
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.WriteString(magic); err != nil {
		return err
	}
	remaining := size - len(magic)
	buf := make([]byte, 1<<20)
	for remaining > 0 {
		n := len(buf)
		if remaining < n {
			n = remaining
		}
		w.rng.Read(buf[:n])
		if _, err := f.Write(buf[:n]); err != nil {
			return err
		}
		remaining -= n
	}
	w.sum.Files++
	w.sum.Bytes += int64(size)
	return nil
}

func (w *writer) text(rel string, size int, withIOC bool) error {
	var b strings.Builder
	b.WriteString("#Software: Microsoft Internet Information Services 10.0\n")
	b.WriteString("#Fields: date time s-sitename cs-method cs-uri-stem sc-status\n")
	line := 0
	for b.Len() < size {
		line++
		fmt.Fprintf(&b, "2026-07-%02d %02d:%02d:%02d W3SVC1 GET /page%04d.aspx %d\n",
			1+w.rng.Intn(28), w.rng.Intn(24), w.rng.Intn(60), w.rng.Intn(60),
			w.rng.Intn(10000), 200+w.rng.Intn(4)*100)
		if withIOC && line == 40 {
			fmt.Fprintf(&b, "2026-07-15 03:14:00 W3SVC1 GET /%s-%04d.aspx 200\n",
				IOCMarker, w.rng.Intn(10000))
		}
	}
	path := filepath.Join(w.root, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	if err := os.WriteFile(path, []byte(b.String()), 0644); err != nil {
		return err
	}
	w.sum.Files++
	w.sum.Bytes += int64(b.Len())
	if withIOC {
		w.sum.IOCFiles++
	}
	return nil
}

func clamp(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// vary returns a size around avg (0.5x to 1.5x), never below 64.
func vary(rng *rand.Rand, avg int) int {
	if avg < 128 {
		avg = 128
	}
	v := avg/2 + rng.Intn(avg)
	if v < 64 {
		v = 64
	}
	return v
}

// Generate writes the tree under root and returns what it wrote. The same
// (totalMB, seed) pair always produces the same tree.
func Generate(root string, totalMB int, seed int64) (*Summary, error) {
	if totalMB < 8 {
		return nil, fmt.Errorf("totalMB %d too small, need at least 8", totalMB)
	}
	total := int64(totalMB) << 20
	w := &writer{root: root, rng: rand.New(rand.NewSource(seed)), sum: &Summary{}}

	// Windows event logs: 45% of the budget across a scaled file count.
	evtxDir := "Windows/System32/winevt/Logs"
	named := []string{"Security", "System", "Application",
		"Microsoft-Windows-PowerShell%4Operational",
		"Microsoft-Windows-Sysmon%4Operational"}
	nEvtx := clamp(totalMB/3, len(named), 300)
	evtxAvg := int(total * 45 / 100 / int64(nEvtx))
	for i := 0; i < nEvtx; i++ {
		name := fmt.Sprintf("Channel%03d.evtx", i)
		if i < len(named) {
			name = named[i] + ".evtx"
		}
		rel := filepath.Join(evtxDir, name)
		if err := w.binary(rel, "ElfFile\x00", vary(w.rng, evtxAvg)); err != nil {
			return nil, err
		}
		w.sum.Extract = append(w.sum.Extract, rel)
	}

	// Registry hives: 30%, split across the machine and user hives.
	hiveBudget := total * 30 / 100
	hives := []struct {
		rel   string
		share int64 // percent of hiveBudget
	}{
		{"Windows/System32/config/SOFTWARE", 45},
		{"Windows/System32/config/SYSTEM", 38},
		{"Windows/System32/config/SAM", 2},
		{"Users/analyst/NTUSER.DAT", 10},
		{"Users/analyst/AppData/Local/Microsoft/Windows/UsrClass.dat", 5},
	}
	for _, h := range hives {
		if err := w.binary(h.rel, "regf", int(hiveBudget*h.share/100)); err != nil {
			return nil, err
		}
		w.sum.Extract = append(w.sum.Extract, h.rel)
	}

	// Prefetch: 5%, many small files, real .pf files stay under ~1 MB.
	nPf := clamp(totalMB*6, 100, 2000)
	pfAvg := int(total * 5 / 100 / int64(nPf))
	if pfAvg > 1<<20 {
		pfAvg = 1 << 20
	}
	for i := 0; i < nPf; i++ {
		rel := filepath.Join("Windows/Prefetch",
			fmt.Sprintf("APP%03d-%08X.pf", i, w.rng.Uint32()))
		if err := w.binary(rel, "MAM\x04", vary(w.rng, pfAvg)); err != nil {
			return nil, err
		}
		w.sum.Extract = append(w.sum.Extract, rel)
	}

	// IIS logs: 15%, text, every 5th file carries the IOC marker.
	nLog := clamp(totalMB, 10, 500)
	logAvg := int(total * 15 / 100 / int64(nLog))
	for i := 0; i < nLog; i++ {
		rel := filepath.Join("inetpub/logs/LogFiles/W3SVC1",
			fmt.Sprintf("u_ex2607%02d_%03d.log", 1+i%28, i))
		if err := w.text(rel, vary(w.rng, logAvg), i%5 == 0); err != nil {
			return nil, err
		}
	}

	// PowerShell history: small, text, always carries the marker.
	histRel := "Users/analyst/AppData/Roaming/Microsoft/Windows/PowerShell/PSReadLine/ConsoleHost_history.txt"
	if err := w.text(histRel, 4096, true); err != nil {
		return nil, err
	}
	w.sum.Extract = append(w.sum.Extract, histRel)

	return w.sum, nil
}
