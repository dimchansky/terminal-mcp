//go:build linux || darwin

package filesystem

import (
	"strings"
	"testing"
)

func TestApplyEditsExplainsConflicts(t *testing.T) {
	cases := []struct {
		name     string
		original string
		edits    []Edit
		want     string
	}{
		{"missing second fragment", "alpha beta", []Edit{{OldText: "alpha", NewText: "A"}, {OldText: "gamma", NewText: "G"}}, "edit[2]: old_text not found"},
		{"three matches", "xx xx xx", []Edit{{OldText: "xx", NewText: "X"}}, "edit[1]: old_text matches 3 locations"},
		{"overlap names both edits", "abcdef", []Edit{{OldText: "abcd", NewText: "A"}, {OldText: "cdef", NewText: "B"}}, "edit[2] overlaps edit[1]"},
		{"overlap independent of input order", "abcdef", []Edit{{OldText: "cdef", NewText: "B"}, {OldText: "abcd", NewText: "A"}}, "edit[1] overlaps edit[2]"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, _, err := applyEdits([]byte(tc.original), tc.edits)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %v does not contain %q", err, tc.want)
			}
			if err != nil && strings.Contains(err.Error(), tc.original) {
				t.Fatalf("error leaks source file contents: %v", err)
			}
		})
	}
}

func TestApplyEditsUniqueReplacementStillWorks(t *testing.T) {
	changed, count, _, err := applyEdits([]byte("hello and world"), []Edit{
		{OldText: "world", NewText: "W"},
		{OldText: "hello", NewText: "H"},
	})
	if err != nil || count != 2 || string(changed) != "H and W" {
		t.Fatalf("result %q, count %d, error %v", changed, count, err)
	}
}
