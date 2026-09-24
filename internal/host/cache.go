package host

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
)

// CacheEnv holds the settings of the host's S3 cache server (versitygw),
// written by install.sh and read by its systemd unit.
const CacheEnv = "/etc/firerunner/cache.env"

// Cache is the S3 store gitlab-runner uses for `cache:`. The runner hands jobs
// presigned URLs only, so the keys never enter a job VM and a job can reach
// nothing but its own project's cache objects.
type Cache struct {
	Server    string // host:port
	Bucket    string
	AccessKey string
	SecretKey string
	Insecure  bool // plain HTTP
}

// LocalCache returns the cache server that install.sh set up on this host.
func LocalCache() (*Cache, error) {
	f, err := os.Open(CacheEnv)
	if err != nil {
		return nil, fmt.Errorf("no local cache server (%w); re-run install.sh", err)
	}
	defer f.Close()
	env := map[string]string{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if k, v, ok := strings.Cut(strings.TrimSpace(sc.Text()), "="); ok && !strings.HasPrefix(k, "#") {
			env[k] = v
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	c := &Cache{
		Server:    env["VGW_PORT"],
		Bucket:    env["FR_CACHE_BUCKET"],
		AccessKey: env["ROOT_ACCESS_KEY_ID"],
		SecretKey: env["ROOT_SECRET_ACCESS_KEY"],
		Insecure:  true,
	}
	if c.Server == "" || c.Bucket == "" || c.AccessKey == "" || c.SecretKey == "" {
		return nil, fmt.Errorf("%s is incomplete; re-run install.sh", CacheEnv)
	}
	return c, nil
}

// SetCache writes the [runners.cache] section of config.toml; nil disables the
// shared cache (cache: then only lives inside the job VM).
func SetCache(c *Cache) error {
	if c != nil && (c.Server == "" || c.Bucket == "" || c.AccessKey == "" || c.SecretKey == "") {
		return errors.New("cache needs a server, a bucket and both keys")
	}
	b, err := os.ReadFile(RunnerConfig)
	if err != nil {
		return err
	}
	out, err := replaceCacheSection(string(b), cacheSection(c))
	if err != nil {
		return err
	}
	if c != nil {
		out = ensureRunnerEnv(out, cacheEnv...)
	}
	return os.WriteFile(RunnerConfig, []byte(out), 0o600)
}

// cacheEnv makes gitlab-runner zip and unzip cache: with its fast archiver
// (measured: restore 8s -> 5s, upload 5s -> 3s for a 367 MB BuildKit cache).
var cacheEnv = []string{"FF_USE_FASTZIP=true", "CACHE_COMPRESSION_LEVEL=fastest"}

// tomlString matches one TOML basic string ("..." with backslash escapes).
var tomlString = regexp.MustCompile(`"(?:[^"\\]|\\.)*"`)

// ensureRunnerEnv adds entries to the runner's `environment` list unless a
// value for that variable is already set there.
func ensureRunnerEnv(conf string, entries ...string) string {
	lines := strings.Split(conf, "\n")
	runner, envLine := -1, -1
	for i, l := range lines {
		t := strings.TrimSpace(l)
		if t == "[[runners]]" {
			runner = i
		}
		if runner >= 0 && strings.HasPrefix(t, "environment") && strings.Contains(t, "=") {
			envLine = i
			break
		}
		if runner >= 0 && i > runner && strings.HasPrefix(t, "[") {
			break
		}
	}
	if runner < 0 {
		return conf
	}
	var have []string
	if envLine >= 0 {
		_, list, _ := strings.Cut(lines[envLine], "=")
		for _, q := range tomlString.FindAllString(list, -1) {
			if v, err := strconv.Unquote(q); err == nil {
				have = append(have, v)
			}
		}
	}
	set := func(name string) bool {
		for _, h := range have {
			if strings.HasPrefix(h, name+"=") {
				return true
			}
		}
		return false
	}
	for _, e := range entries {
		if name, _, _ := strings.Cut(e, "="); !set(name) {
			have = append(have, e)
		}
	}
	quoted := make([]string, len(have))
	for i, h := range have {
		quoted[i] = strconv.Quote(h)
	}
	line := "  environment = [" + strings.Join(quoted, ", ") + "]"
	if envLine >= 0 {
		lines[envLine] = line
	} else {
		lines = append(lines[:runner+1], append([]string{line}, lines[runner+1:]...)...)
	}
	return strings.Join(lines, "\n")
}

func cacheSection(c *Cache) []string {
	if c == nil {
		return []string{"  [runners.cache]"}
	}
	return []string{
		"  [runners.cache]",
		`    Type = "s3"`,
		"    Shared = false",
		"    [runners.cache.s3]",
		"      ServerAddress = " + strconv.Quote(c.Server),
		"      AccessKey = " + strconv.Quote(c.AccessKey),
		"      SecretKey = " + strconv.Quote(c.SecretKey),
		"      BucketName = " + strconv.Quote(c.Bucket),
		"      Insecure = " + strconv.FormatBool(c.Insecure),
	}
}

// replaceCacheSection swaps the [runners.cache] table (with its sub-tables) of
// the single [[runners]] entry for section, or appends section to that entry.
func replaceCacheSection(conf string, section []string) (string, error) {
	lines := strings.Split(strings.TrimRight(conf, "\n"), "\n")
	runners := 0
	for _, l := range lines {
		if strings.TrimSpace(l) == "[[runners]]" {
			runners++
		}
	}
	if runners != 1 {
		return "", fmt.Errorf("%s has %d [[runners]] entries, expected 1", RunnerConfig, runners)
	}
	start, end := -1, len(lines)
	for i, l := range lines {
		t := strings.TrimSpace(l)
		if start < 0 {
			if t == "[runners.cache]" {
				start = i
			}
			continue
		}
		if strings.HasPrefix(t, "[") && !strings.HasPrefix(t, "[runners.cache.") {
			end = i
			break
		}
	}
	var out []string
	if start < 0 {
		out = append(append(out, lines...), section...)
	} else {
		out = append(append(append(out, lines[:start]...), section...), lines[end:]...)
	}
	return strings.Join(out, "\n") + "\n", nil
}

// cacheFromConfig reports the cache type and server configured in config.toml.
func cacheFromConfig(conf string) (typ, server string) {
	table := ""
	for _, l := range strings.Split(conf, "\n") {
		t := strings.TrimSpace(l)
		if strings.HasPrefix(t, "[") {
			table = t
			continue
		}
		k, v, ok := strings.Cut(t, "=")
		if !ok {
			continue
		}
		k, v = strings.TrimSpace(k), strings.Trim(strings.TrimSpace(v), `"`)
		switch {
		case table == "[runners.cache]" && k == "Type":
			typ = v
		case table == "[runners.cache.s3]" && k == "ServerAddress":
			server = v
		}
	}
	return typ, server
}
