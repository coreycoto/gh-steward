package workflow

import "testing"

func TestStripOperationMarkerPreservesContentAndRemovesOnlyTrailingMarker(t *testing.T) {
	marker := "<!-- gh-steward:operation:0123456789abcdef -->"
	for _, tc := range []struct {
		name, body, want string
	}{
		{"ordinary comment", "Reviewed\n", "Reviewed\n"},
		{"trailing marker", "Reviewed\n\n" + marker, "Reviewed\n"},
		{"trailing whitespace", "Reviewed\n\n" + marker + "\n\t ", "Reviewed\n"},
		{"earlier marker content", "Quoted " + marker + "\nReviewed\n\n" + marker, "Quoted " + marker + "\nReviewed\n"},
		{"marker followed by content", "Reviewed\n\n" + marker + "\nKeep this", "Reviewed\n\n" + marker + "\nKeep this"},
		{"unfinished marker", "Reviewed\n\n<!-- gh-steward:operation:incomplete", "Reviewed\n\n<!-- gh-steward:operation:incomplete"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := stripOperationMarker(tc.body); got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}
