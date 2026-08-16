// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.

// ABOUTME: Bounded breadth-first explorer for the two-peer state models.
// ABOUTME: One driver. Each model supplies its own key, successors and checks.

package model

import "fmt"

// Model describes one state machine to explore.
// S is the concrete state type, so every call below is a direct call.
type Model[S any] struct {
	// Key returns a dedup key. Two states with the same key are the same state.
	Key func(S) string
	// Successors returns every enabled action from s. An empty result means s
	// is terminal.
	Successors func(S) []S
	// Quiescent reports whether a terminal state is a legal stopping point.
	// A terminal state that is not quiescent is a deadlock.
	Quiescent func(S) bool
	// CheckTerminal validates one terminal state. Return an error to fail.
	CheckTerminal func(S) error
}

// Report is what one exploration produced.
type Report struct {
	Explored  int
	Terminals int
}

// Explore walks every reachable state once and validates every terminal.
// It returns on the first terminal that fails, matching the t.Fatalf the
// hand-written drivers used.
func Explore[S any](m Model[S], init S) (Report, error) {
	var r Report
	frontier := []S{init}
	visited := map[string]bool{m.Key(init): true}

	for len(frontier) > 0 {
		s := frontier[0]
		frontier = frontier[1:]
		r.Explored++

		succ := m.Successors(s)
		if len(succ) == 0 {
			if !m.Quiescent(s) {
				return r, fmt.Errorf("deadlock: non-quiescent state with no actions: %+v", s)
			}
			r.Terminals++
			if err := m.CheckTerminal(s); err != nil {
				return r, err
			}
			continue
		}

		for _, n := range succ {
			k := m.Key(n)
			if !visited[k] {
				visited[k] = true
				frontier = append(frontier, n)
			}
		}
	}
	return r, nil
}
