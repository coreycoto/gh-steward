package native

import (
	"context"
	"errors"
	"sort"
	"strings"

	"github.com/coreycoto/gh-steward/internal/contract"
)

// PullRequestDraft fixes the authored change and both reviewed branch heads.
// A created PR acknowledgement must prove these values before follow-up writes.
type PullRequestDraft struct {
	Title, Body, HeadBranch, BaseBranch, HeadSHA, BaseSHA string
	Draft                                                 bool
}

func (p PullRequestDraft) Object() contract.Object {
	return contract.Object{"title": p.Title, "body": p.Body, "head_branch": p.HeadBranch,
		"base_branch": p.BaseBranch, "head_sha": p.HeadSHA, "base_sha": p.BaseSHA,
		"draft": p.Draft}
}

// ValidateBranchName accepts ordinary Git branch names while rejecting revision
// expressions, remote-owner qualifiers and caller-controlled Git options.
func ValidateBranchName(name string) error {
	if name == "" || len(name) > 1024 || name == "@" || strings.HasPrefix(name, "-") || strings.HasPrefix(name, "/") || strings.HasSuffix(name, "/") || strings.HasSuffix(name, ".") || strings.Contains(name, "..") || strings.Contains(name, "@{") || strings.Contains(name, "//") || strings.ContainsAny(name, " ~^:?*[\\") {
		return errors.New("branch must be an explicit valid local repository branch name")
	}
	for _, r := range name {
		if r < 32 || r == 127 {
			return errors.New("branch name contains a control character")
		}
	}
	for _, part := range strings.Split(name, "/") {
		if strings.HasPrefix(part, ".") || strings.HasSuffix(part, ".lock") {
			return errors.New("branch name contains a reserved Git component")
		}
	}
	return nil
}

func (p PullRequestDraft) Validate() error {
	if strings.TrimSpace(p.Title) == "" || strings.ContainsAny(p.Title, "\r\n\x00") {
		return errors.New("pull request title must be a nonblank single-line value")
	}
	if strings.Contains(p.Body, "<!-- gh-steward:operation:") {
		return errors.New("pull request body contains a reserved operation marker")
	}
	if err := ValidateBranchName(p.HeadBranch); err != nil {
		return err
	}
	if err := ValidateBranchName(p.BaseBranch); err != nil {
		return err
	}
	if p.HeadBranch == p.BaseBranch {
		return errors.New("pull request head and base branches must differ")
	}
	if _, err := CommitOID(p.HeadSHA); err != nil {
		return err
	}
	_, err := CommitOID(p.BaseSHA)
	return err
}

// ReadBranch retains the immutable ref and repository identity. A null or
// non-commit target is an unavailable branch, never an empty successful read.
func (t *Transport) ReadBranch(ctx context.Context, name string) (contract.Object, error) {
	if err := ValidateBranchName(name); err != nil {
		return nil, err
	}
	variables := t.repoVariables()
	variables["ref"] = "refs/heads/" + name
	data, err := t.graphQL(ctx, `query($owner:String!,$name:String!,$ref:String!){repository(owner:$owner,name:$name){`+repositoryFields+` ref(qualifiedName:$ref){id name prefix target{... on Commit{oid}}}}}`, variables)
	if err != nil {
		return nil, err
	}
	repository, err := t.repositoryResult(data)
	if err != nil {
		return nil, err
	}
	ref, err := contract.ObjectAt(repository, "ref")
	if err != nil || ref["name"] != name || ref["prefix"] != "refs/heads/" {
		return nil, errors.New("selected branch ref was not returned")
	}
	id, err := contract.Nonempty(ref, "id")
	if err != nil {
		return nil, err
	}
	target, err := contract.ObjectAt(ref, "target")
	if err != nil {
		return nil, err
	}
	sha, err := CommitOID(target["oid"])
	if err != nil {
		return nil, err
	}
	return contract.Object{"repo": t.Repository.Object(), "repository_node_id": repository["id"], "id": id, "name": name, "sha": sha}, nil
}

const createdPullRequestFields = `id number url title body state isDraft merged headRefName baseRefName headRefOid baseRefOid maintainerCanModify repository{id nameWithOwner url} headRepository{id nameWithOwner url}`

