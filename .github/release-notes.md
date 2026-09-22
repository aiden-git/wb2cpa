## wb2cpa ${GITHUB_REF_NAME}

### Features

- Added per-account platform credit details, including each active package’s used/total credits, remaining credits, availability, and confirmed billing-cycle end.
- Aggregated all active package balances instead of displaying only the first billing package.
- Added a dedicated **WorkBuddy 国际版登录** management-menu entry for `www.workbuddy.ai` OAuth.
- Kept the CPA generic OAuth card on its compatible CN default and retained protected management API authorization.

### Validation

- `go vet ./...`
- `go test ./...`
- Management-page JavaScript simulation for credit rendering and global OAuth entry
- CGO shared-library build
