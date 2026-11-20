package regfront_test

import (
	"net/http"
	"strings"
	"testing"
)

const hex64 = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// TestRefused: nothing but a pull reaches the registry; the rest is answered
// 404 with a registry error body by the front itself.
func TestRefused(t *testing.T) {
	rec := &recorder{}
	addr, _ := startFront(t, startUpstream(t, rec.ServeHTTP), testLimits())
	get := func(target string) string { return "GET " + target + " HTTP/1.1\r\nHost: mirror\r\n\r\n" }
	cases := []struct {
		name string
		req  string
	}{
		// The catalog, however it is spelled.
		{"catalog", get("/v2/_catalog")},
		{"catalog with a query", get("/v2/_catalog?n=100&last=a")},
		{"catalog with a slash", get("/v2/_catalog/")},
		{"catalog after a double slash", get("//v2/_catalog")},
		{"catalog behind an empty segment", get("/v2//_catalog")},
		{"catalog with an escaped underscore", get("/v2/%5Fcatalog")},
		{"catalog with a lower-case escape", get("/v2/%5fcatalog")},
		{"catalog with an escaped letter", get("/v2/_catalo%67")},
		{"catalog with an escaped prefix", get("/%76%32/_catalog")},
		{"catalog behind a dot-dot", get("/v2/../v2/_catalog")},
		{"catalog behind an escaped dot-dot", get("/v2/%2e%2e/v2/_catalog")},
		{"catalog behind a dot", get("/v2/./_catalog")},
		{"catalog upper-case prefix", get("/V2/_catalog")},
		{"catalog upper-case", get("/v2/_CATALOG")},
		{"catalog with a NUL", get("/v2/_catalog%00")},
		{"catalog with a parameter", get("/v2/_catalog;x=1")},
		{"catalog in absolute form", get("http://127.0.0.1/v2/_catalog")},
		{"catalog as HEAD", "HEAD /v2/_catalog HTTP/1.1\r\nHost: mirror\r\n\r\n"},
		{"manifest path escaping to the catalog", get("/v2/library/alpine/manifests/latest%2F..%2F..%2F_catalog")},

		// Not the pull API.
		{"tags list", get("/v2/library/alpine/tags/list")},
		{"tags list paged", get("/v2/library/alpine/tags/list?n=10&last=3.19")},
		{"referrers", get("/v2/library/alpine/referrers/sha256:" + hex64)},
		{"upload status", get("/v2/library/alpine/blobs/uploads/0123-abcd")},
		{"upload list", get("/v2/library/alpine/blobs/uploads/")},
		{"root", get("/")},
		{"v2 without slash", get("/v2")},
		{"v1 ping", get("/v1/_ping")},
		{"debug", get("/debug/vars")},
		{"metrics", get("/metrics")},
		{"absolute form", get("http://127.0.0.1/v2/")},

		// Malformed pulls.
		{"manifest without reference", get("/v2/library/alpine/manifests/")},
		{"manifest with trailing slash", get("/v2/library/alpine/manifests/latest/")},
		{"manifest upper-case name", get("/v2/Library/alpine/manifests/latest")},
		{"manifest dot-dot name", get("/v2/library/../alpine/manifests/latest")},
		{"manifest escaped slash in name", get("/v2/library%2Falpine/manifests/latest")},
		{"manifest escaped name", get("/v2/%61lpine/manifests/latest")},
		{"manifest tag too long", get("/v2/library/alpine/manifests/" + strings.Repeat("a", 129))},
		{"manifest tag starting with a dot", get("/v2/library/alpine/manifests/.latest")},
		{"blob by tag", get("/v2/library/alpine/blobs/latest")},
		{"blob bad digest", get("/v2/library/alpine/blobs/sha256:")},
		{"blob without name", get("/v2/blobs/sha256:" + hex64)},
		{"GET with a body", "GET /v2/ HTTP/1.1\r\nHost: mirror\r\nContent-Length: 3\r\n\r\nabc"},
		{"GET with a chunked body", "GET /v2/ HTTP/1.1\r\nHost: mirror\r\nTransfer-Encoding: chunked\r\n\r\n0\r\n\r\n"},

		// Writes and other methods, on paths a GET may take.
		{"POST upload", "POST /v2/library/alpine/blobs/uploads/ HTTP/1.1\r\nHost: mirror\r\nContent-Length: 0\r\n\r\n"},
		{"PUT manifest", "PUT /v2/library/alpine/manifests/latest HTTP/1.1\r\nHost: mirror\r\nContent-Length: 2\r\n\r\n{}"},
		{"PATCH upload", "PATCH /v2/library/alpine/blobs/uploads/0123 HTTP/1.1\r\nHost: mirror\r\nContent-Length: 1\r\n\r\nx"},
		{"DELETE blob", "DELETE /v2/library/alpine/blobs/sha256:" + hex64 + " HTTP/1.1\r\nHost: mirror\r\n\r\n"},
		{"DELETE manifest", "DELETE /v2/library/alpine/manifests/sha256:" + hex64 + " HTTP/1.1\r\nHost: mirror\r\n\r\n"},
		{"OPTIONS", "OPTIONS /v2/ HTTP/1.1\r\nHost: mirror\r\n\r\n"},
		{"OPTIONS star", "OPTIONS * HTTP/1.1\r\nHost: mirror\r\n\r\n"},
		{"TRACE", "TRACE /v2/ HTTP/1.1\r\nHost: mirror\r\n\r\n"},
		{"CONNECT", "CONNECT 127.0.0.1:22 HTTP/1.1\r\nHost: 127.0.0.1:22\r\n\r\n"},
		{"lower-case get", "get /v2/ HTTP/1.1\r\nHost: mirror\r\n\r\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec.reset()
			resp, body := rawRequest(t, addr, tc.req)
			if resp.StatusCode != http.StatusNotFound {
				t.Errorf("status %d, want 404", resp.StatusCode)
			}
			if !strings.HasPrefix(tc.req, "HEAD ") {
				if code := errorCode(t, body); code != "UNSUPPORTED" {
					t.Errorf("error code %q, want UNSUPPORTED", code)
				}
			}
			if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
				t.Errorf("Content-Type %q, want application/json", ct)
			}
			if got := rec.requests(); len(got) != 0 {
				t.Errorf("the registry was asked %s %s", got[0].Method, got[0].URL.RequestURI())
			}
		})
	}
}

