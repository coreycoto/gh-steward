package native

import (
	"context"
	"errors"
	"strings"

	"github.com/coreycoto/gh-steward/internal/contract"
)

// ProjectOwnerScope is explicitly broader than repository ownership. Discovery
// reads only Projects visible to the authenticated principal on this host.
type ProjectOwnerScope struct {
	Host, Owner, OwnerType string
}

func (scope ProjectOwnerScope) Validate(repository contract.Repository) error {
	if !strings.EqualFold(scope.Host, repository.Host) || !ownerPattern.MatchString(scope.Owner) || (scope.OwnerType != "User" && scope.OwnerType != "Organization") {
		return errors.New("Project discovery requires the selected host, explicit owner and owner type")
	}
	return nil
}

func (t *Transport) ReadOwnerProjectsPage(ctx context.Context, scope ProjectOwnerScope, cursor *string) (contract.Object, error) {
	if err := scope.Validate(t.Repository); err != nil {
		return nil, err
	}
	data, err := t.graphQL(ctx, `query($owner:String!,$cursor:String){repositoryOwner(login:$owner){id __typename login ... on ProjectV2Owner{projectsV2(first:100,after:$cursor){nodes{`+projectFields+`}pageInfo{hasNextPage endCursor}}}}}`, contract.Object{"owner": scope.Owner, "cursor": cursor})
	if err != nil {
		return nil, err
	}
	owner, err := contract.ObjectAt(data, "repositoryOwner")
	if err != nil {
		return nil, errors.New("selected Project owner was not returned")
	}
	login, err := contract.Nonempty(owner, "login")
	if err != nil || !strings.EqualFold(login, scope.Owner) || owner["__typename"] != scope.OwnerType {
		return nil, errors.New("Project owner identity does not match discovery scope")
	}
	if _, err = contract.Nonempty(owner, "id"); err != nil {
		return nil, err
	}
	if _, _, err = Connection(owner, "projectsV2"); err != nil {
		return nil, err
	}
	return owner, nil
}
