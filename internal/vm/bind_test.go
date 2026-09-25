package vm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/liquidmetal-dev/flintlock/api/types"
)

// fakeSets is the bridge's three binding sets, changed by the scripts Bind
// and Unbind feed to `nft -f -` and listed like `nft -j list set` (nft 1.0.9).
type fakeSets struct {
	taps    map[string]bool
	macs    map[[2]string]bool
	addrs   map[[3]string]bool
	scripts []string
	listErr error
}

func newFakeSets(t *testing.T) *fakeSets {
	t.Helper()
	f := &fakeSets{taps: map[string]bool{}, macs: map[[2]string]bool{}, addrs: map[[3]string]bool{}}
	oldScript, oldList := nftScript, nftListSet
	nftScript, nftListSet = f.script, f.list
	t.Cleanup(func() { nftScript, nftListSet = oldScript, oldList })
	return f
}

// script applies "add|delete element bridge firerunner <set> { <elem> }"
// lines as one transaction; deleting a missing element fails it, like nft.
func (f *fakeSets) script(_ context.Context, s string) error {
	f.scripts = append(f.scripts, s)
	type change struct {
		add   bool
		set   string
		parts []string
	}
	var changes []change
	for _, line := range strings.Split(strings.TrimSpace(s), "\n") {
		fields := strings.Fields(line)
		open, closing := strings.Index(line, "{ "), strings.LastIndex(line, " }")
		if len(fields) < 6 || open < 0 || closing < open {
			return fmt.Errorf("fake nft cannot parse %q", line)
		}
		parts := strings.Split(line[open+2:closing], " . ")
		if p, err := strconv.Unquote(parts[0]); err == nil {
			parts[0] = p
		}
		changes = append(changes, change{add: fields[0] == "add", set: fields[4], parts: parts})
	}
	for _, c := range changes {
		if c.add {
			continue
		}
		missing := false
		switch c.set {
		case "vm_taps":
			missing = !f.taps[c.parts[0]]
		case "vm_macs":
			missing = !f.macs[[2]string{c.parts[0], c.parts[1]}]
		case "vm_addrs":
			missing = !f.addrs[[3]string{c.parts[0], c.parts[1], c.parts[2]}]
		}
		if missing {
			return errors.New("Error: Could not process rule: No such file or directory")
		}
	}
	for _, c := range changes {
		switch c.set {
		case "vm_taps":
			if c.add {
				f.taps[c.parts[0]] = true
			} else {
				delete(f.taps, c.parts[0])
			}
		case "vm_macs":
			k := [2]string{c.parts[0], c.parts[1]}
			if c.add {
				f.macs[k] = true
			} else {
				delete(f.macs, k)
			}
		case "vm_addrs":
			k := [3]string{c.parts[0], c.parts[1], c.parts[2]}
			if c.add {
				f.addrs[k] = true
			} else {
				delete(f.addrs, k)
			}
		}
	}
	return nil
}

func (f *fakeSets) list(_ context.Context, set string) ([]byte, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	var elems []any
	switch set {
	case "vm_taps":
		for tap := range f.taps {
			elems = append(elems, tap)
		}
	case "vm_macs":
		for k := range f.macs {
			elems = append(elems, map[string]any{"concat": []string{k[0], k[1]}})
		}
	case "vm_addrs":
		for k := range f.addrs {
			elems = append(elems, map[string]any{"concat": []string{k[0], k[1], k[2]}})
		}
	}
	return json.Marshal(map[string]any{"nftables": []any{
		map[string]any{"metainfo": map[string]any{"version": "1.0.9"}},
		map[string]any{"set": map[string]any{"family": "bridge", "name": set, "table": "firerunner", "elem": elems}},
	}})
}

func (f *fakeSets) state() string {
	var out []string
	for tap := range f.taps {
		out = append(out, "tap "+tap)
	}
	for k := range f.macs {
		out = append(out, "mac "+k[0]+" "+k[1])
	}
	for k := range f.addrs {
		out = append(out, "addr "+k[0]+" "+k[1]+" "+k[2])
	}
	sort.Strings(out)
	return strings.Join(out, "; ")
}

