package csigw

import (
	"crypto/tls"
	"sync"
	"time"
)

// certReload is how long a loaded certificate is served before the files are read again.
const certReload = time.Minute

// CertFiles serves the gateway's certificate from its files and reads them again at most once a minute, so a renewal
// written in place (cert-manager) is picked up without a restart. A read that fails keeps the previous certificate;
// the first one must succeed.
func CertFiles(certFile, keyFile string) (func(*tls.ClientHelloInfo) (*tls.Certificate, error), error) {
	c, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, err
	}
	var mu sync.Mutex
	cur, at := &c, time.Now()
	return func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
		mu.Lock()
		defer mu.Unlock()
		if time.Since(at) >= certReload {
			at = time.Now()
			if next, err := tls.LoadX509KeyPair(certFile, keyFile); err == nil {
				cur = &next
			}
		}
		return cur, nil
	}, nil
}
