# DNS zones

If your operator offers DNS, **DNS** in the sidebar lists your project's zones. Name servers answer for them once you delegate your domain.

## Create a zone

1. Open **DNS** and choose **Create zone**.
2. Enter the domain, such as `example.org`, then choose **Get ownership TXT record**. International names are converted to their canonical DNS form.
3. At your **current DNS host**, publish the exact TXT value at the full `_proxcloud-ownership.<domain>.` record name shown. Some DNS forms append the domain themselves; use only the relative name in that case. Use TTL60 if available. Do not use a CNAME and **do not change your name servers yet**.
4. Choose **Verify TXT record**. The check uses operator-trusted DNS resolvers, not a server you supply. DNS caches (including cached missing records) can delay visibility; wait at least10seconds between attempts. A challenge expires after30minutes and reserves no domain. The value is shown only when the challenge is issued: if lost, cancel/start over.
5. Once **VERIFIED**, choose **Create verified zone** within10minutes. Proof works once for this exact project, DNS provider and domain. If create fails or times out, refresh the list before retrying: it may have succeeded upstream. If absent, cancel/start a new challenge; consumed proofs cannot be replayed.
6. Copy your existing DNS records, then delegate the domain at your registrar to the name servers shown in the zone list. After successful creation you may remove the ownership TXT record at the old host.

A zone name can be held by one project at a time, so an existing domain is refused. So is a name above or below a zone another project or your operator holds: `www.example.org` can't be added while someone else holds `example.org`, nor the other way round. A public suffix such as `co.uk` or `github.io`, unknown/internal suffixes and malformed labels are refused. Pending challenges never reserve names or exclude other projects, and ownership proof does not override an existing foreign zone.

A parent you already manage in the same project may publish the TXT for its child. That is explicit child-control proof, not an automatic permanent grant. Existing legacy zones remain manageable without retroactive proof; deleting and recreating one needs a new proof. There is no automatic destructive cleanup if a TXT record later disappears.

Wait60seconds between zone-creation attempts across all this project's providers, including after a failed attempt; a busy-global-create response does not spend that attempt. Creation is bounded to10seconds; an uncertain timeout can still mean the upstream accepted it, so refresh the list first.

At most10live challenges and20new challenges/hour are allowed per project. Cancel unused challenges to free live capacity (the hourly budget still applies). A **Close** only hides the dialog, preserving your in-memory value until you leave/reload; **Cancel challenge / start over** invalidates that proof, not an existing zone.

## Records

Choose **Records** on a zone.

- **Name**: `www` for `www.example.org`, or `@` for the zone itself. The full name works too, with or without the final dot.
- **Type**: A, AAAA, CAA, CNAME, MX, NS, PTR, SRV or TXT.
- **Content**: one record per line. Saving replaces all records of that name and type. Text records are quoted for you.

The zone's SOA record and its own NS records are managed by your operator. You can delegate a subdomain with NS records below the zone.

Deleting a zone deletes all its records. Deleting a project deletes its zones.
