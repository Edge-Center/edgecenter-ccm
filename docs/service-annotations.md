## Supported Features

### Service annotations

- `loadbalancer.edgecenter.com/floating-network-id`

  The public network id which will allocate public IP for loadbalancer. This annotation works when the value of `service.beta.kubernetes.io/edgecenter-internal-load-balancer` is false.

- `loadbalancer.edgecenter.com/floating-subnet`

  A public network can have several subnets. This annotation is the name of subnet belonging to the floating network. This annotation is optional.

- `loadbalancer.edgecenter.com/floating-subnet-id`

  This annotation is the ID of a subnet belonging to the floating network, if specified, it takes precedence over `loadbalancer.edgecenter.com/floating-subnet` or `loadbalancer.edgecenter.com/floating-tag`.

- `loadbalancer.edgecenter.com/floating-subnet-tags`

  This annotation is the tag of a subnet belonging to the floating network.

- `loadbalancer.edgecenter.com/class`

  The name of a preconfigured class in the config file. If provided, this config options included in the class section take precedence over the annotations of floating-subnet-id, floating-network-id, network-id, subnet-id and member-subnet-id . See the section below for how it works.

- `loadbalancer.edgecenter.com/subnet-id`

  VIP subnet ID of load balancer created.

- `loadbalancer.edgecenter.com/member-subnet-id`

  Member subnet ID of the load balancer created.

- `loadbalancer.edgecenter.com/network-id`

  The network ID which will allocate virtual IP for loadbalancer.

- `loadbalancer.edgecenter.com/port-id`

  The VIP port ID for load balancer created.

- `loadbalancer.edgecenter.com/connection-limit`

  The maximum number of connections per second allowed for the listener. Positive integer or -1 for unlimited (default). This annotation supports update operation.

- `loadbalancer.edgecenter.com/keep-floatingip`

  If 'true', the floating IP will **NOT** be deleted. Default is 'false'.

- `loadbalancer.edgecenter.com/proxy-protocol`

  Enable the ProxyProtocol on all listeners. Default is 'false'.

  Values:
    - `v1`, `true`: enable the ProxyProtocol version 1
    - `v2`: enable the ProxyProtocol version 2

  Not supported when `lb-provider=ovn` is configured in openstack-cloud-controller-manager.

- `loadbalancer.edgecenter.com/x-forwarded-for`

  If 'true', `X-Forwarded-For` is inserted into the HTTP headers which contains the original client IP address so that the backend HTTP service is able to get the real source IP of the request. Please note that the cloud provider will force the creation of an Octavia listener of type `HTTP` if this option is set. Only applies when using Octavia.

  This annotation also works in conjunction with the `loadbalancer.edgecenter.com/default-tls-container-ref` annotation. In this case the cloud provider will create an Octavia listener of type `TERMINATED_HTTPS` instead of an `HTTP` listener.

  Not supported when `lb-provider=ovn` is configured in openstack-cloud-controller-manager.

- `loadbalancer.edgecenter.com/tls-secret-id`

  Secret ID for TLS termination on the load balancer. When set, listeners created during the initial load balancer creation and newly created listeners during subsequent reconciles will use `TERMINATED_HTTPS` with this secret. Existing listeners are not updated if the annotation changes or is removed.

