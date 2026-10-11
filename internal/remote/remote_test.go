package remote

import "testing"

func TestParse(t *testing.T) {
	cases := []struct {
		in   string
		want Remote
		ok   bool
	}{
		{"git@github.com:octo/repolens.git", Remote{"github.com", "octo", "repolens"}, true},
		{"https://github.com/octo/repolens", Remote{"github.com", "octo", "repolens"}, true},
		{"https://github.com/octo/repolens.git/", Remote{"github.com", "octo", "repolens"}, true},
		{"ssh://git@github.com/octo/repolens.git", Remote{"github.com", "octo", "repolens"}, true},
		{"https://x-access-token:ghp_secret@github.com/Org/Repo.git", Remote{"github.com", "org", "repo"}, true},
		{"git@bitbucket.org:OCTO_99/thing.git", Remote{"bitbucket.org", "octo_99", "thing"}, true},
		{"http://github.com/GoAdminGroup/go-admin.git", Remote{"github.com", "goadmingroup", "go-admin"}, true},
		{"/local/path/repo", Remote{}, false},
		{"", Remote{}, false},
		{"https://github.com/onlyowner", Remote{}, false},
		{"https://github.com/a/b%3Fx", Remote{}, false},
		{"https://github.com/../x", Remote{}, false},
	}
	for _, tc := range cases {
		got, ok := Parse(tc.in)
		if ok != tc.ok || got != tc.want {
			t.Errorf("Parse(%q) = %+v,%v want %+v,%v", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}

func TestKeyNeverContainsCredentials(t *testing.T) {
	r, _ := Parse("https://user:tok@github.com/a/b.git")
	if r.Key() != "github.com/a/b" {
		t.Errorf("Key() = %q", r.Key())
	}
}
