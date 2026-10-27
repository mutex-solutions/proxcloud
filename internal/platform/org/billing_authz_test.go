package org

import (
	"testing"

	"github.com/mutex-solutions/proxcloud/internal/platform/rbac"
)

// TestInvoicePermsDistinctFromRead is the regression for findings [19]/[24]: the invoice reads
// (bills / bill-by-id / statement download) now gate on the dedicated invoice permissions, which are
// STRICTLY separate grants from the coarse billing_profile:read. A principal holding only
// billing_profile:read must NOT satisfy the invoice gates — proving the tightened gate is a real
// restriction and not a no-op.
func TestInvoicePermsDistinctFromRead(t *testing.T) {
	readOnly := []string{rbac.BillingProfileRead}
	if rbac.Matches(readOnly, rbac.BillingProfileReadInvoices) {
		t.Error("billing_profile:read must NOT grant read_invoices")
	}
	if rbac.Matches(readOnly, rbac.BillingProfileDownloadInvoices) {
		t.Error("billing_profile:read must NOT grant download_invoices")
	}
	// The correct grants satisfy their own gate (happy path preserved).
	if !rbac.Matches([]string{rbac.BillingProfileReadInvoices}, rbac.BillingProfileReadInvoices) {
		t.Error("read_invoices must grant read_invoices")
	}
	if !rbac.Matches([]string{rbac.BillingProfileDownloadInvoices}, rbac.BillingProfileDownloadInvoices) {
		t.Error("download_invoices must grant download_invoices")
	}
}

// TestFinancialMutationPermsRestrictMember is the regression for the codex F1/F5 findings: the
// financial MUTATION endpoints — deposit, deposit-by-card, add/delete/set-default card, pay-bill,
// create-order, and create/cancel/extend savings-contract — now gate on dedicated add_funds /
// manage_payment_methods / update permissions instead of the coarse billing_profile:read a default
// MEMBER holds. A MEMBER must NOT satisfy those gates (so charging a saved card / spending credits
// is forbidden), while OWNER/ADMIN (billing_profile:*) still do, and MEMBER keeps plain read.
func TestFinancialMutationPermsRestrictMember(t *testing.T) {
	mutations := []string{
		rbac.BillingProfileAddFunds,             // deposit, depositByCard
		rbac.BillingProfileManagePaymentMethods, // addCard, deleteCard, setDefaultCard
		rbac.BillingProfileUpdate,               // payBill, createOrder, savings create/cancel/extend
	}
	member := rbac.RolePermissions(rbac.RoleMember)
	readOnly := []string{rbac.BillingProfileRead}
	for _, p := range mutations {
		if rbac.Matches(member, p) {
			t.Errorf("MEMBER must NOT grant %q (financial mutation)", p)
		}
		if rbac.Matches(readOnly, p) {
			t.Errorf("billing_profile:read must NOT grant %q (gate must be a real restriction)", p)
		}
		for _, role := range []string{rbac.RoleOwner, rbac.RoleAdmin} {
			if !rbac.RoleHasPermission(role, p) {
				t.Errorf("%s must still grant %q (happy path)", role, p)
			}
		}
	}
	// MEMBER keeps plain read (read endpoints unchanged).
	if !rbac.Matches(member, rbac.BillingProfileRead) {
		t.Error("MEMBER must keep billing_profile:read")
	}
}
