package message

import (
	"context"
	"log/slog"
)

// seed.go defines the 22 built-in email templates, created if-absent (by key) at startup.
// Placeholders are Mustache {{var}}.

// SystemTemplates returns the built-in templates.
func SystemTemplates() []MessageTemplate {
	t := func(key, cat, title, body string) MessageTemplate {
		return MessageTemplate{Key: key, Category: cat, SystemTemplate: true, MessageTitle: title, MessageBody: body}
	}
	return []MessageTemplate{
		t("sandbox_abuse_alert", "SANDBOX", "Sandbox sustained CPU alert: {{sandboxId}}",
			`<p>Provider {{serviceId}}, project {{projectId}}, sandbox {{sandboxId}} matched sustained CPU with no observed exec or traffic activity.</p><p>This is a heuristic, not proof of abuse. Configured action: {{action}}. Review the SANDBOX_ABUSE audit events for the durable detection and action result.</p>`),
		t("send_refunded_invoice", "INVOICE", "Your payment has been refunded",
			`<p>Dear {{firstName}} {{lastName}},</p>
<p>We would like to inform you that your payment of {{amount}} {{currency}} has been refunded to your original payment method.</p>
<p>Credit note {{invoiceNumber}} has been issued for this refund. You can download it from the Billing history page of your account.</p>
<p>If you have any questions or need further assistance, please don't hesitate to reach out to us.</p>
<p>Thank you for your attention.</p>
<p>Best regards,</p>
<p>{{businessName}}</p>`),
		t("bill_is_generated", "BILL", "Your bill has been generated",
			`<p>Dear {{fullName}},</p>
<p>Your invoice <strong>{{invoiceNumber}}</strong> for the period {{startDate}} - {{endDate}} was issued on {{issueDate}}. Here is a summary of your charges:</p>
{{{itemsSummaryHtml}}}
<table role="presentation" cellpadding="0" cellspacing="0" border="0" style="width:100%;border-collapse:collapse;margin:16px 0;">
<tr><td style="padding:12px;background:#f3f4f6;font-family:Arial,Helvetica,sans-serif;font-size:16px;"><strong>Amount due: {{amountDue}} {{currency}}</strong> &mdash; due {{dueDate}}</td></tr>
</table>
<p><strong>Pay by bank transfer</strong></p>
<p>Please transfer {{amountDue}} {{currency}} to the account below. The payment description must quote the invoice number <strong>{{invoiceNumber}}</strong> so we can match your payment.</p>
<table role="presentation" cellpadding="0" cellspacing="0" border="0" style="width:100%;border-collapse:collapse;">
<tr><td style="padding:12px;border:1px solid #e5e7eb;font-family:Arial,Helvetica,sans-serif;font-size:14px;">{{{bankInstructionsHtml}}}</td></tr>
</table>
<p>Once the transfer is made, open the invoice, click <strong>Pay by bank transfer</strong> and then <strong>I have transferred</strong> to send us the transfer date, your bank reference and, optionally, the proof of payment:</p>
<table role="presentation" cellpadding="0" cellspacing="0" border="0" style="margin:16px 0;">
<tr><td style="background:#111827;border-radius:4px;"><a href="{{billUrl}}" style="display:inline-block;padding:10px 18px;color:#ffffff;text-decoration:none;font-family:Arial,Helvetica,sans-serif;font-size:14px;">Open the invoice and confirm your transfer</a></td></tr>
</table>
<p>Or copy this link into your browser: {{billUrl}}</p>
<p>If you have any questions or require further assistance, feel free to contact us.</p>
<p>The invoice (PDF) is attached to this e-mail.</p>
<p>Best regards,</p>
<p>{{businessName}}</p>`),
		t("bill_is_paid", "BILL", "Your bill has been paid",
			`<p>Dear {{fullName}},</p>
<p>Your invoice <strong>{{invoiceNumber}}</strong> for the period {{startDate}} - {{endDate}} has been paid in full. Nothing is due for this invoice.</p>
{{{itemsSummaryHtml}}}
<p>You can view and download the invoice from your account: <a href="{{billUrl}}">{{billUrl}}</a></p>
<p>If you have any questions or need further information, feel free to reach out to us.</p>
<p>Thank you for your continued trust in us.</p>
<p>The invoice (PDF) is attached to this e-mail.</p>
<p>Best regards,</p>
<p>{{businessName}}</p>`),
		t("send_thank_you_to_customer", "PAYMENT", "Thank you for your payment",
			`<p>Dear {{fullName}},</p>
<p>We are pleased to inform you that your payment of {{grossAmount}} {{currency}} has been successfully processed.</p>
<p>Thank you for your prompt payment. If you have any questions or need further assistance, feel free to contact us.</p>
<p>Best regards,</p>
<p>{{businessName}}</p>`),
		t("refund_failed", "INVOICE", "Your refund could not be completed",
			`<p>Dear {{fullName}},</p>
<p>The payment provider could not complete the refund of {{amount}} {{currency}} we requested for you ({{reason}}).</p>
<p>The corresponding credit of {{restoredCredit}} {{currency}} has been put back on your account balance, so nothing is lost. Our finance team has been notified and will contact you to arrange the refund another way if you still need it.</p>
<p>Best regards,</p>
<p>{{businessName}}</p>`),
		t("refund_failed_operator", "INVOICE", "Refund FAILED at the payment provider: {{amount}} {{currency}} for {{fullName}}",
			`<p>The payment provider reported the refund of {{amount}} {{currency}} for {{fullName}} {{companyName}} as FAILED: {{reason}}.</p>
<p>ProxCloud restored the customer's account credit ({{restoredCredit}} {{currency}}) and the deposit transaction {{transactionId}} (gateway reference {{externalId}}) is back to SUCCESS. Arrange the refund manually or retry it from the admin console:</p>
<p><a href="{{adminUrl}}">{{adminUrl}}</a></p>`),
		t("notify_customer_payment_failed", "BILL", "Action required: the payment for invoice {{invoiceNumber}} failed",
			`<p>Dear {{fullName}},</p>
<p>We tried to charge your saved card <strong>{{panMasked}}</strong> for invoice <strong>{{invoiceNumber}}</strong> ({{amountDue}} {{currency}}), but the payment was not accepted.</p>
<p>Reason given by the payment provider: <em>{{errorMessage}}</em></p>
<p>Please check the card (funds, limit, expiry) and retry the payment, pay with another card, or pay by bank transfer from the invoice page:</p>
<table role="presentation" cellpadding="0" cellspacing="0" border="0" style="margin:16px 0;">
<tr><td style="background:#111827;border-radius:4px;"><a href="{{billUrl}}" style="display:inline-block;padding:10px 18px;color:#ffffff;text-decoration:none;font-family:Arial,Helvetica,sans-serif;font-size:14px;">Open the invoice and retry the payment</a></td></tr>
</table>
<p>We will also retry the charge automatically over the next days. An invoice left unpaid after its due date ({{dueDate}}) may lead to the suspension of your services.</p>
<p>Best regards,</p>
<p>{{businessName}}</p>`),
		t("notify_customer_has_no_card", "SUSPENSION", "Action Required: Payment Method Not Found for Your Bill",
			`<p>Dear {{fullName}},</p>
<p>We were unable to identify a valid payment method for your recent bill.</p>
<p>To avoid any interruption of services, please add a valid bank card or deposit funds into your account.</p>
<p>Current Balance: {{balance}} {{currency}}</p>
<p>If you need assistance with updating your payment information, feel free to contact us.</p>
<p>Thank you for your prompt attention to this matter.</p>
<p>Best regards,</p>
<p>{{businessName}}</p>`),
		t("notify_customer_before_suspension", "SUSPENSION", "Reminder: Unpaid Bill Due for Your Account",
			`<p>Dear {{fullName}},</p>
<p>We hope you are doing well. This is a friendly reminder that your account currently has unpaid bill that is overdue.</p>
<p>We kindly ask that you settle this payment at your earliest convenience to avoid any potential late fees or service interruptions.</p>
<p>If you have already made the payment or have any questions regarding your bill, please contact us, and we will be happy to assist you.</p>
<p>Thank you for your prompt attention to this matter.</p>
<p>Best regards,</p>
<p>{{businessName}}</p>`),
		t("notify_customer_before_suspension_balance", "SUSPENSION", "Important: Low Balance Alert for Your Account",
			`<p>Dear {{fullName}},</p>
<p>We hope this message finds you well. We are writing to inform you that the balance in your account is currently running low.</p>
<p>Current Balance: {{balance}} {{currency}}</p>
<p>To avoid any potential service interruptions, we kindly encourage you to top up your account as soon as possible.</p>
<p>If you have any questions or need assistance, feel free to reach out to our customer support team.</p>
<p>Thank you for your attention to this matter.</p>
<p>Best regards,</p>
<p>{{businessName}}</p>`),
		t("send_bank_transfer_instructions", "BANK_TRANSFER", "Bank Transfer Instructions",
			`<p>Dear {{fullName}},</p>
<p>To complete your bank transfer, please send the specified amount to the bank account listed below:</p>
<p>Bank Details:</p>
<p style="white-space: pre-line">{{instructions}}</p>
<p>Amount to Transfer:</p>
<p>{{amount}} {{currency}}{{billReference}}</p>
<p>For quicker processing of your payment, kindly include the following reference number in the payment description:</p>
<p>Reference Number:</p>
<p>{{referenceNumber}}</p>
<p>If you have any questions or need further assistance, feel free to reach out to us.</p>
<p>Thank you for your prompt attention.</p>
<p>Best regards,</p>
<p>{{businessName}}</p>`),
		t("bank_transfer_submitted", "BANK_TRANSFER", "Bank transfer submitted by {{fullName}} — {{amount}} {{currency}}",
			`<p>A customer reports having completed a bank transfer. Please check the incoming payment and approve or reject the transfer in the admin console.</p>
<p>Customer: {{fullName}} {{companyName}}</p>
<p>Amount: {{amount}} {{currency}}</p>
<p>Reference number: {{referenceNumber}}</p>
<p>Bank reference: {{bankReference}}</p>
<p>Transferred on: {{transferredAt}}</p>
<p>Invoice: {{invoiceNumber}} {{billId}}</p>
<p><a href="{{adminUrl}}">Review bank transfers</a></p>`),
		t("notify_customer_is_suspended", "SUSPENSION", "Service Suspension Due to Low Balance",
			`<p>Dear {{fullName}},</p>
<p>We would like to inform you that your services have been temporarily suspended due to an insufficient balance in your account.</p>
<p>Current Balance: {{balance}} {{currency}}</p>
<p>To restore your services, please top up your account at your earliest convenience. If you have any questions or need assistance, feel free to contact us.</p>
<p>Thank you for your understanding.</p>
<p>Best regards,</p>
<p>{{businessName}}</p>`),
		t("notify_customer_is_resumed", "SUSPENSION", "Your Services Have Been Resumed",
			`<p>Dear {{fullName}},</p>
<p>We're pleased to inform you that your services have been successfully resumed. Welcome back!</p>
<p>If you have any questions or need further assistance, feel free to reach out to us.</p>
<p>Thank you for choosing us.</p>
<p>Best regards,</p>
<p>{{businessName}}</p>`),
		t("send_invite_to_project", "PROJECT", "Invitation to Join the Project",
			`<p>Dear {{fullName}},</p>
<p>You have been invited to join the project {{projectName}}. To accept the invitation, please click the link below:</p>
<p><a href="{{projectInviteUrl}}">Accept Invitation</a></p>
<p>Please note that this link will expire in {{expiryHours}} hours. If you have any questions or need assistance, feel free to reach out to us.</p>
<p>Best regards,</p>
<p>{{businessName}}</p>`),
		t("billing_profile_validation_approved", "BILLING", "Your Billing Profile Validation Has Been Approved",
			`<p>Dear {{fullName}},</p>
<p>Your billing profile validation has been approved.</p>
<p>This approval does not confirm account activation, service readiness, or completion of any credits. You can review your account's current status by signing in at <a href="{{loginUrl}}">{{loginUrl}}</a>.</p>
<p>If you have questions, please contact us.</p>
<p>{{businessName}}</p>`),
		t("billing_profile_validated", "BILLING", "Your Billing Profile Has Been Validated",
			`<p>Dear {{fullName}},</p>
<p>We are pleased to inform you that your billing profile has been successfully validated. Your account is now fully activated, and you can access all services by logging into your account at <a href="{{loginUrl}}">{{loginUrl}}</a></p>
<p>If you have any questions or need further assistance, feel free to reach out to us.</p>
<p>Best regards,</p>
<p>{{businessName}}</p>`),
		t("email_address_confirmation", "BILLING", "Please Confirm Your Email Address",
			`<p>Thank you for signing up! To complete your registration, please confirm your email address by clicking the link below:</p>
<p><a href="{{confirmationUrl}}">Confirm Email Address</a></p>
<p>This link will expire in {{expiryHours}} hours. If you did not request this, please ignore this email.</p>
<p>Best regards,</p>
<p>{{businessName}}</p>`),
		t("password_reset_request", "BILLING", "Password Reset Request",
			`<p>Dear {{fullName}},</p>
<p>We received a request to reset your password. To reset your password, please click the link below:</p>
<p><a href="{{resetPasswordUrl}}">Reset Password</a></p>
<p>This link will expire in {{expiryHours}} hours. If you did not request this, please ignore this email.</p>
<p>Best regards,</p>
<p>{{businessName}}</p>`),
		t("order_payment_success", "SAVINGS_CONTRACT", "Order Payment Successful",
			`<p>Dear {{fullName}},</p>
<p>We are pleased to confirm that your payment of {{grossAmount}} {{currency}} has been successfully processed.</p>
<p>Order Details:</p>
<p>{{orderItems}}</p>
<p>Thank you for your purchase. If you have any questions, please don't hesitate to contact us.</p>
<p>Best regards,</p>
<p>{{businessName}}</p>`),
		t("savings_contract_activated", "SAVINGS_CONTRACT", "Your Savings Contract is Now Active",
			`<p>Dear {{fullName}},</p>
<p>We are pleased to inform you that your savings contract for "{{savingsPlanName}}" has been activated.</p>
<p>Contract Details:</p>
<ul>
  <li>Plan: {{savingsPlanName}}</li>
  <li>Start Date: {{startDate}}</li>
  <li>End Date: {{endDate}}</li>
  <li>Monthly Commitment: {{monthlyCommittedAmount}} {{currency}}</li>
  <li>Discount Rate: {{discountRate}}%</li>
</ul>
<p>You will now start receiving discounts on eligible services based on your commitment level.</p>
<p>If you have any questions, feel free to contact us.</p>
<p>Best regards,</p>
<p>{{businessName}}</p>`),
		t("savings_contract_expiry_reminder", "SAVINGS_CONTRACT", "Your Savings Contract is Expiring Soon",
			`<p>Dear {{fullName}},</p>
<p>This is a reminder that your savings contract for "{{savingsPlanName}}" will expire in {{daysUntilExpiry}} days.</p>
<p>Contract Details:</p>
<ul>
  <li>Plan: {{savingsPlanName}}</li>
  <li>Expiration Date: {{endDate}}</li>
  <li>Monthly Commitment: {{monthlyCommittedAmount}} {{currency}}</li>
  <li>Discount Rate: {{discountRate}}%</li>
</ul>
<p>If you would like to renew your contract or explore other savings plans, please log in to your account.</p>
<p>Best regards,</p>
<p>{{businessName}}</p>`),
		t("savings_contract_expired", "SAVINGS_CONTRACT", "Your Savings Contract Has Expired",
			`<p>Dear {{fullName}},</p>
<p>We would like to inform you that your savings contract for "{{savingsPlanName}}" has expired.</p>
<p>Expired Contract Details:</p>
<ul>
  <li>Plan: {{savingsPlanName}}</li>
  <li>Start Date: {{startDate}}</li>
  <li>End Date: {{endDate}}</li>
  <li>Monthly Commitment: {{monthlyCommittedAmount}} {{currency}}</li>
</ul>
<p>You will no longer receive the discounts associated with this contract. If you would like to subscribe to a new savings plan, please log in to your account.</p>
<p>Thank you for being part of our savings program.</p>
<p>Best regards,</p>
<p>{{businessName}}</p>`),
	}
}

// SeedSystemTemplates creates any missing system templates (create-if-absent by key).
// Idempotent; safe to run at every startup.
func SeedSystemTemplates(ctx context.Context, repo *Repo, log *slog.Logger) {
	created := 0
	for _, tmpl := range SystemTemplates() {
		exists, err := repo.ExistsByKey(ctx, tmpl.Key)
		if err != nil {
			if log != nil {
				log.Error("seed message template: exists check", "key", tmpl.Key, "err", err)
			}
			continue
		}
		if exists {
			continue
		}
		tc := tmpl
		if err := repo.Create(ctx, &tc); err != nil {
			if log != nil {
				log.Error("seed message template: create", "key", tmpl.Key, "err", err)
			}
			continue
		}
		created++
	}
	if log != nil && created > 0 {
		log.Info("seeded system message templates", "created", created)
	}
}