- `loadbalancer.edgecenter.com/lb-method`

  Load balancing algorithm to use when distributed to members. [OpenStack Pool Creation | lb_algorithm](https://docs.openstack.org/api-ref/load-balancer/v2/#create-pool)

  Default value: defined in your OCCM configuration

  Possible values: `ROUND_ROBIN`, `LEAST_CONNECTIONS`, `SOURCE_IP`, `SOURCE_IP_PORT`

- `loadbalancer.edgecenter.com/timeout-client-data`

  Frontend client inactivity timeout in milliseconds for the load balancer.

  Not supported when `lb-provider=ovn` is configured in openstack-cloud-controller-manager.

- `loadbalancer.edgecenter.com/timeout-member-connect`

  Backend member connection timeout in milliseconds for the load balancer.

  Not supported when `lb-provider=ovn` is configured in openstack-cloud-controller-manager.

- `loadbalancer.edgecenter.com/timeout-member-data`

  Backend member inactivity timeout in milliseconds for the load balancer.

  Not supported when `lb-provider=ovn` is configured in openstack-cloud-controller-manager.

- `loadbalancer.edgecenter.com/timeout-tcp-inspect`

  Time to wait for additional TCP packets for content inspection in milliseconds for the load balancer.

  Not supported when `lb-provider=ovn` is configured in openstack-cloud-controller-manager.

- `service.beta.kubernetes.io/edgecenter-internal-load-balancer`

  If 'true', the loadbalancer VIP won't be associated with a floating IP. Default is 'false'. This annotation is ignored if only internal Service is allowed to create in the cluster.

- `loadbalancer.edgecenter.com/enable-health-monitor`

  Defines whether to create health monitor for the load balancer pool, if not specified, use `create-monitor` config. The health monitor can be created or deleted dynamically. A health monitor is required for services with `externalTrafficPolicy: Local`.

  NOTE: Health monitors for the `ovn` provider are only supported on OpenStack Wallaby and later.

- `loadbalancer.edgecenter.com/health-monitor-delay`

  Defines the health monitor delay in seconds for the loadbalancer pools.

- `loadbalancer.edgecenter.com/health-monitor-timeout`

  Defines the health monitor timeout in seconds for the loadbalancer pools. This value should be less than delay

- `loadbalancer.edgecenter.com/health-monitor-max-retries`

  Defines the health monitor retry count for the loadbalancer pool members.

- `loadbalancer.edgecenter.com/health-monitor-max-retries-down`

  Defines the health monitor retry count for the loadbalancer pool members to be marked down.

- `loadbalancer.edgecenter.com/flavor-id`

  The id of the flavor that is used for creating the loadbalancer.

  Not supported when `lb-provider=ovn` is configured in openstack-cloud-controller-manager.

- `loadbalancer.edgecenter.com/availability-zone`

  The name of the loadbalancer availability zone to use. It is ignored if the Octavia version doesn't support availability zones yet.

  Not supported when `lb-provider=ovn` is configured in openstack-cloud-controller-manager.

- `loadbalancer.edgecenter.com/default-tls-container-ref`

  Reference to a tls container. This option works with Octavia, when this option is set then the cloud provider will create an Octavia Listener of type `TERMINATED_HTTPS` for a TLS Terminated loadbalancer.
  Format for tls container ref: `https://{keymanager_host}/v1/containers/{uuid}`

  When `container-store` parameter is set to `external` format for `default-tls-container-ref` could be any string.

  Not supported when `lb-provider=ovn` is configured in openstack-cloud-controller-manager.

- `loadbalancer.edgecenter.com/load-balancer-id`

  This annotation is automatically added to the Service if it's not specified when creating. After the Service is created successfully it shouldn't be changed, otherwise the Service won't behave as expected.

  If this annotation is specified with a valid cloud load balancer ID when creating Service, the Service is reusing this load balancer rather than creating another one. Again, it shouldn't be changed after the Service is created.

  If this annotation is specified, the other annotations which define the load balancer features will be ignored.

- `loadbalancer.edgecenter.com/hostname`

  This annotations explicitly sets a hostname in the status of the load balancer service.

- `loadbalancer.edgecenter.com/load-balancer-address`

  This annotation is automatically added and it contains the floating ip address of the load balancer service.
  When using `loadbalancer.edgecenter.com/hostname` annotation it is the only place to see the real address of the load balancer.

- `loadbalancer.edgecenter.com/node-selector`

  A set of key=value annotations used to filter nodes for targeting by the load balancer. When defined, only nodes that match all the specified key=value annotations will be targeted. If an annotation includes only a key without a value, the filter will check only for the existence of the key on the node. If the value is not set, the `node-selector` value defined in the OCCM configuration is applied.

  Example: To filter nodes with the labels `env=production` and `region=default`, set the `loadbalancer.edgecenter.com/node-selector` annotation to `env=production, region=default`

### Switching between Floating Subnets by using preconfigured Classes

If you have multiple `FloatingIPPools` and/or `FloatingIPSubnets` it might be desirable to offer the user logical meanings for `LoadBalancers` like `internetFacing` or `DMZ` instead of requiring the user to select a dedicated network or subnet ID at the service object level as an annotation.

With a `LoadBalancerClass` it possible to specify to which floating network and corresponding subnetwork the `LoadBalancer` belong.

In the example `cloud.conf` below three `LoadBalancerClass`'es have been defined: `internetFacing`, `dmz` and `office`

```ini
[LoadBalancer]
floating-network-id="a57af0a0-da92-49be-a98a-345ceca004b3"
floating-subnet-id="a02eb6c3-fc69-46ae-a3fe-fb43c1563cbc"
subnet-id="fa6a4e6c-6ae4-4dde-ae86-3e2f452c1f03"
create-monitor=true
monitor-delay=60s
monitor-timeout=30s
monitor-max-retries=1
monitor-max-retries-down=3

[LoadBalancerClass "internetFacing"]
floating-network-id="c57af0a0-da92-49be-a98a-345ceca004b3"
floating-subnet-id="f90d2440-d3c6-417a-a696-04e55eeb9279"

[LoadBalancerClass "dmz"]
floating-subnet-id="a374bed4-e920-4c40-b646-2d8927f7f67b"

[LoadBalancerClass "office"]
floating-subnet-id="b374bed4-e920-4c40-b646-2d8927f7f67b"
```

Within a `LoadBalancerClass` one of `floating-subnet-id`, `floating-subnet` or `floating-subnet-tags` is mandatory.
`floating-subnet-id` takes precedence over the other ones with must all match if specified.
If the pattern starts with a `!`, the match is negated.
The rest of the pattern can either be a direct name, a glob or a regular expression if it starts with a `~`.
`floating-subnet-tags` can be a comma separated list of tags. By default it matches a subnet if at least one tag is present.
If the list is preceded by a `&` all tags must be present. Again with a preceding `!` the condition be be negated.
`floating-network-id` is optional can be defined in case it differs from the default `floating-network-id` in the `LoadBalancer` section.

By using the `loadbalancer.edgecenter.com/class` annotation on the service object, you can now select which floating subnets the `LoadBalancer` should be using.

```yaml
apiVersion: v1
kind: Service
metadata:
  annotations:
    loadbalancer.edgecenter.com/class: internetFacing
  name: nginx-internet
spec:
  type: LoadBalancer
  selector:
    app: nginx
  ports:
  - port: 80
    targetPort: 80
```

### Creating Service by specifying a floating IP

Sometimes it's useful to use an existing available floating IP rather than creating a new one, especially in the automation scenario. In the example below, 122.112.219.229 is an available floating IP created in the OpenStack Networking service.

> NOTE: If 122.112.219.229 is not available, a new floating IP will be created automatically from the configured public network. If 122.112.219.229 is already associated with another port, the Service creation will fail.

```yaml
apiVersion: v1
kind: Service
metadata:
  name: nginx-internet
spec:
  type: LoadBalancer
  selector:
    app: nginx
  ports:
  - port: 80
    targetPort: 80
  loadBalancerIP: 122.112.219.229
```

### Restrict Access For LoadBalancer Service

When using a Service with `spec.type: LoadBalancer`, you can specify the IP ranges that are allowed to access the load balancer by using `spec.loadBalancerSourceRanges`. This field takes a list of IP CIDR ranges, which Kubernetes will use to configure firewall exceptions.

In the following example, a load balancer will be created that is only accessible to clients with IP addresses in 192.168.32.1/24.

```yaml
apiVersion: v1
kind: Service
metadata:
  name: test
  namespace: default
spec:
  type: LoadBalancer
  loadBalancerSourceRanges:
    - 192.168.32.1/24
  selector:
    run: echoserver
  ports:
    - protocol: TCP
      port: 80
      targetPort: 8080
```

`loadBalancerSourceRanges` field supports to be updated.