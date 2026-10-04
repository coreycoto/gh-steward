package native

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/coreycoto/gh-steward/internal/contract"
)

var operationIDPattern = regexp.MustCompile(`^[a-f0-9]{32}$`)

func OperationMarker(id string) (string, error) {
	if !operationIDPattern.MatchString(id) {
		return "", errors.New("operation identity must be the durable dispatch nonce")
	}
	return "<!-- gh-steward:operation:" + id + " -->", nil
}
func markedBody(body, id string) (string, error) {
	marker, err := OperationMarker(id)
	if err != nil {
		return "", err
	}
	if strings.Contains(body, "<!-- gh-steward:operation:") {
		return "", errors.New("authored body cannot contain a reserved operation marker")
	}
	return body + "\n\n" + marker, nil
}

func (t *Transport) RepositoryIssue(ctx context.Context, number int64) (contract.Object, error) {
	if number < 1 {
		return nil, errors.New("issue number must be positive")
	}
	i, err := t.REST(ctx, "GET", fmt.Sprintf("repos/%s/issues/%d", t.Repository.FullName(), number), nil)
	if err != nil {
		return nil, err
	}
	n, err := contract.PositiveInteger(i["number"])
	if err != nil || n != number {
		return nil, errors.New("REST issue identity changed")
	}
	if _, isPR := i["pull_request"]; isPR {
		return nil, errors.New("issue primitive does not target a pull request")
	}
	if err = t.ValidateIssueURL(i["html_url"], number); err != nil {
		return nil, err
	}
	if _, err = contract.PositiveInteger(i["id"]); err != nil {
		return nil, err
	}
	if _, err = contract.Nonempty(i, "node_id"); err != nil {
		return nil, err
	}
	return i, nil
}

func validateIssueChanges(changes contract.Object, create bool) error {
	allowed := map[string]bool{"title": true, "body": true, "state": true, "state_reason": true, "labels": true, "milestone": true, "assignees": true}
	if len(changes) == 0 {
		return errors.New("issue primitive requires scoped changes")
	}
	for key, value := range changes {
		if !allowed[key] {
			return fmt.Errorf("unsupported issue change %s", key)
		}
		switch key {
		case "title", "body":
			if _, err := contract.String(changes, key); err != nil {
				return err
			}
		case "state":
			if value != "open" && value != "closed" {
				return errors.New("issue state must be open or closed")
			}
		case "state_reason":
			if value != nil && value != "completed" && value != "not_planned" && value != "reopened" {
				return errors.New("unsupported issue state reason")
			}
		case "labels", "assignees":
			if _, err := contract.Strings(value); err != nil {
				return err
			}
		case "milestone":
			if value != nil {
				if _, err := contract.PositiveInteger(value); err != nil {
					return err
				}
			}
		}
	}
	if create {
		if _, exists := changes["state"]; exists {
			return errors.New("issue creation does not set state; prepare a separate transition")
		}
		if _, exists := changes["state_reason"]; exists {
			return errors.New("issue creation does not set a state reason")
		}
		title, err := contract.Nonempty(changes, "title")
		if err != nil || strings.TrimSpace(title) == "" {
			return errors.New("new issue needs a title")
		}
		if _, err := contract.String(changes, "body"); err != nil {
			return err
		}
	}
	return nil
}

func (t *Transport) CreateIssue(ctx context.Context, draft contract.Object, operationID string) (contract.Object, error) {
	if err := validateIssueChanges(draft, true); err != nil {
		return nil, err
	}
	draft, err := contract.Clone(draft)
	if err != nil {
		return nil, err
	}
	body, _ := contract.String(draft, "body")
	draft["body"], err = markedBody(body, operationID)
	if err != nil {
		return nil, err
	}
	i, err := t.rest(ctx, "POST", "repos/"+t.Repository.FullName()+"/issues", draft)
	if err != nil {
		return nil, err
	}
	n, err := contract.PositiveInteger(i["number"])
	if err != nil {
		return nil, err
	}
	if err = t.ValidateIssueURL(i["html_url"], n); err != nil {
		return nil, err
	}
	if i["body"] != draft["body"] || i["title"] != draft["title"] {
		return nil, errors.New("new issue did not retain the correlated reviewed content")
	}
	if _, err := contract.PositiveInteger(i["id"]); err != nil {
		return nil, err
	}
	if _, err := contract.Nonempty(i, "node_id"); err != nil {
		return nil, err
	}
	return i, nil
}

