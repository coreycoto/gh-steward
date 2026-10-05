package runrecovery

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
)

var legacyStoreEntryName = regexp.MustCompile(`^([0-9]{6})-([a-f0-9]{64})\.json$`)

func legacyPrivateFile(info os.FileInfo) bool {
	if info == nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Uid == uint32(os.Geteuid())
}

// The store is a real, private existing directory. Every ancestor is checked;
// an untrusted package cannot redirect local settlement persistence via a link.
func legacyStoreDirectory(path string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil || path == "" {
		return "", recoveryError("legacy import needs an existing private store directory")
	}
	current := string(filepath.Separator)
	for _, part := range strings.Split(strings.TrimPrefix(absolute, current), string(filepath.Separator)) {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return "", recoveryError("legacy store directory or ancestor is missing or symbolic")
		}
	}
	info, err := os.Lstat(absolute)
	if err != nil || info.Mode().Perm()&0077 != 0 {
		return "", recoveryError("legacy store must be private to its owner")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Geteuid()) {
		return "", recoveryError("legacy store must be owned by the current user")
	}
	return absolute, nil
}

func legacyOpenStore(path string) (*os.Root, error) {
	directory, err := legacyStoreDirectory(path)
	if err != nil {
		return nil, err
	}
	before, err := os.Lstat(directory)
	if err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(directory)
	if err != nil {
		return nil, err
	}
	opened, err := root.Stat(".")
	if err != nil || !os.SameFile(before, opened) {
		root.Close()
		return nil, recoveryError("legacy store directory changed while opening")
	}
	return root, nil
}

func legacyStoreLock(root *os.Root) (*os.File, error) {
	file, err := root.OpenFile(".lock", syscall.O_CREAT|syscall.O_RDWR|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0600)
	if err != nil {
		return nil, err
	}
	info, err := file.Stat()
	if err != nil || !legacyPrivateFile(info) {
		file.Close()
		return nil, recoveryError("legacy store lock must be a private regular file")
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		file.Close()
		return nil, recoveryError("another process owns the legacy import store")
	}
	return file, nil
}

