package github

import (
	"fmt"
	"net/http"
	"strconv"
	"testing"
	"time"
)

// An exhausted rate limit makes the client wait until the reset before the next request, and a
// reset beyond maxWait is an error instead of a silent hour-long hang.
func TestExhaustedLimitWaitsForReset(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	reset := now.Add(30 * time.Second)
	c, slept := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Ratelimit-Remaining", "0")
		w.Header().Set("X-Ratelimit-Reset", strconv.FormatInt(reset.Unix(), 10))
		w.Write([]byte(`{"login":"octo"}`))
	})
	c.now = func() time.Time { return now }
	var out struct{ Login string }
	if _, err := c.GetJSON(t.Context(), "/user", &out); err != nil {
		t.Fatal(err)
	}
	if len(*slept) != 1 || (*slept)[0] != 31*time.Second {
		t.Errorf("slept %v, want 31s (reset + 1s)", *slept)
	}
	reset = now.Add(2 * maxWait)
	if _, err := c.GetJSON(t.Context(), "/user2", &out); err == nil {
		t.Error("a reset beyond maxWait must fail")
	}
}

func TestListOwnerReposPagesAndUnknownOwner(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		vars := graphqlVars(t, r)
		switch {
		case vars["login"] == "ghost":
			w.Write([]byte(`{"data":{"repositoryOwner":null}}`))
		case vars["after"] == nil:
			w.Write([]byte(`{"data":{"repositoryOwner":{"repositories":{"pageInfo":{"hasNextPage":true,"endCursor":"c1"},"nodes":[{"nameWithOwner":"octo/a"}]}}}}`))
		case vars["after"] == "c1":
			w.Write([]byte(`{"data":{"repositoryOwner":{"repositories":{"pageInfo":{"hasNextPage":false},"nodes":[{"nameWithOwner":"octo/b","isFork":true}]}}}}`))
		default:
			http.Error(w, fmt.Sprint("unexpected cursor ", vars["after"]), http.StatusBadRequest)
		}
	})
	repos, err := c.ListOwnerRepos(t.Context(), "octo")
	if err != nil {
		t.Fatal(err)
	}
	if len(repos) != 2 || repos[1].Ref() != (Ref{Owner: "octo", Name: "b"}) || !repos[1].IsFork {
		t.Errorf("repos = %+v", repos)
	}
	if _, err := c.ListOwnerRepos(t.Context(), "ghost"); err == nil {
		t.Error("an unknown owner must be an error")
	}
}
