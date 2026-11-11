//go:build integration

package project

import (
	"context"
	"errors"
	"testing"

	"github.com/mutex-solutions/proxcloud/internal/cloud"
	"github.com/mutex-solutions/proxcloud/internal/cloud/sandbox"
	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/internal/platform/externalservice"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
)

type failedStartupAccessAPI struct {
	sandbox.K8sAPI
	deleted bool
}

func (f *failedStartupAccessAPI) GetCR(ctx context.Context, _, _, _, _, _ string) (map[string]any, error) {
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	return map[string]any{"metadata": map[string]any{"labels": map[string]any{sandbox.LabelProject: "p1", sandbox.LabelService: "s1", sandbox.LabelManagedBy: sandbox.ManagedByValue}}}, nil
}
func (f *failedStartupAccessAPI) DeleteCR(ctx context.Context, _, _, _, _, _ string) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	f.deleted = true
	return nil
}

func TestSavedStartupAccessCancellationRollsBack(t *testing.T) {
	ctx := context.Background()
	pgc, err := tcpostgres.Run(ctx, "postgres:17-alpine", tcpostgres.WithDatabase("startup_access"), tcpostgres.WithUsername("u"), tcpostgres.WithPassword("p"), tcpostgres.BasicWaitStrategies())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = testcontainers.TerminateContainer(pgc) }()
	dsn, _ := pgc.ConnectionString(ctx, "sslmode=disable")
	db, err := pgdoc.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close(ctx)
	if err := db.C("cloudResource").Ensure(ctx); err != nil {
		t.Fatal(err)
	}
	f := &failedStartupAccessAPI{}
	c := &e2bCaller{proj: &Project{ID: "p1"}, es: &externalservice.ExternalService{ID: "s1"}, ss: sandbox.NewWithAPI(f, sandbox.Config{}, "s1")}
	h := &Handler{cloud: cloud.NewRepo(db)}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := h.savedStartupAccess(cancelled, c, "sbx-0123456789abcdef"); !errors.Is(err, context.Canceled) {
		t.Fatalf("want canceled access, got %v", err)
	}
	if !f.deleted {
		t.Fatal("unknown resumed/forked sandbox was not deleted using detached cleanup")
	}
}
