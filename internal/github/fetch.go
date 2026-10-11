package github

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"

	"repolens/internal/gitlog"
)

// GraphQL connections use pages of 30: larger pages time out on big repositories.
const (
	batchSize = 8 // repositories per aliased metadata query
	runsPage  = 100
)

type Ref struct{ Owner, Name string }

type count struct {
	TotalCount int `json:"totalCount"`
}

type Milestone struct {
	Number int        `json:"number"`
	Title  string     `json:"title"`
	State  string     `json:"state"`
	DueOn  *time.Time `json:"dueOn"`
	Open   count      `json:"open"`
	Closed count      `json:"closed"`
}

type Label struct {
	Name  string `json:"name"`
	Color string `json:"color"`
}

type Release struct {
	TagName      string     `json:"tagName"`
	Name         string     `json:"name"`
	PublishedAt  *time.Time `json:"publishedAt"`
	IsPrerelease bool       `json:"isPrerelease"`
}

type RepoMeta struct {
	NameWithOwner    string     `json:"nameWithOwner"`
	Description      string     `json:"description"`
	IsPrivate        bool       `json:"isPrivate"`
	IsFork           bool       `json:"isFork"`
	IsArchived       bool       `json:"isArchived"`
	StargazerCount   int        `json:"stargazerCount"`
	ForkCount        int        `json:"forkCount"`
	PushedAt         *time.Time `json:"pushedAt"`
	DefaultBranchRef *struct {
		Name string `json:"name"`
	} `json:"defaultBranchRef"`
	Issues       count `json:"issues"`
	PullRequests count `json:"pullRequests"`
	Languages    struct {
		Edges []struct {
			Size int64 `json:"size"`
			Node struct {
				Name string `json:"name"`
			} `json:"node"`
		} `json:"edges"`
	} `json:"languages"`
	Milestones struct {
		Nodes []Milestone `json:"nodes"`
	} `json:"milestones"`
	Labels struct {
		Nodes []Label `json:"nodes"`
	} `json:"labels"`
	Releases struct {
		Nodes []Release `json:"nodes"`
	} `json:"releases"`
	// First pages of recently updated issues and PRs, fetched in the same batch.
	RecentIssues *IssuePage `json:"recentIssues"`
	RecentPRs    *IssuePage `json:"recentPRs"`
}

// IssuePage is one page of issues or pull requests ordered by update time, newest first.
type IssuePage struct {
	PageInfo pageInfo `json:"pageInfo"`
	Nodes    []Issue  `json:"nodes"`
}

// Complete reports whether the page holds everything updated since `since`.
func (p *IssuePage) Complete(since time.Time) bool {
	if !p.PageInfo.HasNextPage || len(p.Nodes) == 0 {
		return true
	}
	return !since.IsZero() && p.Nodes[len(p.Nodes)-1].UpdatedAt.Before(since)
}

// Since filters the page to items updated at or after since and marks PRs.
func (p *IssuePage) Since(since time.Time, prs bool) []Issue {
	var out []Issue
	for _, it := range p.Nodes {
		if !since.IsZero() && it.UpdatedAt.Before(since) {
			break
		}
		it.IsPR = prs
		out = append(out, it)
	}
	return out
}

// Ref splits nameWithOwner.
func (m RepoMeta) Ref() Ref {
	o, n, _ := strings.Cut(m.NameWithOwner, "/")
	return Ref{Owner: o, Name: n}
}

func (m RepoMeta) DefaultBranch() string {
	if m.DefaultBranchRef == nil {
		return ""
	}
	return m.DefaultBranchRef.Name
}

const issueFields = `pageInfo{hasNextPage endCursor} nodes{number title state createdAt closedAt updatedAt author{login} milestone{title} labels(first:20){nodes{name}}}`
const prFields = `pageInfo{hasNextPage endCursor} nodes{number title state createdAt closedAt mergedAt updatedAt author{login} milestone{title} labels(first:20){nodes{name}}}`

const metaFields = `nameWithOwner description isPrivate isFork isArchived stargazerCount forkCount pushedAt
defaultBranchRef{name} issues(states:OPEN){totalCount} pullRequests(states:OPEN){totalCount}
languages(first:20,orderBy:{field:SIZE,direction:DESC}){edges{size node{name}}}
milestones(first:50){nodes{number title state dueOn open:issues(states:OPEN){totalCount} closed:issues(states:CLOSED){totalCount}}}
labels(first:100){nodes{name color}}
releases(first:50,orderBy:{field:CREATED_AT,direction:DESC}){nodes{tagName name publishedAt isPrerelease}}`

