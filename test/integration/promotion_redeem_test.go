//go:build integration

package integration

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/internal/platform/billing"
	"github.com/mutex-solutions/proxcloud/internal/platform/promotion"
	"github.com/mutex-solutions/proxcloud/pkg/httpx"
)

// Redeeming a promotion code writes the redemption record and the credit. A failure of either write must leave neither:
// a credit without its record is minted again by the retry, a record without its credit refuses the retry.
func TestPromotionRedeemIsOneWrite(t *testing.T) {
	ctx := context.Background()
	db := freshPG(t)
	h := promotion.NewHandler(billing.NewRepo(db), func(context.Context, string, string) (string, string, error) {
		return "org-1", "bp-1", nil
	})
	r := chi.NewRouter()
	h.Routes(r)
	redeem := func(code string) int {
		req := httptest.NewRequest(http.MethodPost, "/promotion/bp-1/code?code="+code, nil)
		req = req.WithContext(httpx.WithRC(req.Context(), &httpx.RequestContext{Sub: "u-1"}))
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w.Code
	}
	count := func(table string) int64 {
		n, err := db.C(table).Count(ctx, pgdoc.M{})
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	for _, table := range []string{"promotionalCredit", "promotionCodeRedemption"} {
		if err := db.C(table).Ensure(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Pool.Exec(ctx, `CREATE FUNCTION refuse() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'refused'; END $$`); err != nil {
		t.Fatal(err)
	}

	for _, table := range []string{"promotionCodeRedemption", "promotionalCredit"} {
		t.Run("failing write on "+table, func(t *testing.T) {
			for _, tb := range []string{"promotionCode", "promotionalCredit", "promotionCodeRedemption"} {
				if _, err := db.C(tb).DeleteMany(ctx, pgdoc.M{}); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := db.C("promotionCode").InsertOne(ctx, pgdoc.M{"code": "WELCOME", "amount": 10}); err != nil {
				t.Fatal(err)
			}
			if _, err := db.Pool.Exec(ctx, `CREATE TRIGGER refuse BEFORE INSERT ON "`+table+`" FOR EACH ROW EXECUTE FUNCTION refuse()`); err != nil {
				t.Fatal(err)
			}
			if code := redeem("WELCOME"); code != http.StatusInternalServerError {
				t.Fatalf("redeem with %s refusing: %d, want 500", table, code)
			}
			if c, rd := count("promotionalCredit"), count("promotionCodeRedemption"); c != 0 || rd != 0 {
				t.Fatalf("a failed redeem left %d credits, %d redemptions", c, rd)
			}
			if _, err := db.Pool.Exec(ctx, `DROP TRIGGER refuse ON "`+table+`"`); err != nil {
				t.Fatal(err)
			}
			if code := redeem("WELCOME"); code != http.StatusOK {
				t.Fatalf("retry: %d", code)
			}
			if code := redeem("WELCOME"); code != http.StatusBadRequest {
				t.Fatalf("second redeem: %d, want 400", code)
			}
			if c, rd := count("promotionalCredit"), count("promotionCodeRedemption"); c != 1 || rd != 1 {
				t.Fatalf("after the retry: %d credits, %d redemptions, want 1 each", c, rd)
			}
		})
	}

	t.Run("redeems at once", func(t *testing.T) {
		if _, err := db.C("promotionCode").InsertOne(ctx, pgdoc.M{"code": "RACE", "amount": 5}); err != nil {
			t.Fatal(err)
		}
		before := count("promotionalCredit")
		var wg sync.WaitGroup
		codes := make([]int, 8)
		for i := range codes {
			wg.Add(1)
			go func() { defer wg.Done(); codes[i] = redeem("RACE") }()
		}
		wg.Wait()
		ok := 0
		for _, c := range codes {
			if c == http.StatusOK {
				ok++
			} else if c != http.StatusBadRequest {
				t.Errorf("concurrent redeem answered %d", c)
			}
		}
		if got := count("promotionalCredit") - before; ok != 1 || got != 1 {
			t.Fatalf("%d redeems answered 200, %d credits minted; want 1 and 1", ok, got)
		}
	})
}
