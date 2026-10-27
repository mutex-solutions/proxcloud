package billing

import (
	"testing"

	"github.com/mutex-solutions/proxcloud/internal/platform/pricing"
)

// TestBillBelongsToProfile is the regression for finding [8]: PayBillWithCredits must not settle a
// bill that belongs to another billing profile — the owner guard treats a foreign bill as not-found,
// so a member of profile A cannot pay/consume-credit against profile B's bill.
func TestBillBelongsToProfile(t *testing.T) {
	bill := &pricing.Bill{BillingProfileID: "profA"}
	if !billBelongsToProfile(bill, "profA") {
		t.Error("owner must see its own bill")
	}
	if billBelongsToProfile(bill, "profB") {
		t.Error("cross-profile bill must be invisible (owner guard → ErrBillNotFound)")
	}
	if billBelongsToProfile(nil, "profA") {
		t.Error("nil bill must not belong to any profile")
	}
}
