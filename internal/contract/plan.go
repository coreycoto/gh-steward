package contract

import (
	"bytes"
	"errors"
	"fmt"
	"regexp"
	"time"
)

const MachineSchemaVersion = 2

type Operation struct {
	ID     string `json:"id"`
	Kind   string `json:"kind"`
	Target Object `json:"target"`
	Before Object `json:"before"`
	After  Object `json:"after"`
}

type Plan struct {
	SchemaVersion int         `json:"schema_version"`
	Command       string      `json:"command"`
	Repository    Repository  `json:"repository"`
	CapturedAt    string      `json:"captured_at"`
	Sources       Object      `json:"sources"`
	Data          Object      `json:"data"`
	Operations    []Operation `json:"operations"`
	SHA256        string      `json:"sha256"`
}

var planCommand = regexp.MustCompile(`^[a-z][a-z0-9-]{0,79}$`)

func (p Plan) Unsigned() Object {
	operations := make([]any, 0, len(p.Operations))
	for _, op := range p.Operations {
		operations = append(operations, Object{"id": op.ID, "kind": op.Kind, "target": op.Target, "before": op.Before, "after": op.After})
	}
	return Object{"schema_version": p.SchemaVersion, "command": p.Command, "repository": Object{"host": p.Repository.Host, "owner": p.Repository.Owner, "name": p.Repository.Name, "url": p.Repository.URL}, "captured_at": p.CapturedAt, "sources": p.Sources, "data": p.Data, "operations": operations}
}

func (p Plan) Object() Object { o := p.Unsigned(); o["sha256"] = p.SHA256; return o }

func PreparePlan(command string, repository Repository, sources, data Object, operations []Operation, captured time.Time) (Plan, error) {
	p := Plan{SchemaVersion: MachineSchemaVersion, Command: command, Repository: repository, CapturedAt: captured.UTC().Format(time.RFC3339Nano), Sources: sources, Data: data, Operations: operations}
	if err := p.validateEvidence(); err != nil {
		return Plan{}, err
	}
	digest, err := Digest(p.Unsigned())
	if err != nil {
		return Plan{}, err
	}
	p.SHA256 = digest
	// Detach the reviewed evidence from caller-owned mutable maps.
	return ParsePlan(p.Object())
}

func (p Plan) validateEvidence() error {
	if p.SchemaVersion != MachineSchemaVersion {
		return errors.New("unsupported machine plan version; prepare a new Go plan")
	}
	if !planCommand.MatchString(p.Command) {
		return errors.New("plan command is not canonical")
	}
	normalized, err := ParseRepository(p.Repository.Object())
	if err != nil {
		return err
	}
	if normalized != p.Repository {
		return errors.New("plan repository identity must be normalized")
	}
	stamp, err := time.Parse(time.RFC3339Nano, p.CapturedAt)
	if err != nil || stamp.IsZero() {
		return errors.New("plan capture time must include a valid explicit timezone")
	}
	if len(p.Sources) == 0 || p.Data == nil {
		return errors.New("plan requires source provenance and domain data")
	}
	for _, raw := range p.Sources {
		source, ok := raw.(map[string]any)
		if !ok || source["live"] != true || source["complete"] != true {
			return errors.New("apply plans require live complete source provenance")
		}
	}
	seen := map[string]bool{}
	for _, op := range p.Operations {
		if op.ID == "" || seen[op.ID] || !planCommand.MatchString(op.Kind) || len(op.Target) == 0 || len(op.Before) == 0 || len(op.After) == 0 {
			return errors.New("plan operations require unique identity, supported kind, target and scoped before/after values")
		}
		seen[op.ID] = true
		for key := range op.After {
			if _, exists := op.Before[key]; !exists {
				return fmt.Errorf("reviewed before-state does not cover %s", key)
			}
		}
	}
	_, err = Canonical(p.Unsigned())
	return err
}

