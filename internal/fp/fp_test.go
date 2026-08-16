// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
//
// ABOUTME: Unit tests for the fp combinators, checks and Chain.
// ABOUTME: The only file in this package that may import testing.

package fp

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

var errBoom = errors.New("boom")

func TestAll_NilWhenEveryCheckPasses(t *testing.T) {
	if err := All(nil, nil, nil); err != nil {
		t.Fatalf("All of nils: got %v, want nil", err)
	}
	if err := All(); err != nil {
		t.Fatalf("All of nothing: got %v, want nil", err)
	}
}

// All must accumulate. This is what makes it match t.Errorf semantics.
func TestAll_ReportsEveryFailure(t *testing.T) {
	err := All(
		Equal("first", 1, 2),
		nil,
		Equal("second", 3, 4),
		Equal("third", 5, 6),
	)
	if err == nil {
		t.Fatal("want an error")
	}
	got := err.Error()
	for _, want := range []string{"first", "second", "third"} {
		if !strings.Contains(got, want) {
			t.Errorf("message is missing %q: %s", want, got)
		}
	}
	if n := strings.Count(got, "\n"); n != 2 {
		t.Errorf("want 3 lines (2 newlines), got %d: %s", n, got)
	}
}

// Steps must stop. This is what makes it match t.Fatalf semantics.
func TestSteps_StopsAtFirstFailure(t *testing.T) {
	ran := 0
	err := Steps(
		func() error { ran++; return nil },
		func() error { ran++; return errBoom },
		func() error { ran++; return nil },
	)
	if !errors.Is(err, errBoom) {
		t.Fatalf("got %v, want errBoom", err)
	}
	if ran != 2 {
		t.Fatalf("third step must not run: ran=%d, want 2", ran)
	}
}

func TestSteps_SkipsNilAndPasses(t *testing.T) {
	if err := Steps(nil, func() error { return nil }, nil); err != nil {
		t.Fatalf("got %v, want nil", err)
	}
}

// Laziness is the reason Steps exists: a later step may dereference something
// an earlier step proved valid.
func TestSteps_LazinessProtectsADereference(t *testing.T) {
	var p *int
	err := Steps(
		func() error { return True("p is set", p != nil) },
		func() error { return Equal("value", *p, 1) },
	)
	if err == nil {
		t.Fatal("want an error, got nil")
	}
}

func TestEach_LabelsTheIndex(t *testing.T) {
	err := Each("item", []int{1, 2, 3}, func(v int) error {
		return Equal("value", v, 1)
	})
	if err == nil || !strings.Contains(err.Error(), "item[1]") {
		t.Fatalf("want item[1] in %v", err)
	}
}

func TestEach_EmptyAndNilCheck(t *testing.T) {
	if err := Each("none", []int{}, func(int) error { return errBoom }); err != nil {
		t.Fatalf("empty slice: got %v, want nil", err)
	}
	if err := Each[int]("bad", []int{1}, nil); err == nil {
		t.Fatal("nil check must report an error")
	}
}

func TestBytesEqual_ReportsOffsetNotPayload(t *testing.T) {
	err := BytesEqual("payload", []byte{1, 2, 9, 4}, []byte{1, 2, 3, 4})
	if err == nil {
		t.Fatal("want an error")
	}
	if !strings.Contains(err.Error(), "offset 2") {
		t.Fatalf("want the offset, got %v", err)
	}
	if err := BytesEqual("same", []byte{1}, []byte{1}); err != nil {
		t.Fatalf("equal bytes: got %v", err)
	}
	err = BytesEqual("len", []byte{1}, []byte{1, 2})
	if err == nil || !strings.Contains(err.Error(), "want 2 bytes") {
		t.Fatalf("want a length report, got %v", err)
	}
}

func TestChecks(t *testing.T) {
	cases := []struct {
		name string
		got  error
		fail bool
	}{
		{"Equal ok", Equal("a", 1, 1), false},
		{"Equal bad", Equal("a", 1, 2), true},
		{"NotEqual ok", NotEqual("a", 1, 2), false},
		{"NotEqual bad", NotEqual("a", 1, 1), true},
		{"NoErr ok", NoErr("a", nil), false},
		{"NoErr bad", NoErr("a", errBoom), true},
		{"WantErr ok", WantErr("a", errBoom), false},
		{"WantErr bad", WantErr("a", nil), true},
		{"True ok", True("a", true), false},
		{"True bad", True("a", false), true},
		{"False ok", False("a", false), false},
		{"False bad", False("a", true), true},
		{"ErrIs ok", ErrIs("a", fmt.Errorf("w: %w", errBoom), errBoom), false},
		{"ErrIs bad", ErrIs("a", errors.New("x"), errBoom), true},
		{"Len ok", Len("a", []int{1, 2}, 2), false},
		{"Len bad", Len("a", []int{1}, 2), true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if c.fail && c.got == nil {
				t.Fatal("want an error, got nil")
			}
			if !c.fail && c.got != nil {
				t.Fatalf("want nil, got %v", c.got)
			}
		})
	}
}
