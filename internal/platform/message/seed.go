package message

import (
	"context"
	"log/slog"
)

// seed.go defines the 18 built-in email templates, created if-absent (by key) at startup.
// Placeholders are Mustache {{var}}.

// SystemTemplates returns the built-in templates.
func SystemTemplates() []MessageTemplate {
	t := func(key, cat, title, body string) MessageTemplate {
		return MessageTemplate{Key: key, Category: cat, SystemTemplate: true, MessageTitle: title, MessageBody: body}
	}
	return []MessageTemplate{
		t("send_refunded_invoice", "INVOICE", "Refunded invoice",
			`<p>Dear {{firstName}} {{lastName}},</p>
<p>We would like to inform you that the invoice with number {{invoiceNumber}} has been successfully refunded.</p>
<p>If you have any questions or need further assistance, please don't hesitate to reach out to us.</p>
<p>Thank you for your attention.</p>
<p>Best regards,</p>
<p>{{businessName}}</p>`),
		t("bill_is_generated", "BILL", "Your bill has been generated",
			`<p>Dear {{fullName}},</p>
<p>We would like to inform you that your bill for the period {{startDate}} - {{endDate}} has been generated. Please find the attached document, which includes a detailed summary of your usage during this time.</p>
<p>If you have any questions or require further assistance, feel free to contact us.</p>
<p>Thank you for choosing us.</p>
<p>Best regards,</p>
<p>{{businessName}}</p>`),
		t("bill_is_paid", "BILL", "Your bill has been paid",
			`<p>Dear {{fullName}},</p>
<p>We are pleased to inform you that the bill for the period {{startDate}} - {{endDate}} has been successfully paid. Please find the attached document, which includes a detailed summary of your usage during this period.</p>
<p>If you have any questions or need further information, feel free to reach out to us.</p>
<p>Thank you for your continued trust in us.</p>
<p>Best regards,</p>
<p>{{businessName}}</p>`),
		t("send_thank_you_to_customer", "PAYMENT", "Thank you for your payment",
			`<p>Dear {{fullName}},</p>
<p>We are pleased to inform you that your payment of {{grossAmount}} {{currency}} has been successfully processed.</p>
<p>Thank you for your prompt payment. If you have any questions or need further assistance, feel free to contact us.</p>
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
<p>{{instructions}}</p>
<p>Amount to Transfer:</p>
<p>{{amount}} {{currency}}</p>
<p>For quicker processing of your payment, kindly include the following reference number in the payment description:</p>
<p>Reference Number:</p>
<p>{{referenceNumber}}</p>
<p>If you have any questions or need further assistance, feel free to reach out to us.</p>
<p>Thank you for your prompt attention.</p>
<p>Best regards,</p>
<p>{{businessName}}</p>`),
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
