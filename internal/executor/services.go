package executor

import (
	"encoding/json"
	"fmt"
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
func ServicesScript(services []Service) string {
	var b strings.Builder
	b.WriteString("set -e\n")
	fmt.Fprintf(&b, "docker network create %s >/dev/null\n", ServiceNetwork)
	for i, svc := range services {
		name := fmt.Sprintf("svc-%d", i)
		args := []string{"docker", "run", "-d", "--name", name, "--network", ServiceNetwork, "--pull", "missing"}
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
    if timeout 30 bash -c "until (</dev/tcp/$ip/$port) 2>/dev/null; do sleep 1; done"; then
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
