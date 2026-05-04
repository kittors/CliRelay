# Contributing

Contributions are welcome. Please keep pull requests aligned with the project branch workflow so maintainers can review and merge them without retargeting.

## Branch Workflow

1. Sync your fork and create work from the latest `dev` baseline:

   ```bash
   git fetch origin
   git switch -c feature/your-change origin/dev
   ```

2. Keep implementation commits on a feature branch. Do not commit directly to `main` or `dev`.

3. Open pull requests against `dev`, not `main`.

4. Include only files related to the change. Leave local runtime data, generated tool directories, and unrelated edits out of the PR.

5. Run relevant tests before opening or updating a PR. For Go changes, `go test ./...` is the default verification command unless the change has a narrower documented test scope.

Maintainers merge verified feature branches into `dev` first. `main` is reserved for release or stable integration updates and is not the default PR target for normal fixes or features.

## Pull Request Checklist

- The PR base branch is `dev`.
- The branch is based on the latest `origin/dev`.
- The description explains the behavior change and verification performed.
- Tests or focused verification are included for code changes.
- Documentation is updated when user-facing behavior or workflow changes.
