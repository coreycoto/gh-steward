package contract

import (
	"testing"
	"time"
)

func prepared(t *testing.T) Plan {
	t.Helper()
	repo := Repository{Host: "github.com", Owner: "example", Name: "widgets", URL: "https://github.com/example/widgets"}
	p, err := PreparePlan("review-apply", repo, Object{"issues": Object{"live": true, "complete": true}}, Object{"finding": "F-17"}, []Operation{{ID: "comment:17", Kind: "issue-comment", Target: Object{"issue_number": 17}, Before: Object{"body": nil}, After: Object{"body": "Reviewed rationale"}}}, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestReviewedPlanRejectsChangedEvidenceAndForeignTargets(t *testing.T) {
	p := prepared(t)
	for _, mutate := range []func(Object){func(o Object) { o["data"].(map[string]any)["finding"] = "changed" }, func(o Object) {
		o["operations"].([]any)[0].(map[string]any)["before"].(map[string]any)["body"] = "changed"
	}, func(o Object) { o["sources"].(map[string]any)["issues"].(map[string]any)["complete"] = false }, func(o Object) { o["schema_version"] = 1 }, func(o Object) { o["repository"].(map[string]any)["host"] = "other.example" }, func(o Object) { o["approval"] = true }} {
		o, err := Clone(p.Object())
		if err != nil {
			t.Fatal(err)
		}
		mutate(o)
		if _, err := ParsePlan(o); err == nil {
			t.Fatal("changed review artifact accepted")
		}
	}
	if err := p.VerifyTarget("other-apply", p.Repository, p.Operations); err == nil {
		t.Fatal("foreign command accepted")
	}
	foreign := p.Repository
	foreign.Host = "other.example"
	if err := p.VerifyTarget(p.Command, foreign, p.Operations); err == nil {
		t.Fatal("foreign host accepted")
	}
	if err := p.VerifyTarget(p.Command, p.Repository, nil); err == nil {
		t.Fatal("changed primitive set accepted")
	}
}

func TestPreparedEvidenceIsDetachedAndRequiresLiveCompleteSources(t *testing.T) {
	p := prepared(t)
	raw := p.Object()
	parsed, err := ParsePlan(raw)
	if err != nil {
		t.Fatal(err)
	}
	raw["data"].(map[string]any)["finding"] = "mutated caller"
	if parsed.Data["finding"] != "F-17" {
		t.Fatal("parsed plan aliases caller")
	}
	for _, source := range []Object{{"live": false, "complete": true}, {"live": true, "complete": false}, {"live": true}} {
		if _, err := PreparePlan(p.Command, p.Repository, Object{"issues": source}, p.Data, p.Operations, time.Now()); err == nil {
			t.Fatal("seed/incomplete source became write authority")
		}
	}
	ops := append([]Operation{}, p.Operations...)
	ops = append(ops, ops[0])
	if _, err := PreparePlan(p.Command, p.Repository, p.Sources, p.Data, ops, time.Now()); err == nil {
		t.Fatal("duplicate primitive accepted")
	}
	if _, err := PreparePlan(p.Command, p.Repository, p.Sources, p.Data, []Operation{{ID: "missing", Kind: "issue-comment", Target: Object{"issue_number": 17}, Before: Object{"other": false}, After: Object{"body": "x"}}}, time.Now()); err == nil {
		t.Fatal("missing before state accepted")
	}
}
