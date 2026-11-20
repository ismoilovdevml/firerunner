package host

import (
	"strings"
	"testing"
)

// As written by `gitlab-runner register`.
const registered = `concurrent = 4
check_interval = 0

[session_server]
  session_timeout = 1800

[[runners]]
  name = "firerunner-test"
  url = "https://gitlab.example.com"
  token = "glrt-x"
  executor = "custom"
  builds_dir = "/root/builds"
  [runners.cache]
    MaxUploadedArchiveSize = 0
    [runners.cache.s3]
    [runners.cache.gcs]
    [runners.cache.azure]
  [runners.custom]
    prepare_exec = "/usr/local/bin/firerunner"
    prepare_args = ["executor", "prepare"]
`

var testCache = &Cache{Server: "10.200.0.1:9000", Bucket: "runner-cache", AccessKey: "ak", SecretKey: `s"k`, Insecure: true}

func TestReplaceCacheSection(t *testing.T) {
	out, err := replaceCacheSection(registered, cacheSection(testCache))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "[runners.cache.gcs]") || strings.Contains(out, "MaxUploadedArchiveSize") {
		t.Errorf("old cache section left behind:\n%s", out)
	}
	for _, want := range []string{`Type = "s3"`, `ServerAddress = "10.200.0.1:9000"`, `SecretKey = "s\"k"`, "Insecure = true",
		"  [runners.custom]", `prepare_args = ["executor", "prepare"]`} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if typ, server := cacheFromConfig(out); typ != "s3" || server != "10.200.0.1:9000" {
		t.Errorf("cacheFromConfig = %q, %q", typ, server)
	}

	// Idempotent, and "off" removes the settings again.
	again, _ := replaceCacheSection(out, cacheSection(testCache))
	if again != out {
		t.Errorf("second write changed the file:\n%s", again)
	}
	off, _ := replaceCacheSection(out, cacheSection(nil))
	if typ, _ := cacheFromConfig(off); typ != "" || strings.Contains(off, "SecretKey") {
		t.Errorf("cache not disabled:\n%s", off)
	}
}

func TestEnsureRunnerEnv(t *testing.T) {
	out := ensureRunnerEnv(registered, cacheEnv...)
	if !strings.Contains(out, "[[runners]]\n  environment = [\"FF_USE_FASTZIP=true\", \"CACHE_COMPRESSION_LEVEL=fastest\"]\n  name =") {
		t.Fatalf("environment not added after [[runners]]:\n%s", out)
	}
	if again := ensureRunnerEnv(out, cacheEnv...); again != out {
		t.Fatalf("not idempotent:\n%s", again)
	}

	// An operator's own values win and other entries are kept.
	own := strings.Replace(registered, "  executor = \"custom\"\n", "  executor = \"custom\"\n  environment = [\"FF_USE_FASTZIP=false\", \"NO_PROXY=a,b\"]\n", 1)
	out = ensureRunnerEnv(own, cacheEnv...)
	if !strings.Contains(out, `environment = ["FF_USE_FASTZIP=false", "NO_PROXY=a,b", "CACHE_COMPRESSION_LEVEL=fastest"]`) || strings.Count(out, "environment") != 1 {
		t.Fatalf("existing environment not merged:\n%s", out)
	}
}

func TestReplaceCacheSectionAppendsWhenMissing(t *testing.T) {
	conf := strings.Replace(registered, "  [runners.cache]\n    MaxUploadedArchiveSize = 0\n    [runners.cache.s3]\n    [runners.cache.gcs]\n    [runners.cache.azure]\n", "", 1)
	out, err := replaceCacheSection(conf, cacheSection(testCache))
	if err != nil {
		t.Fatal(err)
	}
	if typ, _ := cacheFromConfig(out); typ != "s3" {
		t.Errorf("cache not appended:\n%s", out)
	}
}

func TestReplaceCacheSectionNeedsOneRunner(t *testing.T) {
	for _, conf := range []string{"concurrent = 1\n", registered + "\n[[runners]]\n  name = \"second\"\n"} {
		if _, err := replaceCacheSection(conf, cacheSection(testCache)); err == nil {
			t.Errorf("expected an error for:\n%s", conf)
		}
	}
}

// headersOf counts the table headers of conf by name, as TOML reads them.
func headersOf(conf string) map[string]int {
	n := map[string]int{}
	for _, s := range tomlStatements(conf) {
		if s.header {
			n[s.table]++
		}
	}
	return n
}

