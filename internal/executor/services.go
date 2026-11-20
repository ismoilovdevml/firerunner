package executor

import (
	"encoding/json"
	"fmt"
	"maps"
	"sort"
	"strings"
)

// ServiceNetwork is the Docker network in the job VM that services and the
// job container share, so services resolve by their aliases.
const ServiceNetwork = "firerunner-job"

// Service is one entry of CUSTOM_ENV_CI_JOB_SERVICES, which gitlab-runner's
// custom executor fills from the job's `services:` (executors/custom/custom.go).
type Service struct {
	Name       string            `json:"name"`
	Alias      string            `json:"alias"`
	Entrypoint []string          `json:"entrypoint"`
	Command    []string          `json:"command"`
	Variables  map[string]string `json:"variables,omitempty"`
}

// ParseServices decodes CUSTOM_ENV_CI_JOB_SERVICES; an empty value means no services.
func ParseServices(raw string) ([]Service, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	var s []Service
	if err := json.Unmarshal([]byte(raw), &s); err != nil {
		return nil, fmt.Errorf("parsing CI_JOB_SERVICES: %w", err)
	}
	for _, svc := range s {
		if svc.Name == "" || strings.HasPrefix(svc.Name, "-") {
			return nil, fmt.Errorf("invalid service image %q", svc.Name)
		}
	}
	return s, nil
}

// ServiceJobVariables returns the job variables every service container
// gets, as gitlab-runner's Docker executor passes them
// (Build.GetServiceVariables keeps Variables.PublicOrInternal): each variable
// in the job payload (JOB_RESPONSE_FILE) whose last entry is public, which
// are the .gitlab-ci.yml variables and GitLab's public predefined ones. A
// non-public variable (CI_JOB_TOKEN, CI_REGISTRY_PASSWORD, CI/CD settings
// variables) never reaches a service, nor does a public one that a later
// non-public entry or a `secrets:` variable of the same name overrides. The
// values are gitlab-runner's, already expanded: lookup reads CUSTOM_ENV_<key>.
//
// Unlike the Docker executor, the variables gitlab-runner adds itself
// (CI_PROJECT_DIR, CI_RUNNER_*, FF_*) are left out, since they are not in the
// payload and their CUSTOM_ENV_ copies look like any other, `secrets:` ones
// included. File variables are left out too: the Docker executor passes their
// path in the job container, which a service cannot read.
func ServiceJobVariables(payload []byte, lookup func(string) (string, bool)) (map[string]string, error) {
	var job struct {
		Variables []struct {
			Key    string `json:"key"`
			Public bool   `json:"public"`
			File   bool   `json:"file"`
		} `json:"variables"`
		Secrets map[string]json.RawMessage `json:"secrets"`
	}
	if err := json.Unmarshal(payload, &job); err != nil {
		return nil, fmt.Errorf("parsing the job payload: %w", err)
	}
	last := map[string]int{} // a later entry of a key overrides an earlier one
	for i, v := range job.Variables {
		last[v.Key] = i
	}
	vars := map[string]string{}
	for key, i := range last {
		if v := job.Variables[i]; key == "" || !v.Public || v.File {
			continue
		}
		if _, secret := job.Secrets[key]; secret {
			continue
		}
		if value, ok := lookup("CUSTOM_ENV_" + key); ok {
			vars[key] = value
		}
	}
	return vars, nil
}

// WithJobVariables returns services whose variables are jobVars overridden by
// each service's own, like the Docker executor sets them.
func WithJobVariables(services []Service, jobVars map[string]string) []Service {
	out := make([]Service, len(services))
	for i, svc := range services {
		merged := make(map[string]string, len(jobVars)+len(svc.Variables))
		maps.Copy(merged, jobVars)
		maps.Copy(merged, svc.Variables)
		svc.Variables = merged
		out[i] = svc
	}
	return out
}

// Aliases returns the hostnames a service is reachable under. Like the docker
// executor: an explicit alias list wins; otherwise the image name without tag,
// digest and registry port, with "/" replaced by "__" and by "-".
func (s Service) Aliases() []string {
	if a := strings.Fields(strings.ReplaceAll(s.Alias, ",", " ")); len(a) > 0 {
		return a
	}
	n := s.Name
	if i := strings.Index(n, "@"); i >= 0 {
		n = n[:i]
	}
	if c := strings.LastIndex(n, ":"); c > strings.LastIndex(n, "/") {
		n = n[:c]
	}
	if host, rest, ok := strings.Cut(n, "/"); ok {
		if h, _, hasPort := strings.Cut(host, ":"); hasPort {
			n = h + "/" + rest
		}
	}
	out := []string{strings.ReplaceAll(n, "/", "__")}
	if alt := strings.ReplaceAll(n, "/", "-"); alt != out[0] {
		out = append(out, alt)
	}
	return out
}