func (t *Transport) UpdateIssue(ctx context.Context, number int64, changes contract.Object) (contract.Object, error) {
	if number < 1 {
		return nil, errors.New("issue number must be positive")
	}
	if err := validateIssueChanges(changes, false); err != nil {
		return nil, err
	}
	if _, err := t.RepositoryIssue(ctx, number); err != nil {
		return nil, err
	}
	i, err := t.rest(ctx, "PATCH", fmt.Sprintf("repos/%s/issues/%d", t.Repository.FullName(), number), changes)
	if err != nil {
		return nil, err
	}
	n, err := contract.PositiveInteger(i["number"])
	if err != nil || n != number {
		return nil, errors.New("updated issue has a foreign number")
	}
	if err = t.ValidateIssueURL(i["html_url"], n); err != nil {
		return nil, err
	}
	return i, nil
}

func (t *Transport) CreateIssueComment(ctx context.Context, number int64, body, operationID string) (contract.Object, error) {
	if number < 1 {
		return nil, errors.New("comment issue number must be positive")
	}
	body, err := markedBody(body, operationID)
	if err != nil {
		return nil, err
	}
	r, err := t.rest(ctx, "POST", fmt.Sprintf("repos/%s/issues/%d/comments", t.Repository.FullName(), number), contract.Object{"body": body})
	if err != nil {
		return nil, err
	}
	if _, err := contract.PositiveInteger(r["id"]); err != nil {
		return nil, err
	}
	if r["body"] != body {
		return nil, errors.New("comment did not retain correlated reviewed content")
	}
	if err := t.validateCommentURL(r, number); err != nil {
		return nil, err
	}
	return r, nil
}

func (t *Transport) UpdateIssueComment(ctx context.Context, number, commentID int64, body string) (contract.Object, error) {
	if number < 1 || commentID < 1 {
		return nil, errors.New("comment identity must be positive")
	}
	endpoint := fmt.Sprintf("repos/%s/issues/comments/%d", t.Repository.FullName(), commentID)
	prior, err := t.REST(ctx, "GET", endpoint, nil)
	if err != nil {
		return nil, err
	}
	id, err := contract.PositiveInteger(prior["id"])
	if err != nil || id != commentID {
		return nil, errors.New("comment identity changed")
	}
	if err := t.validateCommentURL(prior, number); err != nil {
		return nil, err
	}
	result, err := t.rest(ctx, "PATCH", endpoint, contract.Object{"body": body})
	if err != nil {
		return nil, err
	}
	id, err = contract.PositiveInteger(result["id"])
	if err != nil || id != commentID || result["body"] != body {
		return nil, errors.New("updated comment does not match reviewed identity/content")
	}
	if err := t.validateCommentURL(result, number); err != nil {
		return nil, err
	}
	return result, nil
}

func (t *Transport) validateCommentURL(comment contract.Object, number int64) error {
	id, err := contract.PositiveInteger(comment["id"])
	if err != nil {
		return err
	}
	raw, err := contract.Nonempty(comment, "html_url")
	if err != nil {
		return err
	}
	u, err := url.Parse(raw)
	if err != nil {
		return err
	}
	if u.Fragment != "issuecomment-"+strconv.FormatInt(id, 10) {
		return errors.New("comment URL has another comment identity")
	}
	u.Fragment = ""
	return t.ValidateIssueURL(u.String(), number)
}