// ReadDeliveryRepository captures the server-owned branch retention policy.
// It is used by composed delivery, without changing repository settings.
func (t *Transport) ReadDeliveryRepository(ctx context.Context) (contract.Object, error) {
	data, err := t.graphQL(ctx, `query($owner:String!,$name:String!){repository(owner:$owner,name:$name){`+repositoryFields+` deleteBranchOnMerge}}`, t.repoVariables())
	if err != nil {
		return nil, err
	}
	repository, err := t.repositoryResult(data)
	if err != nil {
		return nil, err
	}
	defaultRef, err := contract.ObjectAt(repository, "defaultBranchRef")
	if err != nil {
		return nil, errors.New("delivery default branch is unavailable")
	}
	branch, err := contract.Nonempty(defaultRef, "name")
	if err != nil {
		return nil, err
	}
	if err := ValidateBranchName(branch); err != nil {
		return nil, err
	}
	if _, err := contract.Bool(repository, "deleteBranchOnMerge"); err != nil {
		return nil, err
	}
	return repository, nil
}

// ReadPullRequestsForHead paginates every matching PR in all lifecycle states.
// It binds every page and node to one repository incarnation and exact branch.
func (t *Transport) ReadPullRequestsForHead(ctx context.Context, headBranch string) (contract.Object, error) {
	if err := ValidateBranchName(headBranch); err != nil {
		return nil, err
	}
	var cursor *string
	var repositoryNodeID string
	seenCursors, seenIDs, seenNumbers := map[string]bool{}, map[string]bool{}, map[int64]bool{}
	rows := []any{}
	for {
		variables := t.repoVariables()
		variables["head"], variables["cursor"] = headBranch, cursor
		data, err := t.graphQL(ctx, `query($owner:String!,$name:String!,$head:String!,$cursor:String){repository(owner:$owner,name:$name){`+repositoryFields+` pullRequests(first:100,after:$cursor,headRefName:$head,states:[OPEN,CLOSED,MERGED],orderBy:{field:CREATED_AT,direction:ASC}){nodes{`+createdPullRequestFields+`}pageInfo{hasNextPage endCursor}}}}`, variables)
		if err != nil {
			return nil, err
		}
		repository, err := t.repositoryResult(data)
		if err != nil {
			return nil, err
		}
		id, _ := contract.Nonempty(repository, "id")
		if repositoryNodeID != "" && id != repositoryNodeID {
			return nil, errors.New("repository incarnation changed during head PR pagination")
		}
		repositoryNodeID = id
		prs, page, err := Connection(repository, "pullRequests")
		if err != nil {
			return nil, err
		}
		for _, pr := range prs {
			number, err := contract.PositiveInteger(pr["number"])
			if err != nil || seenNumbers[number] {
				return nil, errors.New("head PR collection contains a malformed or repeated number")
			}
			nodeID, err := contract.Nonempty(pr, "id")
			if err != nil || seenIDs[nodeID] {
				return nil, errors.New("head PR collection contains a malformed or repeated immutable identity")
			}
			if err := t.ValidatePullRequestURL(pr["url"], number); err != nil {
				return nil, err
			}
			for _, key := range []string{"repository", "headRepository"} {
				r, err := contract.ObjectAt(pr, key)
				if err != nil || r["id"] != repositoryNodeID {
					return nil, errors.New("head PR has an unavailable or foreign repository identity")
				}
				parsed, err := contract.ParseRepository(r)
				if err != nil || parsed != t.Repository {
					return nil, errors.New("head PR belongs to another repository")
				}
			}
			if pr["headRefName"] != headBranch {
				return nil, errors.New("head PR collection returned another branch")
			}
			for _, key := range []string{"title", "body", "baseRefName"} {
				if _, err := contract.String(pr, key); err != nil {
					return nil, err
				}
			}
			if err := ValidateBranchName(pr["baseRefName"].(string)); err != nil {
				return nil, err
			}
			for _, key := range []string{"headRefOid", "baseRefOid"} {
				if _, err := CommitOID(pr[key]); err != nil {
					return nil, err
				}
			}
			for _, key := range []string{"isDraft", "merged", "maintainerCanModify"} {
				if _, err := contract.Bool(pr, key); err != nil {
					return nil, err
				}
			}
			if (pr["state"] != "OPEN" && pr["state"] != "CLOSED" && pr["state"] != "MERGED") || ((pr["state"] == "MERGED") != (pr["merged"] == true)) {
				return nil, errors.New("head PR state and merged flag are inconsistent")
			}
			seenIDs[nodeID], seenNumbers[number] = true, true
			rows = append(rows, pr)
		}
		if len(rows) > 100000 {
			return nil, errors.New("head PR collection exceeds supported size")
		}
		if page["hasNextPage"] != true {
			break
		}
		next, err := contract.Nonempty(page, "endCursor")
		if err != nil || seenCursors[next] {
			return nil, errors.New("head PR pagination cursor did not advance")
		}
		seenCursors[next], cursor = true, &next
	}
	sort.Slice(rows, func(i, j int) bool {
		a, _ := contract.PositiveInteger(rows[i].(contract.Object)["number"])
		b, _ := contract.PositiveInteger(rows[j].(contract.Object)["number"])
		return a < b
	})
	return contract.Object{"repo": t.Repository.Object(), "repository_node_id": repositoryNodeID, "head_branch": headBranch,
		"pull_requests": rows, "provenance": contract.Object{"live": true, "complete": true, "source": "github_api"}}, nil
}