// Bind feeds nft the same transaction as test/network/run.sh's bind_vm, and
// nothing for values that are not a tap name, a MAC and an IPv4 address.
func TestBindScript(t *testing.T) {
	f := newFakeSets(t)
	if err := Bind(context.Background(), Binding{Tap: "fltap1a2b", MAC: "AA:FC:01:02:03:04", IP: "10.200.0.23"}); err != nil {
		t.Fatal(err)
	}
	want := `add element bridge firerunner vm_macs { "fltap1a2b" . aa:fc:01:02:03:04 }
add element bridge firerunner vm_addrs { "fltap1a2b" . aa:fc:01:02:03:04 . 10.200.0.23 }
add element bridge firerunner vm_taps { "fltap1a2b" }
`
	if len(f.scripts) != 1 || f.scripts[0] != want {
		t.Fatalf("nft got %q, want one transaction %q", f.scripts, want)
	}
	for _, b := range []Binding{
		{Tap: `x" } flush ruleset; {`, MAC: "aa:fc:01:02:03:04", IP: "10.200.0.23"},
		{Tap: "a-name-longer-than-15", MAC: "aa:fc:01:02:03:04", IP: "10.200.0.23"},
		{Tap: "fltap1", MAC: "aa:fc:01:02:03", IP: "10.200.0.23"},
		{Tap: "fltap1", MAC: "aa:fc:01:02:03:04", IP: "fd00::1"},
		{Tap: "fltap1", MAC: "aa:fc:01:02:03:04", IP: "10.200.0.23; flush ruleset"},
	} {
		if err := Bind(context.Background(), b); err == nil {
			t.Errorf("Bind(%+v) accepted", b)
		}
	}
	if len(f.scripts) != 1 {
		t.Fatalf("invalid bindings reached nft: %q", f.scripts[1:])
	}
}

// Unbind removes the tap from vm_taps first, on its own: from then on it is
// unchecked, whatever happens to the rest. Missing elements are fine.
func TestUnbindOrder(t *testing.T) {
	f := newFakeSets(t)
	b := Binding{Tap: "fltap1", MAC: "aa:fc:01:02:03:04", IP: "10.200.0.23"}
	if err := Bind(context.Background(), b); err != nil {
		t.Fatal(err)
	}
	f.scripts = nil
	Unbind(context.Background(), b)
	if len(f.scripts) != 3 || !strings.HasPrefix(f.scripts[0], "delete element bridge firerunner vm_taps") ||
		!strings.Contains(f.scripts[1], "vm_addrs") || !strings.Contains(f.scripts[2], "vm_macs") {
		t.Fatalf("unbind scripts %q", f.scripts)
	}
	if s := f.state(); s != "" {
		t.Fatalf("left %s", s)
	}
	Unbind(context.Background(), b) // nothing left: no panic, nothing to undo
}

// The JSON of `nft -j list set` for the three set types (nft 1.0.9).
func TestSetElementsParsesNftJSON(t *testing.T) {
	for _, c := range []struct {
		out  string
		want string
	}{
		{`{"nftables": [{"metainfo": {"version": "1.0.9", "release_name": "Old Doc Yak #3", "json_schema_version": 1}}, {"set": {"family": "bridge", "name": "vm_taps", "table": "firerunner", "type": "ifname", "handle": 1, "elem": ["fltapA"]}}]}`, "[[fltapA]]"},
		{`{"nftables": [{"metainfo": {"version": "1.0.9", "release_name": "Old Doc Yak #3", "json_schema_version": 1}}, {"set": {"family": "bridge", "name": "vm_macs", "table": "firerunner", "type": ["ifname", "ether_addr"], "handle": 2, "elem": [{"concat": ["fltapA", "aa:fc:01:02:03:04"]}]}}]}`, "[[fltapA aa:fc:01:02:03:04]]"},
		{`{"nftables": [{"metainfo": {"version": "1.0.9", "release_name": "Old Doc Yak #3", "json_schema_version": 1}}, {"set": {"family": "bridge", "name": "vm_addrs", "table": "firerunner", "type": ["ifname", "ether_addr", "ipv4_addr"], "handle": 3, "elem": [{"concat": ["fltapA", "aa:fc:01:02:03:04", "10.200.0.11"]}]}}]}`, "[[fltapA aa:fc:01:02:03:04 10.200.0.11]]"},
		{`{"nftables": [{"metainfo": {"version": "1.0.9"}}, {"set": {"family": "bridge", "name": "vm_taps", "table": "firerunner", "type": "ifname", "handle": 1}}]}`, "[]"},
	} {
		got, err := setElements([]byte(c.out))
		if err != nil || fmt.Sprint(got) != c.want {
			t.Errorf("setElements = %v, %v; want %s", got, err, c.want)
		}
	}
	if _, err := setElements([]byte("Error: No such file")); err == nil {
		t.Error("garbage parsed")
	}
}

