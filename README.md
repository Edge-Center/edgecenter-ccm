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

## Local Debug

Run configuration with program arguments:
```
--kubeconfig=manifests/kube-config.yaml
--cloud-config=manifests/cloud-config.yaml
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

### Example ca-bundle.crt

```yaml
-----BEGIN CERTIFICATE-----
MIIBeTCCAR+gAwIBAgIIW0pjwDxCrKwwCgYIKoZIzj0EAwIwFTETMBEGA1UEAxMK
a3ViZXJuZXRlczAeFw0yNTA1MjEwODUzNDdaFw0zNTA1MTkwODU4NDdaMBUxEzAR
BgNVBAMTCmt1YmVybmV0ZXMwWTATBgcqhkjOPQIBBggqhkjOPQMBBwNCAAQZcd4m
xg9nxLhGXdXbUe+wKfO1q0lWxxcfTa/y1biVySMzyfYqsCPK8ykRlMmLnwtosUPU
pgzZBb4ZrJcL4K4Eo1kwVzAOBgNVHQ8BAf8EBAMCAqQwDwYDVR0TAQH/BAUwAwEB
/zAdBgNVHQ4EFgQUpcP84gBCw8CbIIH6wDNkNFvPMU8wFQYDVR0RBA4wDIIKa3Vi
ZXJuZXRlczAKBggqhkjOPQQDAgNIADBFAiEA/WeEOLbGusSUmgRWgimdfM+mdxTW
SD95GAbGBM8VfUICIACmEaCv/uZ+GKlvoNHHEHQFO//z764yN4i1sRI9yYXU
-----END CERTIFICATE-----
```