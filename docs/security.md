# Security model

A `VTTestServer` is a test database. It is not a secure database. Use it only for development, CI and preview  environments.

## No authentication

`vttestserver` accepts any user name and any password on the MySQL port (15003). It has no flag to change this. TLS is not available for the MySQL port. Access control is only via the Kubernetes namespace boundary and a NetworkPolicy.

## The vtctld gRPC port has no authentication

The gRPC port (15001) is the vtctld API. It has no authentication and it accepts write calls. The operator uses it to apply the VSchema.

The operator does not create NetworkPolicies. Create a NetworkPolicy in each namespace that has a `VTTestServer`. This example allows:

- gRPC from the operator namespace only
- MySQL from pods in the same namespace

Replace `<operator namespace>` with the namespace of the operator.

```yaml
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: vttestserver-grpc
spec:
  podSelector:
    matchLabels:
      app.kubernetes.io/name: vttestserver
  policyTypes: [Ingress]
  ingress:
    - from:
        - namespaceSelector:
            matchLabels:
              kubernetes.io/metadata.name: <operator namespace>
      ports:
        - {port: 15001, protocol: TCP}
    - from:
        - podSelector: {}
      ports:
        - {port: 15003, protocol: TCP}
```

If your application pods run in another namespace, add a `namespaceSelector` entry for port 15003.

## Secrets and S3 access

The operator never reads S3 or Secrets. The owner of the namespace creates the ServiceAccount (IRSA or Pod Identity) or the Secret for S3 access. The object refers to it by name. The Secret goes into the `datactl` containers with `envFrom`.

## Pod security

The server pods, the init containers and the snapshot Jobs pass the `restricted` Pod Security Standard:

- they run as user 999 (the owner of the `vttestserver` data directory), not as root
- they use the `RuntimeDefault` seccomp profile
- they drop all capabilities and do not allow privilege escalation
- they use a read-only root filesystem. Only the data volume and `/tmp` are writable.
