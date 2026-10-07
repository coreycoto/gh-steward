package cli

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/coreycoto/gh-steward/internal/contract"
	"github.com/coreycoto/gh-steward/internal/runrecovery"
)

func promotionCLIFixture(t *testing.T) (contract.Object, contract.Object, contract.Object, *cutoverCLIReader) {
	t.Helper()
	b := cutoverCLIBaseline(t)
	policy := cutoverCLIPolicy(false)
	wf := policy["workflows"].(contract.Object)["task.yml"].(contract.Object)
	wf["history_cutover_reviews"] = []any{b["sha256"]}
	wf["history_promotion_review_issue"] = contract.Object{"number": int64(17), "trusted_logins": []any{"maintainer"}}
	wf["plans"] = contract.Object{"execution": contract.Object{"command": "execution-sync", "domain_profile": "execution", "allowed_operation_kinds": []any{"issue-comment-upsert"}, "attempt_target": "execution", "approval": contract.Object{"kind": "git-slop-execution"}, "event": contract.Object{"kind": "execution_event", "path": "events/dispatch-event.json"}, "parent_merge": nil}}
	reader := emptyCutoverCLIReader()
	reader.objects["repos/example/widgets/issues/18"] = contract.Object{"number": int64(18), "body": "private reconciliation state"}
	engine, err := runrecovery.NewEngine(policy, cutoverCLIRepository(t))
	if err != nil {
		t.Fatal(err)
	}
	p, err := engine.PreviewHistoryPromotion(context.Background(), reader, b, nil, []string{"execution"}, []string{"repos/example/widgets/issues/18"})
	if err != nil {
		t.Fatal(err)
	}
	return b, policy, p, reader
}

func TestPromotionCLISeparatesReadOnlyCaptureAndOfflineValidation(t *testing.T) {
	t.Setenv("GITHUB_ACTIONS", "")
	b, policy, p, reader := promotionCLIFixture(t)
	root := checkout(t)
	writeLegacyCLIJSON(t, root, "policy.json", policy)
	writeLegacyCLIJSON(t, root, "baseline.json", b)
	var stdout, stderr bytes.Buffer
	err := (Runner{Out: &stdout, Err: &stderr, Actions: reader}).Run(context.Background(), []string{"runs", "promotion-preview", "--repo-root", root, "--workflow", "task.yml", "--policy", "policy.json", "--input", "baseline=baseline.json", "--plan", "execution", "--state-read", "repos/example/widgets/issues/18", "--out", "promotion.json"})
	if err != nil {
		t.Fatal(err, stderr.String())
	}
	if strings.Contains(stdout.String(), "private reconciliation state") || strings.Contains(stdout.String(), "state_reads") {
		t.Fatal("stdout leaked full provider evidence")
	}
	result, err := contract.Decode(bytes.NewReader(stdout.Bytes()))
	data, dataErr := contract.ObjectAt(result, "data")
	if err != nil || dataErr != nil || data["promotion_sha256"] != p["sha256"] || data["activation"] != "not-performed" || data["validation"] != "complete-live-read-only-promotion-capture" {
		t.Fatal(result, err)
	}
	if !runrecovery.Equal(data["promotion_schema_version"], int64(2)) || data["state_contract"] != "github-rest-pr-repository-clock-v1" {
		t.Fatal("capture summary hid its reconciliation semantics", data)
	}
	info, err := os.Stat(filepath.Join(root, "promotion.json"))
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("promotion evidence is not private", info, err)
	}
	pathWithGitOnly(t)
	stdout.Reset()
	stderr.Reset()
	err = (Runner{Out: &stdout, Err: &stderr, Actions: forbiddenLegacyCLIProvider{t}}).Run(context.Background(), []string{"runs", "promotion-validate", "--repo-root", root, "--input", "promotion=promotion.json"})
	if err != nil {
		t.Fatal(err, stderr.String())
	}
	result, err = contract.Decode(bytes.NewReader(stdout.Bytes()))
	data, _ = contract.ObjectAt(result, "data")
	if err != nil || data["validation"] != "shape-and-digest-only" || data["activation"] != "not-performed" {
		t.Fatal("offline validation claimed review or activation", result, err)
	}
	if !runrecovery.Equal(data["promotion_schema_version"], int64(2)) || data["state_contract"] != "github-rest-pr-repository-clock-v1" {
		t.Fatal("offline validation lost its sealed reconciliation contract", data)
	}
}

