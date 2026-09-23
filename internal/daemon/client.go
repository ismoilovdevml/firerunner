package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"time"

	"github.com/ismoilovdevml/firerunner/internal/vm"
)

// Client talks to the daemon over its unix socket. Every call fails fast so
// jobs still run (with a cold boot) when the daemon is down.
type Client struct{ http *http.Client }

func NewClient(socket string) *Client {
	return &Client{http: &http.Client{
		Timeout: 3 * time.Second,
		Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", socket)
		}},
	}}
}

// Claim returns a ready pool VM, or nil if the pool is empty.
func (c *Client) Claim(job string) (*vm.Instance, error) {
	resp, err := c.http.Post("http://daemon/claim?job="+url.QueryEscape(job), "", nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusNoContent:
		return nil, nil
	case http.StatusOK:
		inst := &vm.Instance{}
		return inst, json.NewDecoder(resp.Body).Decode(inst)
	}
	return nil, fmt.Errorf("claim: %s", resp.Status)
}

// Send reports a job event; errors are ignored by callers on purpose.
func (c *Client) Send(e Event) error {
	body, _ := json.Marshal(e)
	resp, err := c.http.Post("http://daemon/event", "application/json", bytes.NewReader(body))
	if err != nil {
		return err
	}
	return resp.Body.Close()
}

// Pool returns the daemon's pool view as raw JSON.
func (c *Client) Pool() (json.RawMessage, error) {
	resp, err := c.http.Get("http://daemon/pool")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var raw json.RawMessage
	return raw, json.NewDecoder(resp.Body).Decode(&raw)
}

// Refresh asks the daemon to replace all idle pool VMs.
func (c *Client) Refresh() error {
	resp, err := c.http.Post("http://daemon/pool/refresh", "", nil)
	if err != nil {
		return err
	}
	return resp.Body.Close()
}
