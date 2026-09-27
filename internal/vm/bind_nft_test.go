package vm

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// The bindings against a real nft: the scripts Bind, Unbind and
// ReconcileBindings write, and the JSON they read back. Needs root and nft:
// FR_NFT_INTEGRATION=1 (see the commit that added it for the container run).
func TestBindingsAgainstRealNft(t *testing.T) {
	if os.Getenv("FR_NFT_INTEGRATION") == "" {
		t.Skip("needs root and nft: set FR_NFT_INTEGRATION=1")
	}
	oldScript, oldList, oldLink := nftScript, nftListSet, linkExists
	nftScript, nftListSet = realNftScript, realNftListSet
	gone := map[string]bool{"fltapGone": true}
	linkExists = func(name string) bool { return !gone[name] }
	t.Cleanup(func() { nftScript, nftListSet, linkExists = oldScript, oldList, oldLink })
	nft := func(script string) {
		t.Helper()
		cmd := exec.Command("nft", "-f", "-")
		cmd.Stdin = strings.NewReader(script)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("nft %q: %v: %s", script, err, out)
		}
	}
	ctx := context.Background()
	_ = exec.Command("nft", "delete", "table", "bridge", "firerunner").Run()

	// A firewall from before bindings: no sets, nothing checked, no noise.
	if err := Bind(ctx, Binding{Tap: "fltapA", MAC: "aa:fc:00:00:00:0a", IP: "10.200.0.10"}); !errors.Is(err, ErrNoBindingSets) {
		t.Fatalf("Bind without sets = %v, want ErrNoBindingSets", err)
	}
	if _, err := ListBindings(ctx); !errors.Is(err, ErrNoBindingSets) {
		t.Fatalf("ListBindings without sets = %v, want ErrNoBindingSets", err)
	}

	nft(`table bridge firerunner {
  set vm_taps { type ifname; }
  set vm_macs { type ifname . ether_addr; }
  set vm_addrs { type ifname . ether_addr . ipv4_addr; }
}
`)
	t.Cleanup(func() { _ = exec.Command("nft", "delete", "table", "bridge", "firerunner").Run() })
	for _, b := range []Binding{
		{Tap: "fltapA", MAC: "AA:FC:00:00:00:0A", IP: "10.200.0.10"},
		{Tap: "fltapGone", MAC: "aa:fc:00:00:00:01", IP: "10.200.0.5"},
		{Tap: "fltapReuse", MAC: "aa:fc:00:00:00:03", IP: "10.200.0.7"},
	} {
		if err := Bind(ctx, b); err != nil {
			t.Fatalf("Bind(%+v) = %v", b, err)
		}
	}
	if err := Bind(ctx, Binding{Tap: "fltapA", MAC: "aa:fc:00:00:00:0a", IP: "10.200.0.10"}); err != nil {
		t.Fatalf("binding again = %v, want no error", err)
	}
	have, err := ListBindings(ctx)
	if err != nil || !have.Taps["fltapA"] || !have.MACs[[2]string{"fltapA", "aa:fc:00:00:00:0a"}] ||
		!have.Addrs[[3]string{"fltapA", "aa:fc:00:00:00:0a", "10.200.0.10"}] || len(have.Taps) != 3 {
		t.Fatalf("listed %+v, %v", have, err)
	}

	added, removed, err := ReconcileBindings(ctx, map[string]Binding{
		"fltapA":     {Tap: "fltapA", MAC: "aa:fc:00:00:00:0a", IP: "10.200.0.10"},
		"fltapReuse": {Tap: "fltapReuse", MAC: "aa:fc:00:00:00:0b", IP: "10.200.0.11"},
	})
	if err != nil || strings.Join(added, ",") != "fltapReuse" || strings.Join(removed, ",") != "fltapGone" {
		t.Fatalf("reconcile: added %v, removed %v, %v", added, removed, err)
	}
	have, _ = ListBindings(ctx)
	if have.Taps["fltapGone"] || have.Addrs[[3]string{"fltapReuse", "aa:fc:00:00:00:03", "10.200.0.7"}] ||
		!have.Addrs[[3]string{"fltapReuse", "aa:fc:00:00:00:0b", "10.200.0.11"}] || !have.Taps["fltapReuse"] {
		t.Fatalf("after reconcile %+v", have)
	}

	if err := Unbind(ctx, Binding{Tap: "fltapA", MAC: "aa:fc:00:00:00:0a", IP: "10.200.0.10"}); err != nil {
		t.Fatalf("Unbind = %v", err)
	}
	if err := Unbind(ctx, Binding{Tap: "fltapA", MAC: "aa:fc:00:00:00:0a", IP: "10.200.0.10"}); err != nil { // already gone: fine
		t.Fatalf("Unbind = %v", err)
	}
	have, _ = ListBindings(ctx)
	if have.Taps["fltapA"] || len(have.MACs) != 1 || len(have.Addrs) != 1 {
		t.Fatalf("after unbind %+v", have)
	}
}