func TestPromotionCLIRequiresReviewableScopeBeforeProviderRead(t *testing.T) {
	for _, kind := range []string{"missing-out", "missing-plans", "missing-state", "unpaired-artifact", "overlap", "apply-flag", "symlink", "untrusted-actions", "held-zero", "held-negative", "held-leading-zero", "held-overflow"} {
		t.Run(kind, func(t *testing.T) {
			t.Setenv("GITHUB_ACTIONS", "")
			root := checkout(t)
			b, policy, _, _ := promotionCLIFixture(t)
			writeLegacyCLIJSON(t, root, "policy.json", policy)
			writeLegacyCLIJSON(t, root, "baseline.json", b)
			args := []string{"runs", "promotion-preview", "--repo-root", root, "--workflow", "task.yml", "--policy", "policy.json", "--input", "baseline=baseline.json"}
			if kind != "missing-plans" {
				args = append(args, "--plan", "execution")
			}
			if kind != "missing-state" {
				args = append(args, "--state-read", "repos/example/widgets/issues/18")
			}
			if kind != "missing-out" {
				out := "promotion.json"
				if kind == "overlap" {
					out = "policy.json"
				}
				if kind == "symlink" {
					if err := os.Symlink(t.TempDir(), filepath.Join(root, "outside")); err != nil {
						t.Fatal(err)
					}
					out = "outside/promotion.json"
				}
				args = append(args, "--out", out)
			}
			if kind == "unpaired-artifact" {
				args = append(args, "--checkpoint-artifact-id", "31")
			}
			if kind == "apply-flag" {
				args = append(args, "--apply")
			}
			if raw, exists := map[string]string{"held-zero": "0", "held-negative": "-1", "held-leading-zero": "007", "held-overflow": "9223372036854775808"}[kind]; exists {
				args = append(args, "--held-run-id", raw)
			}
			if kind == "untrusted-actions" {
				t.Setenv("GITHUB_ACTIONS", "true")
				t.Setenv("GITHUB_WORKFLOW_SHA", strings.Repeat("a", 40))
			}
			var out, errOut bytes.Buffer
			err := (Runner{Out: &out, Err: &errOut, Actions: forbiddenLegacyCLIProvider{t}}).Run(context.Background(), args)
			if err == nil || out.Len() != 0 {
				t.Fatal("unsafe capture emitted success", kind, err, out.String())
			}
		})
	}
}

func TestPromotionReviewRequiresCurrentExactHumanPrincipalAndClearsCachedAdmission(t *testing.T) {
	_, policy, p, _ := promotionCLIFixture(t)
	repo := cutoverCLIRepository(t)
	approve := "APPROVE HISTORY PROMOTION example/widgets#17 task.yml " + fmt.Sprint(p["sha256"])
	for _, kind := range []string{"approved", "cutover-only", "revoked", "superseded", "deleted", "bot", "untrusted", "read-only", "foreign-permission", "duplicate", "incomplete", "foreign-comment", "malformed", "timestamp"} {
		t.Run(kind, func(t *testing.T) {
			engine, err := runrecovery.NewEngine(policy, repo)
			if err != nil {
				t.Fatal(err)
			}
			rows := []any{cutoverReviewComment(100, approve, "maintainer", "User")}
			count := int64(1)
			permission := contract.Object{"permission": "write", "user": contract.Object{"login": "maintainer", "type": "User"}}
			reader := &cutoverCLIReader{objects: map[string]contract.Object{"repos/example/widgets/issues/17": {"id": int64(71), "number": int64(17), "comments": count, "html_url": "https://github.com/example/widgets/issues/17"}, "repos/example/widgets/collaborators/maintainer/permission": permission}, pages: map[string][]any{"repos/example/widgets/issues/17/comments?per_page=100": {rows}}}
			if err := resolveHistoryPromotionReviews(context.Background(), reader, engine, repo, "task.yml"); err != nil {
				t.Fatal(err)
			}
			if _, err := engine.ValidateReviewedHistoryPromotion(p, p["target"]); err != nil {
				t.Fatal("initial current approval rejected", err)
			}
			switch kind {
			case "cutover-only":
				rows[0].(contract.Object)["body"] = strings.Replace(approve, "PROMOTION", "CUTOVER", 1)
			case "revoked":
				rows = append(rows, cutoverReviewComment(101, strings.Replace(approve, "APPROVE", "REVOKE", 1), "maintainer", "User"))
				count = 2
			case "superseded":
				rows = append(rows, cutoverReviewComment(101, "APPROVE HISTORY PROMOTION example/widgets#17 task.yml "+strings.Repeat("a", 64), "maintainer", "User"))
				count = 2
			case "deleted":
				rows = []any{}
				count = 0
			case "bot":
				rows[0].(contract.Object)["user"].(contract.Object)["type"] = "Bot"
			case "untrusted":
				rows[0].(contract.Object)["user"].(contract.Object)["login"] = "stranger"
			case "read-only":
				permission["permission"] = "read"
			case "foreign-permission":
				permission["user"].(contract.Object)["login"] = "other"
			case "duplicate":
				rows = append(rows, rows[0])
				count = 2
			case "incomplete":
				count = 2
			case "foreign-comment":
				rows[0].(contract.Object)["html_url"] = "https://github.com/other/widgets/issues/17#issuecomment-100"
			case "malformed":
				rows[0].(contract.Object)["body"] = "APPROVE HISTORY PROMOTION example/widgets#17 task.yml invalid"
			case "timestamp":
				rows[0].(contract.Object)["updated_at"] = "2026-10-01T12:00:00Z"
			}
			reader.objects["repos/example/widgets/issues/17"]["comments"] = count
			reader.pages["repos/example/widgets/issues/17/comments?per_page=100"] = []any{rows}
			resolutionErr := resolveHistoryPromotionReviews(context.Background(), reader, engine, repo, "task.yml")
			_, admittedErr := engine.ValidateReviewedHistoryPromotion(p, p["target"])
			if kind == "approved" {
				if resolutionErr != nil || admittedErr != nil {
					t.Fatal(resolutionErr, admittedErr)
				}
			} else if admittedErr == nil {
				t.Fatal("cached or forged admission survived", kind)
			}
		})
	}
}

