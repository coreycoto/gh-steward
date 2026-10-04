package native

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"

	"github.com/coreycoto/gh-steward/internal/contract"
)

// Queries are compiled here. Callers provide typed targets and cursors, never
// arbitrary GraphQL, owner/name overrides, or unqualified node IDs.
const repositoryFields = `id name nameWithOwner url owner { login __typename } defaultBranchRef { name }`
const issueFields = `id number title body state url
  labels(first:100) { nodes { name } pageInfo { hasNextPage } }
  milestone { id number title }`
const membershipFields = `projectItems(first:100,includeArchived:true) { nodes { id project { id number title url } } pageInfo { hasNextPage } }`

func (t *Transport) repoVariables() contract.Object {
	return contract.Object{"owner": t.Repository.Owner, "name": t.Repository.Name}
}

func (t *Transport) repositoryResult(data contract.Object) (contract.Object, error) {
	r, err := contract.ObjectAt(data, "repository")
	if err != nil {
		return nil, errors.New("selected repository was not returned")
	}
	parsed, err := contract.ParseRepository(r)
	if err != nil || parsed != t.Repository {
		return nil, errors.New("repository result does not match the selected target")
	}
	if _, err := contract.Nonempty(r, "id"); err != nil {
		return nil, err
	}
	return r, nil
}

func (t *Transport) ReadRepository(ctx context.Context) (contract.Object, error) {
	data, err := t.graphQL(ctx, `query($owner:String!,$name:String!){ repository(owner:$owner,name:$name){`+repositoryFields+`} }`, t.repoVariables())
	if err != nil {
		return nil, err
	}
	return t.repositoryResult(data)
}

func issueStates(state string) (string, error) {
	switch strings.ToLower(state) {
	case "open":
		return "[OPEN]", nil
	case "closed":
		return "[CLOSED]", nil
	case "all":
		return "[OPEN,CLOSED]", nil
	default:
		return "", errors.New("issue state must be open, closed or all")
	}
}

func (t *Transport) ReadIssuesPage(ctx context.Context, state string, cursor *string, includeProjects bool) (contract.Object, error) {
	states, err := issueStates(state)
	if err != nil {
		return nil, err
	}
	fields := issueFields
	if includeProjects {
		fields += membershipFields
	}
	variables := t.repoVariables()
	variables["cursor"] = cursor
	data, err := t.graphQL(ctx, `query($owner:String!,$name:String!,$cursor:String){repository(owner:$owner,name:$name){`+repositoryFields+` issues(first:100,after:$cursor,states:`+states+`,orderBy:{field:CREATED_AT,direction:ASC}){nodes{`+fields+`} pageInfo{hasNextPage endCursor}}}}`, variables)
	if err != nil {
		return nil, err
	}
	return t.repositoryResult(data)
}

func (t *Transport) ReadIssue(ctx context.Context, number int64, includeProjects bool) (contract.Object, error) {
	if number < 1 {
		return nil, errors.New("issue number must be positive")
	}
	fields := issueFields
	if includeProjects {
		fields += membershipFields
	}
	variables := t.repoVariables()
	variables["number"] = number
	data, err := t.graphQL(ctx, `query($owner:String!,$name:String!,$number:Int!){repository(owner:$owner,name:$name){`+repositoryFields+` issue(number:$number){`+fields+`}}}`, variables)
	if err != nil {
		return nil, err
	}
	repo, err := t.repositoryResult(data)
	if err != nil {
		return nil, err
	}
	issue, err := contract.ObjectAt(repo, "issue")
	if err != nil {
		return nil, errors.New("selected issue was not returned")
	}
	actual, err := contract.PositiveInteger(issue["number"])
	if err != nil || actual != number {
		return nil, errors.New("issue identity does not match the selected target")
	}
	if err := t.ValidateIssueURL(issue["url"], number); err != nil {
		return nil, err
	}
	return issue, nil
}

func (t *Transport) ValidateIssueURL(raw any, number int64) error {
	s, ok := raw.(string)
	if !ok {
		return errors.New("issue requires a repository-qualified URL")
	}
	u, err := url.Parse(s)
	if err != nil || u.Scheme != "https" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Port() != "" && u.Port() != "443") || !strings.EqualFold(u.Hostname(), t.Repository.Host) || !strings.EqualFold(u.EscapedPath(), fmt.Sprintf("/%s/issues/%d", t.Repository.FullName(), number)) {
		return errors.New("issue URL does not match the selected repository and number")
	}
	return nil
}

// ProjectScope deliberately names the broader user/organization Project. It
// does not inherit repository ownership or infer an owner from @me.
type ProjectScope struct {
	Host      string
	Owner     string
	OwnerType string // User or Organization
	Number    int64
	ID        string // Optional on initial discovery, mandatory for mutations.
}

var ownerPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]*$`)

func (t *Transport) validateProjectScope(p ProjectScope) error {
	if !strings.EqualFold(p.Host, t.Repository.Host) || !ownerPattern.MatchString(p.Owner) || (p.OwnerType != "User" && p.OwnerType != "Organization") || p.Number < 1 {
		return errors.New("Project target requires the selected host, canonical owner, owner type and positive number")
	}
	return nil
}

func (p ProjectScope) Object() contract.Object {
	return contract.Object{"host": p.Host, "owner_login": p.Owner, "owner_type": p.OwnerType, "number": p.Number, "id": p.ID}
}

const projectFields = `id number title url closed public`
const fieldDefinitions = `fields(first:100,after:$cursor){nodes{__typename
 ... on ProjectV2FieldCommon{id name dataType}
 ... on ProjectV2SingleSelectField{options{id name}}
 ... on ProjectV2MultiSelectField{options:multiSelectOptions{id name}}
 ... on ProjectV2IterationField{configuration{iterations{id title startDate duration} completedIterations{id title startDate duration}}}
}pageInfo{hasNextPage endCursor}}`
const fieldValues = `fieldValues(first:100){nodes{__typename
 ... on ProjectV2ItemFieldTextValue{text field{... on ProjectV2FieldCommon{id name}}}
 ... on ProjectV2ItemFieldNumberValue{number field{... on ProjectV2FieldCommon{id name}}}
 ... on ProjectV2ItemFieldDateValue{date field{... on ProjectV2FieldCommon{id name}}}
 ... on ProjectV2ItemFieldSingleSelectValue{name optionId field{... on ProjectV2FieldCommon{id name}}}
 ... on ProjectV2ItemFieldIterationValue{title iterationId field{... on ProjectV2FieldCommon{id name}}}
}pageInfo{hasNextPage}}`
const projectItems = `items(first:100,after:$cursor,archivedStates:[ARCHIVED,NOT_ARCHIVED]){nodes{id isArchived ` + fieldValues + `
 content{__typename ... on Issue{` + issueFields + ` repository{nameWithOwner url} assignees(first:100){nodes{login}pageInfo{hasNextPage}}}}}
 pageInfo{hasNextPage endCursor}}`

func (t *Transport) projectResult(data contract.Object, p ProjectScope) (contract.Object, error) {
	owner, err := contract.ObjectAt(data, "repositoryOwner")
	if err != nil {
		return nil, errors.New("selected Project owner was not returned")
	}
	if owner["__typename"] != p.OwnerType || !strings.EqualFold(fmt.Sprint(owner["login"]), p.Owner) {
		return nil, errors.New("Project owner identity does not match the selected target")
	}
	project, err := contract.ObjectAt(owner, "projectV2")
	if err != nil {
		return nil, errors.New("selected Project was not returned")
	}
	number, err := contract.PositiveInteger(project["number"])
	if err != nil || number != p.Number {
		return nil, errors.New("Project number does not match the selected target")
	}
	id, err := contract.Nonempty(project, "id")
	if err != nil || (p.ID != "" && id != p.ID) {
		return nil, errors.New("Project node identity does not match the selected target")
	}
	base := "users"
	if p.OwnerType == "Organization" {
		base = "orgs"
	}
	expected := fmt.Sprintf("https://%s/%s/%s/projects/%d", strings.ToLower(p.Host), base, strings.ToLower(p.Owner), p.Number)
	if !strings.EqualFold(fmt.Sprint(project["url"]), expected) {
		return nil, errors.New("Project URL does not match the selected target")
	}
	for _, key := range []string{"closed", "public"} {
		if _, err := contract.Bool(project, key); err != nil {
			return nil, err
		}
	}
	if _, err := contract.String(project, "title"); err != nil {
		return nil, err
	}
	return project, nil
}

func (t *Transport) ReadProject(ctx context.Context, p ProjectScope) (contract.Object, error) {
	return t.ReadProjectPage(ctx, p, "metadata", nil)
}

func (t *Transport) ReadProjectPage(ctx context.Context, p ProjectScope, section string, cursor *string) (contract.Object, error) {
	if err := t.validateProjectScope(p); err != nil {
		return nil, err
	}
	selection, variablesDecl := projectFields, `$owner:String!,$number:Int!`
	variables := contract.Object{"owner": p.Owner, "number": p.Number}
	switch section {
	case "metadata":
		if cursor != nil {
			return nil, errors.New("metadata does not accept a pagination cursor")
		}
	case "fields":
		selection += " " + fieldDefinitions
	case "items":
		selection += " " + projectItems
	default:
		return nil, errors.New("unsupported Project read section")
	}
	if section != "metadata" {
		variablesDecl += `,$cursor:String`
		variables["cursor"] = cursor
	}
	query := `query(` + variablesDecl + `){repositoryOwner(login:$owner){__typename login ... on ProjectV2Owner{projectV2(number:$number){` + selection + `}}}}`
	data, err := t.graphQL(ctx, query, variables)
	if err != nil {
		return nil, err
	}
	return t.projectResult(data, p)
}
