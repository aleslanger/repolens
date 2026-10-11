package collect

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"repolens/internal/config"
	"repolens/internal/github"
)

// A repository marked as cloned whose clone is gone is treated as GitHub-only again, so its
// history is read from the API instead of staying frozen.
func TestAccountTargetsNoticeDeletedClones(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"data":{"repositoryOwner":{"repositories":{"pageInfo":{"hasNextPage":false},"nodes":[{"nameWithOwner":"octo/gone"}]}}}}`))
	}))
	defer srv.Close()
	st := newStore(t)
	if _, err := st.DB.Exec(`INSERT INTO repo(key,name,host,owner,local_path,in_local) VALUES('github.com/octo/gone','gone','github.com','octo','/nowhere/gone',1)`); err != nil {
		t.Fatal(err)
	}
	gh := github.New("t", nil, io.Discard).WithBase(srv.URL)
	targets, err := accountTargets(context.Background(), st, config.Config{}, gh, "octo", io.Discard, &Summary{})
	if err != nil {
		t.Fatal(err)
	}
	if len(targets) != 1 || targets[0].InLocal {
		t.Errorf("targets = %+v, want one target that is no longer local", targets)
	}
}
