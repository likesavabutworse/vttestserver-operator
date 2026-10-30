# vttestserver-operator

![License](https://img.shields.io/badge/License-Apache%202.0-blue.svg)
[![Tests](https://github.com/likesavabutworse/vttestserver-operator/actions/workflows/test.yml/badge.svg)](https://github.com/likesavabutworse/vttestserver-operator/actions/workflows/test.yml)
[![Lint](https://github.com/likesavabutworse/vttestserver-operator/actions/workflows/lint.yml/badge.svg)](https://github.com/likesavabutworse/vttestserver-operator/actions/workflows/lint.yml)

`vttestserver-operator` manages ephemeral Vitess [`vttestserver`](https://vitess.io/docs/) instances on
Kubernetes. It is meant for CI pipelines and PR/preview environments, where tests need a real Vitess
instance with a predictable schema and dataset.

A `VTTestServer` resource describes an instance: its image, keyspaces, VSchema and initialization SQL. Once
an instance holds the data you want, a `VTTestServerSnapshot` captures it to S3, and any later `VTTestServer`
can restore from that snapshot, so every PR environment or CI run starts from the same known dataset.

## Features

- Describe a Vitess instance as a Kubernetes object: image, keyspaces, VSchema, init SQL.
- Snapshot an instance's data to S3 and restore it into new instances by tag.
- No controller-side state: everything is in the `status` of the objects, so the operator can run with two
  replicas and restart without affecting instances that are already running.
- Prometheus metrics for instance and snapshot counts, phases and snapshot duration 
- Published images are signed and carry an SBOM and build provenance attestation. See
  [AGENTS.md#releasing](AGENTS.md#releasing) to verify one.

## Getting started

### Prerequisites

- A Kubernetes cluster and Helm 3.
- An S3 bucket (or S3-compatible store) if you plan to use snapshots.

### Install the chart

```sh
helm install vttestserver-operator \
  oci://ghcr.io/likesavabutworse/charts/vttestserver-operator \
  --version 0.1.0 \
  --namespace vttestserver-operator-system --create-namespace
```

This installs the CRDs and the operator, watching every namespace with a ClusterRole. See
[Configuration](#configuration) to scope it to specific namespaces, or [Develop](#develop) to build your own
images instead of the published ones.

### Create your first instance

```sh
kubectl apply -n my-namespace -f examples/server-basic.yaml
kubectl get vttestserver -n my-namespace -w
```

`Ready` means vtgate accepts connections and any VSchema or init SQL has been applied. Find the MySQL
endpoint and connect. Any user name and password are accepted:

```sh
kubectl get vttestserver basic -n my-namespace -o jsonpath='{.status.mysql}{"\n"}'
mysql -h basic.my-namespace.svc -P 15003 -u test
```

## Custom resources

### `VTTestServer`

A `VTTestServer` is one Vitess instance. The fields that matter day to day:

- `image` -- the `vttestserver` image to run. See https://vitess.io/docs/23.0/get-started/vttestserver-docker-image/
- `keyspaces` -- name and shard count for each keyspace.
- `vschema` / `initSQL` -- a ConfigMap reference for the VSchema, and one or more ConfigMap keys of SQL that
  run once the instance is up. A keyspace with more than one shard needs a VSchema with `"sharded": true`.
- `storage` -- `ephemeral` (default; data is lost on pod restart) or `persistent` with a size.
- `restoreFrom` -- restore from a `VTTestServerSnapshot`, by `tag` (the newest snapshot under that tag) or by
  `name` (one specific snapshot). The restored keyspaces must match the snapshot's.

The instance moves through `Pending`, `Restoring` (if `restoreFrom` is set), `Starting`, `Configuring` and
`Ready`. See [examples/server-sharded.yaml](examples/server-sharded.yaml) for a full example with VSchema and
init SQL.

### `VTTestServerSnapshot`

A `VTTestServerSnapshot` captures the data of one `VTTestServer` and stores it in S3, so other instances can
restore from it later. Taking a snapshot briefly stops the source instance, archives its data directory, and
starts it again; `tags` let a restoring `VTTestServer` ask for "the latest snapshot named X" instead of a
specific snapshot name. `deletionPolicy` and `ttl` control whether and when the S3 objects are cleaned up.
See [examples/snapshot.yaml](examples/snapshot.yaml).

## Typical workflow

The snapshot/restore round trip is the main thing this operator is for -- seeding a dataset once and reusing
it across many short-lived environments:

1. **Create a source instance.** Apply [server-basic.yaml](examples/server-basic.yaml) or
   [server-sharded.yaml](examples/server-sharded.yaml) and wait for `Ready`.
2. **Load your data.** Connect with any MySQL client and insert what your tests need.
3. **Snapshot it.** Apply [snapshot.yaml](examples/snapshot.yaml) to persist the dataset to S3.
4. **Restore it into a new instance.** Apply [restore.yaml](examples/restore.yaml), which creates a
   `VTTestServer` with `restoreFrom` pointing at the snapshot's tag.
5. **Run your tests** against the restored instance, then delete it. The snapshot in S3 is untouched.

## Architecture

| Custom resource | Kubernetes resources it manages |
|---|---|
| `VTTestServer` | A StatefulSet (one pod), a Service for MySQL (15003), a Service for vtctld gRPC (15001), and ConfigMaps. |
| `VTTestServerSnapshot` | A Job that stops the instance, uploads its data directory, and starts it again. |

For reconciliation internals, `datactl`, and leader election, see [AGENTS.md](AGENTS.md).

## Configuration

| Value | Description | Default |
|---|---|---|
| `manager.watchNamespaces` | Namespaces the operator watches. Empty means all namespaces. | `[]` |
| `rbac.namespaced` | Use a Role/RoleBinding per watched namespace instead of a ClusterRole. | `false` |
| `manager.replicas` | Operator replicas (leader election is always on). | `2` |
| `manager.image.repository` / `datactl.image.repository` | Operator and `datactl` image repositories. | published `ghcr.io` images |
| `crd.keep` | Keep the CRDs (and your `VTTestServer` data) when the chart is uninstalled. | `true` |
| `metrics.enabled` | Serve Prometheus metrics. | `true` |
| `prometheus.enabled` | Create a `ServiceMonitor` (requires prometheus-operator). | `false` |
| `networkPolicy.enabled` | Restrict ingress to the controller manager. | `false` |

See [dist/chart/values.yaml](dist/chart/values.yaml) for the full set.

## Scope and limitations

Instances are for testing and development, not production workloads. `vttestserver` has no authentication:
any user name and password are accepted on the MySQL port, and the vtctld gRPC port accepts unauthenticated
write calls. The operator does not create NetworkPolicies, does not seed application data beyond what you
give it in `initSQL`, and does not build or publish the `vttestserver` image. See
[docs/security.md](docs/security.md) for the full security model and a NetworkPolicy example.

## Develop

You need Go, Docker, Helm, [Tilt](https://tilt.dev) and a local Kubernetes cluster. 

```sh
make test      # unit tests and envtest (downloads the test binaries into ./bin)
make lint
make helm-check
tilt up         # builds the images, installs the chart, and starts a small S3 store (Silo)
```

In the Tilt UI, trigger the manual resources in this order: `golden-source`, `golden-seed`, `golden-snapshot`,
`pr-env`, `pr-env-verify` — this runs the workflow above against a real cluster. `tilt ci` runs the same
workflow non-interactively (it auto-triggers under `GITHUB_ACTIONS=true`; see the `ci` flag near the top of
the [Tiltfile](Tiltfile)).

Pushing a tag matching `v*` runs [.github/workflows/release.yaml](.github/workflows/release.yaml), which
builds and publishes the images and the Helm chart used above. To build your own images for a private
registry instead, see [AGENTS.md](AGENTS.md#releasing).

## Guides

| Guide | Description |
|---|---|
| [docs/security.md](docs/security.md) | The security model and a NetworkPolicy example. |
| [AGENTS.md](AGENTS.md) | Repository layout, internals, and conventions for contributors and AI agents. |

## License

Apache License 2.0 — see [LICENSE](LICENSE).
