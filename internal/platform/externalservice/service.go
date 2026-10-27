package externalservice

import (
	"context"
	"fmt"
	"regexp"

	"github.com/mutex-solutions/proxcloud/pkg/textcrypt"
)

// serviceIDRe is the allow-list for a service id (hex ObjectId / UUID-style token). Requests can choose
// WHICH service to load (x-service-id header, ?serviceId=), so the id is validated here — at the single
// choke point every ES read funnels through — before it is used as a lookup key. Without this, a
// request-supplied id taints the loaded document (the pgdoc codec splices the id into the decoded JSON)
// and the operator-configured endpoint URLs inside it get flagged as attacker-controlled
// (CodeQL go/request-forgery against the ceph Admin Ops client).
var serviceIDRe = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

// Service is the read side for external services: load them and decrypt their secret in place
// (decrypt = textcrypt.DecryptObject over each textual leaf). The pgdoc JSON codec yields
// primitive.D / pgdoc.A for free-form sub-documents, so config + secret are normalized to
// plain map[string]any / []any first — otherwise the typed accessors and the textcrypt walk
// (which match map[string]any) would skip nested objects.
type Service struct {
	repo *Repo
	enc  *textcrypt.Encryptor
}

func NewService(repo *Repo, enc *textcrypt.Encryptor) *Service {
	return &Service{repo: repo, enc: enc}
}

// Get finds a service by id and decrypts it, or returns a not-found error.
func (s *Service) Get(ctx context.Context, id string) (*ExternalService, error) {
	if !serviceIDRe.MatchString(id) {
		return nil, fmt.Errorf("externalservice: invalid service id %q", id)
	}
	es, err := s.repo.FindByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if es == nil {
		return nil, fmt.Errorf("externalservice: service not found: %s", id)
	}
	return s.decrypt(es), nil
}

// List returns all services, decrypted.
func (s *Service) List(ctx context.Context) ([]ExternalService, error) {
	all, err := s.repo.FindAll(ctx)
	if err != nil {
		return nil, err
	}
	for i := range all {
		s.decrypt(&all[i])
	}
	return all, nil
}

// ListByType mirrors listByType(type): services of one type, decrypted.
func (s *Service) ListByType(ctx context.Context, t string) ([]ExternalService, error) {
	all, err := s.repo.FindByType(ctx, t)
	if err != nil {
		return nil, err
	}
	for i := range all {
		s.decrypt(&all[i])
	}
	return all, nil
}

// EncryptSecret is the write-side inverse of decrypt's secret leg: it encrypts the textual
// leaves of a secret value for persistence, so plaintext cloud credentials never hit the datastore.
// A map's string leaves are encrypted (recursing nested objects via EncryptObject); a bare
// string leaf is encrypted directly (EncryptObject only walks maps). pgdoc.M/D
// inputs are normalized first so nested docs are covered. Callers persist the returned value;
// the read path (Service.decrypt → DecryptObject) round-trips it back to plaintext.
func (s *Service) EncryptSecret(v any) any {
	switch t := normalize(v).(type) {
	case map[string]any:
		return s.enc.EncryptObject(t)
	case string:
		return s.enc.Encrypt(t)
	default:
		return v
	}
}

// decrypt normalizes config + secret and decrypts the secret's textual leaves in place.
func (s *Service) decrypt(es *ExternalService) *ExternalService {
	if m, ok := normalize(es.Config).(map[string]any); ok {
		es.Config = m
	}
	es.Secret = s.enc.DecryptObject(normalize(es.Secret))
	return es
}

// normalize recursively converts pgdoc.M/D → map[string]any and pgdoc.A → []any so the
// typed accessors and the textcrypt walk (which switch on map[string]any) see the nested
// objects. The JSONB store decodes documents as plain map[string]any / []any, so normalize just
// recurses over those to produce fresh, uniformly-typed containers.
func normalize(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			out[k] = normalize(val)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, val := range t {
			out[i] = normalize(val)
		}
		return out
	default:
		return v
	}
}
