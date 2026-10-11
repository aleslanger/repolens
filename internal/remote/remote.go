// Package remote normalises git remote URLs to host/owner/name, dropping credentials.
package remote

import (
	"net/url"
	"regexp"
	"strings"
)

// validPart is what GitHub and similar hosts allow in owner and repository names.
var validPart = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,100}$`)

func validName(s string) bool { return validPart.MatchString(s) && s != "." && s != ".." }

type Remote struct {
	Host, Owner, Name string
}

// Key is the stable identifier "host/owner/name".
func (r Remote) Key() string { return r.Host + "/" + r.Owner + "/" + r.Name }

// Parse understands scp-like (git@host:o/n) and URL forms. Case is folded because
// GitHub owner and repository names are case-insensitive.
func Parse(raw string) (Remote, bool) {
	raw = strings.TrimSpace(raw)
	var host, path string
	switch {
	case strings.Contains(raw, "://"):
		u, err := url.Parse(raw)
		if err != nil || u.Host == "" {
			return Remote{}, false
		}
		host, path = u.Hostname(), u.Path
	case strings.Contains(raw, ":") && !strings.HasPrefix(raw, "/"):
		left, right, _ := strings.Cut(raw, ":")
		if at := strings.LastIndex(left, "@"); at >= 0 {
			left = left[at+1:]
		}
		host, path = left, right
	default:
		return Remote{}, false
	}
	path = strings.TrimSuffix(strings.Trim(path, "/"), ".git")
	parts := strings.Split(path, "/")
	if host == "" || len(parts) != 2 || !validName(parts[0]) || !validName(parts[1]) {
		return Remote{}, false
	}
	return Remote{
		Host:  strings.ToLower(host),
		Owner: strings.ToLower(parts[0]),
		Name:  strings.ToLower(parts[1]),
	}, true
}
