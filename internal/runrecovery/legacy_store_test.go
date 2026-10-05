package runrecovery

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLegacyStoreRejectsCorruptionLinksAndMissingReceipts(t *testing.T) {
	for _, mutation := range []string{"missing-base", "missing-import", "sequence", "predecessor", "checkpoint-bytes", "public-permissions", "entry-link", "store-link", "unknown-entry"} {
		t.Run(mutation, func(t *testing.T) {
			engine, reader, options := legacyImportSetup(t, "legacy_terminal_receipt")
			if _, err := engine.ImportLegacy(context.Background(), reader, options); err != nil {
				t.Fatal(err)
			}
			entries, _ := os.ReadDir(options.StoreDirectory)
			var base, imported string
			for _, entry := range entries {
				if strings.HasPrefix(entry.Name(), "000000-") {
					base = filepath.Join(options.StoreDirectory, entry.Name())
				}
				if strings.HasPrefix(entry.Name(), "000001-") {
					imported = filepath.Join(options.StoreDirectory, entry.Name())
				}
			}
			switch mutation {
			case "missing-base":
				if err := os.Remove(base); err != nil {
					t.Fatal(err)
				}
			case "missing-import":
				if err := os.Rename(imported, strings.Replace(imported, "000001-", "000002-", 1)); err != nil {
					t.Fatal(err)
				}
			case "sequence", "predecessor", "checkpoint-bytes":
				value, _ := LoadJSON(imported)
				entry := value.(Object)
				switch mutation {
				case "sequence":
					entry["sequence"] = int64(2)
				case "predecessor":
					entry["previous_sha256"] = strings.Repeat("f", 64)
				case "checkpoint-bytes":
					entry["checkpoint"].(Object)["sha256"] = strings.Repeat("f", 64)
				}
				bytes, _ := Canonical(entry)
				if err := os.WriteFile(imported, bytes, 0600); err != nil {
					t.Fatal(err)
				}
			case "public-permissions":
				if err := os.Chmod(imported, 0644); err != nil {
					t.Fatal(err)
				}
			case "entry-link":
				bytes, _ := os.ReadFile(imported)
				target := filepath.Join(t.TempDir(), "receipt.json")
				if err := os.WriteFile(target, bytes, 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.Remove(imported); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, imported); err != nil {
					t.Fatal(err)
				}
			case "store-link":
				link := filepath.Join(t.TempDir(), "store-link")
				if err := os.Symlink(options.StoreDirectory, link); err != nil {
					t.Fatal(err)
				}
				options.StoreDirectory = link
			case "unknown-entry":
				if err := os.WriteFile(filepath.Join(options.StoreDirectory, "unreviewed.json"), []byte(`{}`), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := engine.ImportLegacy(context.Background(), reader, options); err == nil {
				t.Fatal("malformed or redirected store admitted an import")
			}
			legacyAssertReadOnly(t, reader)
		})
	}
}

func TestLegacyStoreLockAndPinnedDirectoryKeepOnePersistenceOwner(t *testing.T) {
	engine, reader, options := legacyImportSetup(t, "legacy_terminal_receipt")
	root, err := legacyOpenStore(options.StoreDirectory)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	lock, err := legacyStoreLock(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := engine.ImportLegacy(context.Background(), reader, options); err == nil {
		t.Fatal("second process acquired the same import lock")
	}
	defer lock.Close()
	// Keep descriptor-relative writes in the original locked directory even
	// if its path is renamed and replaced during an import.
	moved := options.StoreDirectory + "-moved"
	if err := os.Rename(options.StoreDirectory, moved); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(moved) })
	if err := os.Mkdir(options.StoreDirectory, 0700); err != nil {
		t.Fatal(err)
	}
	base := Object{"schema_version": int64(1), "sequence": int64(0), "previous_sha256": nil, "review_sha256": nil, "checkpoint": options.Checkpoint}
	name := "000000-" + options.Checkpoint["sha256"].(string) + ".json"
	if err := writeLegacyStoreEntry(root, name, base); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(moved, name)); err != nil {
		t.Fatal("pinned store lost its receipt", err)
	}
	if _, err := os.Stat(filepath.Join(options.StoreDirectory, name)); !os.IsNotExist(err) {
		t.Fatal("store write escaped into replacement directory")
	}
	if err := writeLegacyStoreEntry(root, name, base); err == nil {
		t.Fatal("immutable base receipt was replaced")
	}
}

func TestLegacyStoreRestartsFromSyncedBaseAndIncompleteStagingFile(t *testing.T) {
	engine, reader, options := legacyImportSetup(t, "legacy_terminal_receipt")
	root, err := legacyOpenStore(options.StoreDirectory)
	if err != nil {
		t.Fatal(err)
	}
	base := Object{"schema_version": int64(1), "sequence": int64(0), "previous_sha256": nil, "review_sha256": nil, "checkpoint": options.Checkpoint}
	if err := writeLegacyStoreEntry(root, "000000-"+options.Checkpoint["sha256"].(string)+".json", base); err != nil {
		t.Fatal(err)
	}
	root.Close()
	if err := os.WriteFile(filepath.Join(options.StoreDirectory, ".legacy-pending-interrupted"), []byte(`{"incomplete":`), 0600); err != nil {
		t.Fatal(err)
	}
	result, err := engine.ImportLegacy(context.Background(), reader, options)
	if err != nil || result["outcome"] != "imported" {
		t.Fatal("staging interruption lost reviewed progress", result, err)
	}
	if _, err := os.Stat(filepath.Join(options.StoreDirectory, ".legacy-pending-interrupted")); err != nil {
		t.Fatal("interrupted diagnostic evidence was removed", err)
	}
	legacyAssertReadOnly(t, reader)
}
