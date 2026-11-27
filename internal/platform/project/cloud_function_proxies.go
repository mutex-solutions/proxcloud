package project

// cloud_function_proxies.go — what the portal knows of the sandbox-proxy replicas that serve a provider's functions
// (the `function_proxies` records), and the bearer those replicas call it with.
//
// A replica reports every few seconds which functions have traffic and how much (functionActivity). Its last
// complete report is kept per replica and provider. Three things read the records: scaling on load adds up the
// load of the replicas heard from lately (scaleOnLoad), the operator's gate takes a fresh report as the proof that a
// proxy of this provider reaches this portal with the provider's token (FunctionProxiesReporting), and the deploy
// sync forgets the replicas that are gone.

import (
	"context"
	"crypto/subtle"
	"log/slog"
	"sync"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
)

const (
	// activityEveryDefault is the report period, in seconds, taken for a proxy that names none or one that is out
	// of bounds. Reports that come more often than this are the normal case (sandbox-proxy reports every 5 s).
	activityEveryDefault = 30
	// fnProxyKeep is how long a replica's last report stays on record: FunctionProxyWindow.
	fnProxyKeep = FunctionProxyWindow
	// functionTokenTTL is how long the providers' function wake tokens are kept in memory (functionTokenServices).
	// A token an operator's apply replaced is therefore still accepted for this long.
	functionTokenTTL = 3 * time.Second
	// fnReaperID is the record the reaper keeps of its own last run, beside the replicas' (reaperRanLately). It has
	// no serviceId, so no read of a provider's replicas returns it.
	fnReaperID = "reaper"
	// functionReaperGap: a reaper that did not run for this long was away (the scheduler was off, the API or its
	// database was down), and so were the reports that would have kept serving functions awake. Three periods of
	// the reaper.
	functionReaperGap = 45 * time.Second
)

// FunctionProxyWindow is how old a sandbox-proxy replica's last report may be to count as "this provider's proxy
// reaches the portal", and how long that report stays on record: the gate that lets an operator switch functions on
// asks for a report younger than it (and prints it), and the deploy sync forgets a replica not heard from for as
// long. One constant for both, so a record the gate may count is never one the sync has already removed.
const FunctionProxyWindow = 90 * time.Second

// fnLoad is the load of one function through one sandbox-proxy replica over a report period: the most requests it
// had in flight at once, and how many requests it started.
type fnLoad struct {
	Peak     int `json:"peak"`
	Requests int `json:"requests"`
}

// fnProxy is a `function_proxies` record: what the portal last heard of one sandbox-proxy replica of a provider (its
// id is "<serviceId>/<replica>"; the replica of a proxy that does not name itself is ""). At is when its last
// complete activity report was received, Every the period, in seconds, it reports with, and Load the load per
// function that report carried (scaleOnLoad).
type fnProxy struct {
	ServiceID string            `json:"serviceId"`
	At        time.Time         `json:"at"`
	Every     int               `json:"every"`
	Load      map[string]fnLoad `json:"load,omitempty"`
}

// fnProxies is the function_proxies collection.
func (h *Handler) fnProxies() *pgdoc.Store { return h.fns.DB().C("function_proxies") }

// proxiesOf are the replicas of a provider heard from within fnProxyKeep.
func (h *Handler) proxiesOf(ctx context.Context, serviceID string, now time.Time) ([]fnProxy, error) {
	var proxies []fnProxy
	err := h.fnProxies().Find(ctx, pgdoc.M{"serviceId": serviceID, "at": pgdoc.M{"$gt": now.Add(-fnProxyKeep)}}, &proxies)
	return proxies, err
}

// FunctionProxiesReporting is how many sandbox-proxy replicas of the provider sent this portal a complete activity
// report within fnProxyKeep. A report is only taken with the provider's own function wake token, so one on record
// is the proof the operator's gate asks for: the proxy has the token, reaches this portal, and the portal's routes
// answer it. An empty report counts: a proxy reports before any function exists.
func (h *Handler) FunctionProxiesReporting(ctx context.Context, serviceID string) (int, error) {
	if h.fns == nil {
		return 0, nil
	}
	proxies, err := h.proxiesOf(ctx, serviceID, time.Now().UTC())
	return len(proxies), err
}

