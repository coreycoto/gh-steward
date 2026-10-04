package runrecovery

import (
	"archive/zip"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// archiveFiles fully validates bounded archive entries, even unused files.
// A receipt cannot hide an unsafe extra path or a second copy of its context.
func archiveFiles(payload []byte, checkpoint bool) (map[string][]byte, error) {
	if len(payload) > MaxArtifactBytes {
		return nil, errors.New("artifact archive exceeds its size bound")
	}
	reader, err := zip.NewReader(bytes.NewReader(payload), int64(len(payload)))
	if err != nil {
		return nil, fmt.Errorf("artifact archive is invalid: %w", err)
	}
	limit := MaxArtifactEntries
	if checkpoint {
		limit = 8
	}
	if len(reader.File) > limit {
		return nil, errors.New("artifact archive has too many entries")
	}
	files, seen := map[string][]byte{}, map[string]bool{}
	var expanded uint64
	for _, entry := range reader.File {
		name := entry.Name
		if name == "" || strings.ContainsAny(name, "\\\x00\r\n") || path.IsAbs(name) || path.Clean(strings.TrimSuffix(name, "/")) != strings.TrimSuffix(name, "/") {
			return nil, errors.New("artifact archive contains an unsafe path")
		}
		for _, part := range strings.Split(strings.TrimSuffix(name, "/"), "/") {
			if part == "." || part == ".." || part == "" {
				return nil, errors.New("artifact archive contains traversal")
			}
		}
		key := strings.TrimSuffix(name, "/")
		if seen[key] {
			return nil, errors.New("artifact archive contains duplicate entries")
		}
		seen[key] = true
		mode := entry.Mode()
		if mode&os.ModeSymlink != 0 || (!mode.IsDir() && !mode.IsRegular()) {
			return nil, errors.New("artifact archive contains a nonregular entry")
		}
		if mode.IsDir() {
			if entry.UncompressedSize64 != 0 {
				return nil, errors.New("artifact directory contains data")
			}
			continue
		}
		if entry.UncompressedSize64 > MaxFileBytes {
			return nil, errors.New("artifact file exceeds its size bound")
		}
		expanded += entry.UncompressedSize64
		if expanded > MaxArtifactBytes {
			return nil, errors.New("artifact expanded files exceed their aggregate bound")
		}
		stream, err := entry.Open()
		if err != nil {
			return nil, err
		}
		data, readErr := io.ReadAll(io.LimitReader(stream, MaxFileBytes+1))
		closeErr := stream.Close()
		if readErr != nil || closeErr != nil || len(data) > MaxFileBytes || uint64(len(data)) != entry.UncompressedSize64 {
			return nil, errors.New("artifact file is corrupt or oversized")
		}
		files[name] = data
	}
	for name := range files {
		for parent := path.Dir(name); parent != "."; parent = path.Dir(parent) {
			if _, exists := files[parent]; exists {
				return nil, errors.New("artifact file collides with a parent directory")
			}
		}
	}
	if checkpoint && (len(files) != 1 || files["settlement-chain.json"] == nil) {
		return nil, errors.New("checkpoint archive must contain only settlement-chain.json")
	}
	return files, nil
}

func VerifyUploadedFiles(payload []byte, root string, retained []string) error {
	files, err := archiveFiles(payload, false)
	if err != nil {
		return err
	}
	if len(files) != len(retained) {
		return errors.New("uploaded artifact differs from the exact retained file inventory")
	}
	seen := map[string]bool{}
	for _, relative := range retained {
		if seen[relative] {
			return errors.New("retained artifact inventory repeats a file")
		}
		seen[relative] = true
		local, err := ReadPackageFile(root, relative)
		if err != nil {
			return err
		}
		remote, exists := files[relative]
		if !exists || !bytes.Equal(remote, local) {
			return fmt.Errorf("uploaded artifact differs from retained %s", relative)
		}
	}
	return nil
}

func CheckpointFromArchive(payload []byte) (Object, error) {
	files, err := archiveFiles(payload, true)
	if err != nil {
		return nil, err
	}
	value, err := DecodeValue(files["settlement-chain.json"])
	if err != nil {
		return nil, err
	}
	object, ok := value.(Object)
	if !ok {
		return nil, errors.New("checkpoint chain must be an object")
	}
	return object, nil
}

func ExtractPackageArchive(payload []byte, destination string) error {
	files, err := archiveFiles(payload, false)
	if err != nil {
		return err
	}
	info, err := os.Lstat(destination)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("archive destination must be a real existing directory")
	}
	entries, err := os.ReadDir(destination)
	if err != nil || len(entries) != 0 {
		return errors.New("archive destination must be empty")
	}
	root, err := os.OpenRoot(destination)
	if err != nil {
		return err
	}
	defer root.Close()
	for name, data := range files {
		for parent := path.Dir(name); parent != "."; parent = path.Dir(parent) {
			if err := root.Mkdir(filepath.FromSlash(parent), 0700); err != nil && !errors.Is(err, os.ErrExist) {
				// Create nested parents in their natural order when the full path
				// cannot yet be made. The root handle confines every operation.
				parts := strings.Split(parent, "/")
				for i := range parts {
					if err := root.Mkdir(filepath.FromSlash(strings.Join(parts[:i+1], "/")), 0700); err != nil && !errors.Is(err, os.ErrExist) {
						return err
					}
				}
			}
		}
		file, err := root.OpenFile(filepath.FromSlash(name), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			return err
		}
		_, writeErr := file.Write(data)
		syncErr := file.Sync()
		closeErr := file.Close()
		if writeErr != nil || syncErr != nil || closeErr != nil {
			return errors.New("artifact evidence could not be persisted")
		}
	}
	return nil
}
