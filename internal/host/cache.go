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
	return writeRootOnly(RunnerConfig, []byte(out))
}

// cacheEnv makes gitlab-runner zip and unzip cache: with its fast archiver
// (measured: restore 8s -> 5s, upload 5s -> 3s for a 367 MB BuildKit cache).
var cacheEnv = []string{"FF_USE_FASTZIP=true", "CACHE_COMPRESSION_LEVEL=fastest"}

// tomlString matches one TOML basic ("..." with backslash escapes) or literal
// ('...') string.
var tomlString = regexp.MustCompile(`"(?:[^"\\]|\\.)*"|'[^']*'`)

// ensureRunnerEnv adds entries to the runner's `environment` list unless a
// value for that variable is already set there. With nothing to add conf is
// returned as it is. Otherwise only the list's value is rewritten, on one
// line however many it spanned: the entries already there exactly as written,
// then the new ones. The rest of conf is kept.
func ensureRunnerEnv(conf string, entries ...string) string {
	var runner, env *tomlStmt
	stmts := tomlStatements(conf)
	for i := range stmts {
		s := &stmts[i]
		if runner == nil {
			if s.header && s.table == "[[runners]]" {
				runner = s
			}
			continue
		}
		if s.tableN != runner.tableN {
			break
		}
		if s.key == "environment" {
			env = s
			break
		}
	}
	if runner == nil {
		return conf
	}
	var tokens []string // the list's entries as written
	set := map[string]bool{}
	if env != nil {
		// The value without its comments: a quoted name in a comment is no entry.
		for _, q := range tomlString.FindAllString(env.value, -1) {
			tokens = append(tokens, q)
			v, ok := tomlUnquote(q)
			if !ok {
				v = q[1 : len(q)-1] // kept as written; its name is still plain text
			}
			name, _, _ := strings.Cut(v, "=")
			set[name] = true
		}
	}
	added := false
	for _, e := range entries {
		if name, _, _ := strings.Cut(e, "="); !set[name] {
			tokens = append(tokens, tomlQuote(e))
			set[name], added = true, true
		}
	}
	if !added {
		return conf
	}
	list := "[" + strings.Join(tokens, ", ") + "]"
	if env != nil {
		return conf[:env.valStart] + list + conf[env.end:]
	}
	// A new line right after the [[runners]] header's line.
	at := lineEnd(conf, runner.end)
	if at == len(conf) {
		return conf + "\n  environment = " + list
	}
	return conf[:at+1] + "  environment = " + list + "\n" + conf[at+1:]
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
		"      ServerAddress = " + tomlQuote(c.Server),
		"      AccessKey = " + tomlQuote(c.AccessKey),
		"      SecretKey = " + tomlQuote(c.SecretKey),
		"      BucketName = " + tomlQuote(c.Bucket),
		"      Insecure = " + strconv.FormatBool(c.Insecure),
	}
}

// replaceCacheSection swaps the [runners.cache] table (with its sub-tables) of
// the single [[runners]] entry for section, or appends section to that entry.
// Headers are found as TOML reads them (a comment after one, spaces inside
// the brackets, none inside a string); the lines from the [runners.cache]
// header to its tables' last statement are replaced, everything else is kept.
func replaceCacheSection(conf string, section []string) (string, error) {
	stmts := tomlStatements(conf)
	runners := 0
	for _, s := range stmts {
		if s.header && s.table == "[[runners]]" {
			runners++
		}
	}
	if runners != 1 {
		return "", fmt.Errorf("%s has %d [[runners]] entries, expected 1", RunnerConfig, runners)
	}
	start, end := -1, -1
	for _, s := range stmts {
		if start < 0 {
			if s.header && s.table == "[runners.cache]" {
				start, end = strings.LastIndexByte(conf[:s.start], '\n')+1, s.end
			}
			continue
		}
		if s.header && !strings.HasPrefix(s.table, "[runners.cache.") && !strings.HasPrefix(s.table, "[[runners.cache.") {
			break
		}
		end = s.end
	}
	text := strings.Join(section, "\n") + "\n"
	if start < 0 {
		return strings.TrimRight(conf, "\n") + "\n" + text, nil
	}
	after := lineEnd(conf, end)
	if after < len(conf) {
		after++ // and its newline
	}
	return conf[:start] + text + conf[after:], nil
}

// cacheFromConfig reports the cache type and server configured in config.toml.
func cacheFromConfig(conf string) (typ, server string) {
	for _, s := range tomlStatements(conf) {
		switch {
		case s.table == "[runners.cache]" && s.key == "Type":
			typ, _ = tomlUnquote(s.value)
		case s.table == "[runners.cache.s3]" && s.key == "ServerAddress":
			server, _ = tomlUnquote(s.value)
		}
	}
	return typ, server
}
