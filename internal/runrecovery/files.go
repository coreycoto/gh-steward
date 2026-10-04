// Package runrecovery validates durable GitHub Actions execution evidence.
// Consumer policy stays explicit; a completed workflow alone never settles a write.
package runrecovery

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/coreycoto/gh-steward/internal/contract"
)

type Object = contract.Object

const MaxFileBytes = 8 * 1024 * 1024
const MaxCheckpointBytes = MaxFileBytes
const MaxArtifactBytes = 32 * 1024 * 1024
const MaxArtifactEntries = 1000

func Canonical(value any) ([]byte, error) { return contract.Canonical(value) }

func SHA256(data []byte) string {
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}

func IsSHA256(value any) bool {
	text, ok := value.(string)
	if !ok || len(text) != 64 {
		return false
	}
	for _, c := range text {
		if !strings.ContainsRune("0123456789abcdef", c) {
			return false
		}
	}
	return true
}

func Exact(value any, fields []string, name string) (Object, error) {
	object, ok := value.(map[string]any)
	if !ok || len(object) != len(fields) {
		return nil, fmt.Errorf("%s has an unsupported or incomplete shape", name)
	}
	for _, field := range fields {
		if _, exists := object[field]; !exists {
			return nil, fmt.Errorf("%s lacks %s", name, field)
		}
	}
	return object, nil
}

func Equal(left, right any) bool {
	a, err := Canonical(left)
	if err != nil {
		return false
	}
	b, err := Canonical(right)
	return err == nil && bytes.Equal(a, b)
}

// DecodeValue retains number tokens and rejects duplicates at every depth.
func DecodeValue(data []byte) (any, error) {
	if len(data) > MaxFileBytes || !utf8.Valid(data) {
		return nil, errors.New("retained JSON is oversized or not valid UTF-8")
	}
	wrapped := make([]byte, 0, len(data)+12)
	wrapped = append(wrapped, []byte(`{"value":`)...)
	wrapped = append(wrapped, data...)
	wrapped = append(wrapped, '}')
	object, err := contract.Decode(bytes.NewReader(wrapped))
	if err != nil {
		return nil, err
	}
	return object["value"], nil
}

func LoadFileProof(value any, name string) (any, error) {
	raw, err := LoadRawFileProof(value, name)
	if err != nil {
		return nil, err
	}
	return DecodeValue(raw)
}

// LoadRawFileProof applies the same bounded byte identity to JSON, patches and
// ZIP evidence. Binary proof bytes are never passed through a JSON decoder.
func LoadRawFileProof(value any, name string) ([]byte, error) {
	proof, err := Exact(value, []string{"sha256", "base64"}, name+" file proof")
	if err != nil {
		return nil, err
	}
	encoded, ok := proof["base64"].(string)
	if !ok || len(encoded) > base64.StdEncoding.EncodedLen(MaxFileBytes) || strings.ContainsAny(encoded, "\r\n") || !IsSHA256(proof["sha256"]) {
		return nil, fmt.Errorf("%s has invalid bounded file identity", name)
	}
	raw, err := base64.StdEncoding.Strict().DecodeString(encoded)
	if err != nil || len(raw) > MaxFileBytes || base64.StdEncoding.EncodeToString(raw) != encoded || SHA256(raw) != proof["sha256"] {
		return nil, fmt.Errorf("%s bytes differ from their exact file proof", name)
	}
	return raw, nil
}

func MakeFileProof(data []byte) Object {
	return Object{"sha256": SHA256(data), "base64": base64.StdEncoding.EncodeToString(data)}
}

func readRegularFile(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() > MaxFileBytes {
		return nil, errors.New("retained file is missing, unsafe or oversized")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !opened.Mode().IsRegular() || !os.SameFile(info, opened) {
		return nil, errors.New("retained file changed while opening")
	}
	data, err := io.ReadAll(io.LimitReader(file, MaxFileBytes+1))
	if err != nil || len(data) > MaxFileBytes {
		return nil, errors.New("retained file could not be read within its bound")
	}
	return data, nil
}

func LoadJSON(path string) (any, error) {
	data, err := readRegularFile(path)
	if err != nil {
		return nil, err
	}
	return DecodeValue(data)
}

func PackageFile(root, relative string) (string, error) {
	if relative == "" || filepath.IsAbs(relative) || strings.ContainsAny(relative, "\\\r\n\x00") || filepath.ToSlash(filepath.Clean(relative)) != relative {
		return "", errors.New("package path must be an exact safe relative path")
	}
	for _, part := range strings.Split(relative, "/") {
		if part == "" || part == "." || part == ".." {
			return "", errors.New("package path contains traversal")
		}
	}
	root, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	info, err := os.Lstat(root)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", errors.New("package root must be an existing real directory")
	}
	current := root
	parts := strings.Split(relative, "/")
	for i, part := range parts {
		current = filepath.Join(current, part)
		entry, err := os.Lstat(current)
		if err != nil || entry.Mode()&os.ModeSymlink != 0 {
			return "", errors.New("package path is missing or symbolic")
		}
		if i+1 < len(parts) && !entry.IsDir() {
			return "", errors.New("package parent is not a directory")
		}
		if i+1 == len(parts) && !entry.Mode().IsRegular() {
			return "", errors.New("package evidence must be a regular file")
		}
	}
	return current, nil
}

func ReadPackageFile(root, relative string) ([]byte, error) {
	path, err := PackageFile(root, relative)
	if err != nil {
		return nil, err
	}
	return readRegularFile(path)
}