func ParsePlan(raw Object) (Plan, error) {
	version, err := Integer(raw["schema_version"])
	if err != nil || version != MachineSchemaVersion {
		return Plan{}, errors.New("unsupported machine plan version; Python-v1 artifacts are not accepted")
	}
	allowed := map[string]bool{"schema_version": true, "command": true, "repository": true, "captured_at": true, "sources": true, "data": true, "operations": true, "sha256": true}
	for key := range raw {
		if !allowed[key] {
			return Plan{}, fmt.Errorf("unsupported plan field %s", key)
		}
	}
	command, err := String(raw, "command")
	if err != nil {
		return Plan{}, err
	}
	repo, err := ObjectAt(raw, "repository")
	if err != nil {
		return Plan{}, err
	}
	repository, err := ParseRepository(repo)
	if err != nil {
		return Plan{}, err
	}
	captured, err := String(raw, "captured_at")
	if err != nil {
		return Plan{}, err
	}
	sources, err := ObjectAt(raw, "sources")
	if err != nil {
		return Plan{}, err
	}
	data, err := ObjectAt(raw, "data")
	if err != nil {
		return Plan{}, err
	}
	ops, err := Objects(raw, "operations")
	if err != nil {
		return Plan{}, err
	}
	sha, err := String(raw, "sha256")
	if err != nil {
		return Plan{}, err
	}
	p := Plan{SchemaVersion: int(version), Command: command, Repository: repository, CapturedAt: captured, Sources: sources, Data: data, SHA256: sha, Operations: make([]Operation, 0, len(ops))}
	for _, o := range ops {
		if len(o) != 5 {
			return Plan{}, errors.New("reviewed operation has unsupported or missing fields")
		}
		id, err := Nonempty(o, "id")
		if err != nil {
			return Plan{}, err
		}
		kind, err := Nonempty(o, "kind")
		if err != nil {
			return Plan{}, err
		}
		target, err := ObjectAt(o, "target")
		if err != nil {
			return Plan{}, err
		}
		before, err := ObjectAt(o, "before")
		if err != nil {
			return Plan{}, err
		}
		after, err := ObjectAt(o, "after")
		if err != nil {
			return Plan{}, err
		}
		copyTarget, err := Clone(target)
		if err != nil {
			return Plan{}, err
		}
		copyBefore, err := Clone(before)
		if err != nil {
			return Plan{}, err
		}
		copyAfter, err := Clone(after)
		if err != nil {
			return Plan{}, err
		}
		p.Operations = append(p.Operations, Operation{ID: id, Kind: kind, Target: copyTarget, Before: copyBefore, After: copyAfter})
	}
	if err := p.validateEvidence(); err != nil {
		return Plan{}, err
	}
	digest, err := Digest(p.Unsigned())
	if err != nil || digest != p.SHA256 {
		return Plan{}, errors.New("reviewed source, intent or before-state changed")
	}
	// Include all raw keys in the consistency comparison, including canonical
	// repository shape. Ignored extra identity properties are not accepted.
	a, err := Canonical(raw)
	if err != nil {
		return Plan{}, err
	}
	b, err := Canonical(p.Object())
	if err != nil || !bytes.Equal(a, b) {
		return Plan{}, errors.New("plan is not in its canonical machine contract")
	}
	p.Sources, _ = Clone(p.Sources)
	p.Data, _ = Clone(p.Data)
	return p, nil
}

func (p Plan) VerifyTarget(command string, repository Repository, expected []Operation) error {
	if p.Command != command || p.Repository != repository {
		return errors.New("reviewed plan targets another repository, host or command")
	}
	a, err := Canonical(p.Operations)
	if err != nil {
		return err
	}
	b, err := Canonical(expected)
	if err != nil {
		return err
	}
	if !bytes.Equal(a, b) {
		return errors.New("reviewed primitives differ from domain intent")
	}
	return nil
}