func (t *Transport) relationship(ctx context.Context, number, other int64, kind string, add bool) (contract.Object, error) {
	if number < 1 || other < 1 || number == other {
		return nil, errors.New("relationship requires distinct positive issue numbers")
	}
	if _, err := t.RepositoryIssue(ctx, number); err != nil {
		return nil, err
	}
	issue, err := t.RepositoryIssue(ctx, other)
	if err != nil {
		return nil, err
	}
	databaseID, _ := contract.PositiveInteger(issue["id"])
	method, endpoint, input := "POST", fmt.Sprintf("repos/%s/issues/%d/", t.Repository.FullName(), number), contract.Object{}
	switch kind {
	case "blocked-by":
		endpoint += "dependencies/blocked_by"
		input["issue_id"] = databaseID
		if !add {
			method = "DELETE"
			endpoint += "/" + strconv.FormatInt(databaseID, 10)
			input = nil
		}
	case "child":
		endpoint += "sub_issues"
		input["sub_issue_id"] = databaseID
		if add {
			input["replace_parent"] = false
		} else {
			method = "DELETE"
			endpoint = strings.TrimSuffix(endpoint, "sub_issues") + "sub_issue"
		}
	default:
		return nil, errors.New("unsupported relationship kind")
	}
	return t.restAcknowledgement(ctx, method, endpoint, input)
}
func (t *Transport) AddRelationship(ctx context.Context, number, other int64, kind string) (contract.Object, error) {
	return t.relationship(ctx, number, other, kind, true)
}
func (t *Transport) RemoveRelationship(ctx context.Context, number, other int64, kind string) (contract.Object, error) {
	return t.relationship(ctx, number, other, kind, false)
}
func (t *Transport) restAcknowledgement(ctx context.Context, method, endpoint string, input contract.Object) (contract.Object, error) {
	if err := t.ValidateRESTEndpoint(endpoint); err != nil {
		return nil, err
	}
	args := []string{"api", endpoint, "--method", method}
	var body []byte
	if input != nil {
		var err error
		body, err = contract.Canonical(input)
		if err != nil {
			return nil, err
		}
		args = append(args, "--input", "-")
	}
	r, err := t.run(ctx, args, body, false)
	if err != nil {
		return nil, err
	}
	if r.ExitCode != 0 {
		return nil, fmt.Errorf("native mutation failed with exit code %d", r.ExitCode)
	}
	if len(strings.TrimSpace(string(r.Stdout))) == 0 {
		return contract.Object{"acknowledged": true}, nil
	}
	return contract.Decode(strings.NewReader(string(r.Stdout)))
}

type ProjectItemScope struct {
	Project     ProjectScope
	ItemID      string
	IssueNumber int64
}

func (t *Transport) ProjectItem(ctx context.Context, p ProjectScope, itemID string, issueNumber int64) (contract.Object, error) {
	if err := t.validateProjectScope(p); err != nil {
		return nil, err
	}
	if p.ID == "" || itemID == "" || issueNumber < 1 {
		return nil, errors.New("Project item requires qualified project, item and issue identities")
	}
	var cursor *string
	seen := map[string]bool{}
	for {
		project, err := t.ReadProjectPage(ctx, p, "items", cursor)
		if err != nil {
			return nil, err
		}
		items, page, err := Connection(project, "items")
		if err != nil {
			return nil, err
		}
		for _, item := range items {
			if item["id"] != itemID {
				continue
			}
			content, err := contract.ObjectAt(item, "content")
			if err != nil || content["__typename"] != "Issue" {
				return nil, errors.New("Project item content is not the selected issue")
			}
			n, err := contract.PositiveInteger(content["number"])
			if err != nil || n != issueNumber {
				return nil, errors.New("Project item issue number changed")
			}
			if err = t.ValidateIssueURL(content["url"], n); err != nil {
				return nil, err
			}
			return item, nil
		}
		if page["hasNextPage"] != true {
			return nil, errors.New("selected Project item was not found")
		}
		next, err := contract.Nonempty(page, "endCursor")
		if err != nil || seen[next] {
			return nil, errors.New("Project item pagination did not advance")
		}
		seen[next] = true
		cursor = &next
	}
}

