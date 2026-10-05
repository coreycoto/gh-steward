package cli

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/coreycoto/gh-steward/internal/contract"
)

type legacyResultOutput struct {
	path        string
	dir         *os.Root
	roots       []*os.Root
	name        string
	temp        string
	file        *os.File
	hasExisting bool
}

func legacyOutputParts(path string) ([]string, error) {
	if path == "" || filepath.IsAbs(path) || strings.ContainsAny(path, "\\\r\n\x00") || filepath.ToSlash(filepath.Clean(path)) != path || path == "." {
		return nil, errors.New("legacy full-result path must be a canonical checkout-relative file path")
	}
	parts := strings.Split(path, "/")
	for _, part := range parts {
		if part == "" || part == "." || part == ".." {
			return nil, errors.New("legacy full-result path must not contain traversal")
		}
	}
	return parts, nil
}

func validateLegacyOutputConflicts(checkout, outPath string, inputs inputsFlag, policyPath, storePath string) error {
	if outPath == "" {
		return nil
	}
	outPath, err := filepath.Abs(filepath.Join(checkout, outPath))
	if err != nil {
		return err
	}
	outPath = filepath.Clean(outPath)
	checkAlias := func(label, path string) error {
		if path == "" || path == "-" {
			return nil
		}
		if !filepath.IsAbs(path) {
			path = filepath.Join(checkout, path)
		}
		path, err := filepath.Abs(path)
		if err != nil {
			return err
		}
		path = filepath.Clean(path)
		if path == outPath {
			return fmt.Errorf("legacy full-result output must not replace its %s", label)
		}
		outInfo, outErr := os.Stat(outPath)
		inputInfo, inputErr := os.Stat(path)
		if outErr == nil && inputErr == nil && os.SameFile(outInfo, inputInfo) {
			return fmt.Errorf("legacy full-result output must not alias its %s", label)
		}
		return nil
	}
	for _, input := range inputs {
		parts := strings.SplitN(input, "=", 2)
		if len(parts) == 2 {
			if err := checkAlias("input", parts[1]); err != nil {
				return err
			}
		}
	}
	if err := checkAlias("policy", policyPath); err != nil {
		return err
	}
	if storePath != "" {
		store := filepath.ToSlash(filepath.Clean(storePath))
		relative, err := filepath.Rel(checkout, outPath)
		if err != nil {
			return err
		}
		output := filepath.ToSlash(relative)
		if store == output || strings.HasPrefix(store, output+"/") || strings.HasPrefix(output, store+"/") {
			return errors.New("legacy full-result output must not overlap the append-only store")
		}
	}
	return nil
}

func legacyOwned(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Uid == uint32(os.Geteuid())
}

func legacyPrivateDirectory(info os.FileInfo) bool {
	return info != nil && info.IsDir() && info.Mode()&os.ModeSymlink == 0 && info.Mode().Perm()&0022 == 0 && legacyOwned(info)
}

func legacyPrivateRegular(info os.FileInfo) bool {
	return info != nil && info.Mode().IsRegular() && info.Mode()&os.ModeSymlink == 0 && info.Mode().Perm()&0077 == 0 && legacyOwned(info)
}

// prepareLegacyResultOutput pins every existing path component, creates only
// owner-private missing parents, and creates a synced private temp before any
// provider read or local import can occur.
func prepareLegacyResultOutput(checkout, path string) (*legacyResultOutput, error) {
	if path == "" {
		return nil, nil
	}
	parts, err := legacyOutputParts(path)
	if err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(checkout)
	if err != nil {
		return nil, errors.New("cannot pin trusted checkout for private legacy result output")
	}
	current := root
	ownedRoots := []*os.Root{}
	closeRoots := func() {
		for i := len(ownedRoots) - 1; i >= 0; i-- {
			_ = ownedRoots[i].Close()
		}
		_ = root.Close()
	}
	for _, part := range parts[:len(parts)-1] {
		info, statErr := current.Lstat(part)
		if errors.Is(statErr, os.ErrNotExist) {
			if mkdirErr := current.Mkdir(part, 0700); mkdirErr != nil && !errors.Is(mkdirErr, os.ErrExist) {
				closeRoots()
				return nil, fmt.Errorf("cannot create private legacy result directory: %w", mkdirErr)
			}
			info, statErr = current.Lstat(part)
		}
		if statErr != nil || !legacyPrivateDirectory(info) {
			closeRoots()
			return nil, errors.New("legacy result parent must be an owned directory without symlinks or shared write access")
		}
		next, openErr := current.OpenRoot(part)
		if openErr != nil {
			closeRoots()
			return nil, errors.New("cannot pin legacy result parent directory")
		}
		ownedRoots = append(ownedRoots, next)
		current = next
	}
	name := parts[len(parts)-1]
	roots := append([]*os.Root{root}, ownedRoots...)
	output := &legacyResultOutput{path: path, dir: current, roots: roots, name: name}
	info, statErr := current.Lstat(name)
	if statErr == nil {
		if !legacyPrivateRegular(info) {
			closeRoots()
			return nil, errors.New("existing legacy full-result output must be an owned private regular file")
		}
		file, openErr := current.Open(name)
		if openErr != nil {
			closeRoots()
			return nil, errors.New("cannot open existing legacy full-result output")
		}
		opened, openedErr := file.Stat()
		if openedErr != nil || !legacyPrivateRegular(opened) || !os.SameFile(info, opened) {
			_ = file.Close()
			closeRoots()
			return nil, errors.New("existing legacy full-result output changed while opening")
		}
		data, readErr := io.ReadAll(io.LimitReader(file, contract.MaxJSONBytes+1))
		_ = file.Close()
		if readErr != nil || len(data) > contract.MaxJSONBytes {
			closeRoots()
			return nil, errors.New("existing legacy full-result output exceeds its size bound")
		}
		output.hasExisting = true
		output.temp = ""
		return output, nil
	}
	if !errors.Is(statErr, os.ErrNotExist) {
		closeRoots()
		return nil, errors.New("cannot inspect legacy full-result destination")
	}
	file, temp, err := createLegacyResultTemp(current)
	if err != nil {
		closeRoots()
		return nil, err
	}
	output.file, output.temp = file, temp
	return output, nil
}