// Headers are found as TOML reads them: with a comment after them, with
// spaces inside the brackets, and never inside a multi-line string. A second
// [runners.cache] would be a duplicate table gitlab-runner refuses.
func TestReplaceCacheSectionTOMLHeaders(t *testing.T) {
	for _, c := range []struct{ name, conf string }{
		{"commented cache header", strings.Replace(registered, "  [runners.cache]\n", "  [runners.cache] # s3 cache\n", 1)},
		{"spaces in the cache header", strings.Replace(registered, "  [runners.cache]\n", "  [ runners.cache ]\n", 1)},
		{"commented runners header", strings.Replace(registered, "[[runners]]\n", "[[runners]] # main\n", 1)},
		{"[[runners]] inside a multi-line string", strings.Replace(registered, "  builds_dir =",
			"  pre_build_script = \"\"\"\n[[runners]]\n[runners.cache]\n\"\"\"\n  builds_dir =", 1)},
	} {
		t.Run(c.name, func(t *testing.T) {
			out, err := replaceCacheSection(c.conf, cacheSection(testCache))
			if err != nil {
				t.Fatal(err)
			}
			h := headersOf(out)
			if h["[[runners]]"] != 1 || h["[runners.cache]"] != 1 || h["[runners.cache.s3]"] != 1 || h["[runners.cache.gcs]"] != 0 || h["[runners.custom]"] != 1 {
				t.Fatalf("headers %v in:\n%s", h, out)
			}
			if typ, server := cacheFromConfig(out); typ != "s3" || server != "10.200.0.1:9000" {
				t.Fatalf("cacheFromConfig = %q, %q in:\n%s", typ, server, out)
			}
			if again, _ := replaceCacheSection(out, cacheSection(testCache)); again != out {
				t.Fatalf("second write changed the file:\n%s", again)
			}
		})
	}
}

// Text around the cache tables, comments included, is kept byte for byte.
func TestReplaceCacheSectionKeepsTheRest(t *testing.T) {
	conf := "# top\nconcurrent = 4\n\n[[runners]] # main\n  name = \"a\"\n  [runners.cache] # old\n    Type = \"gcs\"\n    [runners.cache.gcs]\n      BucketName = \"x\"\n\n  # the executor\n  [runners.custom]\n    run_exec = \"/x\"\n"
	out, err := replaceCacheSection(conf, cacheSection(nil))
	if err != nil {
		t.Fatal(err)
	}
	want := "# top\nconcurrent = 4\n\n[[runners]] # main\n  name = \"a\"\n  [runners.cache]\n\n  # the executor\n  [runners.custom]\n    run_exec = \"/x\"\n"
	if out != want {
		t.Fatalf("got:\n%s\nwant:\n%s", out, want)
	}
}

// An environment that already has every entry is left as the operator wrote
// it: install.sh runs SetCache on every run.
func TestEnsureRunnerEnvLeavesACompleteList(t *testing.T) {
	conf := strings.Replace(registered, "  executor = \"custom\"\n", "  executor = \"custom\"\n  environment = [\n    \"FF_USE_FASTZIP=false\", # slower but checked\n    \"CACHE_COMPRESSION_LEVEL=fast\",\n  ]\n", 1)
	if out := ensureRunnerEnv(conf, cacheEnv...); out != conf {
		t.Fatalf("rewritten:\n%s", out)
	}
}

// Existing entries are kept as written, whatever escapes they use; only the
// added ones are quoted, with escapes TOML accepts.
func TestEnsureRunnerEnvKeepsEntriesAsWritten(t *testing.T) {
	conf := strings.Replace(registered, "  executor = \"custom\"\n", "  executor = \"custom\"\n  environment = [\"ESC=\\e[0m\", 'WIN=C:\\x', \"TAB=a\\tb\"]\n", 1)
	out := ensureRunnerEnv(conf, "BELL=a\a\vb", "FF_USE_FASTZIP=true")
	want := `environment = ["ESC=\e[0m", 'WIN=C:\x', "TAB=a\tb", "BELL=a\u0007\u000Bb", "FF_USE_FASTZIP=true"]`
	if !strings.Contains(out, want) {
		t.Fatalf("want %s in:\n%s", want, out)
	}
}

func TestTOMLQuote(t *testing.T) {
	for in, want := range map[string]string{
		"plain":          `"plain"`,
		`q"b\s`:          `"q\"b\\s"`,
		"a\a\vb\x00\x7f": `"a\u0007\u000Bb\u0000\u007F"`,
		"t\tn\nr\r":      `"t\tn\nr\r"`,
		"ünï":            `"ünï"`,
	} {
		got := tomlQuote(in)
		if got != want {
			t.Errorf("tomlQuote(%q) = %s, want %s", in, got, want)
		}
		if back, ok := tomlUnquote(got); !ok || back != in {
			t.Errorf("tomlUnquote(%s) = %q, %v; want %q", got, back, ok, in)
		}
	}
	for in, want := range map[string]string{`"ESC=\e[0m"`: "ESC=\x1b[0m", `"\U0001F600"`: "\U0001F600", `"x\u00e9"`: "xé"} {
		if got, ok := tomlUnquote(in); !ok || got != want {
			t.Errorf("tomlUnquote(%s) = %q, %v; want %q", in, got, ok, want)
		}
	}
	// Read: TOML 1.0 escapes plus TOML 1.1's \e and \xHH; Go-only ones are refused.
	for in, want := range map[string]string{`"\x41"`: "A", `'C:\x'`: `C:\x`} {
		if got, ok := tomlUnquote(in); !ok || got != want {
			t.Errorf("tomlUnquote(%s) = %q, %v; want %q", in, got, ok, want)
		}
	}
	for _, bad := range []string{`"\a"`, `"\v"`, `"\q"`, `"a`, `"\u12"`, `"a"b"`} {
		if got, ok := tomlUnquote(bad); ok {
			t.Errorf("tomlUnquote(%s) = %q, want not ok", bad, got)
		}
	}
}
