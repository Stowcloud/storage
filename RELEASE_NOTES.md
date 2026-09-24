# Release notes

## v0.2.0

Added Linux descriptor-confined local storage, S3-compatible object storage, a VeraCrypt hierarchy adapter, and Linux inotify watching. Existing root-package contracts remain available. Local paths preserve POSIX byte names separately from portable `storage.Path`; backend implementations do not perform product ACL or quota checks.

S3 now owns SDK requests, bounded listings and multipart operations. The VeraCrypt adapter uses `github.com/stowcloud/veracrypt` for container and filesystem operations. Watch subscriptions report lost coverage rather than silently claiming a complete view.

## v0.1.0

Initial public release of backend-neutral hierarchical storage contracts:

- validated relative `Path` values;
- neutral entry and health models;
- read hierarchy and optional capability interfaces;
- idempotent materialization leases;
- independent contract tests and cross-platform compile workflow.

No Stowcloud product policy, ACL, quota, transfer state, or application dependencies are included.