func createLegacyResultTemp(directory *os.Root) (*os.File, string, error) {
	for attempt := 0; attempt < 8; attempt++ {
		random := make([]byte, 16)
		if _, err := rand.Read(random); err != nil {
			return nil, "", err
		}
		name := ".gh-steward-legacy-result-" + hex.EncodeToString(random)
		file, err := directory.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if errors.Is(err, os.ErrExist) {
			continue
		}
		if err != nil {
			return nil, "", fmt.Errorf("cannot create private legacy result temp: %w", err)
		}
		if err := file.Chmod(0600); err != nil {
			_ = file.Close()
			_ = directory.Remove(name)
			return nil, "", err
		}
		if _, err := file.Write([]byte{0}); err != nil {
			_ = file.Close()
			_ = directory.Remove(name)
			return nil, "", err
		}
		if err := file.Truncate(0); err != nil {
			_ = file.Close()
			_ = directory.Remove(name)
			return nil, "", err
		}
		if _, err := file.Seek(0, io.SeekStart); err != nil {
			_ = file.Close()
			_ = directory.Remove(name)
			return nil, "", err
		}
		if err := file.Sync(); err != nil {
			_ = file.Close()
			_ = directory.Remove(name)
			return nil, "", err
		}
		return file, name, nil
	}
	return nil, "", errors.New("could not allocate a unique private legacy result temp")
}

func (o *legacyResultOutput) Close() {
	if o == nil {
		return
	}
	if o.file != nil {
		_ = o.file.Close()
		_ = o.dir.Remove(o.temp)
	}
	for i := len(o.roots) - 1; i >= 0; i-- {
		_ = o.roots[i].Close()
	}
}

func (o *legacyResultOutput) Write(data []byte) error {
	if o == nil {
		return nil
	}
	if len(data) > contract.MaxJSONBytes {
		return errors.New("legacy full result exceeds the supported output bound")
	}
	if o.hasExisting {
		existing, err := readLegacyResult(o.dir, o.name)
		if err != nil {
			return err
		}
		if bytes.Equal(existing, data) {
			return nil
		}
		return errors.New("legacy full-result output already exists with different bytes")
	}
	if o.file == nil || o.temp == "" {
		return errors.New("legacy private result output was not prepared")
	}
	if err := o.file.Truncate(0); err != nil {
		return err
	}
	if _, err := o.file.Seek(0, io.SeekStart); err != nil {
		return err
	}
	if _, err := io.Copy(o.file, bytes.NewReader(data)); err != nil {
		return err
	}
	if err := o.file.Sync(); err != nil {
		return err
	}
	info, err := o.dir.Lstat(o.temp)
	opened, statErr := o.file.Stat()
	if err != nil || statErr != nil || !legacyPrivateRegular(info) || !legacyPrivateRegular(opened) || !os.SameFile(info, opened) {
		return errors.New("private legacy result temp changed before linking")
	}
	if err := o.file.Close(); err != nil {
		return err
	}
	o.file = nil
	if err := o.dir.Link(o.temp, o.name); err != nil {
		if errors.Is(err, os.ErrExist) {
			if existing, readErr := readLegacyResult(o.dir, o.name); readErr == nil && bytes.Equal(existing, data) {
				_ = o.dir.Remove(o.temp)
				o.temp = ""
				return nil
			}
			return errors.New("legacy full-result output appeared with different bytes")
		}
		return fmt.Errorf("cannot link private legacy full-result output: %w", err)
	}
	if err := o.dir.Remove(o.temp); err != nil {
		return fmt.Errorf("legacy result was linked but its temporary name could not be removed: %w", err)
	}
	o.temp = ""
	directory, err := o.dir.Open(".")
	if err != nil {
		return err
	}
	defer directory.Close()
	if err := directory.Sync(); err != nil {
		return err
	}
	return nil
}

func readLegacyResult(directory *os.Root, name string) ([]byte, error) {
	info, err := directory.Lstat(name)
	if err != nil || !legacyPrivateRegular(info) {
		return nil, errors.New("legacy full-result output is unsafe")
	}
	file, err := directory.Open(name)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !legacyPrivateRegular(opened) || !os.SameFile(info, opened) {
		return nil, errors.New("legacy full-result output changed while opening")
	}
	data, err := io.ReadAll(io.LimitReader(file, contract.MaxJSONBytes+1))
	if err != nil || len(data) > contract.MaxJSONBytes {
		return nil, errors.New("legacy full-result output exceeds its size bound")
	}
	return data, nil
}
