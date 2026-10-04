// Package progress records durable per-primitive intent, completion and unknown
// outcomes. Journal consistency is not a permission or an approval grant.
package progress

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/coreycoto/gh-steward/internal/contract"
)

const SchemaVersion = 2

type ObservationRequired struct{ Step string }

func (e *ObservationRequired) Error() string {
	return "primitive " + e.Step + " has an unknown outcome; positively reconcile its exact target before continuing"
}

type Journal struct {
	path  string
	lock  *os.File
	state contract.Object
}

var commandName = regexp.MustCompile(`^[a-z][a-z0-9-]{0,79}$`)
var shaPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)

func Open(root string, repository contract.Repository, command, planDigest string) (*Journal, error) {
	if !commandName.MatchString(command) || !shaPattern.MatchString(planDigest) {
		return nil, errors.New("journal command and plan digest must be canonical")
	}
	normalized, err := contract.ParseRepository(repository.Object())
	if err != nil {
		return nil, err
	}
	repository = normalized
	absolute, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	absolute, err = filepath.EvalSymlinks(absolute)
	if err != nil {
		return nil, err
	}
	identity := contract.Object{"schema_version": SchemaVersion, "repository": repository.Object(), "command": command, "plan_sha256": planDigest}
	key, err := contract.Digest(identity)
	if err != nil {
		return nil, err
	}
	directory := filepath.Join(absolute, ".artifacts", "gh-steward", "journals")
	if err := privateDirectories(absolute, directory); err != nil {
		return nil, err
	}
	path := filepath.Join(directory, key+".json")
	fd, err := syscall.Open(path+".lock", syscall.O_CREAT|syscall.O_RDWR|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0600)
	if err != nil {
		return nil, err
	}
	lock := os.NewFile(uintptr(fd), path+".lock")
	if info, err := lock.Stat(); err != nil || !privateRegular(info) {
		lock.Close()
		return nil, errors.New("journal lock must be a regular file")
	}
	if err := syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		lock.Close()
		return nil, errors.New("another process owns this reviewed plan journal")
	}
	j := &Journal{path: path, lock: lock}
	state, err := readObject(path)
	if errors.Is(err, os.ErrNotExist) {
		state = contract.Object{"identity": identity, "steps": []any{}, "result": nil}
		j.state = state
		if err = j.save(); err != nil {
			j.Close()
			return nil, err
		}
		return j, nil
	}
	if err != nil {
		j.Close()
		return nil, err
	}
	current, err := contract.ObjectAt(state, "identity")
	if err != nil {
		j.Close()
		return nil, err
	}
	want, _ := contract.Digest(identity)
	have, err := contract.Digest(current)
	if err != nil || want != have {
		j.Close()
		return nil, errors.New("journal identity does not match the current repository and reviewed intent")
	}
	j.state = state
	if err = j.validate(); err != nil {
		j.Close()
		return nil, err
	}
	changed := false
	steps, _ := contract.Objects(j.state, "steps")
	for _, step := range steps {
		if step["status"] == "dispatching" {
			step["status"] = "unknown"
			changed = true
		}
	}
	if changed {
		if err = j.save(); err != nil {
			j.Close()
			return nil, err
		}
	}
	return j, nil
}

func privateDirectories(root, target string) error {
	rel, err := filepath.Rel(root, target)
	if err != nil || strings.HasPrefix(rel, "..") {
		return errors.New("journal directory escapes its checkout")
	}
	current := root
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		current = filepath.Join(current, part)
		if err := os.Mkdir(current, 0700); err != nil && !errors.Is(err, os.ErrExist) {
			return err
		}
		info, err := os.Lstat(current)
		if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() || !owned(info) || info.Mode().Perm()&0022 != 0 {
			return errors.New("journal directory must not follow symlinks")
		}
		if current != filepath.Join(root, ".artifacts") && info.Mode().Perm()&0077 != 0 {
			return errors.New("journal-owned directories must be private to the current user")
		}
	}
	return nil
}

func readObject(path string) (contract.Object, error) {
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), path)
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !privateRegular(info) || info.Size() > contract.MaxJSONBytes {
		return nil, errors.New("journal must be a bounded regular file")
	}
	return contract.Decode(f)
}

func owned(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Uid == uint32(os.Geteuid())
}
func privateRegular(info os.FileInfo) bool {
	return info.Mode().IsRegular() && owned(info) && info.Mode().Perm()&0077 == 0
}

