package daemon

import (
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ismoilovdevml/firerunner/internal/config"
)

// Image preload by use (pool.preload_top): the daemon remembers the `image:`
// of the last recentImages jobs and pulls the most used ones into pool VMs,
// next to pool.preload_images, so those jobs skip the pull.
const (
	recentImages = 200
	// minImageUses: an image one job used is not worth a pull into every pool VM.
	minImageUses = 2
	// failedImageFor: an image a pool VM could not pull (private: pool VMs
	// have no credentials) is not tried again for this long.
	failedImageFor = 24 * time.Hour
)

// imageRef is what may be preloaded: a plain image reference, nothing a
// shell or docker would read as anything else (job variables set the image).
var imageRef = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/:@+-]{0,254}$`)

type imageStats struct {
	mu     sync.Mutex
	recent []string             // ring of the last recentImages job images
	next   int                  // where the next one goes once the ring is full
	failed map[string]time.Time // image -> when a pool VM failed to pull it
}

// note records the image a job ran in.
func (s *imageStats) note(image string) {
	if !imageRef.MatchString(image) {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.recent) < recentImages {
		s.recent = append(s.recent, image)
		return
	}
	s.recent[s.next] = image
	s.next = (s.next + 1) % recentImages
}

// pullFailed keeps an image out of the preload for failedImageFor.
func (s *imageStats) pullFailed(image string, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failed == nil {
		s.failed = map[string]time.Time{}
	}
	s.failed[image] = now
	for img, at := range s.failed {
		if now.Sub(at) > failedImageFor {
			delete(s.failed, img)
		}
	}
}

// top returns up to n images used by at least minImageUses of the recent
// jobs, the most used first, leaving out the ones in skip and recent failures.
func (s *imageStats) top(n int, skip []string, now time.Time) []string {
	if n <= 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	uses := map[string]int{}
	for _, img := range s.recent {
		uses[img]++
	}
	var out []string
	for img, c := range uses {
		at, failed := s.failed[img]
		if c >= minImageUses && !slices.Contains(skip, img) && (!failed || now.Sub(at) > failedImageFor) {
			out = append(out, img)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if uses[out[i]] != uses[out[j]] {
			return uses[out[i]] > uses[out[j]]
		}
		return out[i] < out[j]
	})
	if len(out) > n {
		out = out[:n]
	}
	return out
}

// preloadScript pulls the configured images (any failure fails the preload,
// as before) and then the images chosen by use, each on its own: one a pool
// VM cannot pull is reported on a line of its own and skipped.
func preloadScript(static, auto []string) string {
	var b strings.Builder
	b.WriteString("set -e")
	for _, img := range static {
		b.WriteString("; docker pull -q " + shellQuote(img))
	}
	if len(auto) > 0 {
		b.WriteString("; set +e")
		for _, img := range auto {
			q := shellQuote(img)
			b.WriteString("; docker pull -q " + q + " >/dev/null 2>&1 || echo " + shellQuote(preloadFailedMark+img))
		}
	}
	return b.String()
}

const preloadFailedMark = "firerunner-preload-failed "

// failedPulls reads the images preloadScript reported as not pulled.
func failedPulls(out string) []string {
	var failed []string
	for _, line := range strings.Split(out, "\n") {
		if img, ok := strings.CutPrefix(strings.TrimSpace(line), preloadFailedMark); ok {
			failed = append(failed, img)
		}
	}
	return failed
}

// preloadImages is what a pool VM booted with cfg pulls: pool.preload_images,
// then the pool.preload_top most used job images.
func (d *Daemon) preloadImages(cfg config.Config) (static, auto []string) {
	return cfg.Pool.PreloadImages, d.images.top(cfg.Pool.PreloadTop, cfg.Pool.PreloadImages, time.Now())
}
