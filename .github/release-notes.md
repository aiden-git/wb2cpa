## wb2cpa ${GITHUB_REF_NAME}

### Features

- Added per-model reasoning effort display in the WorkBuddy management overview, including the supported levels and the upstream-declared default.
- Uses only explicit `reasoning.supportedEfforts` / `reasoning.defaultEffort` metadata from dynamic model discovery; missing data is shown as unavailable rather than inferred.
- Preserves the protected management API boundary and prevents malformed or unsupported default effort values from being displayed.

### Validation

- `go vet ./...`
- `go test ./...`
- Management-page JavaScript simulation for credit rendering and global OAuth entry
- CGO shared-library build
