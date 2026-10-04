package cli

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/coreycoto/gh-steward/internal/contract"
)

func checkout(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for _, args := range [][]string{{"init", "-q"}, {"remote", "add", "origin", "git@github.com:example/widgets.git"}} {
		c := exec.Command("git", args...)
		c.Dir = root
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatal(string(out), err)
		}
	}
	return root
}
func TestNativeExtensionMachineEnvelopeUsesConsumerCheckout(t *testing.T) {
	root := checkout(t)
	graph := `{"repo":{"nameWithOwner":"example/widgets","url":"https://github.com/example/widgets"},"issues":[]}`
	if err := os.WriteFile(filepath.Join(root, "graph.json"), []byte(graph), 0600); err != nil {
		t.Fatal(err)
	}
	var out, stderr bytes.Buffer
	err := (Runner{Out: &out, Err: &stderr}).Run(context.Background(), []string{"relationships", "audit", "--repo-root", root, "--input", "issue_graph=graph.json"})
	if err != nil {
		t.Fatal(err, stderr.String())
	}
	result, err := contract.Decode(&out)
	if err != nil {
		t.Fatal(err)
	}
	if version, _ := contract.Integer(result["schema_version"]); version != 2 || result["command"] != "relationship-audit" {
		t.Fatal("wrong machine contract", result)
	}
	repo, _ := contract.ObjectAt(result, "repository")
	if repo["nameWithOwner"] != "example/widgets" {
		t.Fatal("checkout target lost")
	}
}

func TestCLIRejectsForeignInputsAmbiguousStdinAndMalformedJSON(t *testing.T) {
	root := checkout(t)
	for _, body := range []string{`{"repo":{"nameWithOwner":"other/widgets","url":"https://github.com/other/widgets"},"issues":[]}`, `{"repo":{},"issues":[]}`, `{"issues":[],"issues":[]}`, `{"schema_version":1,"tool_version":"0.1.0","repository":{},"data":{}}`} {
		var out, stderr bytes.Buffer
		err := (Runner{Out: &out, Err: &stderr, Input: strings.NewReader(body)}).Run(context.Background(), []string{"relationships", "audit", "--repo-root", root, "--input", "issue_graph=-"})
		if err == nil || out.Len() != 0 {
			t.Fatal("invalid scope/input emitted success", body, err, out.String())
		}
	}
	for _, tail := range [][]string{{"--input", "issue_graph=-", "--input", "payload=-"}, {"--input", "issue_graph=-", "--input", "issue_graph=-"}, {"--format", "text"}, {"--unexpected"}, {"position"}} {
		var out, stderr bytes.Buffer
		err := (Runner{Out: &out, Err: &stderr, Input: strings.NewReader(`{"issues":[]}`)}).Run(context.Background(), append([]string{"relationships", "audit", "--repo-root", root}, tail...))
		if err == nil {
			t.Fatal("ambiguous CLI arguments accepted", tail)
		}
	}
}

func TestVersionWorksWithoutGitHubOrCheckoutAndTypedOutputFiles(t *testing.T) {
	var out, stderr bytes.Buffer
	if err := (Runner{Out: &out, Err: &stderr}).Run(context.Background(), []string{"version", "--json"}); err != nil {
		t.Fatal(err)
	}
	info, err := contract.Decode(&out)
	if err != nil || info["tool"] != "gh-steward" || info["source_revision"] != SourceRevision {
		t.Fatal(info, err)
	}
	root := t.TempDir()
	outside := filepath.Join(root, "outside")
	if err := os.WriteFile(outside, []byte("preserve"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "result.json")); err != nil {
		t.Fatal(err)
	}
	if err := writeFile(root, "result.json", contract.Object{"status": "success"}); err == nil {
		t.Fatal("output symlink replaced")
	}
	if err := writeFile(root, "reports/result.json", contract.Object{"status": "planned"}); err != nil {
		t.Fatal(err)
	}
	got, err := loadObject(root, "reports/result.json")
	if err != nil || got["status"] != "planned" {
		t.Fatal(got, err)
	}
}
