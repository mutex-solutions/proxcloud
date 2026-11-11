// Package certfile serves a TLS certificate from files that are renewed in place.
package certfile

import (
	"crypto/tls"
	"sync"
	"time"
)

// reload is how long a loaded certificate is served before the files are read again.
var reload = time.Minute

// Files serves a certificate from its files and reads them again at most once a minute, so a renewal
// written in place (cert-manager, a mounted Secret) is picked up without a restart. A read that fails keeps the previous certificate;
// the first one must succeed.
func Files(certFile, keyFile string) (func(*tls.ClientHelloInfo) (*tls.Certificate, error), error) {
	c, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, err
	}
	var mu sync.Mutex
	cur, at := &c, time.Now()
	return func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
		mu.Lock()
		defer mu.Unlock()
		if time.Since(at) >= reload {
			at = time.Now()
			if next, err := tls.LoadX509KeyPair(certFile, keyFile); err == nil {
				cur = &next
			}
		}
		return cur, nil
	}, nil
}
