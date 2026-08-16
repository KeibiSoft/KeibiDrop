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

// Go randomizes map iteration. EachKV sorts, so the reported key is stable.
func TestEachKV_ReportsTheSameKeyEveryRun(t *testing.T) {
	m := map[string]int{}
	for i := 0; i < 10; i++ {
		m[fmt.Sprintf("k%02d", i)] = i
	}
	first := ""
	for i := 0; i < 100; i++ {
		err := EachKV("m", m, func(_ string, v int) error {
			return Equal("value", v, -1)
		})
		if err == nil {
			t.Fatal("want an error")
		}
		if i == 0 {
			first = err.Error()
			continue
		}
		if err.Error() != first {
			t.Fatalf("unstable message on run %d:\n%s\nvs\n%s", i, err, first)
		}
	}
	if !strings.Contains(first, "m[k00]") {
		t.Fatalf("want the lowest key first, got %s", first)
	}
}

func TestMapFilterFold(t *testing.T) {
	in := []int{1, 2, 3, 4}
	doubled := Map(in, func(v int) int { return v * 2 })
	if len(doubled) != 4 || doubled[3] != 8 {
		t.Fatalf("Map: %v", doubled)
	}
	even := Filter(in, func(v int) bool { return v%2 == 0 })
	if len(even) != 2 || even[0] != 2 {
		t.Fatalf("Filter: %v", even)
	}
	sum := Fold(in, 0, func(a, v int) int { return a + v })
	if sum != 10 {
		t.Fatalf("Fold: %d", sum)
	}
	if Map[int, int](in, nil) != nil || Filter(in, nil) != nil {
		t.Fatal("nil function must give nil")
	}
	if Fold(in, 7, nil) != 7 {
		t.Fatal("nil step must give zero")
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
		{"Empty ok", Empty("a", []int{}), false},
		{"Empty bad", Empty("a", []int{1}), true},
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

func TestChain_BindPropagatesAndSkipsTheStage(t *testing.T) {
	called := false
	src := Wrap(1).WithError(errBoom)
	out := Bind(src, func(int) Chain[string] {
		called = true
		return Wrap("never")
	})
	if called {
		t.Fatal("stage must not run when the chain holds an error")
	}
	if _, err := out.Result(); !errors.Is(err, errBoom) {
		t.Fatalf("got %v, want errBoom", err)
	}
}

// go-fp returns a zero value with no error here. That hides a bug, so we differ.
func TestChain_NilStageIsAnError(t *testing.T) {
	if _, err := Bind(Wrap(1), (func(int) Chain[int])(nil)).Result(); err == nil {
		t.Fatal("Bind with a nil stage must report an error")
	}
	if _, err := Wrap(1).Then(nil).Result(); err == nil {
		t.Fatal("Then with a nil stage must report an error")
	}
	if _, err := LiftResult[int](nil).Result(); err == nil {
		t.Fatal("LiftResult with a nil function must report an error")
	}
}

func TestChain_ThenSkipsAfterAnError(t *testing.T) {
	ran := 0
	out := Wrap(1).
		Then(func(v int) (int, error) { ran++; return v + 1, nil }).
		Then(func(v int) (int, error) { ran++; return 0, errBoom }).
		Then(func(v int) (int, error) { ran++; return v + 1, nil })
	if ran != 2 {
		t.Fatalf("third stage must not run: ran=%d", ran)
	}
	if _, err := out.Result(); !errors.Is(err, errBoom) {
		t.Fatalf("got %v", err)
	}
}

// Chain is a value type, so a chain can be reused without sharing state.
func TestChain_IsAValueAndForksCleanly(t *testing.T) {
	base := Wrap(10)
	a := base.Then(func(v int) (int, error) { return v + 1, nil })
	b := base.Then(func(v int) (int, error) { return v + 2, nil })

	av, _ := a.Result()
	bv, _ := b.Result()
	basev, _ := base.Result()
	if av != 11 || bv != 12 || basev != 10 {
		t.Fatalf("a=%d b=%d base=%d, want 11 12 10", av, bv, basev)
	}
}

func TestChain_MatchAndFlags(t *testing.T) {
	okCalled, badCalled := 0, 0
	Wrap(1).Match(func(int) { okCalled++ }, func(error) { badCalled++ })
	Wrap(1).WithError(errBoom).Match(func(int) { okCalled++ }, func(error) { badCalled++ })
	if okCalled != 1 || badCalled != 1 {
		t.Fatalf("ok=%d bad=%d, want 1 1", okCalled, badCalled)
	}

	// Nil funcs are ignored, not a panic.
	Wrap(1).Match(nil, nil)
	Wrap(1).WithError(errBoom).Match(nil, nil)

	if !Wrap(1).IsOK() || Wrap(1).WithError(errBoom).IsOK() {
		t.Fatal("IsOK is wrong")
	}
	if !errors.Is(Wrap(1).WithError(errBoom).Err(), errBoom) {
		t.Fatal("Err is wrong")
	}
}

func TestChain_LiftResultCarriesBothHalves(t *testing.T) {
	c := LiftResult(func() (int, error) { return 5, nil })
	if v, err := c.Result(); v != 5 || err != nil {
		t.Fatalf("got %d, %v", v, err)
	}
	c = LiftResult(func() (int, error) { return 7, errBoom })
	v, err := c.Result()
	if v != 7 || !errors.Is(err, errBoom) {
		t.Fatalf("the failed value must be kept: got %d, %v", v, err)
	}
}