// ServicesScript returns the shell script run in the job VM to start the
// services on ServiceNetwork, publish their aliases in /etc/hosts (for jobs
// without image:) and wait up to 30 s for each exposed TCP port, like the
// docker executor's health check (a port that never opens is only a warning).
//
// With the proxy, noProxy (the proxy exceptions plus the service aliases)
// replaces the VM's no_proxy in /etc/environment, and extra (vm.ContainerArgs)
// is passed to each service container. pam_env reads /etc/environment once
// per SSH connection, so only connections opened later see the new no_proxy:
// stages, which share the connection prepare opened before, export it
// themselves (see Run).
func ServicesScript(services []Service, noProxy string, extra ...string) string {
	var b strings.Builder
	b.WriteString("set -e\n")
	if noProxy != "" {
		fmt.Fprintf(&b, "sed -i -e '/^no_proxy=/d' -e '/^NO_PROXY=/d' /etc/environment\nprintf '%%s\\n' %s %s >>/etc/environment\n",
			shellQuote("no_proxy="+noProxy), shellQuote("NO_PROXY="+noProxy))
	}
	fmt.Fprintf(&b, "docker network create %s >/dev/null\n", ServiceNetwork)
	// Several services: fetch their images at once instead of one `docker run`
	// after another. Only a head start: a pull that fails here is tried again,
	// with its error, by `docker run --pull missing` below.
	if len(services) > 1 {
		seen := map[string]bool{}
		for _, svc := range services {
			if seen[svc.Name] {
				continue
			}
			seen[svc.Name] = true
			fmt.Fprintf(&b, "(docker image inspect %[1]s >/dev/null 2>&1 || docker pull -q %[1]s >/dev/null 2>&1) &\n", shellQuote(svc.Name))
		}
		b.WriteString("wait\n")
	}
	for i, svc := range services {
		name := fmt.Sprintf("svc-%d", i)
		args := []string{"docker", "run", "-d", "--name", name, "--network", ServiceNetwork, "--pull", "missing"}
		for _, a := range extra {
			args = append(args, shellQuote(a))
		}
		for _, a := range svc.Aliases() {
			args = append(args, "--network-alias", shellQuote(a))
		}
		for _, k := range sortedKeys(svc.Variables) {
			args = append(args, "-e", shellQuote(k+"="+svc.Variables[k]))
		}
		cmd := svc.Command
		if len(svc.Entrypoint) > 0 {
			args = append(args, "--entrypoint", shellQuote(svc.Entrypoint[0]))
			cmd = append(append([]string{}, svc.Entrypoint[1:]...), svc.Command...)
		}
		args = append(args, shellQuote(svc.Name))
		for _, c := range cmd {
			args = append(args, shellQuote(c))
		}
		fmt.Fprintf(&b, "echo %s\n", shellQuote("Starting service "+svc.Name+" as "+strings.Join(svc.Aliases(), ", ")))
		b.WriteString(strings.Join(args, " ") + " >/dev/null\n")
	}
	b.WriteString(`set +e
for c in $(docker ps -q --filter network=` + ServiceNetwork + `); do
  ip=$(docker inspect -f '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}' "$c")
  for a in $(docker inspect -f '{{range .NetworkSettings.Networks}}{{range .Aliases}}{{.}} {{end}}{{end}}' "$c"); do
    echo "$ip $a" >>/etc/hosts
  done
  for p in $(docker inspect -f '{{range $p, $_ := .Config.ExposedPorts}}{{$p}} {{end}}' "$c"); do
    case $p in */tcp) port=${p%/tcp} ;; *) continue ;; esac
    if timeout 30 bash -c "until (</dev/tcp/$ip/$port) 2>/dev/null; do sleep 0.2; done"; then
      echo "Service $(docker inspect -f '{{.Config.Image}}' "$c") is listening on $port"
    else
      echo "WARNING: service $(docker inspect -f '{{.Config.Image}}' "$c") did not open port $port within 30s"
      docker logs --tail 20 "$c" 2>&1 | sed 's/^/  /'
    fi
  done
done
exit 0
`)
	return b.String()
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
