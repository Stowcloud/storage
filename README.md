# storage

`github.com/stowcloud/storage` defines small backend-neutral contracts for hierarchical storage. It validates relative paths, describes entries and health, and exposes optional capabilities through ordinary Go interfaces.

The module deliberately does not own ACL, quotas, shares, upload policy, indexing, application dependency injection, or filesystem confinement. Product applications keep those policies at their integration boundary. A local adapter may use this package's contracts while retaining the product's own confinement implementation.

## Requirements

- Go 1.27.1 or newer.
- Linux amd64 and arm64 are tested by CI.
- Windows, macOS, and FreeBSD compile checks are part of the portability matrix.

## Usage

```go
path, err := storage.ParsePath("photos/2026/image.jpg")
if err != nil {
    return err
}
entry, err := backend.Stat(ctx, path)
if err != nil {
    return err
}
_ = entry
```

Capabilities are discovered with type assertions:

```go
if materializer, ok := backend.(storage.Materializer); ok {
    lease, err := materializer.Materialize(ctx, path)
    if err != nil {
        return err
    }
    defer lease.Release()
}
```

`Materialized.Release` is idempotent and owns backend cleanup. Callers must not close or delete the resource independently when using a returned lease.

## Verification

```sh
go test ./...
go vet ./...
go test -race ./...
```

The release workflow also compiles Windows, macOS, and FreeBSD targets. Linux behavior is tested on amd64 and arm64.

## Compatibility and releases

This is a pre-1.0 module. Public APIs and behavior may change in minor releases when needed to correct a contract or security issue. Release notes document compatibility impact. Tags are immutable; corrections use a new version.

See [CONTRIBUTING](CONTRIBUTING), [SUPPORT](SUPPORT), [SECURITY](SECURITY), and [MAINTAINERS](MAINTAINERS) for governance.

## License

See [LICENSE](LICENSE).
