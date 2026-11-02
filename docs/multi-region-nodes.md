# Nodes in multiple regions

CCM can manage node metadata and lifecycle across explicitly configured
Edgecenter region/project scopes. Load balancers default to the primary `[Edgecenter]` scope. A Service can select
another configured region, with its own network settings and backend selector.
This does not add cross-region routing, a single load balancer spanning regions,
static IP reservation, CSI volume attachment, or control-plane disaster recovery.

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

The regional network and selector interface is adapted from
[PR #20 by roquie](https://github.com/Edge-Center/edgecenter-ccm/pull/20), without
its static-IP feature or dependency upgrades. A Service needs only `region-id`;
there is no requirement for an `ip-address` annotation in this implementation.

For a DT region with cloud ID `20`, also configured under `[NodeRegion "dt"]`:

```ini
[LoadBalancerRegion "20"]
network-id=dt-network-id
subnet-id=dt-subnet-id
floating-network-id=dt-floating-network-id
node-selector=edgecenter.com/node-region=dt
```

```yaml
apiVersion: v1
kind: Service
metadata:
  name: example-dt
  annotations:
    loadbalancer.edgecenter.com/region-id: "20"
    service.beta.kubernetes.io/edgecenter-internal-load-balancer: "true"
spec:
  type: LoadBalancer
  selector:
    app: example
  ports:
    - port: 8080
      targetPort: 8080
```

Use actual cloud IDs for network settings, not the example names. A foreign
region must have a `NodeRegion`, a `LoadBalancerRegion` with a non-empty selector,
and an explicit network or subnet. Its client is reused from the configured
node scopes, including the configured project. Region IDs appearing in several
configured projects are ambiguous and cannot be selected by `region-id`.

Foreign-region Services never inherit network/subnet/floating-network IDs from
DM. Specify DT's network settings; for an external LB, set its floating network
explicitly or allow discovery within DT. Primary-region Services keep their
existing defaults. Service network/class annotations still override the network
settings and must reference resources in the selected region.

The node-selector first selects candidate nodes, then CCM resolves each VM using
the node-region resolver. A selected node belonging to another actual configured
scope causes an error **before any LB mutation**. A label such as `placement=dt`
alone is not sufficient to override the resolved scope. Pool members and security
groups receive the same verified list. API failures also stop reconciliation.

Without a selector, primary-region LBs continue to exclude foreign-region nodes.
If no suitable nodes remain, reconciliation returns an error instead of creating
an empty pool; existing backends are not changed in that attempt. Lookup, update,
and deletion use the same Service-selected regional client as creation.

Use one Service per regional LB. Do not change `region-id` on an existing
Service: there is no automatic migration or historical-region tracking. Delete
it, wait for finalizer cleanup using the original annotation/configuration, then
recreate it in the new region. Keep the regional configuration and token access
until cleanup completes. Moving a Service without this sequence may orphan its
old regional load balancer. Enforce immutability through your deployment or
admission workflow if users can edit these annotations.

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
5. Check existing Kafka/DM LB pools contain only DM backend nodes; a DT Service
   must contain only DT backend nodes. Check health monitors and service traffic,
   and test pod-to-pod traffic across regions.
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

Cloud configuration is not printed in full. Logs use the existing allowlist of
API endpoint, project ID, and region ID; API tokens are omitted. Go and Kubernetes
dependency versions remain unchanged from the parent branch.