// MetaRequest asks for one repository; Since (zero = everything) bounds the bundled issue pages.
type MetaRequest struct {
	Ref
	Since time.Time
}

// FetchMeta loads metadata plus the first page of recently updated issues and PRs for many
// repositories, several per GraphQL call via aliases. A repository GitHub refuses (deleted,
// renamed, no access, SSO-protected) comes back as nil with the reason in skipped; transport
// and rate-limit failures are returned as errors.
func (c *Client) FetchMeta(ctx context.Context, reqs []MetaRequest) ([]*RepoMeta, []string, error) {
	metas := make([]*RepoMeta, 0, len(reqs))
	skipped := make([]string, 0, len(reqs))
	for start := 0; start < len(reqs); start += batchSize {
		end := min(start+batchSize, len(reqs))
		data, err := c.metaBatch(ctx, reqs[start:end])
		if err == nil {
			for i := range reqs[start:end] {
				metas = append(metas, data[fmt.Sprintf("r%d", i)])
				skipped = append(skipped, "")
			}
			continue
		}
		var apiErr *APIError
		if !errors.As(err, &apiErr) || apiErr.Path != "/graphql" {
			return nil, nil, err
		}
		// A GraphQL error in a batch: retry one by one so a single refused repository does not
		// take the others down.
		for _, r := range reqs[start:end] {
			one, err := c.metaBatch(ctx, []MetaRequest{r})
			switch {
			case err == nil:
				metas, skipped = append(metas, one["r0"]), append(skipped, "")
			case errors.As(err, &apiErr) && apiErr.Path == "/graphql":
				fmt.Fprintf(c.log, "  GitHub: %s/%s přeskočeno: %s\n", r.Owner, r.Name, apiErr.Msg)
				metas, skipped = append(metas, nil), append(skipped, apiErr.Msg)
			default:
				return nil, nil, err
			}
		}
	}
	return metas, skipped, nil
}

// metaBatch runs one aliased metadata query for up to batchSize repositories.
func (c *Client) metaBatch(ctx context.Context, reqs []MetaRequest) (map[string]*RepoMeta, error) {
	var q strings.Builder
	var params []string
	vars := map[string]any{}
	for i, r := range reqs {
		params = append(params, fmt.Sprintf("$o%d:String!,$n%d:String!,$s%d:DateTime", i, i, i))
		fmt.Fprintf(&q, "r%d: repository(owner:$o%d,name:$n%d){%s\nrecentIssues: issues(first:30,orderBy:{field:UPDATED_AT,direction:DESC},filterBy:{since:$s%d}){%s}\nrecentPRs: pullRequests(first:30,orderBy:{field:UPDATED_AT,direction:DESC}){%s}}\n",
			i, i, i, metaFields, i, issueFields, prFields)
		vars[fmt.Sprintf("o%d", i)] = r.Owner
		vars[fmt.Sprintf("n%d", i)] = r.Name
		var since any
		if !r.Since.IsZero() {
			since = r.Since.UTC().Format(time.RFC3339)
		}
		vars[fmt.Sprintf("s%d", i)] = since
	}
	query := "query(" + strings.Join(params, ",") + "){\n" + q.String() + "}"
	data := map[string]*RepoMeta{}
	if err := c.GraphQL(ctx, query, vars, &data); err != nil {
		return nil, err
	}
	return data, nil
}

// ListOwnerRepos lists repositories owned by login (a user or an organisation).
func (c *Client) ListOwnerRepos(ctx context.Context, login string) ([]RepoMeta, error) {
	const q = `query($login:String!,$after:String){repositoryOwner(login:$login){repositories(first:30,after:$after,ownerAffiliations:OWNER,orderBy:{field:PUSHED_AT,direction:DESC}){pageInfo{hasNextPage endCursor} nodes{` + metaFields + `}}}}`
	var all []RepoMeta
	var after any
	for {
		var data struct {
			RepositoryOwner *struct {
				Repositories struct {
					PageInfo pageInfo   `json:"pageInfo"`
					Nodes    []RepoMeta `json:"nodes"`
				} `json:"repositories"`
			} `json:"repositoryOwner"`
		}
		if err := c.GraphQL(ctx, q, map[string]any{"login": login, "after": after}, &data); err != nil {
			return nil, err
		}
		if data.RepositoryOwner == nil {
			return nil, fmt.Errorf("GitHub účet %q neexistuje nebo k němu není přístup", login)
		}
		r := data.RepositoryOwner.Repositories
		all = append(all, r.Nodes...)
		if !r.PageInfo.HasNextPage {
			return all, nil
		}
		after = r.PageInfo.EndCursor
	}
}

