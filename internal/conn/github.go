package conn

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/cli/go-gh/v2/pkg/api"
)

// Repo is a GitHub repository.
type Repo struct {
	Host  string
	Owner string
	Name  string
}

func (r Repo) String() string { return r.Owner + "/" + r.Name }

// PR is the newest pull request whose head is a given branch in one of your
// repositories (the repository itself, or a remote: your fork).
type PR struct {
	Number   int    `json:"number"`
	State    string `json:"state"` // OPEN, MERGED or CLOSED
	HeadOID  string `json:"head_oid"`
	HeadRepo string `json:"head_repo"` // OWNER/REPO the head branch lives in
	URL      string `json:"url"`
}

// PRsPerBranch is how many PRs, newest first, are read for each head branch name
// to find the newest one from your repositories: a fork's PR from a branch of the
// same name (patch-1) is not yours.
const PRsPerBranch = 10

// PRResult is the answer to one PullRequests call.
type PRResult struct {
	DefaultBranch string
	ByBranch      map[string]PR // branches without a PR are absent
}

// BatchSize is how many branches one GraphQL query asks about.
const BatchSize = 50

// PRQuery builds the GraphQL query for one batch of n branches, passed as variables
// $b0..$b<n-1>. Variables need no escaping, whatever a branch is called.
func PRQuery(n int) string {
	var vars, fields strings.Builder
	for i := 0; i < n; i++ {
		fmt.Fprintf(&vars, ", $b%d: String!", i)
		fmt.Fprintf(&fields, "\n    b%d: pullRequests(headRefName: $b%d, first: %d, states: [OPEN, MERGED, CLOSED], orderBy: {field: CREATED_AT, direction: DESC}) { nodes { number state headRefOid url headRepository { nameWithOwner } } }", i, i, PRsPerBranch)
	}
	return fmt.Sprintf("query($owner: String!, $name: String!%s) {\n  repository(owner: $owner, name: $name) {\n    defaultBranchRef { name }%s\n  }\n}", vars.String(), fields.String())
}

// ParsePRResponse reads the `data` of one batch: aliases b<i> answer branches[i].
// For each branch it keeps the newest PR whose head repository is one of heads.
func ParsePRResponse(data []byte, branches []string, heads []Repo, into *PRResult) error {
	var resp struct {
		Repository map[string]json.RawMessage `json:"repository"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return fmt.Errorf("reading pull requests: %w", err)
	}
	if resp.Repository == nil {
		return fmt.Errorf("reading pull requests: repository not found")
	}
	// The default branch is protected, so an answer without it is no answer.
	var ref *struct{ Name string }
	if err := json.Unmarshal(resp.Repository["defaultBranchRef"], &ref); err != nil || ref == nil || ref.Name == "" {
		return fmt.Errorf("reading pull requests: no default branch in the answer")
	}
	into.DefaultBranch = ref.Name
	for i, b := range branches {
		raw, ok := resp.Repository[fmt.Sprintf("b%d", i)]
		if !ok {
			return fmt.Errorf("reading pull requests: no answer for branch %q", b)
		}
		var conn struct {
			Nodes []struct {
				Number         int
				State          string
				HeadRefOid     string
				URL            string
				HeadRepository *struct{ NameWithOwner string }
			}
		}
		if err := json.Unmarshal(raw, &conn); err != nil {
			return fmt.Errorf("reading pull requests for %q: %w", b, err)
		}
		for _, n := range conn.Nodes {
			// A deleted fork has no head repository: it cannot be shown to be yours.
			if n.HeadRepository == nil || !isOneOf(n.HeadRepository.NameWithOwner, heads) {
				continue
			}
			into.ByBranch[b] = PR{Number: n.Number, State: n.State, HeadOID: n.HeadRefOid, HeadRepo: n.HeadRepository.NameWithOwner, URL: n.URL}
			break
		}
	}
	return nil
}

func isOneOf(nameWithOwner string, repos []Repo) bool {
	for _, r := range repos {
		if strings.EqualFold(nameWithOwner, r.String()) {
			return true
		}
	}
	return false
}

// PullRequests asks GitHub through go-gh, which finds the host's token the way gh does.
func (x *Exec) PullRequests(ctx context.Context, repo Repo, heads []Repo, branches []string) (PRResult, error) {
	res := PRResult{ByBranch: map[string]PR{}}
	client, err := api.NewGraphQLClient(api.ClientOptions{Host: repo.Host})
	if err != nil {
		return res, err
	}
	// Always one query, even with no branches, so the default branch is known.
	for start := 0; start == 0 || start < len(branches); start += BatchSize {
		batch := branches[start:min(start+BatchSize, len(branches))]
		vars := map[string]any{"owner": repo.Owner, "name": repo.Name}
		for i, b := range batch {
			vars[fmt.Sprintf("b%d", i)] = b
		}
		var data json.RawMessage
		x.debugf("graphql %s/%s: %d branches", repo.Host, repo, len(batch))
		if err := client.DoWithContext(ctx, PRQuery(len(batch)), vars, &data); err != nil {
			return res, fmt.Errorf("asking %s for pull requests of %s: %w", repo.Host, repo, err)
		}
		if err := ParsePRResponse(data, batch, heads, &res); err != nil {
			return res, err
		}
	}
	return res, nil
}
