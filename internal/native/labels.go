package native

import (
	"context"
	"errors"
	"regexp"
	"strings"

	"github.com/coreycoto/gh-steward/internal/contract"
)

var labelHexColor = regexp.MustCompile(`^[0-9a-fA-F]{6}$`)

const labelMutationFields = `id name color description repository{id nameWithOwner url}`

func validateLabelDraft(draft contract.Object) error {
	if len(draft) != 3 {
		return errors.New("label primitive requires exactly name, color and description")
	}
	name, err := contract.Nonempty(draft, "name")
	if err != nil || strings.TrimSpace(name) != name || strings.ContainsAny(name, "\r\n\x00") {
		return errors.New("label name must be an explicit nonblank single-line name")
	}
	color, err := contract.Nonempty(draft, "color")
	if err != nil || !labelHexColor.MatchString(color) {
		return errors.New("label color must be six hexadecimal characters")
	}
	if _, err := contract.String(draft, "description"); err != nil {
		return err
	}
	return nil
}

func (t *Transport) labelIdentity(label contract.Object, repositoryNodeID, labelNodeID, name string) error {
	id, err := contract.Nonempty(label, "id")
	if err != nil || (labelNodeID != "" && id != labelNodeID) || label["name"] != name {
		return errors.New("label result has a different immutable identity or name")
	}
	repository, err := contract.ObjectAt(label, "repository")
	if err != nil || repository["id"] != repositoryNodeID {
		return errors.New("label result belongs to another immutable repository")
	}
	parsed, err := contract.ParseRepository(repository)
	if err != nil || parsed != t.Repository {
		return errors.New("label result has foreign repository scope")
	}
	return nil
}

// ReadLabel proves the label belongs to the selected repository before a
// node-ID mutation. It returns the repository and label exactly as observed.
func (t *Transport) ReadLabel(ctx context.Context, name string) (contract.Object, error) {
	if err := validateLabelDraft(contract.Object{"name": name, "color": "000000", "description": ""}); err != nil {
		return nil, err
	}
	variables := t.repoVariables()
	variables["label"] = name
	data, err := t.graphQL(ctx, `query($owner:String!,$name:String!,$label:String!){repository(owner:$owner,name:$name){`+repositoryFields+` label(name:$label){`+labelMutationFields+`}}}`, variables)
	if err != nil {
		return nil, err
	}
	repository, err := t.repositoryResult(data)
	if err != nil {
		return nil, err
	}
	nodeID, err := contract.Nonempty(repository, "id")
	if err != nil {
		return nil, err
	}
	label, err := contract.ObjectAt(repository, "label")
	if err != nil {
		return nil, errors.New("selected repository label was not returned")
	}
	if err := t.labelIdentity(label, nodeID, "", name); err != nil {
		return nil, err
	}
	return contract.Object{"repository": repository, "label": label}, nil
}

func (t *Transport) labelAcknowledgement(data contract.Object, key, nonce, repositoryNodeID, labelNodeID string, draft contract.Object) (contract.Object, error) {
	payload, err := contract.ObjectAt(data, key)
	if err != nil || payload["clientMutationId"] != nonce {
		return nil, errors.New("label acknowledgement lacks the durable native mutation identity")
	}
	label, err := contract.ObjectAt(payload, "label")
	if err != nil {
		return nil, err
	}
	if err := t.labelIdentity(label, repositoryNodeID, labelNodeID, draft["name"].(string)); err != nil {
		return nil, err
	}
	color, err := contract.String(label, "color")
	if err != nil || strings.ToLower(color) != strings.ToLower(draft["color"].(string)) {
		return nil, errors.New("label acknowledgement differs from reviewed color")
	}
	description, present := label["description"]
	if !present {
		return nil, errors.New("label nullable description is missing")
	}
	if description == nil {
		description = ""
	}
	if description != draft["description"] {
		return nil, errors.New("label acknowledgement differs from reviewed description")
	}
	return payload, nil
}

func (t *Transport) CreateLabel(ctx context.Context, draft contract.Object, operationID string) (contract.Object, error) {
	if err := validateLabelDraft(draft); err != nil {
		return nil, err
	}
	if _, err := OperationMarker(operationID); err != nil {
		return nil, err
	}
	repository, err := t.ReadRepository(ctx)
	if err != nil {
		return nil, err
	}
	nodeID, err := contract.Nonempty(repository, "id")
	if err != nil {
		return nil, err
	}
	input := contract.Object{"clientMutationId": operationID, "repositoryId": nodeID,
		"name": draft["name"], "color": draft["color"], "description": draft["description"]}
	data, err := t.graphQL(ctx, `mutation($input:CreateLabelInput!){createLabel(input:$input){clientMutationId label{`+labelMutationFields+`}}}`, contract.Object{"input": input})
	if err != nil {
		return nil, err
	}
	return t.labelAcknowledgement(data, "createLabel", operationID, nodeID, "", draft)
}

func (t *Transport) UpdateLabel(ctx context.Context, labelNodeID string, draft contract.Object, operationID string) (contract.Object, error) {
	if labelNodeID == "" {
		return nil, errors.New("label update requires an immutable node identity")
	}
	if err := validateLabelDraft(draft); err != nil {
		return nil, err
	}
	if _, err := OperationMarker(operationID); err != nil {
		return nil, err
	}
	prior, err := t.ReadLabel(ctx, draft["name"].(string))
	if err != nil {
		return nil, err
	}
	repository, _ := contract.ObjectAt(prior, "repository")
	label, _ := contract.ObjectAt(prior, "label")
	nodeID, _ := contract.Nonempty(repository, "id")
	if err := t.labelIdentity(label, nodeID, labelNodeID, draft["name"].(string)); err != nil {
		return nil, err
	}
	input := contract.Object{"clientMutationId": operationID, "id": labelNodeID,
		"name": draft["name"], "color": draft["color"], "description": draft["description"]}
	data, err := t.graphQL(ctx, `mutation($input:UpdateLabelInput!){updateLabel(input:$input){clientMutationId label{`+labelMutationFields+`}}}`, contract.Object{"input": input})
	if err != nil {
		return nil, err
	}
	return t.labelAcknowledgement(data, "updateLabel", operationID, nodeID, labelNodeID, draft)
}
