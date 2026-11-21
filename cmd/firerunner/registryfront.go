package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/url"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/ismoilovdevml/firerunner/internal/regfront"
)

const registryFrontUsage = "usage: firerunner registry-front --listen <addr:port> --upstream http://127.0.0.1:<port>"

// cmdRegistryFront runs the pull-only front of one registry mirror
// (systemd: firerunner-registry-front@<name>.service), until SIGTERM.
func cmdRegistryFront(args []string) error {
	listen, upstream, err := parseRegistryFront(args, os.Stderr)
	if err != nil {
		return err
	}
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	f, err := regfront.New(upstream, regfront.DefaultLimits(), log)
	if err != nil {
		return err
	}
	lctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	loops, err := frontLoops(lctx, listen, upstream, net.DefaultResolver.LookupIP, interfaceIPs)
	cancel()
	if err != nil {
		return err
	}
	if loops {
		return fmt.Errorf("--upstream %s is --listen %s itself: give the registry its own (loopback) address", upstream, listen)
	}
	l, err := net.Listen("tcp", listen)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return f.Serve(ctx, l)
}

// parseRegistryFront reads --listen and --upstream; flag errors and -h go
// to out.
func parseRegistryFront(args []string, out io.Writer) (listen, upstream string, err error) {
	fs := flag.NewFlagSet("registry-front", flag.ContinueOnError)
	fs.SetOutput(out)
	fs.StringVar(&listen, "listen", "", "address:port to serve job microVMs on (the bridge side)")
	fs.StringVar(&upstream, "upstream", "", "the registry, http://127.0.0.1:<port>")
	if err := fs.Parse(args); err != nil {
		return "", "", err
	}
	if fs.NArg() > 0 || listen == "" || upstream == "" {
		return "", "", errors.New(registryFrontUsage)
	}
	if _, _, err := net.SplitHostPort(listen); err != nil {
		return "", "", fmt.Errorf("--listen %q: %w", listen, err)
	}
	return listen, upstream, nil
}

// frontLoops reports whether upstream (http://host:port) reaches the
// front's own listen address (host:port): the front would forward every
// request to itself until the connection limit. Names on either side are
// resolved with lookup and compared as addresses and ports. An unspecified
// listen address (0.0.0.0, ::, none) is every local address; an unspecified
// upstream is loopback, where Linux connects it.
//
// local lists the host's addresses, only for an unspecified listen address.
// When it fails, loopback is the only local address known: a unit with
// RestrictAddressFamilies may forbid the netlink socket it needs, and a
// front that does not start is worse than a loop this check cannot see.
func frontLoops(ctx context.Context, listen, upstream string,
	lookup func(ctx context.Context, network, host string) ([]net.IP, error),
	local func() ([]net.IP, error)) (bool, error) {
	lhost, lport, err := net.SplitHostPort(listen)
	if err != nil {
		return false, fmt.Errorf("--listen %q: %w", listen, err)
	}
	u, err := url.Parse(upstream)
	if err != nil {
		return false, fmt.Errorf("--upstream %q: %w", upstream, err)
	}
	lp, lerr := net.DefaultResolver.LookupPort(ctx, "tcp", lport)
	up, uerr := net.DefaultResolver.LookupPort(ctx, "tcp", u.Port())
	if lerr != nil || uerr != nil || lp != up {
		return false, nil
	}
	resolve := func(flag, host string) ([]net.IP, error) {
		if ip := net.ParseIP(host); ip != nil {
			return []net.IP{ip}, nil
		}
		ips, err := lookup(ctx, "ip", host)
		if err != nil {
			return nil, fmt.Errorf("%s: cannot resolve %s to check that it is not the front itself: %w", flag, host, err)
		}
		return ips, nil
	}
	var listenIPs []net.IP
	anyLocal := lhost == ""
	if !anyLocal {
		if listenIPs, err = resolve("--listen", lhost); err != nil {
			return false, err
		}
	}
	for _, ip := range listenIPs {
		anyLocal = anyLocal || ip.IsUnspecified()
	}
	upIPs, err := resolve("--upstream", u.Hostname())
	if err != nil {
		return false, err
	}
	var localIPs []net.IP
	localListed := false
	for _, ip := range upIPs {
		switch {
		case ip.IsUnspecified() && ip.To4() != nil:
			ip = net.IPv4(127, 0, 0, 1)
		case ip.IsUnspecified():
			ip = net.IPv6loopback
		}
		for _, l := range listenIPs {
			if l.Equal(ip) {
				return true, nil
			}
		}
		if !anyLocal {
			continue
		}
		if ip.IsLoopback() {
			return true, nil
		}
		if !localListed {
			localIPs, _ = local()
			localListed = true
		}
		for _, l := range localIPs {
			if l.Equal(ip) {
				return true, nil
			}
		}
	}
	return false, nil
}

// interfaceIPs are the host's own addresses.
func interfaceIPs() ([]net.IP, error) {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return nil, err
	}
	var out []net.IP
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok {
			out = append(out, n.IP)
		}
	}
	return out, nil
}
