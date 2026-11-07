# DNS zones

If your operator offers DNS, **DNS** in the sidebar lists your project's zones. Name servers answer for them once you delegate your domain.

## Create a zone

1. Open **DNS** and choose **Create zone**.
2. Enter the domain, such as `example.org`.

A zone name can be held by one project at a time, so a domain someone else already added is refused. So is a name above or below a zone another project or your operator holds: `www.example.org` can't be added while someone else holds `example.org`, nor the other way round. A public suffix such as `co.uk` is refused too. ProxCloud doesn't check that you own the domain. The zone only takes effect when your registrar points the domain at the name servers shown in the list.

## Records

Choose **Records** on a zone.

- **Name**: `www` for `www.example.org`, or `@` for the zone itself. The full name works too, with or without the final dot.
- **Type**: A, AAAA, CAA, CNAME, MX, NS, PTR, SRV or TXT.
- **Content**: one record per line. Saving replaces all records of that name and type. Text records are quoted for you.

The zone's SOA record and its own NS records are managed by your operator. You can delegate a subdomain with NS records below the zone.

Deleting a zone deletes all its records. Deleting a project deletes its zones.
