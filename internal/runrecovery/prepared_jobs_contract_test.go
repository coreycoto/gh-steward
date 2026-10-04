package runrecovery

import "testing"

func preparedJobsContractFixture(status string) Object {
	qualifierStatus, qualifierConclusion := "in_progress", any(nil)
	if status == "completed" {
		qualifierStatus, qualifierConclusion = "completed", "success"
	}
	return Object{
		"run_id": int64(42), "attempt": int64(3),
		"pages": []any{Object{
			"total_count": int64(1),
			"jobs": []any{Object{
				"id": int64(4203), "run_id": int64(42), "run_attempt": int64(3),
				"name": "Apply reviewed plan", "status": status,
				"steps": []any{
					Object{"name": "Apply retained plan", "status": "completed", "conclusion": "skipped"},
					Object{"name": "Qualify prepared work", "status": qualifierStatus, "conclusion": qualifierConclusion},
				},
			}},
		}},
	}
}

func preparedJobsContractJob(packet Object) Object {
	return packet["pages"].([]any)[0].(Object)["jobs"].([]any)[0].(Object)
}

func TestPreparedMutationJobAllowsLiveQualifierOnlyWithPositivelySkippedMutator(t *testing.T) {
	required := map[string]map[string]bool{"Apply reviewed plan": {"Apply retained plan": true}}
	for _, status := range []string{"in_progress", "completed"} {
		t.Run(status, func(t *testing.T) {
			if err := validatePreparedMutationJobs(preparedJobsContractFixture(status), required, 42, 3); err != nil {
				t.Fatalf("the qualifier may run after its exact mutator was skipped: %v", err)
			}
		})
	}
}

func TestPreparedMutationJobRejectsUnobservedOrAmbiguousDispatch(t *testing.T) {
	cases := []struct {
		name   string
		change func(Object)
	}{
		{"queued job", func(packet Object) { preparedJobsContractJob(packet)["status"] = "queued" }},
		{"unknown job status", func(packet Object) { preparedJobsContractJob(packet)["status"] = "waiting" }},
		{"started mutator", func(packet Object) {
			step := preparedJobsContractJob(packet)["steps"].([]any)[0].(Object)
			step["status"], step["conclusion"] = "in_progress", nil
		}},
		{"pending mutator", func(packet Object) {
			step := preparedJobsContractJob(packet)["steps"].([]any)[0].(Object)
			step["status"], step["conclusion"] = "queued", nil
		}},
		{"successful mutator", func(packet Object) {
			preparedJobsContractJob(packet)["steps"].([]any)[0].(Object)["conclusion"] = "success"
		}},
		{"failed mutator", func(packet Object) {
			preparedJobsContractJob(packet)["steps"].([]any)[0].(Object)["conclusion"] = "failure"
		}},
		{"cancelled mutator", func(packet Object) {
			preparedJobsContractJob(packet)["steps"].([]any)[0].(Object)["conclusion"] = "cancelled"
		}},
		{"skipped job without step proof", func(packet Object) {
			job := preparedJobsContractJob(packet)
			job["status"], job["conclusion"], job["steps"] = "completed", "skipped", []any{}
		}},
		{"wrong packet run", func(packet Object) { packet["run_id"] = int64(41) }},
		{"wrong packet attempt", func(packet Object) { packet["attempt"] = int64(2) }},
		{"wrong job run", func(packet Object) { preparedJobsContractJob(packet)["run_id"] = int64(41) }},
		{"wrong job attempt", func(packet Object) { preparedJobsContractJob(packet)["run_attempt"] = int64(2) }},
		{"duplicate named job", func(packet Object) {
			page := packet["pages"].([]any)[0].(Object)
			job := preparedJobsContractJob(packet)
			duplicate := Object{}
			for key, value := range job {
				duplicate[key] = value
			}
			duplicate["id"] = int64(4204)
			page["jobs"], page["total_count"] = []any{job, duplicate}, int64(2)
		}},
		{"duplicate named step", func(packet Object) {
			job := preparedJobsContractJob(packet)
			steps := job["steps"].([]any)
			job["steps"] = append(steps, Object{"name": "Apply retained plan", "status": "completed", "conclusion": "skipped"})
		}},
		{"missing named step", func(packet Object) {
			job := preparedJobsContractJob(packet)
			job["steps"] = job["steps"].([]any)[1:]
		}},
		{"incomplete jobs inventory", func(packet Object) {
			packet["pages"].([]any)[0].(Object)["total_count"] = int64(2)
		}},
	}
	required := map[string]map[string]bool{"Apply reviewed plan": {"Apply retained plan": true}}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			packet := preparedJobsContractFixture("in_progress")
			test.change(packet)
			if err := validatePreparedMutationJobs(packet, required, 42, 3); err == nil {
				t.Fatal("unobserved or ambiguous mutation must hold prepared recovery")
			}
		})
	}
}

func TestPreparedMutationJobRequiresEveryDeclaredMutator(t *testing.T) {
	required := map[string]map[string]bool{
		"Apply reviewed plan": {"Apply retained plan": true, "Apply second retained plan": true},
	}
	if err := validatePreparedMutationJobs(preparedJobsContractFixture("in_progress"), required, 42, 3); err == nil {
		t.Fatal("one skipped plan cannot qualify another plan's unobserved mutator")
	}
}