func readLegacyStoreEntry(root *os.Root, name string) (Object, error) {
	file, err := root.OpenFile(name, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !legacyPrivateFile(info) || info.Size() > MaxFileBytes {
		return nil, recoveryError("legacy store entry must be an owned private regular file")
	}
	bytes, err := io.ReadAll(io.LimitReader(file, MaxFileBytes+1))
	if err != nil || len(bytes) > MaxFileBytes {
		return nil, recoveryError("legacy store entry exceeds its byte bound")
	}
	value, err := DecodeValue(bytes)
	if err != nil {
		return nil, err
	}
	return Exact(value, []string{"schema_version", "sequence", "previous_sha256", "review_sha256", "checkpoint"}, "legacy store entry")
}

// Linking a fully synced temporary file commits without replacing any existing
// receipt. A crash before the link leaves inert scratch; after it, a fresh
// process verifies and returns the same exact imported checkpoint.
func writeLegacyStoreEntry(root *os.Root, name string, entry Object) error {
	bytes, err := Canonical(entry)
	if err != nil || len(bytes)+1 > MaxFileBytes {
		return recoveryError("legacy store receipt exceeds its explicit bound")
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return err
	}
	path := ".legacy-pending-" + hex.EncodeToString(nonce[:])
	file, err := root.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return err
	}
	defer root.Remove(path)
	defer file.Close()
	if err := file.Chmod(0600); err != nil {
		return err
	}
	if _, err := file.Write(append(bytes, '\n')); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := root.Link(path, name); err != nil {
		return err
	}
	dir, err := root.Open(".")
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func validateLegacyStoreAdvance(previous, next Object, reviewSHA string) error {
	before, after := previous["settlements"].([]any), next["settlements"].([]any)
	if len(after) != len(before)+1 || !Equal(before, after[:len(before)]) ||
		!Equal(previous["prepared_frontier"], next["prepared_frontier"]) ||
		!Equal(previous["prepared_terminal_proofs"], next["prepared_terminal_proofs"]) ||
		!exactInt(next["schema_version"], legacySettlementSchemaVersion) {
		return recoveryError("legacy store advancement replaces history or is not one explicit legacy append")
	}
	record := after[len(before)].(Object)
	if !isLegacyRecord(record) {
		return recoveryError("legacy store advancement lacks an explicit legacy outcome")
	}
	review := record["settlement"].(Object)["import_review"].(Object)
	if review["sha256"] != reviewSHA || review["previous_checkpoint_sha256"] != previous["sha256"] {
		return recoveryError("legacy store advancement differs from its exact reviewed predecessor")
	}
	return nil
}

func (e *Engine) persistLegacyImport(directory string, supplied, target Object, runs []Object, latest map[int64]int64, review Object, build func(Object) (Object, error)) (Object, error) {
	root, err := legacyOpenStore(directory)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	lock, err := legacyStoreLock(root)
	if err != nil {
		return nil, err
	}
	defer lock.Close() // Closing releases the advisory lock, including on crash.
	dir, err := root.Open(".")
	if err != nil {
		return nil, err
	}
	defer dir.Close()
	entries, err := dir.ReadDir(maxPendingAttempts + 3)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	if len(entries) > maxPendingAttempts+2 {
		return nil, recoveryError("legacy store entry inventory exceeds its bound")
	}
	names := []string{}
	for _, item := range entries {
		name := item.Name()
		if name == ".lock" || strings.HasPrefix(name, ".legacy-pending-") {
			continue
		}
		if !legacyStoreEntryName.MatchString(name) || item.IsDir() {
			return nil, recoveryError("legacy store contains an unsupported entry")
		}
		names = append(names, name)
		if len(names) > maxPendingAttempts+1 {
			return nil, recoveryError("legacy store entry inventory exceeds its bound")
		}
	}
	sort.Strings(names)
	var previous Object
	var importedCheckpointSHA any
	for index, name := range names {
		entry, err := readLegacyStoreEntry(root, name)
		if err != nil || !exactInt(entry["schema_version"], 1) || !exactInt(entry["sequence"], int64(index)) ||
			!strings.HasPrefix(name, fmt.Sprintf("%06d-", index)) {
			return nil, recoveryError("legacy store has malformed, missing or repeated sequence receipts")
		}
		chain, err := e.ValidateChain(entry["checkpoint"], target, runs, latest)
		if err != nil {
			return nil, err
		}
		if name != fmt.Sprintf("%06d-%s.json", index, chain["sha256"]) {
			return nil, recoveryError("legacy store filename differs from its immutable checkpoint")
		}
		if index == 0 {
			if entry["previous_sha256"] != nil || entry["review_sha256"] != nil {
				return nil, recoveryError("legacy store base cannot pretend to be an imported receipt")
			}
		} else {
			sha, ok := entry["review_sha256"].(string)
			if !ok || !IsSHA256(sha) || entry["previous_sha256"] != previous["sha256"] {
				return nil, recoveryError("legacy store predecessor link is invalid")
			}
			if err := validateLegacyStoreAdvance(previous, chain, sha); err != nil {
				return nil, err
			}
			if sha == review["sha256"] {
				if importedCheckpointSHA != nil {
					return nil, recoveryError("legacy store repeats an imported review")
				}
				importedCheckpointSHA = chain["sha256"]
			}
		}
		previous = chain
	}
	if importedCheckpointSHA != nil {
		return Object{"outcome": "already_imported", "checkpoint": previous, "imported_checkpoint_sha256": importedCheckpointSHA, "review_sha256": review["sha256"]}, nil
	}
	if previous == nil {
		previous, err = e.ValidateChain(supplied, target, runs, latest)
		if err != nil {
			return nil, err
		}
	} else if !Equal(previous, supplied) {
		return nil, recoveryError("supplied checkpoint is not the latest append-only store head")
	}
	next, err := build(previous)
	if err != nil {
		return nil, err
	}
	if err := validateLegacyStoreAdvance(previous, next, review["sha256"].(string)); err != nil {
		return nil, err
	}
	if len(names) == 0 {
		base := Object{"schema_version": int64(1), "sequence": int64(0), "previous_sha256": nil, "review_sha256": nil, "checkpoint": previous}
		if err := writeLegacyStoreEntry(root, fmt.Sprintf("000000-%s.json", previous["sha256"]), base); err != nil {
			return nil, err
		}
		names = append(names, "base")
	}
	entry := Object{"schema_version": int64(1), "sequence": int64(len(names)), "previous_sha256": previous["sha256"], "review_sha256": review["sha256"], "checkpoint": next}
	if err := writeLegacyStoreEntry(root, fmt.Sprintf("%06d-%s.json", len(names), next["sha256"]), entry); err != nil {
		if errors.Is(err, os.ErrExist) {
			return nil, recoveryError("legacy import receipt already exists; reopen the store to verify its exact outcome")
		}
		return nil, err
	}
	return Object{"outcome": "imported", "checkpoint": next, "imported_checkpoint_sha256": next["sha256"], "review_sha256": review["sha256"]}, nil
}
