// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026 KeibiSoft S.R.L.

// dfirgen writes a synthetic Windows triage tree for DFIR-shaped testing
// and demos: event logs, registry hives, prefetch, IIS logs, PowerShell
// history. Deterministic for a given -mb and -seed.
//
//	go run ./tests/cmd/dfirgen -dir /path/to/share/host1 -mb 2048
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/KeibiSoft/KeibiDrop/tests/dfirgen"
)

func main() {
	dir := flag.String("dir", "", "target directory (created if missing)")
	mb := flag.Int("mb", 2048, "total size budget in MB")
	seed := flag.Int64("seed", 1, "RNG seed; same seed and mb = same tree")
	flag.Parse()
	if *dir == "" {
		fmt.Fprintln(os.Stderr, "need -dir")
		os.Exit(2)
	}
	sum, err := dfirgen.Generate(*dir, *mb, *seed)
	if err != nil {
		fmt.Fprintln(os.Stderr, "generate:", err)
		os.Exit(1)
	}
	fmt.Printf("wrote %d files, %d MB under %s\n", sum.Files, sum.Bytes>>20, *dir)
	fmt.Printf("grep target: %q in %d text files\n", dfirgen.IOCMarker, sum.IOCFiles)
	fmt.Printf("triage pull set: %d artifacts (winevt, hives, prefetch, PS history)\n", len(sum.Extract))
}
