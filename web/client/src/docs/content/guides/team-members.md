# Teammates and Invitations

Projects are meant to be shared. Anyone with the right permissions can invite a colleague by email; the colleague follows a link, accepts inside the portal, and is a project member from that moment on. All of this happens under **Organization → Members**.

## Inviting by email

1. Open **Organization → Members**.
2. In the invite area, type the teammate's **email address**.
3. Click **Send invite**. ProxCloud mails them a personal invitation link.

An invitation is valid for 24 hours. Until it is accepted it is listed under **Pending invitations** on the same page, with **Revoke**: a revoked invitation's link stops working, and you can invite the person again. A project owner or admin sees every pending invitation of the project; anyone else sees the ones they sent.

### When your site sends no email

Whether mail is set up is the operator's choice. On a site without it the Members page says so, and **Send invite** does not claim to have sent anything: the dialog shows the invitation link with **Copy link**, and the link stays available in **Pending invitations**. Send it to your teammate yourself. They can also do without the link: once they sign in with the invited email address, the invitation is waiting on their start page.

The link alone does not let anyone into the project. Accepting needs an account with the invited email address, and that address must be verified. On a site without mail the sign-in service cannot send a verification mail either, so ask your operator to mark the address as verified if the invitation does not show up after sign-in.

If the person is already part of your organization, skip the email round-trip: use **Add member** to attach an existing organization member straight to the current project. Pick the member, assign a role, and click **Add to project**.

## Accepting an invitation

The invitation email, or the link you copied, is a deep link into the portal (`/join-project?invite-token=…`). When the recipient opens it:

1. They sign in — or register first if they don't yet have an account. Registration uses the exact email the invite went to.
2. The portal presents the invitation with the project's name and two buttons: **Accept invite** or **Decline**.
3. Accepting makes them a member and drops them straight into the project. Declining throws the invitation away.

![Current invitation screen with a synthetic project invitation and Accept invite / Decline controls](/docs-img/join-project-accept.png)

*Illustration: current console UI with a synthetic invitation; no real invitation was accepted or declined.*

## Roles and permissions

Every project member has a role, shown and editable through **Set role** on the Members page:

| Role | Typical rights |
|------|----------------|
| OWNER | Full control: the project, its members, and its resources |
| ADMIN | Manage resources and everyday settings |
| MEMBER | Work with the project's cloud resources |

New invitees arrive as a regular member. That lets them create and manage cloud resources, but **not** rename or delete the project, invite anyone else, or remove members — those stay with owners and admins. If your operator has enabled custom roles, you'll find them under **Organization → Roles**. A role's name starts with a letter and holds letters, digits and underscores only; it is stored in upper case (`BILLING_VIEWER`).

## Removing members

Owners and admins can remove someone from the project whenever needed, via **Remove from project** on the Members page. Removal cuts off project access at once; it does not delete the person's account or touch their membership in other projects.

Every membership change — invites sent, invites accepted, members removed, roles changed — is written to **Organization → Audit log**.
