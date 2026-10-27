package admin

// audit_hydrate.go hydrates the admin audit-log list: each event is returned
// WRAPPED as AuditEventDto {event, organization, project, user}, NOT the bare AuditEvent. The
// admin UI's table binds row.event.* / row.organization / row.project / row.user, so a bare-event
// response renders 24 blank rows (data.length still drives the "Showing N items" footer). The org/
// project/user refs are resolved in one batched read each (orgId/projectId/actor.id→sub);
// refs that don't resolve stay null (the field is omitted).

import (
	"context"
	"strings"

	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/internal/platform/audit"
)

// auditEventDto wraps an event (null org/project/user omitted; event always
// present). The nested refs use the same {id,name} / {id,sub,firstName,lastName,email} shapes.
type auditEventDto struct {
	Event        audit.AuditEvent `json:"event"`
	Organization *auditOrgRef     `json:"organization,omitempty"`
	Project      *auditProjectRef `json:"project,omitempty"`
	User         *auditUserRef    `json:"user,omitempty"`
}

type auditOrgRef struct {
	ID   string `json:"id,omitempty"`
	Name string `json:"name,omitempty"`
}

type auditProjectRef struct {
	ID   string `json:"id,omitempty"`
	Name string `json:"name,omitempty"`
}

type auditUserRef struct {
	ID        string `json:"id,omitempty"`
	Sub       string `json:"sub,omitempty"`
	FirstName string `json:"firstName,omitempty"`
	LastName  string `json:"lastName,omitempty"`
	Email     string `json:"email,omitempty"`
}

// HydrateAuditEvents batch-resolves org/project/user refs and wraps
// each event in an AuditEventDto. Returns a non-nil slice (empty when events is empty).
func (r *Repo) HydrateAuditEvents(ctx context.Context, events []audit.AuditEvent) ([]auditEventDto, error) {
	out := make([]auditEventDto, 0, len(events))
	if len(events) == 0 {
		return out, nil
	}

	orgIDs := distinctNonBlank(events, func(e audit.AuditEvent) string { return e.OrganizationID })
	projectIDs := distinctNonBlank(events, func(e audit.AuditEvent) string { return e.ProjectID })
	actorSubs := distinctNonBlank(events, func(e audit.AuditEvent) string {
		if e.Actor != nil && e.Actor.Type == audit.ActorUser {
			return e.Actor.ID
		}
		return ""
	})

	orgMap := map[string]*auditOrgRef{}
	if len(orgIDs) > 0 {
		docs, err := r.findByIDs(ctx, "organization", orgIDs)
		if err != nil {
			return nil, err
		}
		for _, d := range docs {
			id := idToString(d["_id"])
			name, _ := d["name"].(string)
			orgMap[id] = &auditOrgRef{ID: id, Name: name}
		}
	}

	projectMap := map[string]*auditProjectRef{}
	if len(projectIDs) > 0 {
		docs, err := r.findByIDs(ctx, "project", projectIDs)
		if err != nil {
			return nil, err
		}
		for _, d := range docs {
			id := idToString(d["_id"])
			name, _ := d["name"].(string)
			projectMap[id] = &auditProjectRef{ID: id, Name: name}
		}
	}

	userMap := map[string]*auditUserRef{}
	if len(actorSubs) > 0 {
		var docs []pgdoc.M
		if err := r.c("users").Find(ctx, pgdoc.M{"sub": pgdoc.M{"$in": actorSubs}}, &docs); err != nil {
			return nil, err
		}
		for _, d := range docs {
			sub, _ := d["sub"].(string)
			if sub == "" {
				continue
			}
			ref := &auditUserRef{ID: idToString(d["_id"]), Sub: sub}
			ref.FirstName, _ = d["firstName"].(string)
			ref.LastName, _ = d["lastName"].(string)
			ref.Email, _ = d["email"].(string)
			userMap[sub] = ref
		}
	}

	for _, e := range events {
		dto := auditEventDto{Event: e}
		if strings.TrimSpace(e.OrganizationID) != "" {
			dto.Organization = orgMap[e.OrganizationID]
		}
		if strings.TrimSpace(e.ProjectID) != "" {
			dto.Project = projectMap[e.ProjectID]
		}
		if e.Actor != nil && e.Actor.Type == audit.ActorUser {
			dto.User = userMap[e.Actor.ID]
		}
		out = append(out, dto)
	}
	return out, nil
}

// findByIDs batch-reads every doc in collection whose id is one of ids (plain string ids).
func (r *Repo) findByIDs(ctx context.Context, collection string, ids []string) ([]pgdoc.M, error) {
	var docs []pgdoc.M
	if err := r.c(collection).Find(ctx, pgdoc.M{"_id": pgdoc.M{"$in": ids}}, &docs); err != nil {
		return nil, err
	}
	return docs, nil
}

// idToString renders a stored `_id` (a plain string) as a string ("" for anything else),
// so the resolved ref keys match the event's stored organizationId/projectId.
func idToString(v any) string {
	s, _ := v.(string)
	return s
}

// distinctNonBlank collects the distinct, non-blank values returned by sel over events (preserving
// first-seen order).
func distinctNonBlank(events []audit.AuditEvent, sel func(audit.AuditEvent) string) []string {
	seen := map[string]struct{}{}
	out := []string{}
	for _, e := range events {
		v := sel(e)
		if strings.TrimSpace(v) == "" {
			continue
		}
		if _, ok := seen[v]; ok {
			continue
		}
		seen[v] = struct{}{}
		out = append(out, v)
	}
	return out
}
