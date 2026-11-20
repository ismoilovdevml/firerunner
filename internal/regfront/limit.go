package regfront

import (
	"log/slog"
	"net"
	"sync"
)

// limitListener drops a connection past perClient open from its address or
// total open from all (0: no limit). It counts connections, not requests:
// HTTP/1.1 runs one request at a time on each, and a connection is what
// holds a file descriptor and a goroutine whether or not it sends anything.
// A dropped client sees a connection error, which Docker and containerd
// take as a reason to pull from the registry itself.
type limitListener struct {
	net.Listener
	perClient, total int
	log              *slog.Logger

	mu   sync.Mutex
	open map[string]int
	all  int
}

func newLimitListener(l net.Listener, perClient, total int, log *slog.Logger) *limitListener {
	return &limitListener{Listener: l, perClient: perClient, total: total, log: log, open: map[string]int{}}
}

func (l *limitListener) Accept() (net.Conn, error) {
	for {
		c, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		client := hostOf(c.RemoteAddr().String())
		if l.take(client) {
			return &limitedConn{Conn: c, release: func() { l.release(client) }}, nil
		}
		l.log.Warn("registry connection refused: too many connections", "client", client)
		_ = c.Close()
	}
}

func (l *limitListener) take(client string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if (l.total > 0 && l.all >= l.total) || (l.perClient > 0 && l.open[client] >= l.perClient) {
		return false
	}
	l.open[client]++
	l.all++
	return true
}

func (l *limitListener) release(client string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.open[client]--; l.open[client] <= 0 {
		delete(l.open, client)
	}
	l.all--
}

// limitedConn gives its place back once, on the first Close: net/http may
// close a connection more than once.
type limitedConn struct {
	net.Conn
	once    sync.Once
	release func()
}

func (c *limitedConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(c.release)
	return err
}

// CloseWrite lets net/http half-close the connection after an answer it
// sends before reading the whole request, so the client still reads it.
func (c *limitedConn) CloseWrite() error {
	if cw, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return cw.CloseWrite()
	}
	return c.Conn.Close()
}