func (j *Journal) validate() error {
	steps, err := contract.Objects(j.state, "steps")
	if err != nil {
		return err
	}
	seen := map[string]bool{}
	for _, step := range steps {
		id, err := contract.Nonempty(step, "id")
		if err != nil || seen[id] {
			return errors.New("journal step identities must be nonempty and unique")
		}
		seen[id] = true
		intent, err := contract.ObjectAt(step, "intent")
		if err != nil {
			return err
		}
		digest, err := contract.Digest(intent)
		if err != nil || step["intent_sha256"] != digest {
			return errors.New("journal primitive intent changed")
		}
		operation, err := contract.Nonempty(step, "operation_id")
		if err != nil || len(operation) != 32 {
			return errors.New("journal operation identity is invalid")
		}
		if _, err := hex.DecodeString(operation); err != nil {
			return errors.New("journal operation identity is invalid")
		}
		if raw, exists := step["acknowledgement"]; exists {
			ack, ok := raw.(map[string]any)
			if !ok || ack["operation_id"] != operation {
				return errors.New("native acknowledgement has another dispatch identity")
			}
		}
		status, err := contract.String(step, "status")
		if err != nil {
			return err
		}
		switch status {
		case "completed":
			if _, err := contract.ObjectAt(step, "result"); err != nil {
				return err
			}
		case "dispatching", "unknown":
			if step["result"] != nil {
				return errors.New("unfinished primitive has a completion result")
			}
		default:
			return errors.New("journal primitive status is invalid")
		}
	}
	if result := j.state["result"]; result != nil {
		if _, ok := result.(map[string]any); !ok {
			return errors.New("journal terminal result must be an object")
		}
		for _, step := range steps {
			if step["status"] != "completed" {
				return errors.New("terminal journal contains an unfinished primitive")
			}
		}
	}
	return nil
}

