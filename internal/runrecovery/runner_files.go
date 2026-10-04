package runrecovery

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

// runnerDirectory confines invocation packages to a real direct child of the
// runner scratch directory. Only RUNNER_TEMP itself may be an OS path alias.
func runnerDirectory(value, runnerTemp string, create, empty bool) (string, error) {
	if !filepath.IsAbs(runnerTemp) || filepath.Clean(runnerTemp) != runnerTemp ||
		!filepath.IsAbs(value) || filepath.Clean(value) != value || strings.ContainsAny(value, "\r\n\x00") {
		return "", errors.New("runner paths must be absolute and contain no traversal")
	}
	temp, err := filepath.EvalSymlinks(runnerTemp)
	if err != nil {
		return "", errors.New("RUNNER_TEMP must be an existing directory")
	}
	info, err := os.Stat(temp)
	if err != nil || !info.IsDir() {
		return "", errors.New("RUNNER_TEMP must be an existing directory")
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(value))
	if err != nil || parent != temp || filepath.Base(value) == "." || filepath.Base(value) == string(filepath.Separator) {
		return "", errors.New("package must be a direct child of RUNNER_TEMP")
	}
	destination := filepath.Join(temp, filepath.Base(value))
	info, err = os.Lstat(destination)
	if errors.Is(err, os.ErrNotExist) && create {
		if err := os.Mkdir(destination, 0700); err != nil {
			return "", err
		}
		info, err = os.Lstat(destination)
	}
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", errors.New("package must be an existing real directory")
	}
	if empty {
		entries, err := os.ReadDir(destination)
		if err != nil || len(entries) != 0 {
			return "", errors.New("package destination must be empty")
		}
	}
	if create {
		if err := os.Chmod(destination, 0700); err != nil {
			return "", err
		}
	}
	return destination, nil
}

// ContextPackageDirectory applies the workflow scratch boundary to the local
// context commands before they read or persist any execution evidence.
func ContextPackageDirectory(value, runnerTemp string) (string, error) {
	return runnerDirectory(value, runnerTemp, false, false)
}

func checkpointDestination(value, runnerTemp string) (string, error) {
	if !filepath.IsAbs(value) || filepath.Clean(value) != value || strings.ContainsAny(value, "\r\n\x00") {
		return "", errors.New("checkpoint path must be absolute without traversal")
	}
	temp, err := filepath.EvalSymlinks(runnerTemp)
	if err != nil {
		return "", err
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(value))
	if err != nil || parent != temp {
		return "", errors.New("checkpoint destination must be a direct child of RUNNER_TEMP")
	}
	destination := filepath.Join(temp, filepath.Base(value))
	if _, err := os.Lstat(destination); !errors.Is(err, os.ErrNotExist) {
		return "", errors.New("checkpoint destination must be new")
	}
	return destination, nil
}

// persistPackageFile atomically replaces only a confined regular file. Raw
// evidence is durable before callers can expose a new execution phase.
func persistPackageFile(root, relative string, data []byte) error {
	if len(data) > MaxFileBytes || !observerSafeRelativePath(relative) {
		return errors.New("package output is oversized or has an unsafe path")
	}
	directory, err := os.OpenRoot(root)
	if err != nil {
		return err
	}
	defer directory.Close()
	parts := strings.Split(path.Dir(relative), "/")
	if path.Dir(relative) != "." {
		for i := range parts {
			parent := strings.Join(parts[:i+1], "/")
			if err := directory.Mkdir(filepath.FromSlash(parent), 0700); err != nil && !errors.Is(err, os.ErrExist) {
				return err
			}
			info, err := directory.Lstat(filepath.FromSlash(parent))
			if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
				return errors.New("package output has a symbolic or non-directory parent")
			}
		}
	}
	if info, err := directory.Lstat(filepath.FromSlash(relative)); err == nil {
		if !info.Mode().IsRegular() {
			return errors.New("package output cannot replace a nonregular file")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return err
	}
	temporary := relative + ".tmp-" + hex.EncodeToString(nonce[:])
	file, err := directory.OpenFile(filepath.FromSlash(temporary), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	defer directory.Remove(filepath.FromSlash(temporary))
	_, writeErr := file.Write(data)
	syncErr := file.Sync()
	closeErr := file.Close()
	if writeErr != nil || syncErr != nil || closeErr != nil {
		return errors.New("package output could not be persisted")
	}
	if err := directory.Rename(filepath.FromSlash(temporary), filepath.FromSlash(relative)); err != nil {
		return err
	}
	parent, err := directory.Open(filepath.FromSlash(path.Dir(relative)))
	if err != nil {
		return err
	}
	defer parent.Close()
	return parent.Sync()
}

func persistPackageJSON(root, relative string, value any) error {
	data, err := Canonical(value)
	if err != nil {
		return err
	}
	return persistPackageFile(root, relative, append(data, '\n'))
}

// WriteActionsOutput appends bounded single-line values to the caller's
// already-existing output file. A symlink cannot redirect workflow outputs.
func WriteActionsOutput(filename string, values Object) error {
	if filename == "" {
		return nil
	}
	if !filepath.IsAbs(filename) {
		return errors.New("GITHUB_OUTPUT must be an absolute existing regular file")
	}
	info, err := os.Lstat(filename)
	if err != nil || !info.Mode().IsRegular() {
		return errors.New("GITHUB_OUTPUT must be an existing regular file")
	}
	keys := make([]string, 0, len(values))
	for key, raw := range values {
		value, ok := raw.(string)
		if !ok || !policyPlanName.MatchString(key) || strings.ContainsAny(value, "\r\n\x00") || len(value) > 4096 {
			return errors.New("workflow output must be bounded single-line strings")
		}
		keys = append(keys, key)
	}
	sort.Strings(keys)
	file, err := os.OpenFile(filename, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		return err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !opened.Mode().IsRegular() || !os.SameFile(info, opened) {
		return errors.New("GITHUB_OUTPUT changed while opening")
	}
	var payload strings.Builder
	for _, key := range keys {
		fmt.Fprintf(&payload, "%s=%s\n", key, values[key])
	}
	if _, err := file.WriteString(payload.String()); err != nil {
		return err
	}
	return file.Sync()
}
