package host

import (
	"strings"
	"testing"
)

// An operator-edited config.toml: comments everywhere, a multi-line
// environment array, and keys named like the runner's in other tables.
const edited = `# managed by firerunner
concurrent = 4 # max jobs at once
check_interval = 0

[session_server]
  session_timeout = 1800

[[runners]]
  name = "firerunner-a" # shown in GitLab
  url = "https://gitlab.example.com" # the main instance
  token = "glrt-x"
  executor = "custom"
  environment = [
    "FF_USE_FASTZIP=false", # "CACHE_COMPRESSION_LEVEL=slow" is only a comment
    'NO_PROXY=a,b',
  ]
  [runners.cache]
    Type = "s3" # versitygw on this host
    [runners.cache.s3]
      ServerAddress = "10.200.0.1:9000" # bridge
  [runners.autoscaler]
    concurrent_limit = 2
  [[runners.docker.services]]
    name = "postgres"
    url = "http://not-gitlab"
`

func TestParseRunner(t *testing.T) {
	r, err := parseRunner(edited)
	if err != nil {
		t.Fatal(err)
	}
	want := Runner{Name: "firerunner-a", URL: "https://gitlab.example.com", Executor: "custom", Concurrent: 4,
		CacheType: "s3", CacheServer: "10.200.0.1:9000"}
	if *r != want {
		t.Fatalf("parseRunner =\n %+v, want\n %+v", *r, want)
	}
	for _, conf := range []string{"", "concurrent = 4\n", "concurrent = 4\n# url = \"https://old\"\n[[runners]]\n  name = \"x\"\n"} {
		if r, err := parseRunner(conf); err == nil {
			t.Errorf("parseRunner(%q) = %+v, want no runner", conf, r)
		}
	}
}

func TestSetConcurrent(t *testing.T) {
	for _, c := range []struct{ name, in, want string }{
		{"only the root key, comment kept", edited,
			strings.Replace(edited, "concurrent = 4 # max", "concurrent = 7 # max", 1)},
		{"missing: added on top", "check_interval = 0\n[[runners]]\n  concurrent_limit = 2\n",
			"concurrent = 7\ncheck_interval = 0\n[[runners]]\n  concurrent_limit = 2\n"},
		{"after a comment", "# concurrent = 1\nconcurrent=3\n", "# concurrent = 1\nconcurrent=7\n"},
		{"at the end without a newline", "check_interval = 0\nconcurrent = 3", "check_interval = 0\nconcurrent = 7"},
		{"in a multi-line string is no key", "motd = \"\"\"\nconcurrent = 1\n\"\"\"\nconcurrent = 3\n", "motd = \"\"\"\nconcurrent = 1\n\"\"\"\nconcurrent = 7\n"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := setConcurrent(c.in, 7); got != c.want {
				t.Fatalf("setConcurrent =\n%s\nwant\n%s", got, c.want)
			}
		})
	}
}

// A multi-line environment array is replaced whole, and only that: the rest
// of the file stays byte for byte.
func TestEnsureRunnerEnvMultiLine(t *testing.T) {
	got := ensureRunnerEnv(edited, cacheEnv...)
	const oldEnv = "environment = [\n    \"FF_USE_FASTZIP=false\", # \"CACHE_COMPRESSION_LEVEL=slow\" is only a comment\n    'NO_PROXY=a,b',\n  ]"
	want := strings.Replace(edited, oldEnv,
		`environment = ["FF_USE_FASTZIP=false", "NO_PROXY=a,b", "CACHE_COMPRESSION_LEVEL=fastest"]`, 1)
	if !strings.Contains(edited, oldEnv) || got != want {
		t.Fatalf("ensureRunnerEnv =\n%s\nwant\n%s", got, want)
	}
	if again := ensureRunnerEnv(got, cacheEnv...); again != got {
		t.Fatalf("not idempotent:\n%s", again)
	}
}

func TestTOMLStatements(t *testing.T) {
	doc := "a = \"x # y\" # c\n[t] # h\nb = [\n 1, # one\n [2, 3],\n]\nc = \"\"\"\nline = 1\n[not.a.table]\n\"\"\"\n[[ arr . sub ]]\nd = 'lit # x'\n"
	var got []string
	for _, s := range tomlStatements(doc) {
		if s.header {
			got = append(got, s.table)
			continue
		}
		got = append(got, s.table+" "+s.key+"="+s.value+" @"+doc[s.start:s.end])
	}
	want := []string{
		` a="x # y" @a = "x # y"`,
		`[t]`,
		"[t] b=[\n 1, \n [2, 3],\n] @b = [\n 1, # one\n [2, 3],\n]",
		"[t] c=\"\"\"\nline = 1\n[not.a.table]\n\"\"\" @c = \"\"\"\nline = 1\n[not.a.table]\n\"\"\"",
		`[[arr.sub]]`,
		`[[arr.sub]] d='lit # x' @d = 'lit # x'`,
	}
	if strings.Join(got, "\n---\n") != strings.Join(want, "\n---\n") {
		t.Fatalf("statements:\n%s\n\nwant:\n%s", strings.Join(got, "\n---\n"), strings.Join(want, "\n---\n"))
	}
}
