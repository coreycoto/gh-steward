package governance

import (
	"fmt"
	"strings"
	"testing"

	"github.com/coreycoto/gh-steward/internal/contract"
)

func TestMilestoneRationaleRulesPreserveCompoundAlternatives(t *testing.T) {
	fixture := baseline(t)
	policy := clone(t, nested(t, fixture, "milestone_policy"))
	policy["rationale_markers"] = []any{"quarter commitment rationale"}
	policy["rationale_rules"] = []any{
		contract.Object{"markers": []any{"quarter milestone target"}, "require_milestone": true},
		contract.Object{"markers": []any{"quarter", "rationale"}, "require_milestone": true},
	}
	for _, tc := range []struct {
		text  string
		valid bool
	}{
		{"Quarter commitment rationale: approved", true},
		{"QUARTER MILESTONE TARGET: <milestone>", true},
		{"<milestone>: the quarter decision and its rationale", true},
		{"Quarter milestone target: a different period", false},
		{"<milestone>: quarter decision without explanation", false},
		{"<milestone>: rationale without a period decision", false},
		{"<milestone>: incidental text", false},
	} {
		t.Run(tc.text, func(t *testing.T) {
			snapshot := clone(t, nested(t, fixture, "milestone_snapshot"))
			issues, _ := contract.Objects(snapshot, "issues")
			var number int64
			for _, issue := range issues {
				if issue["milestone"] == nil {
					continue
				}
				number, _ = contract.PositiveInteger(issue["number"])
				milestone := issue["milestone"].(string)
				text := tc.text
				// Each case uses the exact issue-specific milestone, not a
				// static current-quarter token that could accept another issue.
				text = strings.ReplaceAll(text, "<milestone>", milestone)
				snapshot["issues"] = []any{issue}
				snapshot["issue_details"] = contract.Object{fmt.Sprint(number): contract.Object{"body": text, "comments": []any{}}}
				break
			}
			if number == 0 {
				t.Fatal("fixture has no quarter issue")
			}
			result, err := MilestoneCheck(policy, snapshot)
			if err != nil {
				t.Fatal(err)
			}
			if missing := findingExists(t, result, "missing-quarter-rationale", number); missing == tc.valid {
				t.Fatalf("rationale match=%v, want %v", !missing, tc.valid)
			}
		})
	}
}

func TestMilestoneRationaleRulesRejectMalformedPolicy(t *testing.T) {
	fixture := baseline(t)
	for _, raw := range []any{
		nil, true, []any{true},
		[]any{contract.Object{"markers": []any{}, "require_milestone": true}},
		[]any{contract.Object{"markers": []any{"quarter", "quarter"}, "require_milestone": true}},
		[]any{contract.Object{"markers": []any{" "}, "require_milestone": true}},
		[]any{contract.Object{"markers": []any{"quarter"}, "require_milestone": "true"}},
		[]any{contract.Object{"markers": []any{"quarter"}, "require_milestone": true, "extra": false}},
	} {
		policy := clone(t, nested(t, fixture, "milestone_policy"))
		policy["rationale_rules"] = raw
		if _, err := MilestoneCheck(policy, nested(t, fixture, "milestone_snapshot")); err == nil {
			t.Fatalf("malformed rationale rules accepted: %#v", raw)
		}
	}
}
