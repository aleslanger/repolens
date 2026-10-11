package report

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

type PersonRepo struct {
	RepoID  int64  `json:"repoId"`
	Name    string `json:"name"`
	Commits int    `json:"commits"`
	Last    int64  `json:"last"`
}

type IdentityRow struct {
	Name  string `json:"name"`
	Email string `json:"email"`
	Via   string `json:"via"`
}

type PersonRow struct {
	Author
	AgentCommits int           `json:"agentCommits"`
	Repos        []PersonRepo  `json:"repos"`
	Identities   []IdentityRow `json:"identities"`
	Monthly      []MonthCount  `json:"monthly"`
}

type MonthCount struct {
	Month string `json:"month"`
	Count int    `json:"count"`
}

type PeopleView struct {
	People []PersonRow `json:"people"`
}

// People aggregates commits across repositories per merged identity.
func People(ctx context.Context, db *sql.DB) (*PeopleView, error) { return PeopleSince(ctx, db, 0) }

// PeopleSince counts only commits from since on (0 = all time); identities are always all.
func PeopleSince(ctx context.Context, db *sql.DB, since int64) (*PeopleView, error) {
	v := &PeopleView{People: []PersonRow{}}
	idx := map[string]int{}
	period := []any{since}
	err := queryEach(ctx, db, fmt.Sprintf(authorSQL, "WHERE c.author_time>=?"), period, func(r *sql.Rows) error {
		a, err := scanAuthor(r)
		if err != nil {
			return err
		}
		idx[personKey(a.PersonID, a.Emails)] = len(v.People)
		v.People = append(v.People, PersonRow{Author: a, Repos: []PersonRepo{}, Identities: []IdentityRow{}, Monthly: []MonthCount{}})
		return nil
	})
	if err != nil {
		return nil, err
	}
	const groupKey = `IFNULL(CAST(i.person_id AS TEXT), 'e:'||c.author_email)`
	const join = `FROM commits c LEFT JOIN identity i ON i.name=c.author_name AND i.email=c.author_email`
	find := func(key string) *PersonRow {
		if i, ok := idx[key]; ok {
			return &v.People[i]
		}
		return nil
	}
	err = queryEach(ctx, db, `SELECT `+groupKey+`, r.id, r.name, COUNT(*), MAX(c.author_time), SUM(c.agents<>'' OR c.in_session) `+join+`
		JOIN repo r ON r.id=c.repo_id WHERE c.author_time>=? GROUP BY 1, r.id ORDER BY COUNT(*) DESC`, period, func(r *sql.Rows) error {
		var key string
		var pr PersonRepo
		var agent int
		if err := r.Scan(&key, &pr.RepoID, &pr.Name, &pr.Commits, &pr.Last, &agent); err != nil {
			return err
		}
		if p := find(key); p != nil {
			p.Repos = append(p.Repos, pr)
			p.AgentCommits += agent
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	err = queryEach(ctx, db, `SELECT `+groupKey+`, strftime('%Y-%m', c.author_time, 'unixepoch', 'localtime'), COUNT(*) `+join+`
		WHERE c.author_time>=? GROUP BY 1, 2 ORDER BY 2`, period, func(r *sql.Rows) error {
		var key string
		var m MonthCount
		if err := r.Scan(&key, &m.Month, &m.Count); err != nil {
			return err
		}
		if p := find(key); p != nil {
			p.Monthly = append(p.Monthly, m)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	err = queryEach(ctx, db, `SELECT CAST(person_id AS TEXT), name, email, via FROM identity ORDER BY person_id, email`, nil, func(r *sql.Rows) error {
		var key string
		var ir IdentityRow
		if err := r.Scan(&key, &ir.Name, &ir.Email, &ir.Via); err != nil {
			return err
		}
		if p := find(key); p != nil {
			p.Identities = append(p.Identities, ir)
		}
		return nil
	})
	return v, err
}

func personKey(id int64, emails []string) string {
	if id != 0 {
		return fmt.Sprint(id)
	}
	return "e:" + strings.Join(emails, ",")
}
