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
	// The front forwarding to itself would loop every request until the
	// connection limit.
	if u, err := url.Parse(upstream); err == nil && u.Host == listen {
		return "", "", fmt.Errorf("--upstream %s is --listen itself: give the registry its own (loopback) address", upstream)
	}
	return listen, upstream, nil
}