// TestPassed: the requests a mirror pull makes reach the registry with the
// same method and path, and no query (containerd's ns= means nothing there).
func TestPassed(t *testing.T) {
	rec := &recorder{}
	addr, _ := startFront(t, startUpstream(t, rec.ServeHTTP), testLimits())
	cases := []struct {
		method, target, want string
	}{
		{"GET", "/v2/", "/v2/"},
		{"HEAD", "/v2/", "/v2/"},
		{"GET", "/v2/?x=1", "/v2/"},
		{"GET", "/v2/library/alpine/manifests/latest", "/v2/library/alpine/manifests/latest"},
		{"HEAD", "/v2/library/alpine/manifests/3.20", "/v2/library/alpine/manifests/3.20"},
		{"GET", "/v2/library/alpine/manifests/latest?ns=docker.io", "/v2/library/alpine/manifests/latest"},
		{"GET", "/v2/library/alpine/manifests/sha256:" + hex64, "/v2/library/alpine/manifests/sha256:" + hex64},
		{"GET", "/v2/library/alpine/blobs/sha256:" + hex64, "/v2/library/alpine/blobs/sha256:" + hex64},
		{"HEAD", "/v2/library/alpine/blobs/sha256:" + hex64 + "?ns=docker.io", "/v2/library/alpine/blobs/sha256:" + hex64},
		{"GET", "/v2/my-org/sub.group/my__repo/manifests/v1.2.3-rc_1", "/v2/my-org/sub.group/my__repo/manifests/v1.2.3-rc_1"},
		{"GET", "/v2/a/b/c/d/blobs/sha512:" + hex64 + hex64, "/v2/a/b/c/d/blobs/sha512:" + hex64 + hex64},
		{"GET", "/v2/x/manifests/" + strings.Repeat("a", 128), "/v2/x/manifests/" + strings.Repeat("a", 128)},
	}
	for _, tc := range cases {
		t.Run(tc.method+" "+tc.target, func(t *testing.T) {
			rec.reset()
			resp, _ := rawRequest(t, addr, tc.method+" "+tc.target+" HTTP/1.1\r\nHost: mirror\r\n\r\n")
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status %d, want 200", resp.StatusCode)
			}
			got := rec.requests()
			if len(got) != 1 {
				t.Fatalf("the registry was asked %d times, want once", len(got))
			}
			if got[0].Method != tc.method || got[0].URL.RequestURI() != tc.want {
				t.Errorf("the registry was asked %s %s, want %s %s", got[0].Method, got[0].URL.RequestURI(), tc.method, tc.want)
			}
		})
	}
}