func TestReconcileBindings(t *testing.T) {
	f := newFakeSets(t)
	exists := map[string]bool{"fltapA": true, "fltapBoot": true, "fltapReuse": true}
	oldLink := linkExists
	linkExists = func(name string) bool { return exists[name] }
	t.Cleanup(func() { linkExists = oldLink })
	ctx := context.Background()
	for _, b := range []Binding{
		{Tap: "fltapGone", MAC: "aa:fc:00:00:00:01", IP: "10.200.0.5"},  // its VM was deleted
		{Tap: "fltapBoot", MAC: "aa:fc:00:00:00:02", IP: "10.200.0.6"},  // booted after the listing
		{Tap: "fltapReuse", MAC: "aa:fc:00:00:00:03", IP: "10.200.0.7"}, // name now used by a new VM
	} {
		if err := Bind(ctx, b); err != nil {
			t.Fatal(err)
		}
	}
	want := map[string]Binding{
		"fltapA":     {Tap: "fltapA", MAC: "AA:FC:00:00:00:0A", IP: "10.200.0.10"},
		"fltapReuse": {Tap: "fltapReuse", MAC: "aa:fc:00:00:00:0b", IP: "10.200.0.11"},
	}
	added, removed, err := ReconcileBindings(ctx, want)
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(added)
	if fmt.Sprint(added) != "[fltapA fltapReuse]" || fmt.Sprint(removed) != "[fltapGone]" {
		t.Fatalf("added %v, removed %v", added, removed)
	}
	wantState := "addr fltapA aa:fc:00:00:00:0a 10.200.0.10; addr fltapBoot aa:fc:00:00:00:02 10.200.0.6; " +
		"addr fltapReuse aa:fc:00:00:00:0b 10.200.0.11; mac fltapA aa:fc:00:00:00:0a; mac fltapBoot aa:fc:00:00:00:02; " +
		"mac fltapReuse aa:fc:00:00:00:0b; tap fltapA; tap fltapBoot; tap fltapReuse"
	if s := f.state(); s != wantState {
		t.Fatalf("sets after reconcile:\n %s\nwant\n %s", s, wantState)
	}
	// In line: nothing more to do.
	f.scripts = nil
	if added, removed, err := ReconcileBindings(ctx, want); err != nil || len(added)+len(removed) != 0 || len(f.scripts) != 0 {
		t.Fatalf("second pass: %v %v %v, nft %q", added, removed, err, f.scripts)
	}
	// Sets that cannot be listed change nothing.
	f.listErr = ErrNoBindingSets
	if _, _, err := ReconcileBindings(ctx, map[string]Binding{"fltapNew": {Tap: "fltapNew", MAC: "aa:fc:00:00:00:0c", IP: "10.200.0.12"}}); !errors.Is(err, ErrNoBindingSets) || f.taps["fltapNew"] {
		t.Fatalf("listing failed: %v, bound %v", err, f.taps["fltapNew"])
	}
}

// Boot binds the new VM's tap to its MAC and address before it waits for
// SSH, and a boot that fails removes the binding with the VM.
func TestBootBindsItsTap(t *testing.T) {
	const id = "job-9"
	mac := MAC(id)
	exp := time.Now().Add(15 * time.Minute).Unix()
	cfg, srv, fl, _ := bootEnv(t, "")
	f := newFakeSets(t)
	// Once created, flintlock lists the VM with its tap, and dnsmasq leases it.
	done := make(chan struct{})
	go func() {
		defer close(done)
		for len(srv.Created()) == 0 {
			time.Sleep(5 * time.Millisecond)
		}
		uid := "uid-new"
		srv.SetVMs(&types.MicroVM{Spec: &types.MicroVMSpec{Id: id, Uid: &uid},
			Status: &types.MicroVMStatus{State: types.MicroVMStatus_CREATED,
				NetworkInterfaces: map[string]*types.NetworkInterfaceStatus{bridgedDevice: {HostDeviceName: "fltap9"}}}})
		_ = os.WriteFile(cfg.Network.LeasesFile, []byte(fmt.Sprintf("%d %s 127.0.0.1 %s 01:%s\n", exp, mac, id, mac)), 0o600)
	}()
	inst, err := Boot(context.Background(), cfg, fl, id, nil)
	<-done
	if inst != nil || err == nil {
		t.Fatalf("Boot = %v, %v; want a failure at the SSH wait (no sshd in tests)", inst, err)
	}
	if len(f.scripts) < 2 || !strings.Contains(f.scripts[0], `vm_addrs { "fltap9" . `+mac+` . 127.0.0.1 }`) ||
		!strings.HasPrefix(f.scripts[1], `delete element bridge firerunner vm_taps { "fltap9" }`) {
		t.Fatalf("nft scripts %q: want the binding, then its removal", f.scripts)
	}
	if s := f.state(); s != "" {
		t.Fatalf("binding left after the failed boot: %s", s)
	}
}