// CreatePullRequest uses GitHub's typed correlated response and verifies the
// captured heads before dispatch. GitHub's create input has no atomic expected-
// head field, so any acknowledgement mismatch remains an unknown operation;
// it never authorizes a retry or downstream state changes.
func (t *Transport) CreatePullRequest(ctx context.Context, draft PullRequestDraft, repositoryNodeID, operationID string) (contract.Object, error) {
	if err := draft.Validate(); err != nil {
		return nil, err
	}
	if repositoryNodeID == "" {
		return nil, errors.New("pull request creation requires the reviewed repository node ID")
	}
	body, err := markedBody(draft.Body, operationID)
	if err != nil {
		return nil, err
	}
	for _, branch := range []struct{ name, sha string }{{draft.HeadBranch, draft.HeadSHA}, {draft.BaseBranch, draft.BaseSHA}} {
		observed, err := t.ReadBranch(ctx, branch.name)
		if err != nil {
			return nil, err
		}
		if observed["repository_node_id"] != repositoryNodeID || observed["sha"] != branch.sha {
			return nil, errors.New("reviewed repository or branch head changed before pull request creation")
		}
	}
	input := contract.Object{"repositoryId": repositoryNodeID, "headRepositoryId": repositoryNodeID,
		"clientMutationId": operationID, "headRefName": draft.HeadBranch, "baseRefName": draft.BaseBranch,
		"title": draft.Title, "body": body, "draft": draft.Draft}
	data, err := t.graphQL(ctx, `mutation($input:CreatePullRequestInput!){createPullRequest(input:$input){clientMutationId pullRequest{`+createdPullRequestFields+`}}}`, contract.Object{"input": input})
	if err != nil {
		return nil, err
	}
	ack, err := contract.ObjectAt(data, "createPullRequest")
	if err != nil || ack["clientMutationId"] != operationID {
		return nil, errors.New("pull request creation lacks the durable native acknowledgement identity")
	}
	pr, err := contract.ObjectAt(ack, "pullRequest")
	if err != nil {
		return nil, err
	}
	if err := t.ValidateCreatedPullRequest(pr, draft, repositoryNodeID, operationID); err != nil {
		return nil, err
	}
	return ack, nil
}

// ValidateCreatedPullRequest is also used when validating durable native ACKs
// in a fresh process. Its result is the provider payload, without fabrication.
func (t *Transport) ValidateCreatedPullRequest(pr contract.Object, draft PullRequestDraft, repositoryNodeID, operationID string) error {
	if err := draft.Validate(); err != nil {
		return err
	}
	if repositoryNodeID == "" {
		return errors.New("created pull request requires a repository node identity")
	}
	body, err := markedBody(draft.Body, operationID)
	if err != nil {
		return err
	}
	if _, err := contract.Nonempty(pr, "id"); err != nil {
		return err
	}
	number, err := contract.PositiveInteger(pr["number"])
	if err != nil {
		return err
	}
	if err := t.ValidatePullRequestURL(pr["url"], number); err != nil {
		return err
	}
	for _, key := range []string{"repository", "headRepository"} {
		repository, err := contract.ObjectAt(pr, key)
		if err != nil || repository["id"] != repositoryNodeID {
			return errors.New("created pull request has a foreign immutable repository identity")
		}
		actual, err := contract.ParseRepository(repository)
		if err != nil || actual != t.Repository {
			return errors.New("created pull request has a foreign repository scope")
		}
	}
	if pr["title"] != draft.Title || pr["body"] != body || pr["state"] != "OPEN" || pr["merged"] != false || pr["isDraft"] != draft.Draft || pr["headRefName"] != draft.HeadBranch || pr["baseRefName"] != draft.BaseBranch || pr["headRefOid"] != draft.HeadSHA || pr["baseRefOid"] != draft.BaseSHA {
		return errors.New("created pull request acknowledgement differs from the reviewed draft or exact heads")
	}
	return nil
}
