## wb2cpa ${GITHUB_REF_NAME}

### Fixes

- Fixed a JavaScript syntax error in the WorkBuddy management page that prevented every page control from working.
- Removed the manual Management Token field. The page now reuses the same-origin CPA control-panel authorization when the user has enabled “remember password”.
- Kept account overview, OAuth login, API Key creation, and refresh routes behind the existing CPA management authorization boundary.
- Improved management-page feedback for missing authorization, expired authorization, disabled management APIs, and malformed responses.

### Validation

- `go vet ./...`
- `go test ./...`
- CGO shared-library build
