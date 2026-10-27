package admin

import (
	"context"

	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
)

// priceplan_repo.go holds the PricePlan / PricePlanRule store helpers the admin mutations need.
// PricePlan and PricePlanRule use a String `id` field stored as the String `_id` (a fresh
// 24-char hex string on a null id).
//
// The documents are kept as free-form pgdoc.M because PricePlanRule prices/filters/modifiers carry
// nested tier money and arbitrary attribute shapes that are passed through from the request body. The
// money round-tripping (a decimal string vs the request's JSON numbers) is a known fidelity gap,
// deferred.

// InsertPricePlanDoc saves a NEW plan: assign a freshly-generated hex
// String `_id` and insert. The doc is returned with `_id` set (callers shapeDoc it).
func (r *Repo) InsertPricePlanDoc(ctx context.Context, doc pgdoc.M) (pgdoc.M, error) {
	delete(doc, "id")
	delete(doc, "_id")
	doc["_id"] = pgdoc.NewID()
	if _, err := r.c(pricePlanCollection).InsertOne(ctx, doc); err != nil {
		return nil, err
	}
	return doc, nil
}

// PricePlanByID loads a plan by id: the raw plan doc, or (nil,nil) when absent.
func (r *Repo) PricePlanByID(ctx context.Context, id string) (pgdoc.M, error) {
	return r.FindDoc(ctx, pricePlanCollection, id)
}

// ReplacePricePlanDoc saves an EXISTING plan: id-preserving replace.
func (r *Repo) ReplacePricePlanDoc(ctx context.Context, id string, doc pgdoc.M) error {
	return r.ReplaceDoc(ctx, pricePlanCollection, id, doc)
}

// DeletePricePlanDoc deletes a plan → deleted count.
func (r *Repo) DeletePricePlanDoc(ctx context.Context, id string) (int64, error) {
	return r.DeleteDoc(ctx, pricePlanCollection, id)
}

// PricePlanUsedInExternalServices reports whether any externalService's
// `pricePlanIds` array contains this id (array containment).
func (r *Repo) PricePlanUsedInExternalServices(ctx context.Context, id string) (bool, error) {
	return r.c("externalService").Exists(ctx, pgdoc.M{"pricePlanIds": pgdoc.M{"$contains": id}})
}

// PricePlanUsedInProjects reports whether any project's services
// array has an element referencing this price plan (`services[].pricePlanId`).
func (r *Repo) PricePlanUsedInProjects(ctx context.Context, id string) (bool, error) {
	return r.c("project").Exists(ctx, pgdoc.M{"services": pgdoc.M{"$contains": pgdoc.M{"pricePlanId": id}}})
}

// ── PricePlanRule ─────────────────────────────────────────────────────────────────────────────────

// InsertPricePlanRuleDoc saves a NEW rule (fresh hex String `_id`).
func (r *Repo) InsertPricePlanRuleDoc(ctx context.Context, doc pgdoc.M) (pgdoc.M, error) {
	delete(doc, "id")
	delete(doc, "_id")
	doc["_id"] = pgdoc.NewID()
	if _, err := r.c(pricePlanRuleCollection).InsertOne(ctx, doc); err != nil {
		return nil, err
	}
	return doc, nil
}

// PricePlanRuleByID loads a rule by id: the raw rule doc, or (nil,nil).
func (r *Repo) PricePlanRuleByID(ctx context.Context, id string) (pgdoc.M, error) {
	return r.FindDoc(ctx, pricePlanRuleCollection, id)
}

// ReplacePricePlanRuleDoc saves an EXISTING rule (id-preserving).
func (r *Repo) ReplacePricePlanRuleDoc(ctx context.Context, id string, doc pgdoc.M) error {
	return r.ReplaceDoc(ctx, pricePlanRuleCollection, id, doc)
}

// DeletePricePlanRuleDoc deletes a rule → deleted count.
func (r *Repo) DeletePricePlanRuleDoc(ctx context.Context, id string) (int64, error) {
	return r.DeleteDoc(ctx, pricePlanRuleCollection, id)
}

// PricePlanRulesByPlanID loads all rules for a plan: the raw rule docs
// (never nil).
func (r *Repo) PricePlanRulesByPlanID(ctx context.Context, pricePlanID string) ([]pgdoc.M, error) {
	out := []pgdoc.M{}
	if err := r.c(pricePlanRuleCollection).Find(ctx, pgdoc.M{"pricePlanId": pricePlanID}, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// DeletePricePlanRulesByPlanID deletes every rule of a price plan (the delete cascade that
// removes all rules belonging to the plan).
func (r *Repo) DeletePricePlanRulesByPlanID(ctx context.Context, pricePlanID string) error {
	_, err := r.c(pricePlanRuleCollection).DeleteMany(ctx, pgdoc.M{"pricePlanId": pricePlanID})
	return err
}

// PricePlanRuleByPlanIDAndName loads a rule by (plan,name): the matching
// rule, or (nil,nil) when none (used by the clone same-name conflict check).
func (r *Repo) PricePlanRuleByPlanIDAndName(ctx context.Context, pricePlanID, name string) (pgdoc.M, error) {
	var doc pgdoc.M
	found, err := r.c(pricePlanRuleCollection).FindOne(ctx,
		pgdoc.M{"pricePlanId": pricePlanID, "name": name}, &doc)
	if err != nil || !found {
		return nil, err
	}
	return doc, nil
}

// SetPricePlanRuleField persists a single default attribute field on a rule.
func (r *Repo) SetPricePlanRuleField(ctx context.Context, id, field string, value any) (int64, error) {
	ok, err := r.c(pricePlanRuleCollection).SetByID(ctx, id, pgdoc.M{field: value}, nil)
	if err != nil {
		return 0, err
	}
	if ok {
		return 1, nil
	}
	return 0, nil
}
