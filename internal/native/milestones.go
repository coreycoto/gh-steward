package native

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/coreycoto/gh-steward/internal/contract"
)

func (t *Transport) ValidateMilestoneURL(raw any, number int64) error {
	s, ok := raw.(string)
	if !ok || number < 1 {
		return errors.New("milestone requires a qualified URL and positive number")
	}
	u, err := url.Parse(s)
	if err != nil || u.Scheme != "https" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Port() != "" && u.Port() != "443") || !strings.EqualFold(u.Hostname(), t.Repository.Host) || !strings.EqualFold(u.EscapedPath(), fmt.Sprintf("/%s/milestone/%d", t.Repository.FullName(), number)) {
		return errors.New("milestone URL targets another repository or number")
	}
	return nil
}

func (t *Transport) milestoneIdentity(raw contract.Object, expected int64) error {
	n, err := contract.PositiveInteger(raw["number"])
	if err != nil || (expected != 0 && n != expected) {
		return errors.New("milestone returned a different number")
	}
	if _, err := contract.PositiveInteger(raw["id"]); err != nil {
		return err
	}
	if _, err := contract.Nonempty(raw, "node_id"); err != nil {
		return err
	}
	return t.ValidateMilestoneURL(raw["html_url"], n)
}

func validateMilestoneChanges(changes contract.Object, create bool) error {
	if len(changes) == 0 {
		return errors.New("milestone primitive requires scoped changes")
	}
	for key, value := range changes {
		switch key {
		case "title":
			text, err := contract.Nonempty(changes, key)
			if err != nil || strings.TrimSpace(text) == "" {
				return errors.New("milestone title cannot be blank")
			}
		case "description":
			if _, err := contract.String(changes, key); err != nil {
				return err
			}
		case "state":
			if value != "open" && value != "closed" {
				return errors.New("milestone state must be open or closed")
			}
		case "due_on":
			if value != nil {
				date, ok := value.(string)
				if !ok {
					return errors.New("milestone due date must be a timestamp or null")
				}
				if _, err := time.Parse(time.RFC3339, date); err != nil {
					return errors.New("milestone due date must be an RFC3339 timestamp")
				}
			}
		default:
			return fmt.Errorf("unsupported milestone change %s", key)
		}
	}
	if create {
		if _, err := contract.Nonempty(changes, "title"); err != nil {
			return err
		}
		if _, err := contract.String(changes, "description"); err != nil {
			return err
		}
	}
	return nil
}

func (t *Transport) CreateMilestone(ctx context.Context, draft contract.Object, operationID string) (contract.Object, error) {
	if err := validateMilestoneChanges(draft, true); err != nil {
		return nil, err
	}
	copy, err := contract.Clone(draft)
	if err != nil {
		return nil, err
	}
	description, _ := contract.String(copy, "description")
	copy["description"], err = markedBody(description, operationID)
	if err != nil {
		return nil, err
	}
	result, err := t.rest(ctx, "POST", "repos/"+t.Repository.FullName()+"/milestones", copy)
	if err != nil {
		return nil, err
	}
	if err := t.milestoneIdentity(result, 0); err != nil {
		return nil, err
	}
	if result["title"] != copy["title"] || result["description"] != copy["description"] {
		return nil, errors.New("new milestone lost the reviewed correlated content")
	}
	return result, nil
}

func (t *Transport) UpdateMilestone(ctx context.Context, number int64, changes contract.Object) (contract.Object, error) {
	if number < 1 {
		return nil, errors.New("milestone number must be positive")
	}
	if err := validateMilestoneChanges(changes, false); err != nil {
		return nil, err
	}
	endpoint := fmt.Sprintf("repos/%s/milestones/%d", t.Repository.FullName(), number)
	prior, err := t.REST(ctx, "GET", endpoint, nil)
	if err != nil {
		return nil, err
	}
	if err := t.milestoneIdentity(prior, number); err != nil {
		return nil, err
	}
	result, err := t.rest(ctx, "PATCH", endpoint, changes)
	if err != nil {
		return nil, err
	}
	if err := t.milestoneIdentity(result, number); err != nil {
		return nil, err
	}
	if result["id"] != prior["id"] || result["node_id"] != prior["node_id"] {
		return nil, errors.New("milestone immutable identity changed during update")
	}
	return result, nil
}