func (t *Transport) AddProjectIssue(ctx context.Context, p ProjectScope, number int64, operationID string) (contract.Object, error) {
	if err := t.validateProjectScope(p); err != nil {
		return nil, err
	}
	if p.ID == "" {
		return nil, errors.New("Project node identity is mandatory for membership writes")
	}
	if _, err := OperationMarker(operationID); err != nil {
		return nil, err
	}
	if _, err := t.ReadProject(ctx, p); err != nil {
		return nil, err
	}
	issue, err := t.ReadIssue(ctx, number, false)
	if err != nil {
		return nil, err
	}
	id, err := contract.Nonempty(issue, "id")
	if err != nil {
		return nil, err
	}
	query := `mutation($project:ID!,$content:ID!,$operation:String!){addProjectV2ItemById(input:{projectId:$project,contentId:$content,clientMutationId:$operation}){clientMutationId item{id isArchived ` + fieldValues + ` content{... on Issue{id number url}}}}}`
	data, err := t.graphQL(ctx, query, contract.Object{"project": p.ID, "content": id, "operation": operationID})
	if err != nil {
		return nil, err
	}
	payload, err := contract.ObjectAt(data, "addProjectV2ItemById")
	if err != nil || payload["clientMutationId"] != operationID {
		return nil, errors.New("membership result lacks the durable mutation identity")
	}
	item, err := contract.ObjectAt(payload, "item")
	if err != nil {
		return nil, err
	}
	if _, err := contract.Nonempty(item, "id"); err != nil {
		return nil, err
	}
	content, err := contract.ObjectAt(item, "content")
	if err != nil || content["id"] != id {
		return nil, errors.New("membership result has a foreign issue")
	}
	if _, err := CompleteConnection(item, "fieldValues"); err != nil {
		return nil, err
	}
	// Preserve the provider's durable mutation identity and original payload
	// alongside the flat item shape consumed by reviewed workflow adapters.
	ack := make(contract.Object, len(item)+2)
	for key, value := range item {
		ack[key] = value
	}
	ack["clientMutationId"] = payload["clientMutationId"]
	ack["native_payload"] = payload
	return ack, nil
}

func (t *Transport) projectField(ctx context.Context, p ProjectScope, fieldID string) (contract.Object, error) {
	var cursor *string
	seen := map[string]bool{}
	for {
		project, err := t.ReadProjectPage(ctx, p, "fields", cursor)
		if err != nil {
			return nil, err
		}
		fields, page, err := Connection(project, "fields")
		if err != nil {
			return nil, err
		}
		for _, f := range fields {
			if f["id"] == fieldID {
				return f, nil
			}
		}
		if page["hasNextPage"] != true {
			return nil, errors.New("field does not belong to selected Project")
		}
		next, err := contract.Nonempty(page, "endCursor")
		if err != nil || seen[next] {
			return nil, errors.New("Project field pagination did not advance")
		}
		seen[next] = true
		cursor = &next
	}
}

