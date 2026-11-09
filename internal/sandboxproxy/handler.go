package sandboxproxy

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"
)

// Handler returns the proxy handler for sandboxes under domain.
func Handler(domain string, lookup Lookup, log *slog.Logger) http.Handler {
	return WakingHandler(domain, lookup, nil, nil, log)
}

// WakingHandler is Handler that, when a sandbox has no pod, asks waker to resume it (E2B
// autoResume) and then forwards. forget drops the cached miss. waker nil = no wake.
func WakingHandler(domain string, lookup Lookup, waker *Waker, forget func(id string), log *slog.Logger) http.Handler {
	tr := &http.Transport{
		DialContext:        (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		DisableCompression: true,
		MaxIdleConns:       256,
		IdleConnTimeout:    90 * time.Second,
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, port, err := parseTarget(r, domain)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"message": err.Error(), "code": 400})
			return
		}
		if port == EnvdPort && isInternalPath(r.URL.Path) {
			writeJSON(w, http.StatusNotFound, map[string]any{
				"message": "This endpoint is reserved for the control plane and is not reachable through the sandbox URL",
				"path":    r.URL.Path, "code": 404,
			})
			return
		}
		pod, err := lookup(r.Context(), id)
		woke := false
		if errors.Is(err, ErrNotFound) && waker != nil {
			werr := waker.Wake(r.Context(), id, port, r.Header.Get(EnvdTokenHeader), r.Header.Get(TrafficTokenHeader))
			var we *WakeError
			switch {
			case werr == nil:
				woke = true
				if forget != nil {
					forget(id)
				}
				pod, err = lookup(r.Context(), id)
			case errors.As(werr, &we):
				if we.Status >= 500 {
					log.Warn("sandbox wake", "sandbox", id, "err", werr)
				}
				if we.RetryAfter != "" {
					w.Header().Set("Retry-After", we.RetryAfter)
				}
				writeJSON(w, we.Status, map[string]any{"sandboxId": id, "message": we.Message, "code": we.Status})
				return
			case errors.Is(werr, errNoWake):
				if forget != nil {
					forget(id)
				}
				pod, err = lookup(r.Context(), id)
			case !errors.Is(werr, ErrNotFound):
				return // the client went away
			}
		}
		if errors.Is(err, ErrNotReady) {
			w.Header().Set("Retry-After", "1")
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{"sandboxId": id, "message": "The sandbox is starting or stopping", "code": 503})
			return
		}
		if errors.Is(err, ErrNotFound) {
			writeJSON(w, http.StatusBadGateway, map[string]any{"sandboxId": id, "message": "The sandbox was not found", "code": 502})
			return
		}
		if err != nil {
			log.Error("sandbox lookup failed", "sandbox", id, "err", err)
			writeJSON(w, http.StatusBadGateway, map[string]any{"sandboxId": id, "message": "Sandbox lookup failed", "code": 502})
			return
		}
		if port != EnvdPort && pod.TrafficToken != "" {
			got := r.Header.Get(TrafficTokenHeader)
			msg := ""
			if got == "" {
				msg = "Sandbox is secured with traffic access token. Token header '" + TrafficTokenHeader + "' is missing"
			} else if subtle.ConstantTimeCompare([]byte(got), []byte(pod.TrafficToken)) != 1 {
				msg = "Sandbox is secured with traffic access token. Provided token in header '" + TrafficTokenHeader + "' is invalid"
			}
			if msg != "" {
				writeJSON(w, http.StatusForbidden, map[string]any{"sandboxId": id, "message": msg, "code": 403})
				return
			}
		}
		target := &url.URL{Scheme: "http", Host: net.JoinHostPort(pod.IP, strconv.Itoa(port))}
		if woke && port != EnvdPort {
			waitListening(r.Context(), target.Host, 10*time.Second) // the start command may still be starting its server
		}
		(&httputil.ReverseProxy{
			Rewrite: func(pr *httputil.ProxyRequest) {
				pr.SetURL(target)
				if port != EnvdPort {
					pr.Out.Host = pr.In.Host // SetURL rewrites Host; keep the original
				}
			},
			Transport:     tr,
			FlushInterval: -1,
			ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
				var oe *net.OpError
				if errors.As(err, &oe) && oe.Op == "dial" {
					writeJSON(w, http.StatusBadGateway, map[string]any{
						"sandboxId": id, "message": "The sandbox is running but port is not open", "port": port, "code": 502})
					return
				}
				log.Warn("upstream error", "sandbox", id, "port", port, "err", err)
				writeJSON(w, http.StatusBadGateway, map[string]any{"sandboxId": id, "message": "Bad gateway", "code": 502})
			},
		}).ServeHTTP(w, r)
	})
}

func waitListening(ctx context.Context, hostport string, limit time.Duration) {
	deadline := time.Now().Add(limit)
	for time.Now().Before(deadline) && ctx.Err() == nil {
		if c, err := (&net.Dialer{Timeout: time.Second}).DialContext(ctx, "tcp", hostport); err == nil {
			_ = c.Close()
			return
		}
		time.Sleep(250 * time.Millisecond)
	}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// isInternalPath reports an envd control route in any spelling envd's router might accept
// (`//init`, `/init/`, `/./init`, `/INIT`): clean, trim the trailing slash, lowercase.
func isInternalPath(p string) bool {
	c := strings.ToLower(strings.TrimSuffix(path.Clean("/"+p), "/"))
	return internalPaths[c]
}
