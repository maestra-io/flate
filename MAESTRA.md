# Maestra fork of flate

Real GitHub fork of [home-operations/flate](https://github.com/home-operations/flate); tracking issue maestra-io/issues-maestra#1513.

- `main` is an untouched mirror of upstream `main`: fast-forward only, never commit to it.
- `maestra/main` (default branch) is upstream `main` plus our commits. All our work lands here via PRs (`fix/<blocker>` branches).

Update from upstream:
```sh
git fetch upstream && git checkout main && git merge --ff-only upstream/main && git push origin main
git checkout maestra/main && git merge main && git push origin maestra/main
```

Merge, not rebase: `maestra/main` is the default branch with open PRs on it; a rebase would force-push it and invalidate every branch cut from it.
Changes meant for upstream: branch from `main`, cherry-pick, open the PR against home-operations/flate.
