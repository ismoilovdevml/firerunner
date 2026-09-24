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
