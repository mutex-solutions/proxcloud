# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Fixed

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
