package host

import (
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"os"
	"time"
)

// CertsValidFor returns an error naming the first certificate in the PEM files
// that is unreadable or expires within d. Empty paths are skipped.
func CertsValidFor(d time.Duration, paths ...string) error {
	deadline := time.Now().Add(d)
	for _, p := range paths {
		if p == "" {
			continue
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		n := 0
		for {
			var b *pem.Block
			b, data = pem.Decode(data)
			if b == nil {
				break
			}
			if b.Type != "CERTIFICATE" {
				continue
			}
			c, err := x509.ParseCertificate(b.Bytes)
			if err != nil {
				return fmt.Errorf("%s: %w", p, err)
			}
			n++
			if c.NotAfter.Before(deadline) {
				return fmt.Errorf("%s expires %s", p, c.NotAfter.UTC().Format(time.DateOnly))
			}
		}
		if n == 0 {
			return fmt.Errorf("%s: no certificate", p)
		}
	}
	return nil
}
