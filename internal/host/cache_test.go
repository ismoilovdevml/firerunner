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
