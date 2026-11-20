package regfront

import (
	"net/http"
	"regexp"
)

// The grammar of the OCI distribution spec (v1.1.0, "Pulling manifests" and
// "Pulling blobs"). A path the registry would read as another route never
// matches it: no component may be empty, ".", ".." or start with "_".
const (
	pathComponent = `[a-z0-9]+(?:(?:\.|_|__|-+)[a-z0-9]+)*`
	repoName      = pathComponent + `(?:/` + pathComponent + `)*`
	tag           = `[a-zA-Z0-9_][a-zA-Z0-9._-]{0,127}`
	digest        = `[a-z0-9]+(?:[+._-][a-z0-9]+)*:[a-zA-Z0-9=_-]+`
)

// Compiled once and never changed; a *regexp.Regexp is safe for concurrent
// use.
var (
	manifestRoute = regexp.MustCompile(`^/v2/` + repoName + `/manifests/(?:` + tag + `|` + digest + `)$`)
	blobRoute     = regexp.MustCompile(`^/v2/` + repoName + `/blobs/` + digest + `$`)
)

// refusal returns why r is refused, or "" when it is one of the requests a
// mirror pull makes.
//
// The pull API is GET or HEAD of /v2/ (the API check), a manifest by tag or
// digest, and a blob by digest. tags/list is left out: dockerd asks for it
// only for `docker pull --all-tags`, and falls back to the registry itself
// when a mirror refuses it; containerd and BuildKit never ask a mirror for it.
func refusal(r *http.Request) string {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return "method"
	}
	if r.URL.Scheme != "" || r.URL.Host != "" || r.URL.Opaque != "" {
		return "not an origin-form request"
	}
	if r.ContentLength != 0 || len(r.TransferEncoding) > 0 {
		return "request body"
	}
	p := r.URL.Path
	// The path is taken only as written: an escape (%5F), checked here
	// decoded, could reach a registry router that matches it otherwise.
	if r.URL.RawPath != "" || r.URL.EscapedPath() != p {
		return "escaped path"
	}
	if p == "/v2/" || manifestRoute.MatchString(p) || blobRoute.MatchString(p) {
		return ""
	}
	return "not a pull"
}
