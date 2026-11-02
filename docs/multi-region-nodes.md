# Nodes in multiple regions

CCM can manage node metadata and lifecycle across explicitly configured
Edgecenter region/project scopes. Load balancers and their security groups stay
in the primary `[Edgecenter]` scope; only nodes from that scope become backends.
This does not add cross-region routing, cross-region load balancers, CSI volume
attachment, or control-plane disaster recovery.

## Configuration

Keep the current DM configuration in `[Edgecenter]` and add a named node scope
for DT. The IDs below are examples; replace them with actual cloud IDs.

```ini
[Edgecenter]
api-url=https://api.example.invalid/cloud
api-token=<existing-token>
project-id=100
region-id=10
cluster-id=<existing-cluster-id>

[NodeRegion "dt"]
region-id=20
# Optional; if omitted/zero, inherit project-id from [Edgecenter].
project-id=100
```

Additional regions reuse the primary API endpoint, token, CA/TLS settings,
timeouts, and user agent. The existing token must have access to every scope.
Names must be unique and cannot be `default`; duplicate region/project scopes
are rejected at startup. A single-region configuration remains valid unchanged.

Prefer setting the region label through kubelet's `--node-labels` **before the
node registers**:

```text
# DT node
--node-labels=edgecenter.com/node-region=dt

# DM node (the primary [Edgecenter] scope)
--node-labels=edgecenter.com/node-region=default
```

These aliases select configuration sections; they are not inferred from a
hostname or from `topology.kubernetes.io/region`. A provider ID, when supplied,
retains the compatible format `edgecenter:///<instance-uuid>`.

Without the custom label, CCM searches every configured scope by UUID, or by
exact VM name if providerID is empty. Names are compared case-insensitively to
support the existing lowercased node-name convention. Ambiguous names/UUIDs or
unknown region labels fail reconciliation instead of selecting the first VM.
Legacy Instances and Zones calls, which do not receive a Node object, search all
configured scopes too. Avoid duplicate VM names across configured scopes.

## Deletion safety

A miss in DM is insufficient to delete a DT node. CCM reports a VM as absent
only after successful empty name lookups or explicit instance-GET 404 responses
in all candidate scopes. An explicit region label narrows those candidates to
one scope, so its configuration and label must be correct.

Permission errors (including 403), transport errors, invalid provider IDs, and
ambiguous results propagate as errors. They do not confirm VM deletion. If an
unlabelled node's other candidate scope is inaccessible, reconciliation waits
even when one VM was found, because uniqueness cannot be established safely.
Configure all required scopes before joining a foreign-region node; CCM cannot
discover regions that are absent from its configuration.

## Load balancers

Both pool members and security-group reconciliation use the same filtered node
list. DT nodes are excluded from DM load balancers. Unknown/ambiguous node
identities cause an error before cloud resources are changed. If no primary-
region nodes remain, reconciliation returns an error and does not create an
empty pool; existing backends are not changed by that reconciliation attempt.

With `externalTrafficPolicy: Cluster`, a DM backend can forward to pods on DT
only if the CNI/data path supports it. With `Local`, only local ready endpoints
can serve traffic on a backend. Verify pod placement and inter-region networking
before moving workloads. This change does not make regional disks portable.

## Rollout and verification

1. Build and test the branch; publish an immutable image tag/digest through the
   normal release process. Do not deploy directly over an unknown `latest`.
2. Save the current image digest, launch configuration, and cloud config for
   rollback. Add the node-region sections and check token permissions.
3. Update CCM on **all** masters, including standby replicas. Old replicas can
   become leader and delete foreign-region nodes again. Account for any
   separately migrated leader-election controllers in your deployment.
4. Set node labels at registration, then join the DT worker. Check `providerID`,
   InternalIP, topology labels, Ready state and node UID across multiple lifecycle
   reconciliation intervals.
5. Check Kafka/other LB pools contain only DM backend nodes and their health
   monitors pass. Test service traffic and pod-to-pod traffic across regions.
6. In staging, test VM deletion, 403, API outages, duplicate names, and an unknown
   region label. Only a confirmed VM absence may remove the Kubernetes Node.

Controller flag aliases are fixed as part of this branch. Both forms work:

```text
--controllers=*,-route,-cloud-node-lifecycle
--controllers=*,-node-route-controller,-cloud-node-lifecycle-controller
```

Disabling lifecycle is an optional temporary measure, not a requirement for
multi-region lookup. It disables automatic cloud-backed Node removal for the
entire cluster. Metadata initialization remains enabled. Retain existing
leader-election/migration configuration unless separately reviewed.

Before rolling back to a single-region CCM, stop/remove foreign-region nodes
from the cluster or use a verified build that disables lifecycle: the old
controller can otherwise resume deleting those Node objects.

Tests use mocked HTTP responses through the real SDK. No production cloud or
Kubernetes integration tests are performed by the local test suite.
