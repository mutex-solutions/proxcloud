# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Changed

- Replace application RabbitMQ dispatch with primary PostgreSQL durable native
  billing jobs: frozen original cycles, fenced at-least-once workers, atomic
  all-provider financial receipt/ACK, transactional enqueue, bounded retry/dead
  letters and audited operator replay. Reject old fanout flags until legacy queues
  are drained/reconciled; scheduler stays off by default. Optional read-only pool
  serves lag-tolerant history only. Mail/invite durability and broker deployment
  removal remain separate follow-ups.

### Fixed

- Protect full and sampled Proxmox SERVER/VOLUME/IMAGE reconciliation from
  filtered, malformed or unreadable VM inventories. Same-credential scoped proof
  and whole-row CAS preserve cache/history on unknown reads; identity retirement
  requires complete existing ACL/node authority. Delay protection healing until
  proof succeeds. Orphan IP cleanup now requires authoritative VM/address non-use
  and exact allocation/archive receipts instead of age or bulk NIC release;
  ambiguous reuse, other guest NICs and legacy/BeforeRow allocations stay held.
- Fail closed on permission-filtered PVE NETWORK inventories: prove effective
  same-token visibility and revalidate omitted networks/subnets before cache
  writes, preserving tenant rows and NAS exports on unknown results while genuine
  delete-of-vanished still archives normally. Surface typed repair warnings.
- Refuse conflicting TrueNAS provider owners on the same project's NAS backend and
  parent dataset before share/export mutations or duplicate share caching, rather
  than letting different provider settings repeatedly rewrite the same exports.

Initial ProxCloud release: a self-service private-cloud and billing portal for
Proxmox VE. History from the product this codebase was forked from is not
carried over.
