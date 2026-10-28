## Summary

<!-- What does this change do? -->

## Why

<!-- The problem, decision or request behind it. Link issues, e.g. Closes #123. -->

## Type of change

- [ ] Bug fix
- [ ] New feature
- [ ] Refactor / cleanup
- [ ] Documentation
- [ ] Build / CI / deployment

## Architecture impact

<!--
Tick exactly one. A change to a container, component, external system, trust
boundary or key flow needs the C4 update in THIS PR.
See docs/architecture/c4/README.md#maintenance-rule.
-->

- [ ] C4 diagrams updated **and** an entry appended to `docs/architecture/c4/CHANGELOG.md` (date, PR number, what changed, why)
- [ ] No architecture impact. Reason:

## Breaking changes

<!-- Config, API, chart or data changes operators or clients must act on. "None" if none. -->

## Test plan

<!-- How this was verified: commands run, manual steps, environments. -->

- [ ] `gofmt` clean and `go vet ./...` passes
- [ ] `go test ./...` passes (and `make test-integration` if behavior needs it)
- [ ] For UI changes: `npm run lint` and `npm run build` pass in the affected `web/*` app
- [ ] For chart changes under `deploy/charts/proxcloud/**`: bumped `Chart.yaml` `version`
- [ ] Updated docs / `CHANGELOG.md` where relevant
- [ ] No secrets or environment-specific values committed

## Notes for reviewers

<!-- Anything reviewers should focus on, test manually, or be aware of. -->
