# Kubernetes Cloud Provider for Edgecenter Cloud

The Edgecenter Cloud Controller Manager provides the interface between a Kubernetes cluster and Edgecenter Cloud service APIs.
This project allows a Kubernetes cluster to provision, monitor, and remove Edgecenter Cloud resources necessary for the operation of the cluster.

See [Cloud Controller Manager Administration](https://kubernetes.io/docs/tasks/administer-cluster/running-cloud-controller/)
for more about Kubernetes cloud controller manager.

## Implementation Details

Currently `edgecenter-cloud-controller-manager` implements:

* servicecontroller - responsible for creating LoadBalancers when a service of `Type: LoadBalancer` is created in Kubernetes.
* nodecontroller - updates nodes with cloud provider specific labels and addresses.

## Compatibility with Kubernetes

| Kubernetes Version | Edgecenter Cloud Controller Manager Version |
|--------------------|---------------------------------------------|
| v1.31              | v1.0.0                                      |

## LoadBalancer Service Annotations

The following annotations can be used to customize LoadBalancer behavior.

| Annotation | Description | Create | Update | Notes |
|------------|------------|--------|--------|------|
| `loadbalancer.edgecenter.com/class` | Logical LB class (predefined network/subnet/floating config) | ✅ | ⚠️ Partial | Changes may not fully apply to existing LB |
| `loadbalancer.edgecenter.com/network-id` | Network ID for LB VIP | ✅ | ❌ | Used only during LB creation |
| `loadbalancer.edgecenter.com/subnet-id` | Subnet ID for LB VIP and pool members | ✅ | ⚠️ Partial | Does not move existing VIP |
| `loadbalancer.edgecenter.com/floating-network-id` | Floating network ID | ✅ | ❌ | Not fully applied on update |
| `service.beta.kubernetes.io/edgecenter-internal-load-balancer` | Internal LB (no floating IP) | ✅ | ⚠️ Partial | Switching may not clean up existing floating IP |
| `loadbalancer.edgecenter.com/default-tls-container-ref` | TLS secret ID for HTTPS termination | ✅ | ✅ | Updates listener or recreates it |
| `loadbalancer.edgecenter.com/x-forwarded-for` | Enable X-Forwarded-For (HTTP mode) | ✅ | ♻️ Recreate | Triggers listener/pool recreation |
| `loadbalancer.edgecenter.com/timeout-client-data` | Listener client timeout | ✅ | ✅ | Updated in-place |
| `loadbalancer.edgecenter.com/timeout-member-data` | Listener backend timeout | ✅ | ✅ | Updated in-place |
| `loadbalancer.edgecenter.com/timeout-member-connect` | Listener connect timeout | ✅ | ✅ | Updated in-place |
| `loadbalancer.edgecenter.com/proxy-protocol` | Enable PROXY protocol | ⚠️ | ❌ | Parsed but not applied |
| `loadbalancer.edgecenter.com/connection-limit` | Connection limit | ⚠️ | ❌ | Not implemented |
| `loadbalancer.edgecenter.com/timeout-tcp-inspect` | TCP inspect timeout | ⚠️ | ❌ | Not implemented |
| `loadbalancer.edgecenter.com/save-floating` | Preserve floating IP on delete | N/A | N/A | Used only during deletion |
| `loadbalancer.edgecenter.com/floating-subnet` | Floating subnet name | ⚠️ | ❌ | Not used |
| `loadbalancer.edgecenter.com/floating-subnet-id` | Floating subnet ID | ⚠️ | ❌ | Not used |
| `loadbalancer.edgecenter.com/keep-floatingip` | Keep floating IP | ⚠️ | ❌ | Not used |
| `loadbalancer.edgecenter.com/port-id` | Existing port ID | ⚠️ | ❌ | Not used |

---

### Legend

- ✅ — fully supported
- ⚠️ — partially supported or limited behavior
- ❌ — not supported
- ♻️ Recreate — change triggers resource recreation
- N/A — not applicable during normal reconciliation

---

### Notes

- Some changes (e.g. TLS or X-Forwarded-For) trigger **listener and pool recreation**, not in-place updates.
- Network-related settings (`network-id`, VIP subnet) are **immutable after creation**.
- Switching between internal and external LoadBalancers is **not fully reversible**.
- Some annotations are defined but **not implemented yet**.

---

## Local Debug

Run configuration with program arguments:
```
--kubeconfig=/deploy/manifests/kube-config.yaml
--cloud-config=/deploy/manifests/cloud-config.yaml
--cloud-provider=edgecenter
--v=4
```

### Example kube-config.yaml

Replace 127.0.0.1 with your cluster fip
```yaml
apiVersion: v1
clusters:
  - cluster:
      certificate-authority-data: <certificate-authority-data>
      server: https://127.0.0.1:6443
    name: kubernetes
contexts:
  - context:
      cluster: kubernetes
      user: kubernetes-admin
    name: kubernetes-admin@kubernetes
current-context: kubernetes-admin@kubernetes
kind: Config
preferences: { }
users:
  - name: kubernetes-admin
    user:
      client-certificate-data: <client-certificate-data>
      client-key-data: <client-key-data>
```

### Example cloud-config.yaml

```yaml
[ Global ]
  ca-file=/etc/kubernetes/ca-bundle.crt
  [LoadBalancer]
  use-octavia=true
  network-id=<network-id>
  subnet-id=<subnet-id>
  floating-network-id=<floating-network-id>
  create-monitor=yes
  monitor-delay=1m
  monitor-timeout=30s
  monitor-max-retries=3
  manage-security-groups=true
  [BlockStorage]
  bs-version=v2
  [Edgecenter]
  api-url=<api-url>
  project-id=<project-id>
  region-id=<region-id>
  api-token=<api-token>
  cluster-id=<cluster-id>
```