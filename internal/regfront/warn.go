package regfront

import (
	"log/slog"
	"slices"
	"sync"
	"time"
)

// maxWarnKeys bounds the problems tracked one by one. Past it, a new
// client's repeats of a problem are counted together under client "*":
// clients the front has never seen cannot grow the map without end.
const maxWarnKeys = 4096

// warnLimiter logs a problem (a reason, from a client) at Warn the first
// time, then at most once per every: the repeats in between are counted and
// logged as one line with "repeats". A job looping on /v2/_catalog, or
// reconnecting at its connection limit, would otherwise flood journald, whose
// per-unit rate limit then drops the warnings that matter.
type warnLimiter struct {
	log   *slog.Logger
	every time.Duration // 0: every problem is logged

	mu   sync.Mutex
	seen map[warnKey]*warning
}

type warnKey struct{ reason, client string }

type warning struct {
	msg     string
	args    []any     // the latest repeat's
	logged  time.Time // the last line about it
	last    time.Time // its latest repeat
	repeats int       // since the last line
}

func newWarnLimiter(log *slog.Logger, every time.Duration) *warnLimiter {
	return &warnLimiter{log: log, every: every, seen: map[warnKey]*warning{}}
}

func (w *warnLimiter) warn(reason, client, msg string, args ...any) {
	if w.every <= 0 {
		w.log.Warn(msg, args...)
		return
	}
	now := time.Now()
	k := warnKey{reason, client}
	w.mu.Lock()
	e := w.seen[k]
	if e == nil && len(w.seen) >= maxWarnKeys {
		k.client = "*"
		e = w.seen[k]
	}
	if e == nil {
		w.seen[k] = &warning{msg: msg, logged: now, last: now}
		w.mu.Unlock()
		w.log.Warn(msg, args...)
		return
	}
	e.repeats++
	e.args, e.last = args, now
	if now.Sub(e.logged) < w.every {
		w.mu.Unlock()
		return
	}
	n := e.repeats
	e.repeats, e.logged = 0, now
	w.mu.Unlock()
	w.log.Warn(msg, w.summary(args, n)...)
}

func (w *warnLimiter) summary(args []any, repeats int) []any {
	return append(slices.Clip(args), "repeats", repeats, "period", w.every.String())
}

// flush logs the repeats of problems last logged every or longer ago (all
// of them with final) and forgets the problems that stopped, so their next
// occurrence is logged at once.
func (w *warnLimiter) flush(final bool) {
	if w.every <= 0 {
		return
	}
	now := time.Now()
	type line struct {
		msg  string
		args []any
	}
	var lines []line
	w.mu.Lock()
	for k, e := range w.seen {
		switch {
		case e.repeats > 0 && (final || now.Sub(e.logged) >= w.every):
			lines = append(lines, line{e.msg, w.summary(e.args, e.repeats)})
			e.repeats, e.logged = 0, now
		case e.repeats == 0 && now.Sub(e.last) >= w.every:
			delete(w.seen, k)
		}
	}
	w.mu.Unlock()
	for _, l := range lines {
		w.log.Warn(l.msg, l.args...)
	}
}

// run flushes until stop is closed, often enough that a summary comes at
// most a quarter period late.
func (w *warnLimiter) run(stop <-chan struct{}) {
	if w.every <= 0 {
		return
	}
	t := time.NewTicker(max(w.every/4, time.Millisecond))
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			w.flush(false)
		}
	}
}