func (j *Journal) save() error {
	data, err := contract.Canonical(j.state)
	if err != nil {
		return err
	}
	if len(data)+1 > contract.MaxJSONBytes {
		return errors.New("journal exceeds the supported size before dispatch")
	}
	directory := filepath.Dir(j.path)
	f, err := os.CreateTemp(directory, ".journal-*")
	if err != nil {
		return err
	}
	temporary := f.Name()
	defer os.Remove(temporary)
	if err = f.Chmod(0600); err != nil {
		f.Close()
		return err
	}
	if _, err = f.Write(append(data, '\n')); err != nil {
		f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = os.Rename(temporary, j.path); err != nil {
		return err
	}
	d, err := os.Open(directory)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

func (j *Journal) Close() error {
	if j.lock == nil {
		return nil
	}
	fd := int(j.lock.Fd())
	_ = syscall.Flock(fd, syscall.LOCK_UN)
	err := j.lock.Close()
	j.lock = nil
	return err
}
func (j *Journal) Path() string { return j.path }

func (j *Journal) step(id string) contract.Object {
	steps, _ := contract.Objects(j.state, "steps")
	for _, step := range steps {
		if step["id"] == id {
			return step
		}
	}
	return nil
}

func (j *Journal) ValidateIntent(id string, intent contract.Object) error {
	if strings.TrimSpace(id) == "" || intent == nil {
		return errors.New("primitive identity and intent must be complete")
	}
	digest, err := contract.Digest(intent)
	if err != nil {
		return err
	}
	if step := j.step(id); step != nil && step["intent_sha256"] != digest {
		return errors.New("reviewed primitive intent does not match its durable receipt")
	}
	return nil
}

func (j *Journal) Status(id string) string {
	if step := j.step(id); step != nil {
		return step["status"].(string)
	}
	return "pending"
}
func (j *Journal) OperationID(id string) string {
	if step := j.step(id); step != nil {
		return step["operation_id"].(string)
	}
	return ""
}

// Acknowledge durably captures a successful native response while its exact
// primitive is still dispatching. A domain adapter validates the response
// identity before calling this method, and verifies fresh after-state before
// completion. An acknowledgement is immutable and cannot authorize a retry.
func (j *Journal) Acknowledge(id string, intent, acknowledgement contract.Object) error {
	if err := j.ValidateIntent(id, intent); err != nil {
		return err
	}
	step := j.step(id)
	if step == nil || step["status"] != "dispatching" {
		return errors.New("only the current dispatch may record a native acknowledgement")
	}
	if acknowledgement["operation_id"] != step["operation_id"] {
		return errors.New("native acknowledgement has another dispatch identity")
	}
	copy, err := contract.Clone(acknowledgement)
	if err != nil {
		return err
	}
	if prior, exists := step["acknowledgement"]; exists {
		x, err := contract.Digest(prior)
		if err != nil {
			return err
		}
		y, err := contract.Digest(copy)
		if err != nil || x != y {
			return errors.New("native acknowledgement changed after capture")
		}
		return nil
	}
	step["acknowledgement"] = copy
	return j.save()
}

func (j *Journal) Execute(id string, intent contract.Object, action func(string) (contract.Object, error)) (contract.Object, error) {
	if err := j.ValidateIntent(id, intent); err != nil {
		return nil, err
	}
	if step := j.step(id); step != nil {
		if step["status"] != "completed" {
			return nil, &ObservationRequired{Step: id}
		}
		return contract.Clone(step["result"].(map[string]any))
	}
	if j.state["result"] != nil {
		return nil, errors.New("cannot append a primitive to a completed plan")
	}
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return nil, err
	}
	copyIntent, err := contract.Clone(intent)
	if err != nil {
		return nil, err
	}
	digest, _ := contract.Digest(copyIntent)
	step := contract.Object{"id": id, "intent": copyIntent, "intent_sha256": digest, "operation_id": hex.EncodeToString(random[:]), "status": "dispatching", "result": nil, "started_at": time.Now().UTC().Format(time.RFC3339Nano)}
	j.state["steps"] = append(j.state["steps"].([]any), step)
	if err := j.save(); err != nil {
		return nil, err
	}
	result, err := action(step["operation_id"].(string))
	if err != nil {
		step["status"] = "unknown"
		if saveErr := j.save(); saveErr != nil {
			return nil, fmt.Errorf("primitive outcome is unknown and its receipt could not be persisted: %w", saveErr)
		}
		return nil, err
	}
	copyResult, err := contract.Clone(result)
	if err != nil {
		step["status"] = "unknown"
		_ = j.save()
		return nil, err
	}
	step["status"], step["result"] = "completed", copyResult
	step["completed_at"] = time.Now().UTC().Format(time.RFC3339Nano)
	if err := j.save(); err != nil {
		step["status"], step["result"] = "unknown", nil
		return nil, err
	}
	return contract.Clone(copyResult)
}

// Observe is called only after the domain adapter has positively verified the
// exact target, operation identity and intended after-state from a fresh read.
// Absence, a matching title, or a transport error is not completion evidence.
func (j *Journal) Observe(id string, intent, result, evidence contract.Object) error {
	if err := j.ValidateIntent(id, intent); err != nil {
		return err
	}
	step := j.step(id)
	if step == nil || step["status"] != "unknown" {
		return errors.New("only an existing unknown primitive can be reconciled")
	}
	if evidence["positive_identity"] != true || evidence["after_state_verified"] != true || evidence["operation_id"] != step["operation_id"] {
		return errors.New("unknown primitive requires exact positive observation evidence")
	}
	if _, err := contract.Nonempty(evidence, "reference"); err != nil {
		return err
	}
	copyResult, err := contract.Clone(result)
	if err != nil {
		return err
	}
	copyEvidence, err := contract.Clone(evidence)
	if err != nil {
		return err
	}
	step["status"], step["result"], step["observation"] = "completed", copyResult, copyEvidence
	step["completed_at"] = time.Now().UTC().Format(time.RFC3339Nano)
	return j.save()
}

func (j *Journal) Receipts() ([]contract.Object, error) {
	steps, _ := contract.Objects(j.state, "steps")
	result := make([]contract.Object, 0, len(steps))
	for _, step := range steps {
		copy, err := contract.Clone(step)
		if err != nil {
			return nil, err
		}
		result = append(result, copy)
	}
	return result, nil
}

func (j *Journal) Finish(result contract.Object, expectedIDs []string) error {
	steps, _ := contract.Objects(j.state, "steps")
	if len(steps) != len(expectedIDs) {
		return errors.New("terminal result requires the exact complete primitive receipt list")
	}
	for i, step := range steps {
		if step["id"] != expectedIDs[i] || step["status"] != "completed" {
			return errors.New("terminal result has mismatched or unfinished primitives")
		}
	}
	copy, err := contract.Clone(result)
	if err != nil {
		return err
	}
	if prior := j.state["result"]; prior != nil {
		a, _ := contract.Canonical(prior)
		b, _ := contract.Canonical(copy)
		if !bytes.Equal(a, b) {
			return errors.New("terminal result changed after completion")
		}
		return nil
	}
	j.state["result"] = copy
	return j.save()
}

func (j *Journal) Result() (contract.Object, error) {
	if j.state["result"] == nil {
		return nil, nil
	}
	return contract.Clone(j.state["result"].(map[string]any))
}
