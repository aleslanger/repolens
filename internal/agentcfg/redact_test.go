package agentcfg

import (
	"strings"
	"testing"
)

func TestRedactRule(t *testing.T) {
	cases := map[string]string{
		`Bash(PGPASSWORD=app psql:*)`: `Bash(PGPASSWORD=*** psql:*)`,
		`Bash(GOWORK=off PG_URL="postgres://app:s3cret@localhost:15433/db?sslmode=disable" go run:*)`: `Bash(GOWORK=*** PG_URL=*** go run:*)`,
		`Bash(curl https://user:hunter2@example.com/x:*)`:                                             `Bash(curl …:*)`,
		`Bash(tool --token abc123:*)`:                                                                 `Bash(tool …:*)`,
		`Bash(curl -H "Authorization: Bearer ghp_abc123" https://x:*)`:                                `Bash(curl …:*)`,
		`Bash(curl -u admin:hunter2 https://x:*)`:                                                     `Bash(curl …:*)`,
		`Bash(mysql -phunter2 -u root:*)`:                                                             `Bash(mysql …:*)`,
		`Bash(sshpass -p hunter2 ssh h:*)`:                                                            `Bash(sshpass …:*)`,
		`Bash(aws --secret-access-key AKIAABCDEFGHIJKLMNOP:*)`:                                        `Bash(aws …:*)`,
		`Bash(git push:*)`:                      `Bash(git push:*)`,
		`Bash(go test ./...)`:                   `Bash(go test …)`,
		`Bash(npm run build)`:                   `Bash(npm run build)`, // build is a known script name, not a value
		`Read(./.env)`:                          `Read(./.env)`,
		`WebFetch(domain:github.com)`:           `WebFetch(domain:github.com)`,
		`WebFetch(https://u:pw123@example.com)`: `WebFetch(https://u:***@example.com)`,
		`mcp__x__call(ghp_abcdefghijklmnopqrstuvwxyz0123456789)`: `mcp__x__call(***)`,
		`Write|Edit`: `Write|Edit`,
		// Audit findings: lower-case secrets in the first words, query secrets, newlines.
		`Bash(python3 -m pytest:*)`:                    `Bash(python3 …:*)`, // -m takes a value, so it is cut
		`Bash(mysql s3cr3tpw:*)`:                       `Bash(mysql …:*)`,
		`Bash(curl a1b2c3d4e5f6a7b8:*)`:                `Bash(curl …:*)`,
		`Bash(git push --force:*)`:                     `Bash(git push --force:*)`,
		`Bash(git push --tags:*)`:                      `Bash(git push --tags:*)`,
		`Bash(mysql -psecret -u root:*)`:               `Bash(mysql …:*)`,
		"Bash(echo ok\nexport TOKEN=abc:*)":            `Bash(echo …:*)`,
		`WebFetch(https://h.example/x?api_key=abc123)`: `WebFetch(https://h.example/x)`,
	}
	for in, want := range cases {
		if got := RedactRule(in); got != want {
			t.Errorf("RedactRule(%q) = %q, want %q", in, got, want)
		}
	}
	for _, secret := range []string{"app psql", "s3cret", "hunter2", "abc123", "pw123", "ghp_", "AKIA", "admin", "s3cr3tpw", "a1b2c3", "psecret", "TOKEN=abc"} {
		for in := range cases {
			if strings.Contains(in, secret) && strings.Contains(RedactRule(in), secret) {
				t.Errorf("secret %q leaked from %q -> %q", secret, in, RedactRule(in))
			}
		}
	}
}
