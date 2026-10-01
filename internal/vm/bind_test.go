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

	"github.com/ismoilovdevml/firerunner/internal/flintlock/flintlocktest"
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
		if missing { // what nftScript makes of nft's answer
			return fmt.Errorf("%w: Error: Could not process rule: No such file or directory", ErrNoBindingSets)
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
	if err := Unbind(context.Background(), b); err != nil {
		t.Fatalf("Unbind = %v", err)
	}
	if len(f.scripts) != 3 || !strings.HasPrefix(f.scripts[0], "delete element bridge firerunner vm_taps") ||
		!strings.Contains(f.scripts[1], "vm_addrs") || !strings.Contains(f.scripts[2], "vm_macs") {
		t.Fatalf("unbind scripts %q", f.scripts)
	}
	if s := f.state(); s != "" {
		t.Fatalf("left %s", s)
	}
	if err := Unbind(context.Background(), b); err != nil { // nothing left: no panic, nothing to undo
		t.Fatalf("Unbind = %v", err)
	}
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

// bootListedWithTap boots id against a fake flintlock that, once the VM is
// created, lists it with tap and leases it 127.0.0.1 (no sshd there: the boot
// ends at the SSH wait).
func bootListedWithTap(t *testing.T, ctx context.Context, id, tap string) (*Instance, error) {
	t.Helper()
	mac := MAC(id)
	exp := time.Now().Add(15 * time.Minute).Unix()
	cfg, srv, fl, _ := bootEnv(t, "")
	done := make(chan struct{})
	go func() {
		defer close(done)
		for len(srv.Created()) == 0 {
			time.Sleep(5 * time.Millisecond)
		}
		uid := "uid-new"
		srv.SetVMs(&types.MicroVM{Spec: &types.MicroVMSpec{Id: id, Uid: &uid},
			Status: &types.MicroVMStatus{State: types.MicroVMStatus_CREATED,
				NetworkInterfaces: map[string]*types.NetworkInterfaceStatus{bridgedDevice: {HostDeviceName: tap}}}})
		_ = os.WriteFile(cfg.Network.LeasesFile, []byte(fmt.Sprintf("%d %s 127.0.0.1 %s 01:%s\n", exp, mac, id, mac)), 0o600)
	}()
	inst, err := Boot(ctx, cfg, fl, id, nil)
	<-done
	return inst, err
}

// Boot binds the new VM's tap to its MAC and address before it waits for
// SSH. A failed boot keeps the binding while the tap exists: flintlock
// deletes the VM later, and it must not send as another VM meanwhile.
func TestBootBindsItsTap(t *testing.T) {
	f := newFakeSets(t)
	if inst, err := bootListedWithTap(t, context.Background(), "job-9", "fltap9"); inst != nil || !errors.Is(err, ErrNotReady) {
		t.Fatalf("Boot = %v, %v; want a failure at the SSH wait (no sshd in tests)", inst, err)
	}
	if len(f.scripts) != 1 || !strings.Contains(f.scripts[0], `vm_addrs { "fltap9" . `+MAC("job-9")+` . 127.0.0.1 }`) {
		t.Fatalf("nft scripts %q: want the binding only", f.scripts)
	}
	if !f.taps["fltap9"] {
		t.Fatal("binding removed while the tap still exists")
	}
}

// A binding nft refuses does not fail the boot: the VM runs unchecked.
func TestBootRunsUncheckedWhenBindingFails(t *testing.T) {
	old := nftScript
	nftScript = func(context.Context, string) error { return errors.New("nft: netlink: Operation not permitted") }
	t.Cleanup(func() { nftScript = old })
	if inst, err := bootListedWithTap(t, context.Background(), "job-10", "fltap10"); !errors.Is(err, ErrNotReady) || inst != nil {
		t.Fatalf("Boot = %v, %v; want it to go on to the SSH wait", inst, err)
	}
}

// A job cancelled while its VM is being bound ends the boot as cancelled.
func TestBootCancelledWhileBinding(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	old := nftScript
	nftScript = func(context.Context, string) error { cancel(); return context.Canceled }
	t.Cleanup(func() { nftScript = old })
	if _, err := bootListedWithTap(t, ctx, "job-11", "fltap11"); !errors.Is(err, context.Canceled) {
		t.Fatalf("Boot = %v, want context.Canceled", err)
	}
}

// A deleted VM keeps its binding until its tap is gone; then it goes.
func TestUnbindInstanceWaitsForTheTapToGo(t *testing.T) {
	f := newFakeSets(t)
	exists := true
	old := linkExists
	linkExists = func(string) bool { return exists }
	t.Cleanup(func() { linkExists = old })
	inst := &Instance{ID: "job-5", IP: "10.200.0.5", Tap: "fltap5"}
	if err := Bind(context.Background(), Binding{Tap: inst.Tap, MAC: MAC(inst.ID), IP: inst.IP}); err != nil {
		t.Fatal(err)
	}
	UnbindInstance(inst)
	if !f.taps["fltap5"] || len(f.addrs) != 1 {
		t.Fatalf("unbound while the tap exists: %s", f.state())
	}
	exists = false
	UnbindInstance(inst)
	if s := f.state(); s != "" {
		t.Fatalf("left after the tap is gone: %s", s)
	}
	UnbindInstance(&Instance{ID: "job-6"}) // never bound: nothing to do
}

// A tap still in vm_taps keeps its addresses: when its delete fails, Unbind
// removes nothing else (the VM would be cut off).
func TestUnbindStopsWhenTheTapStaysChecked(t *testing.T) {
	f := newFakeSets(t)
	b := Binding{Tap: "fltap7", MAC: "aa:fc:00:00:00:07", IP: "10.200.0.7"}
	if err := Bind(context.Background(), b); err != nil {
		t.Fatal(err)
	}
	nftScript = func(ctx context.Context, s string) error {
		if strings.Contains(s, "delete element bridge firerunner vm_taps") {
			return errors.New("nft: signal: killed")
		}
		return f.script(ctx, s)
	}
	if err := Unbind(context.Background(), b); err == nil {
		t.Fatal("Unbind hid the failed vm_taps delete")
	}
	if !f.taps["fltap7"] || len(f.macs) != 1 || len(f.addrs) != 1 {
		t.Fatalf("addresses removed from a tap still checked: %s", f.state())
	}
}

// A binding nft refuses is reported, and the pass still removes the elements
// of taps that are gone (only reconcile removes them).
func TestReconcileBindingsReportsBindErrors(t *testing.T) {
	f := newFakeSets(t)
	if err := Bind(context.Background(), Binding{Tap: "fltapGone", MAC: "aa:fc:00:00:00:01", IP: "10.200.0.5"}); err != nil {
		t.Fatal(err)
	}
	old := linkExists
	linkExists = func(name string) bool { return name != "fltapGone" }
	t.Cleanup(func() { linkExists = old })
	nftScript = func(ctx context.Context, s string) error {
		if strings.HasPrefix(s, "add element") {
			return errors.New("nft: netlink: Operation not permitted")
		}
		return f.script(ctx, s)
	}
	added, removed, err := ReconcileBindings(context.Background(), map[string]Binding{"fltap8": {Tap: "fltap8", MAC: "aa:fc:00:00:00:08", IP: "10.200.0.8"}})
	if err == nil || !strings.Contains(err.Error(), "fltap8") || len(added) != 0 || strings.Join(removed, ",") != "fltapGone" {
		t.Fatalf("reconcile = %v, %v, %v", added, removed, err)
	}
	if s := f.state(); s != "" {
		t.Fatalf("sets %s, want the gone tap removed and nothing bound", s)
	}
}

// A job that makes dnsmasq lease it another VM's address (release that VM's
// lease, then ask for the address) must not get bound to it: a listed VM keeps
// the address it was bound with, and the pass says so.
func TestReconcileKeepsABoundAddress(t *testing.T) {
	f := newFakeSets(t)
	oldLink := linkExists
	linkExists = func(string) bool { return true }
	t.Cleanup(func() { linkExists = oldLink })
	ctx := context.Background()
	attacker := Binding{Tap: "fltapJob", MAC: "aa:fc:00:00:00:01", IP: "10.200.0.21"}
	victim := Binding{Tap: "fltapVictim", MAC: "aa:fc:00:00:00:02", IP: "10.200.0.22"}
	for _, b := range []Binding{attacker, victim} {
		if err := Bind(ctx, b); err != nil {
			t.Fatal(err)
		}
	}
	before := f.state()
	// The leases now say: the job has the victim's address, the victim none.
	stolen := attacker
	stolen.IP = victim.IP
	added, removed, err := ReconcileBindings(ctx, map[string]Binding{"fltapJob": stolen})
	if err == nil || !strings.Contains(err.Error(), "fltapJob: its lease now names 10.200.0.22; it keeps 10.200.0.21") {
		t.Fatalf("err = %v, want the changed lease reported", err)
	}
	if len(added)+len(removed) != 0 || f.state() != before {
		t.Fatalf("added %v removed %v, sets:\n %s\nwant unchanged:\n %s", added, removed, f.state(), before)
	}
	// A new VM on a reused tap name (another MAC) still gets its own address.
	reused := Binding{Tap: "fltapJob", MAC: "aa:fc:00:00:00:03", IP: "10.200.0.23"}
	if _, _, err := ReconcileBindings(ctx, map[string]Binding{"fltapJob": reused}); err != nil {
		t.Fatal(err)
	}
	if !f.addrs[[3]string{"fltapJob", "aa:fc:00:00:00:03", "10.200.0.23"}] || f.addrs[[3]string{"fltapJob", "aa:fc:00:00:00:01", "10.200.0.21"}] {
		t.Fatalf("reused tap: %s", f.state())
	}
}

// bind asks flintlock for its own VM only: a listing, which fails while
// flintlockd deletes another VM, is not needed to find the tap.
func TestBindGetsItsOwnVM(t *testing.T) {
	f := newFakeSets(t)
	srv := flintlocktest.NewServer("")
	uid := "uid-new"
	srv.SetVMs(&types.MicroVM{
		Spec:   &types.MicroVMSpec{Id: "job-1", Uid: &uid},
		Status: &types.MicroVMStatus{NetworkInterfaces: map[string]*types.NetworkInterfaceStatus{bridgedDevice: {HostDeviceName: "fltap1a2b"}}},
	})
	srv.FailList(errors.New("failed reading from content store"))
	fl := dialFake(t, srv)
	inst := &Instance{ID: "job-1", UID: uid, IP: "10.200.0.23"}
	if err := bind(context.Background(), fl, inst, "aa:fc:01:02:03:04"); err != nil {
		t.Fatalf("bind = %v", err)
	}
	if inst.Tap != "fltap1a2b" || len(f.scripts) != 1 || srv.ListCalls() != 0 || srv.GetCalls() != 1 {
		t.Fatalf("tap %q, nft %q, %d lists, %d gets", inst.Tap, f.scripts, srv.ListCalls(), srv.GetCalls())
	}

	other := &Instance{ID: "job-2", UID: "uid-gone", IP: "10.200.0.24"}
	if err := bind(context.Background(), fl, other, "aa:fc:01:02:03:05"); err == nil {
		t.Fatal("bind of a VM flintlock does not know succeeded")
	}
}
