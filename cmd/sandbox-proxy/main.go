// Command sandbox-proxy routes https://<port>-<sandboxID>.<domain> to the sandbox pod (docs/managed-sandbox.md §4).
// TLS ends here: the wildcard certificate is a mounted Secret, read again when it is renewed. The public entry in
// front only passes TCP through.
//
//	sandbox-proxy             serve (in the sandbox cluster)
//	sandbox-proxy cert-sync   copy the wildcard certificate from the services cluster to the sandbox cluster
package main

import (
	"context"
	"crypto/tls"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/cloud/kamajik8s"
	"github.com/mutex-solutions/proxcloud/internal/sandboxproxy"
	"github.com/mutex-solutions/proxcloud/pkg/certfile"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()
	run := serve
	if len(os.Args) > 1 && os.Args[1] == "cert-sync" {
		run = certSync
	}
	if err := run(ctx, log); err != nil {
		log.Error("sandbox-proxy", "err", err)
		os.Exit(1)
	}
}

// serve: SANDBOX_DOMAIN (required); SANDBOX_TLS_DIR with tls.crt and tls.key (HTTPS on :8443; unset = plain HTTP on
// :8080, for tests only); PROXCLOUD_WAKE_URL and PROXCLOUD_WAKE_TOKEN for E2B autoResume (unset = off). Health on :8081.
func serve(ctx context.Context, log *slog.Logger) error {
	domain := os.Getenv("SANDBOX_DOMAIN")
	if domain == "" {
		return errors.New("SANDBOX_DOMAIN is required")
	}
	k, err := kamajik8s.NewInCluster()
	if err != nil {
		return err
	}
	ctx, stop := context.WithCancel(ctx)
	defer stop()
	listen := func(s *http.Server) {
		var err error
		if s.TLSConfig != nil {
			err = s.ListenAndServeTLS("", "")
		} else {
			err = s.ListenAndServe()
		}
		if !errors.Is(err, http.ErrServerClosed) {
			log.Error("listen", "addr", s.Addr, "err", err)
			stop()
		}
	}
	// Health first: alive while the proxy waits for its certificate, ready once it serves.
	var ready atomic.Bool
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		if !ready.Load() {
			http.Error(w, "waiting for the certificate", http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte("ok"))
	})
	health := &http.Server{Addr: ":8081", Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go listen(health)

	cache := sandboxproxy.NewCache(sandboxproxy.K8sLookup(k), 2*time.Second, 2*time.Second) // short hits: a killed sandbox's IP may be reused by another pod
	waker := sandboxproxy.NewWaker(os.Getenv("PROXCLOUD_WAKE_URL"), os.Getenv("PROXCLOUD_WAKE_TOKEN"))
	// No WriteTimeout/ReadTimeout: sandbox responses and WebSockets stream indefinitely.
	proxy := &http.Server{Addr: ":8080", Handler: sandboxproxy.WakingHandler(domain, cache.Lookup, waker, cache.Forget, log),
		ReadHeaderTimeout: 10 * time.Second}
	if dir := os.Getenv("SANDBOX_TLS_DIR"); dir != "" {
		// The Secret is empty until cert-sync's first copy: wait for it rather than crash-loop into a long backoff.
		var get func(*tls.ClientHelloInfo) (*tls.Certificate, error)
		for get == nil && ctx.Err() == nil {
			if get, err = certfile.Files(filepath.Join(dir, "tls.crt"), filepath.Join(dir, "tls.key")); err != nil {
				log.Warn("waiting for the certificate", "dir", dir, "err", err)
				select {
				case <-ctx.Done():
				case <-time.After(10 * time.Second):
				}
			}
		}
		proxy.Addr = ":8443"
		proxy.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS12, GetCertificate: get}
	}
	if ctx.Err() == nil {
		go listen(proxy)
		ready.Store(true)
		log.Info("sandbox-proxy started", "domain", domain, "addr", proxy.Addr, "tls", proxy.TLSConfig != nil, "wake", waker != nil)
	}
	<-ctx.Done()
	sctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	_ = proxy.Shutdown(sctx)
	_ = health.Shutdown(sctx)
	return nil
}

// certSync runs on the services cluster: CERT_SOURCE (namespace/name, read with the pod's service account),
// CERT_TARGET (namespace/name on the sandbox cluster), TARGET_KUBECONFIG (a credential that may get and patch that
// one Secret) and SANDBOX_DOMAIN (a certificate that does not cover *.<domain> is not copied). It copies every
// SYNC_INTERVAL (default 1m).
func certSync(ctx context.Context, log *slog.Logger) error {
	from, err1 := ref(os.Getenv("CERT_SOURCE"))
	to, err2 := ref(os.Getenv("CERT_TARGET"))
	domain := os.Getenv("SANDBOX_DOMAIN")
	if domain == "" {
		err2 = errors.Join(err2, errors.New("SANDBOX_DOMAIN is required"))
	}
	if err := errors.Join(err1, err2); err != nil {
		return err
	}
	src, err := kamajik8s.NewInCluster()
	if err != nil {
		return err
	}
	kc, err := os.ReadFile(os.Getenv("TARGET_KUBECONFIG"))
	if err != nil {
		return err
	}
	dst, err := kamajik8s.New(string(kc))
	if err != nil {
		return err
	}
	every := time.Minute
	if d, err := time.ParseDuration(os.Getenv("SYNC_INTERVAL")); err == nil && d > 0 {
		every = d
	}
	log.Info("cert-sync started", "from", from, "to", to, "every", every.String())
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		switch changed, err := sandboxproxy.SyncCert(ctx, src, from, dst, to, domain); {
		case err != nil:
			log.Error("cert-sync", "err", err)
		case changed:
			log.Info("cert-sync: certificate copied", "to", to)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}

func ref(s string) (sandboxproxy.SecretRef, error) {
	ns, name, ok := strings.Cut(s, "/")
	if !ok || ns == "" || name == "" {
		return sandboxproxy.SecretRef{}, errors.New("want namespace/name, got " + s)
	}
	return sandboxproxy.SecretRef{Namespace: ns, Name: name}, nil
}