func fieldInput(field contract.Object, value any) (contract.Object, error) {
	if value == nil {
		return nil, nil
	}
	switch field["dataType"] {
	case "TEXT":
		if str, ok := value.(string); ok {
			return contract.Object{"text": str}, nil
		}
	case "NUMBER":
		if _, err := contract.Number(value); err == nil {
			return contract.Object{"number": value}, nil
		}
	case "DATE":
		if str, ok := value.(string); ok {
			if _, err := time.Parse("2006-01-02", str); err == nil {
				return contract.Object{"date": str}, nil
			}
		}
	case "SINGLE_SELECT":
		name, ok := value.(string)
		if !ok {
			break
		}
		options, err := contract.Objects(field, "options")
		if err != nil {
			return nil, err
		}
		seenNames, seenIDs := map[string]bool{}, map[string]bool{}
		var selected string
		for _, o := range options {
			optionName, err := contract.Nonempty(o, "name")
			if err != nil || seenNames[optionName] {
				return nil, errors.New("Project option name missing or duplicated")
			}
			seenNames[optionName] = true
			optionID, err := contract.Nonempty(o, "id")
			if err != nil || seenIDs[optionID] {
				return nil, errors.New("Project option identity missing or duplicated")
			}
			seenIDs[optionID] = true
			if o["name"] == name {
				selected = optionID
			}
		}
		if selected != "" {
			return contract.Object{"singleSelectOptionId": selected}, nil
		}
	case "ITERATION":
		id, ok := value.(string)
		if !ok {
			break
		}
		config, err := contract.ObjectAt(field, "configuration")
		if err != nil {
			return nil, err
		}
		for _, key := range []string{"iterations", "completedIterations"} {
			a, err := contract.Objects(config, key)
			if err != nil {
				return nil, err
			}
			for _, i := range a {
				if i["id"] == id {
					return contract.Object{"iterationId": id}, nil
				}
			}
		}
	}
	return nil, errors.New("field value does not match selected Project definition")
}

func (t *Transport) SetProjectField(ctx context.Context, item ProjectItemScope, fieldID string, value any, operationID string) (contract.Object, error) {
	if _, err := OperationMarker(operationID); err != nil {
		return nil, err
	}
	if _, err := t.ProjectItem(ctx, item.Project, item.ItemID, item.IssueNumber); err != nil {
		return nil, err
	}
	field, err := t.projectField(ctx, item.Project, fieldID)
	if err != nil {
		return nil, err
	}
	input, err := fieldInput(field, value)
	if err != nil {
		return nil, err
	}
	variables := contract.Object{"project": item.Project.ID, "item": item.ItemID, "field": fieldID, "operation": operationID}
	name, decl, argument := "updateProjectV2ItemFieldValue", ",$value:ProjectV2FieldValue!", ",value:$value"
	if input == nil {
		name, decl, argument = "clearProjectV2ItemFieldValue", "", ""
	} else {
		variables["value"] = input
	}
	query := `mutation($project:ID!,$item:ID!,$field:ID!,$operation:String!` + decl + `){` + name + `(input:{projectId:$project,itemId:$item,fieldId:$field,clientMutationId:$operation` + argument + `}){clientMutationId projectV2Item{id}}}`
	data, err := t.graphQL(ctx, query, variables)
	if err != nil {
		return nil, err
	}
	payload, err := contract.ObjectAt(data, name)
	if err != nil || payload["clientMutationId"] != operationID {
		return nil, errors.New("field result lacks durable mutation identity")
	}
	result, err := contract.ObjectAt(payload, "projectV2Item")
	if err != nil || result["id"] != item.ItemID {
		return nil, errors.New("field result has a foreign item")
	}
	return payload, nil
}

func (t *Transport) ArchiveProjectItem(ctx context.Context, item ProjectItemScope, archived bool, operationID string) (contract.Object, error) {
	if _, err := OperationMarker(operationID); err != nil {
		return nil, err
	}
	if _, err := t.ProjectItem(ctx, item.Project, item.ItemID, item.IssueNumber); err != nil {
		return nil, err
	}
	name := "archiveProjectV2Item"
	if !archived {
		name = "unarchiveProjectV2Item"
	}
	query := `mutation($project:ID!,$item:ID!,$operation:String!){` + name + `(input:{projectId:$project,itemId:$item,clientMutationId:$operation}){clientMutationId item{id}}}`
	data, err := t.graphQL(ctx, query, contract.Object{"project": item.Project.ID, "item": item.ItemID, "operation": operationID})
	if err != nil {
		return nil, err
	}
	payload, err := contract.ObjectAt(data, name)
	if err != nil || payload["clientMutationId"] != operationID {
		return nil, errors.New("archive result lacks durable mutation identity")
	}
	result, err := contract.ObjectAt(payload, "item")
	if err != nil || result["id"] != item.ItemID {
		return nil, errors.New("archive result has a foreign item")
	}
	return payload, nil
}
