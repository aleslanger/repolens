package report

import (
	"context"
	"database/sql"
	"fmt"
)

// personCommitsMax bounds the recent commits listed on a person's page.
const personCommitsMax = 30

// PersonView is one merged person: their repositories, monthly activity, commit types and
// recent commits, limited to the period like the people list.
type PersonView struct {
	Person PersonRow        `json:"person"`
	Types  map[string]int   `json:"types"`
	Recent []PersonCommit   `json:"recent"`
	ByRepo []RepoMonthCount `json:"byRepo"`
	Since  int64            `json:"since"`
}

// PersonCommit is a commit with the repository it belongs to.
type PersonCommit struct {
	CommitRow
	RepoID int64  `json:"repoId"`
	Repo   string `json:"repo"`
}

// RepoMonthCount is a person's commits in one repository and month.
type RepoMonthCount struct {
	RepoID int64  `json:"repoId"`
	Month  string `json:"month"`
	Count  int    `json:"count"`
}

// personJoin restricts commits to the identities of person ? from since ?.
const personJoin = `FROM commits c JOIN identity i ON i.name=c.author_name AND i.email=c.author_email
	WHERE i.person_id=? AND c.author_time>=?`

// PersonDetail returns one person, or nil when the id is unknown or has no commits in the period.
func PersonDetail(ctx context.Context, db *sql.DB, id, since int64) (*PersonView, error) {
	all, err := PeopleSince(ctx, db, since)
	if err != nil {
		return nil, err
	}
	v := &PersonView{Types: map[string]int{}, Recent: []PersonCommit{}, ByRepo: []RepoMonthCount{}, Since: since}
	found := false
	for _, p := range all.People {
		if p.PersonID == id {
			v.Person, found = p, true
		}
	}
	if !found {
		return nil, nil
	}
	args := []any{id, since}
	steps := []struct {
		q    string
		scan func(*sql.Rows) error
	}{
		{`SELECT c.ctype, COUNT(*) ` + personJoin + ` GROUP BY 1`, func(r *sql.Rows) error {
			var t string
			var n int
			if err := r.Scan(&t, &n); err != nil {
				return err
			}
			v.Types[t] = n
			return nil
		}},
		{`SELECT c.repo_id, strftime('%Y-%m', c.author_time, 'unixepoch', 'localtime'), COUNT(*) ` + personJoin + ` GROUP BY 1, 2 ORDER BY 2`,
			func(r *sql.Rows) error {
				var x RepoMonthCount
				if err := r.Scan(&x.RepoID, &x.Month, &x.Count); err != nil {
					return err
				}
				v.ByRepo = append(v.ByRepo, x)
				return nil
			}},
		{`SELECT c.hash, c.author_time, c.author_name, c.subject, c.ctype, c.additions, c.deletions, c.agents, c.in_session, c.source,
			c.repo_id, (SELECT name FROM repo WHERE id=c.repo_id) ` + personJoin + ` ORDER BY c.author_time DESC LIMIT ` + fmt.Sprint(personCommitsMax),
			func(r *sql.Rows) error {
				var x PersonCommit
				if err := r.Scan(&x.Hash, &x.Time, &x.Author, &x.Subject, &x.Type, &x.Add, &x.Del, &x.Agents, &x.InSession, &x.Source,
					&x.RepoID, &x.Repo); err != nil {
					return err
				}
				v.Recent = append(v.Recent, x)
				return nil
			}},
	}
	for _, s := range steps {
		if err := queryEach(ctx, db, s.q, args, s.scan); err != nil {
			return nil, fmt.Errorf("person %d: %w", id, err)
		}
	}
	return v, nil
}
