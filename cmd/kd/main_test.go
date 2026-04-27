// ABOUTME: Tests for kd CLI argument parsing helpers.
// ABOUTME: Currently covers isShowAll — see cmdShow for the call site.
package main

import "testing"

func TestIsShowAll(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want bool
	}{
		{"no args", nil, true},
		{"empty slice", []string{}, true},
		{"explicit all", []string{"all"}, true},
		{"single field", []string{"fingerprint"}, false},
		{"compound peer ip", []string{"peer", "ip"}, false},
		{"all with extra", []string{"all", "extra"}, false},
		{"capitalised", []string{"All"}, false}, // case-sensitive on purpose
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isShowAll(tc.args); got != tc.want {
				t.Fatalf("isShowAll(%v) = %v, want %v", tc.args, got, tc.want)
			}
		})
	}
}
