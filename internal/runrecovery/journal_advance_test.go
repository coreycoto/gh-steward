package runrecovery

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/coreycoto/gh-steward/internal/contract"
)

func TestCaptureJournalAdvancesExactUnknownDispatchAndPreservesSourceProof(t *testing.T) {
	for _, mutation := range []string{"none", "dispatch-id", "acknowledgement", "completed-receipt"} {
		t.Run(mutation, func(t *testing.T) {
			e, r, inv, metadata := preparedHandoffFixture(t)
			if _, err := e.AcquireHandoff(context.Background(), r, inv, 31, metadata["digest"].(string)); err != nil {
				t.Fatal(err)
			}
			planValue, err := LoadJSON(filepath.Join(inv.PackageRoot, "plans/sync.json"))
			if err != nil {
				t.Fatal(err)
			}
			plan, err := contract.ParsePlan(planValue.(Object))
			if err != nil {
				t.Fatal(err)
			}
			_, proof := makeNativeTerminalProofFromPlan(t, plan, nativeAckAcknowledged)
			currentValue, err := LoadFileProof(proof["journal_file"], "completed journal")
			if err != nil {
				t.Fatal(err)
			}
			current := currentValue.(Object)
			previous, err := cloneObject(current)
			if err != nil {
				t.Fatal(err)
			}
			step := previous["steps"].([]any)[0].(Object)
			if mutation != "completed-receipt" {
				step["status"], step["result"], step["completed_at"] = "unknown", nil, nil
				previous["result"] = nil
			}
			contextValue, err := LoadJSON(filepath.Join(inv.PackageRoot, "run-context.json"))
			if err != nil {
				t.Fatal(err)
			}
			contextValue.(Object)["phase"] = "dispatching"
			contextValue.(Object)["plans"].([]any)[0].(Object)["status"] = "dispatching"
			if err := persistPackageJSON(inv.PackageRoot, "run-context.json", contextValue); err != nil {
				t.Fatal(err)
			}
			journalRoot, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			id := journalIDForPlan(plan.Object())
			if err := persistPackageJSON(journalRoot, id+".json", previous); err != nil {
				t.Fatal(err)
			}
			if _, err := e.CaptureJournal(inv.PackageRoot, journalRoot, "sync"); err != nil {
				t.Fatal(err)
			}
			before, err := ReadPackageFile(inv.PackageRoot, "journal/"+id+".json")
			if err != nil {
				t.Fatal(err)
			}
			if err := persistPackageJSON(inv.PackageRoot, "recovery-source.json", Object{"journal_file": MakeFileProof(before)}); err != nil {
				t.Fatal(err)
			}
			sourceBefore, err := ReadPackageFile(inv.PackageRoot, "recovery-source.json")
			if err != nil {
				t.Fatal(err)
			}
			next := current["steps"].([]any)[0].(Object)
			switch mutation {
			case "none":
				// Real positive reconciliation retains its prior ACK and appends
				// independent observation of the same durable operation identity.
				next["observation"] = Object{"positive_identity": true, "after_state_verified": true, "operation_id": next["operation_id"], "reference": "refs/heads/reviewed"}
			case "dispatch-id":
				next["operation_id"] = strings.Repeat("f", 32)
				next["acknowledgement"].(Object)["operation_id"] = next["operation_id"]
				next["result"].(Object)["operation_id"] = next["operation_id"]
			case "acknowledgement", "completed-receipt":
				next["acknowledgement"].(Object)["provider_result"] = Object{"changed": "receipt"}
				next["result"].(Object)["provider_result"] = Object{"changed": "receipt"}
			}
			current["result"].(Object)["receipts"] = current["steps"]
			if err := persistPackageJSON(journalRoot, id+".json", current); err != nil {
				t.Fatal(err)
			}
			_, err = e.CaptureJournal(inv.PackageRoot, journalRoot, "sync")
			if mutation == "none" {
				if err != nil {
					t.Fatalf("valid same-operation progress rejected: %v", err)
				}
				updated, err := LoadJSON(filepath.Join(inv.PackageRoot, "journal", id+".json"))
				if err != nil || !Equal(updated, current) {
					t.Fatal("advanced journal was not captured", err)
				}
			} else {
				if err == nil {
					t.Fatal("capture rewrote immutable dispatch evidence")
				}
				unchanged, err := ReadPackageFile(inv.PackageRoot, "journal/"+id+".json")
				if err != nil || !Equal(unchanged, before) {
					t.Fatal("rejected capture changed original package journal")
				}
			}
			sourceAfter, err := os.ReadFile(filepath.Join(inv.PackageRoot, "recovery-source.json"))
			if err != nil || !Equal(sourceBefore, sourceAfter) {
				t.Fatal("progress capture changed immutable recovery source")
			}
		})
	}
}
