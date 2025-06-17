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

