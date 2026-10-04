package runrecovery

import (
	"archive/zip"
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

type recoveryZipEntry struct {
	name string
	data []byte
	mode os.FileMode
}

func recoveryZip(t *testing.T, entries ...recoveryZipEntry) []byte {
	t.Helper()
	var buffer bytes.Buffer
	writer := zip.NewWriter(&buffer)
	for _, entry := range entries {
		header := &zip.FileHeader{Name: entry.name, Method: zip.Deflate}
		mode := entry.mode
		if mode == 0 {
			mode = 0600
		}
		header.SetMode(mode)
		stream, err := writer.CreateHeader(header)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := stream.Write(entry.data); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

func TestArtifactRejectsUnsafeUnusedEntriesAndDuplicateContexts(t *testing.T) {
	good := recoveryZipEntry{name: "run-context.json", data: []byte(`{"phase":"completed"}`)}
	for _, name := range []string{"../outside", "/outside", "a//b", "a/../b", "a\\b", "a\nignored"} {
		if _, err := archiveFiles(recoveryZip(t, good, recoveryZipEntry{name: name}), false); err == nil {
			t.Fatalf("unused unsafe ZIP path accepted: %q", name)
		}
	}
	for _, entries := range [][]recoveryZipEntry{
		{good, good},
		{good, {name: "escape", data: []byte("/outside"), mode: os.ModeSymlink | 0777}},
		{good, {name: "journal", data: []byte("file")}, {name: "journal/run.json", data: []byte(`{}`)}},
		{good, {name: "oversized.json", data: bytes.Repeat([]byte("a"), MaxFileBytes+1)}},
	} {
		if _, err := archiveFiles(recoveryZip(t, entries...), false); err == nil {
			t.Fatal("duplicate, symbolic, colliding or oversized artifact accepted")
		}
	}
	if _, err := archiveFiles([]byte("not a ZIP"), false); err == nil {
		t.Fatal("invalid archive accepted")
	}
}

func TestUploadedArtifactMustContainExactRetainedBytes(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "plans"), 0700); err != nil {
		t.Fatal(err)
	}
	context, plan := []byte(`{"phase":"completed"}`), []byte(`{"sha256":"original"}`)
	for name, data := range map[string][]byte{"run-context.json": context, "plans/change.json": plan} {
		if err := os.WriteFile(filepath.Join(root, name), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	payload := recoveryZip(t, recoveryZipEntry{name: "run-context.json", data: context}, recoveryZipEntry{name: "plans/change.json", data: plan})
	if err := VerifyUploadedFiles(payload, root, []string{"run-context.json", "plans/change.json"}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "plans/change.json"), []byte(`{"sha256":"modified"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := VerifyUploadedFiles(payload, root, []string{"plans/change.json"}); err == nil {
		t.Fatal("post-upload local proof modification accepted")
	}
	if err := VerifyUploadedFiles(payload, root, []string{"missing.json"}); err == nil {
		t.Fatal("missing retained evidence accepted")
	}
}

func TestCheckpointRejectsExtraFilesAndDuplicateJSONFields(t *testing.T) {
	for _, entries := range [][]recoveryZipEntry{
		{{name: "settlement-chain.json", data: []byte(`{"schema_version":3}`)}, {name: "extra.json", data: []byte(`{}`)}},
		{{name: "settlement-chain.json", data: []byte(`{"schema_version":3,"schema_version":2}`)}},
		{{name: "elsewhere.json", data: []byte(`{}`)}},
	} {
		if _, err := CheckpointFromArchive(recoveryZip(t, entries...)); err == nil {
			t.Fatal("ambiguous checkpoint accepted")
		}
	}
	chain, err := CheckpointFromArchive(recoveryZip(t, recoveryZipEntry{name: "settlement-chain.json", data: []byte(`{"schema_version":3}`)}))
	if err != nil || !exactInt(chain["schema_version"], 3) {
		t.Fatalf("valid checkpoint lost: %v", err)
	}
}

func TestArtifactExtractionPersistsInFreshProcessScratch(t *testing.T) {
	if os.Getenv("GH_STEWARD_TEST_ARCHIVE_CHILD") == "1" {
		payload, err := os.ReadFile(os.Getenv("GH_STEWARD_TEST_ARCHIVE_INPUT"))
		if err != nil {
			t.Fatal(err)
		}
		if err := ExtractPackageArchive(payload, os.Getenv("GH_STEWARD_TEST_ARCHIVE_DESTINATION")); err != nil {
			t.Fatal(err)
		}
		return
	}
	root := t.TempDir()
	destination := filepath.Join(root, "restored")
	if err := os.Mkdir(destination, 0700); err != nil {
		t.Fatal(err)
	}
	context, journal := []byte(`{"phase":"dispatching"}`), []byte(`{"steps":[{"status":"unknown"}]}`)
	filename := filepath.Join(root, "source.zip")
	if err := os.WriteFile(filename, recoveryZip(t, recoveryZipEntry{name: "run-context.json", data: context}, recoveryZipEntry{name: "journal/nested/receipt.json", data: journal}), 0600); err != nil {
		t.Fatal(err)
	}
	command := exec.Command(os.Args[0], "-test.run=^TestArtifactExtractionPersistsInFreshProcessScratch$")
	command.Env = append(os.Environ(), "GH_STEWARD_TEST_ARCHIVE_CHILD=1", "GH_STEWARD_TEST_ARCHIVE_INPUT="+filename, "GH_STEWARD_TEST_ARCHIVE_DESTINATION="+destination)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("fresh extraction process: %v %s", err, output)
	}
	for name, expected := range map[string][]byte{"run-context.json": context, "journal/nested/receipt.json": journal} {
		actual, err := ReadPackageFile(destination, name)
		if err != nil || !bytes.Equal(actual, expected) {
			t.Fatalf("original durable bytes changed after process exit: %s %v", name, err)
		}
		info, err := os.Stat(filepath.Join(destination, name))
		if err != nil || info.Mode().Perm() != 0600 {
			t.Fatal("retained file permissions differ")
		}
	}
	if err := ExtractPackageArchive(recoveryZip(t, recoveryZipEntry{name: "run-context.json", data: []byte(`{}`)}), destination); err == nil || !strings.Contains(err.Error(), "empty") {
		t.Fatal("existing source evidence overwritten")
	}
}
