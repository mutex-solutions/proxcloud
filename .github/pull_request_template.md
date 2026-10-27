## Summary

<!-- What does this change do, and why? -->

## Related issues

<!-- e.g. Closes #123 -->

## Type of change

- [ ] Bug fix
- [ ] New feature
- [ ] Refactor / cleanup
- [ ] Documentation
- [ ] Build / CI / deployment

## Checklist

- [ ] `gofmt` clean and `go vet ./...` passes
- [ ] `go test ./...` passes (and `make test-integration` if behavior needs it)
- [ ] For UI changes: `npm run lint` and `npm run build` pass in the affected `web/*` app
- [ ] For chart changes under `deploy/charts/proxcloud/**`: bumped `Chart.yaml` `version`
- [ ] Updated docs / `CHANGELOG.md` where relevant
- [ ] No secrets or environment-specific values committed

## Notes for reviewers

<!-- Anything reviewers should focus on, test manually, or be aware of. -->
