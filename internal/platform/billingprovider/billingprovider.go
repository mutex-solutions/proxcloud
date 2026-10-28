// Package billingprovider selects which engine charges bills.
//
// ProxCloud ships a complete billing system: profiles, a rating engine, bills, credits, savings
// plans, dunning and suspension all live in this repository and work with nothing but PostgreSQL.
// That is the NATIVE provider, and it is the default — anyone who checks out proxcloud gets working
// billing with no external dependency.
//
// Some deployments run a separate billing service instead, with capabilities that do not live here.
// That is the EXTERNAL provider: proxcloud still resolves what to bill (it owns the cloud
// resource cache, and nothing else can) but hands the rating and accrual to that service over HTTP.
//
// The split is deliberately narrow. Only the CHARGE step differs between the two:
//
//	native    resolve -> rate in-process (optionally fanned out across pods via RabbitMQ)
//	external  resolve -> POST the resolved resources to the billing service, which rates them
//
// Everything else — the client and admin REST surfaces, project cost reads, the billing repository
// — is untouched and identical under both providers. That is what keeps the native path a
// first-class citizen rather than a legacy branch: it is not a fallback, it is the default, and the
// external provider is the special case.
package billingprovider

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/platform/billingclient"
	"github.com/mutex-solutions/proxcloud/internal/platform/billingjob"
	"github.com/mutex-solutions/proxcloud/internal/platform/chargefanout"
)

// Provider names, as they appear in configuration.
const (
	// Native rates bills inside this process, using the billing engine in this repository.
	Native = "native"
	// External delegates rating to a separate billing service over HTTP.
	External = "external"
)

// Provider charges every eligible billing profile for one cadence tick.
type Provider interface {
	// Name is the configured provider name, for logs and the debug trigger's response.
	Name() string
	// Charge runs one charge cycle for the given time unit (minute/hour/month).
	Charge(ctx context.Context, timeUnit string, now time.Time) error
}

// Publisher is the RabbitMQ handle the native provider fans out through (satisfied by *amqp.Client).
type Publisher interface {
	Publish(ctx context.Context, queue string, body []byte) error
}

// nativeProvider charges in-process using the in-tree engine.
//
// When fan-out is enabled AND the broker is connected it publishes one message per active profile
// so any pod can pick it up; otherwise it charges every profile in this process. The fallback is
// deliberate here (unlike the external provider): both paths run the same engine against the same
// database, so charging locally when the broker is down is a scaling downgrade, not a correctness
// risk.
type nativeProvider struct {
	job    *billingjob.Service
	broker func() Publisher // nil, or returns nil, when no broker is connected
	fanout bool
	log    *slog.Logger
}

func (p *nativeProvider) Name() string { return Native }

func (p *nativeProvider) Charge(ctx context.Context, timeUnit string, now time.Time) error {
	if p.fanout && p.broker != nil {
		if rc := p.broker(); rc != nil {
			n, err := chargefanout.Publish(ctx, rc, p.job, timeUnit)
			if err == nil {
				p.log.Info("charge fan-out published", "timeUnit", timeUnit, "count", n)
			}
			return err
		}
		p.log.Warn("rabbit fan-out on but broker not connected — charging in-process this tick",
			"timeUnit", timeUnit)
	}
	return p.job.Charge(ctx, timeUnit, now)
}

// NewNative returns the in-tree provider. broker may be nil when no message broker is configured.
func NewNative(job *billingjob.Service, broker func() Publisher, fanout bool, log *slog.Logger) Provider {
	if log == nil {
		log = slog.Default()
	}
	return &nativeProvider{job: job, broker: broker, fanout: fanout, log: log}
}

// externalProvider resolves locally and hands the rating to a separate billing service.
//
// It deliberately does NOT fall back to the native engine on error. Both engines write bills, and
// during a migration they may write to different databases — a silent fallback could charge the
// same cycle twice, into two places. A visibly failed run is much easier to recover from.
type externalProvider struct {
	resolver billingjob.Resolver
	client   billingjob.Charger
	log      *slog.Logger
}

func (p *externalProvider) Name() string { return External }

func (p *externalProvider) Charge(ctx context.Context, timeUnit string, now time.Time) error {
	return billingjob.RemoteCharge(ctx, p.resolver, p.client, timeUnit, now, p.log)
}

// NewExternal returns the billing-service-backed provider.
func NewExternal(resolver billingjob.Resolver, client billingjob.Charger, log *slog.Logger) Provider {
	if log == nil {
		log = slog.Default()
	}
	return &externalProvider{resolver: resolver, client: client, log: log}
}

// Normalize lower-cases and trims a configured provider name, defaulting to native. An empty
// setting means native on purpose: a deployment that has never heard of this option gets the
// in-tree engine.
func Normalize(name string) string {
	name = strings.ToLower(strings.TrimSpace(name))
	if name == "" {
		return Native
	}
	return name
}

// Validate rejects an unknown provider name rather than silently falling back. A typo in
// PROXCLOUD_BILLING_PROVIDER should stop the deployment, not quietly charge bills with the wrong
// engine.
func Validate(name string) error {
	switch Normalize(name) {
	case Native, External:
		return nil
	default:
		return fmt.Errorf("unknown billing provider %q (want %q or %q)", name, Native, External)
	}
}

// compile-time proof that the concrete client satisfies the charger the external provider needs.
var _ billingjob.Charger = (*billingclient.Client)(nil)