func TestPromotionContextActionsRefreshReviewBeforePersisting(t *testing.T) {
	t.Setenv("GITHUB_ACTIONS", "")
	for _, approved := range []bool{false, true} {
		t.Run(fmt.Sprint(approved), func(t *testing.T) {
			_, policy, p, reader := promotionCLIFixture(t)
			root := checkout(t)
			writeLegacyCLIJSON(t, root, "policy.json", policy)
			temp := t.TempDir()
			packageRoot := filepath.Join(temp, "package")
			if err := os.Mkdir(packageRoot, 0700); err != nil {
				t.Fatal(err)
			}
			// A promoted checkpoint starts from the sealed empty native preview prefix.
			preview := p["preview_checkpoint"].(contract.Object)
			chain := contract.Object{"schema_version": int64(7), "target": p["target"], "history_promotion": p, "inventory": preview["inventory"], "settlements": preview["settlements"], "prepared_frontier": []any{}, "prepared_terminal_proofs": []any{}}
			encoded, err := runrecovery.Canonical(chain)
			if err != nil {
				t.Fatal(err)
			}
			chain["sha256"] = runrecovery.SHA256(encoded)
			encoded, _ = runrecovery.Canonical(chain)
			if err := os.WriteFile(filepath.Join(packageRoot, "settlement-chain.json"), encoded, 0600); err != nil {
				t.Fatal(err)
			}
			rows := []any{}
			if approved {
				rows = append(rows, cutoverReviewComment(100, "APPROVE HISTORY PROMOTION example/widgets#17 task.yml "+fmt.Sprint(p["sha256"]), "maintainer", "User"))
			}
			reader.objects["repos/example/widgets/issues/17"] = contract.Object{"id": int64(71), "number": int64(17), "comments": int64(len(rows)), "html_url": "https://github.com/example/widgets/issues/17"}
			reader.objects["repos/example/widgets/collaborators/maintainer/permission"] = contract.Object{"permission": "write", "user": contract.Object{"login": "maintainer", "type": "User"}}
			reader.pages["repos/example/widgets/issues/17/comments?per_page=100"] = []any{rows}
			writeLegacyCLIJSON(t, root, "context.json", contract.Object{"workflow_file": "task.yml", "repository": "example/widgets", "recovery_key": "task", "run_name": "Current", "run_id": int64(101), "attempt": int64(1), "attempt_target": contract.Object{"kind": "issue", "number": int64(17)}, "dispatch_steps": []any{"Apply reviewed plan"}})
			var out, errOut bytes.Buffer
			err = (Runner{Out: &out, Err: &errOut, Actions: reader}).Run(context.Background(), []string{"runs", "context-start", "--repo-root", root, "--workflow", "task.yml", "--policy", "policy.json", "--runner-temp", temp, "--package-root", packageRoot, "--input", "context=context.json"})
			if (err == nil) != approved {
				t.Fatal("context ignored current promotion review", approved, err, errOut.String())
			}
			if !approved {
				if _, err := os.Stat(filepath.Join(packageRoot, "run-context.json")); !os.IsNotExist(err) {
					t.Fatal("denied promotion wrote native context")
				}
			}
		})
	}
}
