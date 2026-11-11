package mail

import (
	"context"
	"errors"
	"maps"

	"github.com/mutex-solutions/proxcloud/internal/platform/message"
)

var ErrPreparation = errors.New("mail: template preparation failed")

// PrepareTemplateSnapshot renders the ONE template already read in the business
// transaction. It never performs a second lookup that could confuse concurrent
// deletion with an intentionally disabled template.
func (s *Service) PrepareTemplateSnapshot(ctx context.Context, title, body string, disabled bool, vars map[string]any) (Message, bool, error) {
	if s == nil {
		return Message{}, false, ErrPreparation
	}
	copy := *s
	copy.render = func(_ context.Context, _ string, vars map[string]any) (string, string, bool, error) {
		if disabled {
			return "", "", false, nil
		}
		r := message.RenderSnapshot(title, body, vars)
		return r.Title, r.Body, true, nil
	}
	return copy.PrepareTemplate(ctx, "", vars)
}

// PrepareTemplate renders an in-memory draft without resolving/calling a Mailer.
// false means the caller's known-disabled template was suppressed. The caller
// must distinguish a missing template BEFORE calling (legacy RenderFunc combines
// missing/disabled), and propagate errors before its business transaction commits.
func (s *Service) PrepareTemplate(ctx context.Context, key string, vars map[string]any) (Message, bool, error) {
	if s == nil || s.render == nil {
		return Message{}, false, ErrPreparation
	}
	vars = maps.Clone(vars)
	if vars == nil {
		vars = map[string]any{}
	}
	if _, ok := vars["businessName"]; !ok {
		name := s.business
		if s.businessFn != nil {
			if value := s.businessFn(ctx); value != "" {
				name = value
			}
		}
		vars["businessName"] = name
	}
	title, body, ok, err := s.render(ctx, key, vars)
	if err != nil {
		return Message{}, false, ErrPreparation // renderer errors may contain sensitive data
	}
	if !ok {
		return Message{}, false, nil
	}
	return Message{Subject: title, HTMLBody: wrapShell(body)}, true, nil
}
