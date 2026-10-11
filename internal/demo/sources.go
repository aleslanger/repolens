package demo

import (
	"fmt"
	"time"
)

var labels = []string{"bug", "enhancement", "documentation", "good first issue", "performance"}

// github writes issues, PRs, milestones, labels, releases, languages and CI runs.
func (g *gen) github(id int64, r repoSpec) error {
	for _, l := range labels {
		if err := g.exec(`INSERT INTO gh_label(repo_id,name) VALUES(?,?)`, id, l); err != nil {
			return err
		}
	}
	if err := g.exec(`INSERT INTO gh_language(repo_id,lang,bytes) VALUES(?,?,?),(?,?,?)`,
		id, r.lang, 200000+g.rng.IntN(400000), id, "Shell", 4000+g.rng.IntN(9000)); err != nil {
		return err
	}
	for m := 1; m <= 3; m++ {
		due := g.now.Add(time.Duration(m*30-60) * day).Unix()
		state := "OPEN"
		if m == 1 {
			state = "CLOSED"
		}
		if err := g.exec(`INSERT INTO gh_milestone(repo_id,number,title,state,due_on,open_issues,closed_issues) VALUES(?,?,?,?,?,?,?)`,
			id, m, fmt.Sprintf("v0.%d", m+1), state, due, g.rng.IntN(8)*(m-1), 3+g.rng.IntN(9)); err != nil {
			return err
		}
	}
	for v := 1; v <= 4; v++ {
		pub := g.start.Add(time.Duration(v*45) * day)
		if pub.After(g.now) || (r.archive && v > 1) {
			break
		}
		if err := g.exec(`INSERT INTO gh_release(repo_id,tag,name,published_at,prerelease) VALUES(?,?,?,?,?)`,
			id, fmt.Sprintf("v0.%d.0", v), fmt.Sprintf("%s 0.%d", r.name, v), pub.Unix(), b2i(v == 4)); err != nil {
			return err
		}
	}
	if err := g.issues(id, r); err != nil {
		return err
	}
	return g.ciRuns(id, r)
}

func (g *gen) issues(id int64, r repoSpec) error {
	number := 0
	for d := g.start; d.Before(g.now); d = d.Add(day) {
		for range g.poisson(r.activity * 0.35 * growth(d, g.start, g.now)) {
			number++
			isPR := g.rng.IntN(2) == 0
			created := d.Add(time.Duration(g.rng.IntN(20)) * time.Hour)
			closed := created.Add(time.Duration(2+g.rng.IntN(200)) * time.Hour)
			state, closedAt, mergedAt := "CLOSED", any(closed.Unix()), any(nil)
			if closed.After(g.now) || g.rng.IntN(6) == 0 {
				state, closedAt = "OPEN", nil
			} else if isPR {
				state, mergedAt = "MERGED", closed.Unix()
			}
			who := []string{"octo", "mona", "hubot", "dependabot"}[g.rng.IntN(4)]
			err := g.exec(`INSERT INTO gh_issue(repo_id,number,is_pr,state,title,author,created_at,closed_at,merged_at,milestone,labels)
				VALUES(?,?,?,?,?,?,?,?,?,?,?)`, id, number, b2i(isPR), state, "Improve "+subjects[g.rng.IntN(len(subjects))], who,
				created.Unix(), closedAt, mergedAt, fmt.Sprintf("v0.%d", 2+g.rng.IntN(3)), labels[g.rng.IntN(len(labels))])
			if err != nil {
				return err
			}
		}
	}
	return g.exec(`UPDATE repo SET open_issues=(SELECT COUNT(*) FROM gh_issue WHERE repo_id=? AND is_pr=0 AND state='OPEN'),
		open_prs=(SELECT COUNT(*) FROM gh_issue WHERE repo_id=? AND is_pr=1 AND state='OPEN') WHERE id=?`, id, id, id)
}

func (g *gen) ciRuns(id int64, r repoSpec) error {
	for d := g.now.Add(-90 * day); d.Before(g.now); d = d.Add(day) {
		if r.archive {
			break
		}
		for range g.poisson(r.activity * 0.8) {
			conclusion := "success"
			switch x := g.rng.IntN(100); {
			case x < 12:
				conclusion = "failure"
			case x < 15:
				conclusion = "cancelled"
			}
			wf := []string{"CI", "CI", "Release", "CodeQL"}[g.rng.IntN(4)]
			err := g.exec(`INSERT INTO gh_run(repo_id,id,workflow,event,branch,status,conclusion,created_at,duration_s) VALUES(?,?,?,?,?,?,?,?,?)`,
				id, g.next(), wf, "push", "main", "completed", conclusion, d.Add(time.Duration(g.rng.IntN(86400))*time.Second).Unix(), 60+g.rng.IntN(600))
			if err != nil {
				return err
			}
		}
	}
	return nil
}

// runs records collection history and a couple of data-quality entries.
func (g *gen) runs() error {
	for i := 3; i >= 0; i-- {
		start := g.now.Add(-time.Duration(i) * day).Add(-2 * time.Hour)
		err := g.exec(`INSERT INTO collect_run(mode,target,started_at,finished_at,repos,skipped,commits,sessions,unrecognized,errors)
			VALUES('all','/home/octo/code, octo',?,?,7,2,?,?,0,?)`, start.Unix(), start.Add(70*time.Second).Unix(), 5+g.rng.IntN(40), 3+g.rng.IntN(20), b2i(i == 0))
		if err != nil {
			return err
		}
	}
	// The latest run met an unreadable directory, as a real one often does (database volumes).
	if err := g.exec(`INSERT INTO collect_error(run_id,target,message) VALUES((SELECT MAX(id) FROM collect_run),
		'/home/octo/code/infra/data/postgres','open /home/octo/code/infra/data/postgres: permission denied')`); err != nil {
		return err
	}
	return g.exec(`INSERT INTO parse_issue(source,scope,kind,what,count) VALUES
		('claude-jsonl','demo','unrecognized','attachment:future_type',2), ('git-local','/home/octo/code/notes','error','permission denied',1)`)
}