// forgetProxies deletes the records of the replicas not heard from for fnProxyKeep (every proxy pod that ever
// reported has one), and the reaper's own when it is as old.
func (h *Handler) forgetProxies(ctx context.Context, now time.Time) {
	if _, err := h.fnProxies().DeleteMany(ctx, pgdoc.M{"at": pgdoc.M{"$lt": now.Add(-fnProxyKeep)}}); err != nil {
		slog.Warn("function sync: forget the sandbox-proxy replicas that are gone", "err", err)
	}
}

// reaperRanLately records this run of the reaper and reports whether the one before it was within functionReaperGap.
// Where it was not, nobody took activity reports either, or nobody acted on them: every function's lastActiveAt is
// as old as the gap, whatever it is serving. That run stops nothing for being idle; the replicas report every few
// seconds, so by the next run the functions with traffic are stamped again.
//
// ponytail: one period of grace after a gap. A replica whose first report takes longer than a reaper period to
// arrive after the portal is back can still lose a serving function to the idle rule; compare each function with
// its replicas' reports if that is ever seen.
func (h *Handler) reaperRanLately(ctx context.Context, now time.Time) bool {
	var last fnProxy
	found, err := h.fnProxies().Get(ctx, fnReaperID, &last)
	if uerr := h.fnProxies().Upsert(ctx, fnReaperID, pgdoc.M{"at": now}); uerr != nil {
		slog.Warn("function reaper: record the run", "err", uerr)
	}
	return err == nil && found && now.Sub(last.At) <= functionReaperGap
}

// fnTokens are the function wake tokens of the sandbox providers, by provider id, as last read.
type fnTokens struct {
	mu     sync.Mutex
	at     time.Time
	tokens map[string]string
}

// functionTokenServices returns the ids of the sandbox providers whose function wake token (secret
// functionWakeToken) is bearer: none for a bearer no provider has. It is what both proxy routes check before they
// look anything up. The token is the functions' own: a provider's proxyWakeToken (the sandbox wake route's bearer)
// is never compared, so neither token opens the other's route.
//
// Both routes are reachable without a user credential, so the providers are not read from the database on each
// call: the tokens are kept in memory for functionTokenTTL. Every provider is compared, in constant time each, so
// the time taken does not say which one matched.
func (h *Handler) functionTokenServices(ctx context.Context, bearer string) []string {
	if h.esSvc == nil || bearer == "" {
		return nil
	}
	h.fnTokens.mu.Lock()
	tokens := h.fnTokens.tokens
	if tokens == nil || time.Since(h.fnTokens.at) > functionTokenTTL {
		es, err := h.esSvc.List(ctx)
		if err != nil {
			h.fnTokens.mu.Unlock()
			slog.Warn("function: read the providers' wake tokens", "err", err)
			return nil
		}
		tokens = map[string]string{}
		for i := range es {
			if tok := es[i].SandboxConfig().FunctionWakeToken; es[i].IsSandbox() && tok != "" {
				tokens[es[i].ID] = tok
			}
		}
		h.fnTokens.tokens, h.fnTokens.at = tokens, time.Now()
	}
	h.fnTokens.mu.Unlock()
	var ids []string
	for id, tok := range tokens {
		if subtle.ConstantTimeCompare([]byte(bearer), []byte(tok)) == 1 {
			ids = append(ids, id)
		}
	}
	return ids
}

// ForgetFunctionTokens drops the tokens kept in memory: the next call of a proxy route reads the providers again.
// For whoever writes a provider's token in this process; another API replica keeps its copy for functionTokenTTL.
func (h *Handler) ForgetFunctionTokens() {
	h.fnTokens.mu.Lock()
	h.fnTokens.tokens = nil
	h.fnTokens.mu.Unlock()
}
