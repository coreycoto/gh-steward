package runrecovery

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestRetainedProofRejectsTamperingDuplicatesAndInvalidUTF8(t *testing.T) {
	raw := []byte(`{"run":42,"attempt":1}`)
	value, err := LoadFileProof(MakeFileProof(raw), "run")
	if err != nil || value.(Object)["run"] != json.Number("42") {
		t.Fatalf("valid raw proof: %v %v", value, err)
	}
	bad := MakeFileProof(raw)
	bad["sha256"] = SHA256([]byte("other"))
	if _, err := LoadFileProof(bad, "run"); err == nil {
		t.Fatal("tampered raw bytes accepted")
	}
	for _, data := range [][]byte{[]byte(`{"run":42,"run":43}`), []byte(`{"nested":{"run":1,"run":2}}`), {0xff}, []byte(`{} {}`)} {
		if _, err := LoadFileProof(MakeFileProof(data), "run"); err == nil {
			t.Fatalf("malformed retained proof accepted: %q", data)
		}
	}
	bad = MakeFileProof(raw)
	bad["base64"] = bad["base64"].(string) + "\n"
	if _, err := LoadFileProof(bad, "run"); err == nil {
		t.Fatal("noncanonical base64 accepted")
	}
}

func TestPackageEvidenceRejectsTraversalAndSymbolicParents(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret.json"), []byte(`{}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "evidence"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "evidence", "run.json"), []byte(`{}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadPackageFile(root, "evidence/run.json"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "secret.json"), filepath.Join(root, "link.json")); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"../secret.json", "evidence/../evidence/run.json", "escape/secret.json", "link.json", "/secret.json", "evidence\\run.json"} {
		if _, err := ReadPackageFile(root, name); err == nil {
			t.Fatalf("unsafe evidence path accepted: %q", name)
		}
	}
}
