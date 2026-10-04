package workflow

import (
	"testing"

	"github.com/coreycoto/gh-steward/internal/contract"
)

func backlogMetadataOnlySource() contract.Object {
	source := backlogTestInventory(false)
	graph := source["issue_inventory"].(contract.Object)
	rows, _ := contract.Objects(graph, "issues")
	for _, row := range rows {
		for _, key := range []string{"blocked_by_numbers", "child_numbers", "parent_number"} {
			delete(row, key)
		}
	}
	source["provenance"].(contract.Object)["relationships_source"] = "not_requested"
	return source
}

func TestBacklogMetadataOnlyScopeNeverRequiresOrInventsRelationshipEvidence(t *testing.T) {
	source := backlogMetadataOnlySource()
	normalized, err := normalizeBacklogInventory(source, testRepo(), BacklogInventoryRequest{})
	if err != nil {
		t.Fatal("complete real metadata-only source rejected", err)
	}
	if _, err := validateStoredBacklogInventory(normalized, testRepo(), BacklogInventoryRequest{}); err != nil {
		t.Fatal("metadata-only saved plan cannot validate its own evidence", err)
	}
	graph, _ := contract.ObjectAt(normalized, "issue_inventory")
	rows, _ := contract.Objects(graph, "issues")
	for _, row := range rows {
		for _, key := range []string{"blocked_by_numbers", "child_numbers", "parent_number"} {
			if _, exists := row[key]; exists {
				t.Fatal("unrequested topology was fabricated", key, row)
			}
		}
	}
	if _, err := normalizeBacklogInventory(source, testRepo(), BacklogInventoryRequest{IncludeRelationships: true}); err == nil {
		t.Fatal("metadata-only evidence authorized relationship scope")
	}
}

func TestBacklogMetadataScopeStillRejectsIncompleteForeignOrAmbiguousIssues(t *testing.T) {
	for name, change := range map[string]func(contract.Object, contract.Object, contract.Object){
		"missing milestone":   func(_, _ contract.Object, row contract.Object) { delete(row, "milestone") },
		"invalid state":       func(_, _ contract.Object, row contract.Object) { row["state"] = "ARCHIVED" },
		"fractional identity": func(_, _ contract.Object, row contract.Object) { row["number"] = 17.5 },
		"foreign issue": func(_, _ contract.Object, row contract.Object) {
			row["url"] = "https://github.com/foreign/widgets/issues/17"
		},
		"missing immutable identity": func(_, _ contract.Object, row contract.Object) { row["id"] = "" },
		"duplicate identity":         func(_, graph contract.Object, row contract.Object) { graph["issues"] = []any{row, row} },
		"false completeness": func(_, graph contract.Object, _ contract.Object) {
			graph["provenance"].(contract.Object)["complete"] = false
		},
		"seed source": func(_, graph contract.Object, _ contract.Object) {
			graph["provenance"].(contract.Object)["issues_source"] = "seed"
		},
		"unobserved issues": func(_, graph contract.Object, _ contract.Object) {
			graph["provenance"].(contract.Object)["issues_live"] = false
		},
		"wrong issue count": func(_, graph contract.Object, _ contract.Object) { graph["issue_count"] = int64(0) },
		"repository incarnation drift": func(source, _ contract.Object, _ contract.Object) {
			source["provenance"].(contract.Object)["repository_node_id"] = "R_other"
		},
	} {
		t.Run(name, func(t *testing.T) {
			source := backlogMetadataOnlySource()
			graph := source["issue_inventory"].(contract.Object)
			rows, _ := contract.Objects(graph, "issues")
			change(source, graph, rows[0])
			if _, err := normalizeBacklogInventory(source, testRepo(), BacklogInventoryRequest{}); err == nil {
				t.Fatal("bad metadata-only source accepted")
			}
		})
	}
}