type pageInfo struct {
	HasNextPage bool   `json:"hasNextPage"`
	EndCursor   string `json:"endCursor"`
}

type Issue struct {
	Number    int        `json:"number"`
	Title     string     `json:"title"`
	State     string     `json:"state"`
	CreatedAt time.Time  `json:"createdAt"`
	ClosedAt  *time.Time `json:"closedAt"`
	MergedAt  *time.Time `json:"mergedAt"`
	UpdatedAt time.Time  `json:"updatedAt"`
	Author    *struct {
		Login string `json:"login"`
	} `json:"author"`
	Milestone *struct {
		Title string `json:"title"`
	} `json:"milestone"`
	Labels struct {
		Nodes []struct {
			Name string `json:"name"`
		} `json:"nodes"`
	} `json:"labels"`
	IsPR bool `json:"-"`
}

func (i Issue) AuthorLogin() string {
	if i.Author == nil {
		return ""
	}
	return i.Author.Login
}

func (i Issue) LabelNames() []string {
	out := make([]string, 0, len(i.Labels.Nodes))
	for _, l := range i.Labels.Nodes {
		out = append(out, l.Name)
	}
	return out
}

// FetchIssues returns issues or pull requests updated at or after since (zero = all).
func (c *Client) FetchIssues(ctx context.Context, r Ref, prs bool, since time.Time) ([]Issue, error) {
	conn, extra := "issues", ""
	if prs {
		conn, extra = "pullRequests", " mergedAt"
	}
	q := `query($owner:String!,$name:String!,$after:String){repository(owner:$owner,name:$name){` + conn +
		`(first:30,after:$after,orderBy:{field:UPDATED_AT,direction:DESC}){pageInfo{hasNextPage endCursor} nodes{number title state createdAt closedAt updatedAt` + extra +
		` author{login} milestone{title} labels(first:20){nodes{name}}}}}}`
	var all []Issue
	var after any
	for {
		var data struct {
			Repository *map[string]struct {
				PageInfo pageInfo `json:"pageInfo"`
				Nodes    []Issue  `json:"nodes"`
			} `json:"repository"`
		}
		if err := c.GraphQL(ctx, q, map[string]any{"owner": r.Owner, "name": r.Name, "after": after}, &data); err != nil {
			return nil, err
		}
		if data.Repository == nil {
			return nil, fmt.Errorf("repozitář %s/%s nenalezen", r.Owner, r.Name)
		}
		page := (*data.Repository)[conn]
		for _, it := range page.Nodes {
			if !since.IsZero() && it.UpdatedAt.Before(since) {
				return all, nil
			}
			it.IsPR = prs
			all = append(all, it)
		}
		if !page.PageInfo.HasNextPage {
			return all, nil
		}
		after = page.PageInfo.EndCursor
	}
}

type Run struct {
	ID           int64      `json:"id"`
	Name         string     `json:"name"`
	Event        string     `json:"event"`
	HeadBranch   string     `json:"head_branch"`
	Status       string     `json:"status"`
	Conclusion   *string    `json:"conclusion"`
	CreatedAt    time.Time  `json:"created_at"`
	RunStartedAt *time.Time `json:"run_started_at"`
	UpdatedAt    time.Time  `json:"updated_at"`
}

// Duration is the wall time of a completed run, or 0 when unknown.
func (r Run) Duration() time.Duration {
	if r.Status != "completed" || r.RunStartedAt == nil || r.UpdatedAt.Before(*r.RunStartedAt) {
		return 0
	}
	return r.UpdatedAt.Sub(*r.RunStartedAt)
}

