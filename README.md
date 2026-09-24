# storage

`github.com/stowcloud/storage` defines backend-neutral hierarchical storage contracts and reusable backends. The root package owns validated portable paths, entries, health, and optional capabilities. Subpackages own Linux confined local filesystems (`local`), S3-compatible buckets (`s3`), VeraCrypt filesystems (`veracrypt`), and Linux filesystem notifications (`watch`).

`local.Path` preserves existing POSIX byte names, including names that portable `storage.Path` cannot represent. Callers must validate product paths, reserved namespaces, grants, quotas, and upload policies before invoking a backend. The local backend confines operations to an opened root; `s3` and `veracrypt` use caller-provided scratch space for materialization.

No backend owns Stowcloud ACL, share identities, quota, transfer sessions, indexing, or application dependency injection. Product applications adapt backend capabilities at their integration boundary.

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
