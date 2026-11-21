package main

import (
	"context"
	"slices"
	"testing"

	"github.com/ismoilovdevml/firerunner/internal/host"
)

// fakeSystemctl plays host.ServicesActiveContext: the units in up are
// active, and every run is recorded.
type fakeSystemctl struct {
	up    map[string]bool
	short bool // answers for one unit fewer than asked, like a cut-off run
	runs  [][]string
}

func (f *fakeSystemctl) probe(_ context.Context, names []string) []bool {
	f.runs = append(f.runs, slices.Clone(names))
	out := make([]bool, len(names))
	for i, n := range names {
		out[i] = f.up[n]
	}
	if f.short && len(out) > 0 {
		out = out[:len(out)-1]
	}
	return out
}

// status lists every unit with its state, in order, as it did with one
// systemctl per unit, from one systemctl run for all of them.
func TestServicesLine(t *testing.T) {
	units := host.MonitoredServices(true, []string{"firerunner-registry@docker.io", "firerunner-registry-front@docker.io"})
	for _, c := range []struct {
		name string
		up   map[string]bool
		want string
	}{
		{"all down", nil, "containerd-flintlock down, firerunner-net down, firerunner-dnsmasq down, " +
			"firerunner-registry down, firerunner-registry@docker.io down, firerunner-registry-front@docker.io down, " +
			"firerunner-cache down, flintlockd down, firerunner-proxy down, gitlab-runner down"},
		{"some up", map[string]bool{"containerd-flintlock": true, "firerunner-registry-front@docker.io": true, "gitlab-runner": true},
			"containerd-flintlock up, firerunner-net down, firerunner-dnsmasq down, " +
				"firerunner-registry down, firerunner-registry@docker.io down, firerunner-registry-front@docker.io up, " +
				"firerunner-cache down, flintlockd down, firerunner-proxy down, gitlab-runner up"},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := &fakeSystemctl{up: c.up}
			ctx := context.Background()
			if got := servicesLine(ctx, probeServices(ctx, f.probe, units), units); got != c.want {
				t.Errorf("services line\n got %q\nwant %q", got, c.want)
			}
			if len(f.runs) != 1 || !slices.Equal(f.runs[0], units) {
				t.Errorf("systemctl runs %q, want one for %q", f.runs, units)
			}
		})
	}
}

// One systemctl run answers for every unit doctor checks; a unit not asked
// about up front is probed on its own, not taken as down.
func TestServiceStates(t *testing.T) {
	hostUnits := host.HostServices([]string{"firerunner-registry@ghcr.io", "firerunner-registry-front@ghcr.io"})
	for _, c := range []struct {
		name    string
		proxy   bool
		short   bool
		ask     string
		up      map[string]bool
		want    bool
		wantRun int // systemctl runs in all
	}{
		{"host unit up", false, false, "firerunner-registry-front@ghcr.io", map[string]bool{"firerunner-registry-front@ghcr.io": true}, true, 1},
		{"host unit down", false, false, "flintlockd", nil, false, 1},
		{"daemon", false, false, "firerunner", map[string]bool{"firerunner": true}, true, 1},
		{"gitlab-runner", false, false, "gitlab-runner", map[string]bool{"gitlab-runner": true}, true, 1},
		{"cache server", false, false, "firerunner-cache", map[string]bool{"firerunner-cache": true}, true, 1},
		{"forwarder with proxy.enabled", true, false, host.ProxyService, map[string]bool{host.ProxyService: true}, true, 1},
		{"a unit not asked up front", false, false, "sshd", map[string]bool{"sshd": true}, true, 2},
		{"a run cut short answers nothing", false, true, "sshd", map[string]bool{"sshd": true}, false, 2},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := &fakeSystemctl{up: c.up, short: c.short}
			ctx := context.Background()
			s := probeServices(ctx, f.probe, doctorUnits(hostUnits, c.proxy))
			if got := s.active(ctx, c.ask); got != c.want {
				t.Errorf("active(%q) = %v, want %v", c.ask, got, c.want)
			}
			if len(f.runs) != c.wantRun {
				t.Errorf("systemctl runs %q, want %d", f.runs, c.wantRun)
			}
		})
	}
}

// doctor asks about the forwarder only where it runs.
func TestDoctorUnits(t *testing.T) {
	hostUnits := host.HostServices(nil)
	for _, c := range []struct {
		proxy bool
		want  []string
	}{
		{false, append(slices.Clone(hostUnits), "firerunner", "gitlab-runner")},
		{true, append(slices.Clone(hostUnits), "firerunner", "gitlab-runner", host.ProxyService)},
	} {
		if got := doctorUnits(hostUnits, c.proxy); !slices.Equal(got, c.want) {
			t.Errorf("proxy %v: %q, want %q", c.proxy, got, c.want)
		}
	}
}
