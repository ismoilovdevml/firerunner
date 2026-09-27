package vm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"github.com/liquidmetal-dev/flintlock/api/types"

	"github.com/ismoilovdevml/firerunner/internal/flintlock"
)

// Address binding on the bridge. install.sh's table bridge firerunner checks
// every tap in vm_taps: it may only send frames from the MAC bound to it in
// vm_macs, and IPv4 and ARP only from the MAC and address in vm_addrs. So a
// microVM cannot take another one's address (ARP) or MAC. A tap that is not
// in vm_taps is not checked: a VM whose binding failed runs unchecked rather
// than cut off.

// bridgedDevice is the guest device of the bridged interface (see Spec).
const bridgedDevice = "eth1"

// Binding is a microVM's tap on the bridge and the addresses it may use.
type Binding struct{ Tap, MAC, IP string }

// ErrNoBindingSets means the host's firewall has no binding sets yet (it
// predates them): nothing is checked, and nothing to warn about.
var ErrNoBindingSets = errors.New("the bridge firewall has no address binding sets")

// A tap name as the kernel allows it; flintlock's are short and plain.
var tapName = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,15}$`)

func (b Binding) valid() error {
	if !tapName.MatchString(b.Tap) {
		return fmt.Errorf("tap name %q", b.Tap)
	}
	if hw, err := net.ParseMAC(b.MAC); err != nil || len(hw) != 6 {
		return fmt.Errorf("MAC %q", b.MAC)
	}
	if ip := net.ParseIP(b.IP); ip == nil || ip.To4() == nil {
		return fmt.Errorf("IPv4 address %q", b.IP)
	}
	return nil
}

// nftScript runs `nft -f -` with script, bounded (a variable for tests).
var nftScript = func(ctx context.Context, script string) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "nft", "-f", "-")
	cmd.Stdin = strings.NewReader(script)
	if out, err := cmd.CombinedOutput(); err != nil {
		if strings.Contains(string(out), "No such file or directory") {
			return fmt.Errorf("%w: %s", ErrNoBindingSets, strings.TrimSpace(string(out)))
		}
		return fmt.Errorf("nft: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// Bind lets b.Tap send only from b.MAC and b.IP. One transaction: the tap is
// never checked without its addresses. Adding an element that is there
// already is no error, so Bind can be repeated.
func Bind(ctx context.Context, b Binding) error {
	if err := b.valid(); err != nil {
		return fmt.Errorf("not binding: %w", err)
	}
	mac := strings.ToLower(b.MAC)
	return nftScript(ctx, fmt.Sprintf("add element bridge firerunner vm_macs { %q . %s }\n"+
		"add element bridge firerunner vm_addrs { %q . %s . %s }\n"+
		"add element bridge firerunner vm_taps { %q }\n", b.Tap, mac, b.Tap, mac, b.IP, b.Tap))
}

// Unbind removes a binding (see unbindTap).
func Unbind(ctx context.Context, b Binding) error {
	var macs []string
	var addrs [][2]string
	if b.MAC != "" {
		macs = []string{b.MAC}
		if b.IP != "" {
			addrs = [][2]string{{b.MAC, b.IP}}
		}
	}
	return unbindTap(ctx, b.Tap, macs, addrs)
}

// unbindTap removes a tap from vm_taps first and on its own, so it is
// unchecked before its addresses go. If that delete fails for any reason but
// a missing element, nothing more is removed: a tap in vm_taps keeps its
// addresses, or its VM would be cut off.
func unbindTap(ctx context.Context, tap string, macs []string, addrs [][2]string) error {
	if !tapName.MatchString(tap) {
		return fmt.Errorf("not unbinding: tap name %q", tap)
	}
	if err := nftScript(ctx, fmt.Sprintf("delete element bridge firerunner vm_taps { %q }\n", tap)); err != nil &&
		!errors.Is(err, ErrNoBindingSets) {
		return err
	}
	deleteTuples(ctx, tap, macs, addrs)
	return nil
}

// deleteTuples removes addresses of a tap from vm_macs and vm_addrs, each on
// its own (a missing one would abort a transaction). Values that are not a
// MAC and an IPv4 address never reach nft.
func deleteTuples(ctx context.Context, tap string, macs []string, addrs [][2]string) {
	for _, a := range addrs {
		if (Binding{Tap: tap, MAC: a[0], IP: a[1]}).valid() == nil {
			_ = nftScript(ctx, fmt.Sprintf("delete element bridge firerunner vm_addrs { %q . %s . %s }\n", tap, strings.ToLower(a[0]), a[1]))
		}
	}
	for _, m := range macs {
		if hw, err := net.ParseMAC(m); err == nil && len(hw) == 6 && tapName.MatchString(tap) {
			_ = nftScript(ctx, fmt.Sprintf("delete element bridge firerunner vm_macs { %q . %s }\n", tap, strings.ToLower(m)))
		}
	}
}

// UnbindInstance removes the binding of a deleted microVM once its tap is
// gone. flintlock deletes a VM after Delete returns (its one worker may run
// other plans first), and a VM still running must not send from any address
// meanwhile; reconcile removes the binding once the tap is gone.
func UnbindInstance(inst *Instance) {
	if inst.Tap != "" && !linkExists(inst.Tap) {
		_ = Unbind(context.Background(), Binding{Tap: inst.Tap, MAC: MAC(inst.ID), IP: inst.IP})
	}
}

// Tap is the host tap of v's bridged interface, "" while flintlock does not
// report it.
func Tap(v *types.MicroVM) string {
	return v.GetStatus().GetNetworkInterfaces()[bridgedDevice].GetHostDeviceName()
}

// GuestMAC is the MAC of v's bridged interface.
func GuestMAC(v *types.MicroVM) string {
	for _, iface := range v.GetSpec().GetInterfaces() {
		if iface.GetDeviceId() == bridgedDevice {
			return iface.GetGuestMac()
		}
	}
	return ""
}

// bindTries, bindPoll and bindCallTimeout bound how long Boot asks flintlock
// for the new VM's tap (variables for tests): a few seconds at most, since
// the time comes out of the boot's wait for SSH. Once the VM has a lease its
// tap exists, and flintlock reports it with the VM's status.
var (
	bindTries       = 5
	bindPoll        = 200 * time.Millisecond
	bindCallTimeout = 2 * time.Second
)

// bind binds inst's tap (set in inst.Tap) to its MAC and leased address.
func bind(ctx context.Context, fl *flintlock.Client, inst *Instance, mac string) error {
	tap := ""
	for i := 0; i < bindTries && tap == ""; i++ {
		if i > 0 {
			if err := sleep(ctx, bindPoll); err != nil {
				return err
			}
		}
		lctx, cancel := context.WithTimeout(ctx, bindCallTimeout)
		vms, err := fl.ListOnce(lctx)
		cancel()
		if ctx.Err() != nil {
			return ctx.Err()
		}
		for _, v := range vms {
			if err == nil && v.GetSpec().GetUid() == inst.UID {
				tap = Tap(v)
			}
		}
	}
	if tap == "" {
		return errors.New("flintlock does not report its tap")
	}
	if err := Bind(ctx, Binding{Tap: tap, MAC: mac, IP: inst.IP}); err != nil {
		return err
	}
	inst.Tap = tap
	return nil
}

// Bindings are the elements of the binding sets, as nft lists them.
type Bindings struct {
	Taps  map[string]bool    // vm_taps
	MACs  map[[2]string]bool // vm_macs: tap, MAC
	Addrs map[[3]string]bool // vm_addrs: tap, MAC, IP
}

// nftListSet returns `nft -j list set bridge firerunner <set>` (a variable
// for tests).
var nftListSet = func(ctx context.Context, set string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var stderr strings.Builder
	cmd := exec.CommandContext(ctx, "nft", "-j", "list", "set", "bridge", "firerunner", set)
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		if strings.Contains(stderr.String(), "No such file or directory") {
			return nil, ErrNoBindingSets
		}
		return nil, fmt.Errorf("nft list set %s: %w: %s", set, err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

// ListBindings reads the three binding sets.
func ListBindings(ctx context.Context) (Bindings, error) {
	b := Bindings{Taps: map[string]bool{}, MACs: map[[2]string]bool{}, Addrs: map[[3]string]bool{}}
	for _, set := range []string{"vm_taps", "vm_macs", "vm_addrs"} {
		out, err := nftListSet(ctx, set)
		if err != nil {
			return b, err
		}
		elems, err := setElements(out)
		if err != nil {
			return b, fmt.Errorf("nft set %s: %w", set, err)
		}
		for _, e := range elems {
			switch {
			case set == "vm_taps" && len(e) == 1:
				b.Taps[e[0]] = true
			case set == "vm_macs" && len(e) == 2:
				b.MACs[[2]string{e[0], strings.ToLower(e[1])}] = true
			case set == "vm_addrs" && len(e) == 3:
				b.Addrs[[3]string{e[0], strings.ToLower(e[1]), e[2]}] = true
			}
		}
	}
	return b, nil
}

// setElements parses the elements of `nft -j list set`: plain values or
// concatenations ({"concat": [...]}), each as its parts.
func setElements(out []byte) ([][]string, error) {
	var doc struct {
		Nftables []struct {
			Set *struct {
				Elem []json.RawMessage `json:"elem"`
			} `json:"set"`
		} `json:"nftables"`
	}
	if err := json.Unmarshal(out, &doc); err != nil {
		return nil, err
	}
	var elems [][]string
	for _, item := range doc.Nftables {
		if item.Set == nil {
			continue
		}
		for _, raw := range item.Set.Elem {
			var s string
			if json.Unmarshal(raw, &s) == nil {
				elems = append(elems, []string{s})
				continue
			}
			var c struct {
				Concat []string `json:"concat"`
			}
			if json.Unmarshal(raw, &c) == nil && len(c.Concat) > 0 {
				elems = append(elems, c.Concat)
			}
		}
	}
	return elems, nil
}

// linkExists reports whether a network interface exists on the host (a
// variable for tests).
var linkExists = func(name string) bool {
	_, err := net.InterfaceByName(name)
	return err == nil
}

// boundIP is the address tap is bound to with mac, or "" (the smallest of
// several, which only a hand-made element could leave).
func boundIP(have Bindings, tap, mac string) string {
	ip := ""
	for k := range have.Addrs {
		if k[0] == tap && k[1] == mac && (ip == "" || k[2] < ip) {
			ip = k[2]
		}
	}
	return ip
}

// ReconcileBindings makes the binding sets match want (tap -> binding of the
// listed microVMs whose address is known): missing bindings are added, and
// elements of taps that no longer exist on the host are removed. Elements of
// a tap that exists but is not in want are left alone: that is a microVM
// booting after the listing was taken, which binds itself. It reports what
// it added and removed. A listed VM keeps the address it is bound to.
func ReconcileBindings(ctx context.Context, want map[string]Binding) (added, removed []string, err error) {
	have, err := ListBindings(ctx)
	if err != nil {
		return nil, nil, err
	}
	// A tap nft refuses does not hold up the others, nor the removals, which
	// only this pass makes since deletes keep a VM's binding.
	var errs []error
	for tap, b := range want {
		mac := strings.ToLower(b.MAC)
		// A VM keeps the address it was bound with. The lease file is what
		// dnsmasq made of the VMs' own DHCP messages: a job can release
		// another VM's lease (client ids are not secret) and ask for its
		// address, and following the lease would hand it that address.
		if ip := boundIP(have, tap, mac); ip != "" && ip != b.IP {
			errs = append(errs, fmt.Errorf("%s: its lease now names %s; it keeps %s", tap, b.IP, ip))
			b.IP = ip
		}
		if !have.Taps[tap] || !have.MACs[[2]string{tap, mac}] || !have.Addrs[[3]string{tap, mac, b.IP}] {
			if err := Bind(ctx, b); err != nil {
				// This tap stays as it was; the others still get their turn.
				errs = append(errs, fmt.Errorf("binding %s: %w", tap, err))
				continue
			}
			added = append(added, tap)
		}
		// Addresses a tap had before (a tap name used again by a new VM): the
		// new VM must not send as the old one. Its own binding is in place,
		// so it stays reachable.
		var macs []string
		var addrs [][2]string
		for k := range have.MACs {
			if k[0] == tap && k[1] != mac {
				macs = append(macs, k[1])
			}
		}
		for k := range have.Addrs {
			if k[0] == tap && (k[1] != mac || k[2] != b.IP) {
				addrs = append(addrs, [2]string{k[1], k[2]})
			}
		}
		deleteTuples(ctx, tap, macs, addrs)
	}
	// Every element of a tap that no longer exists, whatever its addresses.
	macs, addrs := map[string][]string{}, map[string][][2]string{}
	taps := map[string]bool{}
	for tap := range have.Taps {
		taps[tap] = true
	}
	for k := range have.MACs {
		taps[k[0]] = true
		macs[k[0]] = append(macs[k[0]], k[1])
	}
	for k := range have.Addrs {
		taps[k[0]] = true
		addrs[k[0]] = append(addrs[k[0]], [2]string{k[1], k[2]})
	}
	for tap := range taps {
		if _, live := want[tap]; live || linkExists(tap) {
			continue
		}
		if err := unbindTap(ctx, tap, macs[tap], addrs[tap]); err != nil {
			errs = append(errs, fmt.Errorf("unbinding %s: %w", tap, err))
			continue
		}
		removed = append(removed, tap)
	}
	return added, removed, errors.Join(errs...)
}