// FetchRuns lists GitHub Actions runs created on or after since. The first page uses a stable
// URL so an unchanged repository is answered from the ETag cache (304).
func (c *Client) FetchRuns(ctx context.Context, r Ref, since time.Time) ([]Run, error) {
	path := fmt.Sprintf("/repos/%s/%s/actions/runs?per_page=%d", url.PathEscape(r.Owner), url.PathEscape(r.Name), runsPage)
	var all []Run
	for path != "" {
		var page struct {
			WorkflowRuns []Run `json:"workflow_runs"`
		}
		next, err := c.GetJSON(ctx, path, &page)
		if err != nil {
			return nil, err
		}
		for _, run := range page.WorkflowRuns {
			if run.CreatedAt.Before(since) {
				return all, nil
			}
			all = append(all, run)
		}
		path = next
	}
	return all, nil
}

type historyCommit struct {
	OID             string    `json:"oid"`
	MessageHeadline string    `json:"messageHeadline"`
	Message         string    `json:"message"`
	AuthoredDate    time.Time `json:"authoredDate"`
	Additions       int       `json:"additions"`
	Deletions       int       `json:"deletions"`
	ChangedFiles    *int      `json:"changedFilesIfAvailable"`
	Parents         count     `json:"parents"`
	Author          struct {
		Name  string `json:"name"`
		Email string `json:"email"`
	} `json:"author"`
}

// HistoryCommit is a commit from the API: no per-file stats, only totals.
type HistoryCommit struct {
	gitlog.Commit
	Additions, Deletions, Files int
}

var trailerLine = regexp.MustCompile(`^([A-Za-z][A-Za-z0-9-]*):\s+(.+)$`)

// trailersFromMessage reads the last paragraph of a commit message as trailers.
func trailersFromMessage(msg string) []gitlog.Trailer {
	paras := strings.Split(strings.TrimSpace(msg), "\n\n")
	if len(paras) < 2 {
		return nil
	}
	var out []gitlog.Trailer
	for line := range strings.SplitSeq(paras[len(paras)-1], "\n") {
		m := trailerLine.FindStringSubmatch(strings.TrimSpace(line))
		if m == nil {
			return nil // not a trailer block
		}
		out = append(out, gitlog.Trailer{Key: m[1], Value: m[2]})
	}
	return out
}

func (h historyCommit) toCommit() HistoryCommit {
	files := 0
	if h.ChangedFiles != nil {
		files = *h.ChangedFiles
	}
	parents := make([]string, h.Parents.TotalCount)
	return HistoryCommit{
		Commit: gitlog.Commit{
			Hash: h.OID, AuthorName: h.Author.Name, AuthorEmail: h.Author.Email,
			AuthorTime: h.AuthoredDate, CommitTime: h.AuthoredDate, Parents: parents,
			Subject: h.MessageHeadline, Trailers: trailersFromMessage(h.Message),
		},
		Additions: h.Additions, Deletions: h.Deletions, Files: files,
	}
}

// FetchHistory returns default-branch commits authored after since (zero = all).
func (c *Client) FetchHistory(ctx context.Context, r Ref, since time.Time, fn func(HistoryCommit) error) error {
	const q = `query($owner:String!,$name:String!,$after:String,$since:GitTimestamp){repository(owner:$owner,name:$name){defaultBranchRef{target{... on Commit{history(first:30,after:$after,since:$since){pageInfo{hasNextPage endCursor} nodes{oid messageHeadline message authoredDate additions deletions changedFilesIfAvailable parents{totalCount} author{name email}}}}}}}}`
	var after, sinceVar any
	if !since.IsZero() {
		sinceVar = since.UTC().Format(time.RFC3339)
	}
	for {
		var data struct {
			Repository *struct {
				DefaultBranchRef *struct {
					Target struct {
						History struct {
							PageInfo pageInfo        `json:"pageInfo"`
							Nodes    []historyCommit `json:"nodes"`
						} `json:"history"`
					} `json:"target"`
				} `json:"defaultBranchRef"`
			} `json:"repository"`
		}
		vars := map[string]any{"owner": r.Owner, "name": r.Name, "after": after, "since": sinceVar}
		if err := c.GraphQL(ctx, q, vars, &data); err != nil {
			return err
		}
		if data.Repository == nil || data.Repository.DefaultBranchRef == nil {
			return nil // empty repository
		}
		h := data.Repository.DefaultBranchRef.Target.History
		for _, n := range h.Nodes {
			if err := fn(n.toCommit()); err != nil {
				return err
			}
		}
		if !h.PageInfo.HasNextPage {
			return nil
		}
		after = h.PageInfo.EndCursor
	}
}
