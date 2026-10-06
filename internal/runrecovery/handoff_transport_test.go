package runrecovery

import (
	"bytes"
	"context"
	"testing"
)

func TestHistoricalHandoffTransportKeepsEarlierHoldsAndPreservesCompleteHistory(t *testing.T) {
	for _, test := range []struct {
		name, purpose string
		accepted      bool
	}{
		{"later-run", "transport", true},
		{"same-time-larger-id", "transport", true},
		{"later-time-smaller-id", "transport", true},
		{"earlier-run", "transport", false},
		{"same-time-smaller-id", "transport", false},
		{"earlier-time-larger-id", "transport", false},
		{"stale-source-attempt", "transport", false},
		{"incomplete-successor-history", "transport", false},
		{"invalid-successor-history", "transport", false},
		{"same-successor-apply", "apply", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			e, reader, invocation, metadata := preparedHandoffFixture(t)
			endpoint := "repos/example/widgets/actions/workflows/task.yml/runs?per_page=100"
			page := reader.pages[endpoint][0].(Object)
			source := page["workflow_runs"].([]any)[0].(Object)
			successor := recoveryHistoryRun(91, 1, "Later apply")
			successor["created_at"] = "2026-01-02T00:00:00Z"
			rows := []any{source, successor}
			switch test.name {
			case "same-time-larger-id":
				successor["created_at"] = source["created_at"]
			case "later-time-smaller-id":
				successor["id"] = int64(89)
			case "earlier-run":
				predecessor := recoveryHistoryRun(89, 1, "Unsettled predecessor")
				predecessor["created_at"] = "2025-12-31T00:00:00Z"
				rows = append(rows, predecessor)
			case "same-time-smaller-id":
				rows = append(rows, recoveryHistoryRun(89, 1, "Unsettled predecessor"))
			case "earlier-time-larger-id":
				successor["created_at"] = "2025-12-31T00:00:00Z"
			case "stale-source-attempt":
				source["run_attempt"] = int64(2)
			case "invalid-successor-history":
				delete(successor, "head_sha")
			}
			page["workflow_runs"], page["total_count"] = rows, int64(len(rows))
			if test.name == "incomplete-successor-history" {
				page["total_count"] = int64(len(rows) + 1)
			}
			originalArchive := append([]byte(nil), reader.archives[31]...)
			result, err := e.AcquireHandoffFor(context.Background(), reader, invocation, 31, metadata["digest"].(string), test.purpose)
			if !test.accepted {
				if err == nil {
					t.Fatal("transport bypassed a predecessor, stale source, incomplete history, or apply guard")
				}
				return
			}
			if err != nil || result["purpose"] != "transport" {
				t.Fatalf("inert selected-source transport rejected its successor: %#v %v", result, err)
			}
			originalFiles, err := archiveFiles(originalArchive, false)
			if err != nil {
				t.Fatal(err)
			}
			chainBytes, err := ReadPackageFile(invocation.PackageRoot, "settlement-chain.json")
			if err != nil || !bytes.Equal(chainBytes, originalFiles["settlement-chain.json"]) {
				t.Fatalf("transport rewrote settlement history: %v", err)
			}
			chain, err := DecodeValue(chainBytes)
			if err != nil {
				t.Fatal(err)
			}
			_, _, _, observed, attempts, err := e.invocationHistory(context.Background(), reader, invocation)
			if err != nil {
				t.Fatal(err)
			}
			pending, err := PendingAttempts(chain.(Object), observed, attempts, invocation.RunID, invocation.Attempt)
			if err != nil || len(pending) != 1 || pending[0]["run"].(Object)["id"] != successor["id"] {
				t.Fatalf("transport settled or discarded its successor from full recovery history: %#v %v", pending, err)
			}
		})
	}
}
