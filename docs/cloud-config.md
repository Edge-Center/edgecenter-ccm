## Config openstack-cloud-controller-manager

###  Load Balancer

Although the edgecenter-cloud-controller-manager was initially implemented with Neutron-LBaaS support, Octavia is mandatory now because Neutron-LBaaS has been deprecated since Queens OpenStack release cycle and no longer accepted new feature enhancements.

* `enabled`
  Whether or not to enable the LoadBalancer type of Services integration at all.
  Default: true

* `floating-network-id`
  Optional. The external network used to create floating IP for the load balancer VIP. If there are multiple external networks in the cloud, either this option must be set or user must specify `loadbalancer.edgecenter.com/floating-network-id` in the Service annotation.

* `floating-subnet-id`
  Optional. The external network subnet used to create floating IP for the load balancer VIP. Can be overridden by the Service annotation `loadbalancer.edgecenter.com/floating-subnet-id`.

* `floating-subnet`
  Optional. A name pattern (glob or regexp if starting with `~`) for the external network subnet used to create floating IP for the load balancer VIP. Can be overridden by the Service annotation `loadbalancer.edgecenter.com/floating-subnet`. If multiple subnets match the first one with still available IPs is used.

* `floating-subnet-tags`
  Optional. Tags for the external network subnet used to create floating IP for the load balancer VIP. Can be overridden by the Service annotation `loadbalancer.edgecenter.com/floating-subnet-tags`. If multiple subnets match the first one with still available IPs is used.

* `lb-method`
  The load balancing algorithm used to create the load balancer pool.

  If `lb-provider` is set to "amphora" or "octavia" the value can be one of:
    * `ROUND_ROBIN` (default)
    * `LEAST_CONNECTIONS`
    * `SOURCE_IP`

  If `lb-provider` is set to "ovn" the value must be set to `SOURCE_IP_PORT`.

* `lb-provider`
  Optional. Used to specify the provider of the load balancer, e.g. "amphora" (default), "octavia" (deprecated alias for "amphora"), "ovn" or "f5". Only the "amphora", "octavia", "ovn" and "f5" providers are officially tested, other providers will cause a warning log.

* `lb-version`
  Optional. If specified, only "v2" is supported.

* `subnet-id`
  ID of the Neutron subnet on which to create load balancer VIP. This ID is also used to create pool members, if `member-subnet-id` is not set. For dual-stack deployments it's recommended to not set this option and let cloud-provider-openstack autodetect which subnet to use for which load balancer.

* `member-subnet-id`
  ID of the Neutron network on which to create the members of the load balancer. The load balancer gets another network port on this subnet. Defaults to `subnet-id` if not set.

* `network-id`
  ID of the Neutron network on which to create load balancer VIP, not needed if `subnet-id` is set. If not set network will be autodetected based on the network used by cluster nodes.

* `manage-security-groups`
  If the Neutron security groups should be managed separately. Default: false

* `create-monitor`
  Indicates whether or not to create a health monitor for the service load balancer. A health monitor required for services that declare `externalTrafficPolicy: Local`. Default: false

  NOTE: Health monitors for the `ovn` provider are only supported on OpenStack Wallaby and later.

* `monitor-delay`
  The time, in seconds, between sending probes to members of the load balancer. Default: 5

* `monitor-max-retries`
  The number of successful checks before changing the operating status of the load balancer member to ONLINE. A valid value is from 1 to 10. Default: 1

* `monitor-max-retries-down`
  The number of unsuccessful checks before changing the operating status of the load balancer member to ERROR. A valid value is from 1 to 10. Default: 3

* `monitor-timeout`
  The maximum time, in seconds, that a monitor waits to connect backend before it times out. Default: 3

* `internal-lb`
  Determines whether or not to create an internal load balancer (no floating IP) by default. Default: false.

* `node-selector`
  A comma separated list of key=value annotations used to filter nodes for targeting by the load balancer. When defined, only nodes that match all the specified key=value annotations will be targeted. If an annotation includes only a key without a value, the filter will check only for the existence of the key on the node. When node-selector is not set (default value), all nodes will be added as members to a load balancer pool.

  Note: This configuration option can be overridden with the `loadbalancer.edgecenter.com/node-selector` service annotation. Refer to [Exposing applications using services of LoadBalancer type](./expose-applications-using-loadbalancer-type-service.md)

  Example: To filter nodes with the labels `env=production` and `region=default`, set the `node-selector` as follows:

  ```
  node-selector="env=production, region=default"
  ```

  Example: To filter nodes that have the key `env` with any value and the key `region` specifically set to `default`, set the `node-selector` as follows:

  ```
  node-selector="env, region=default"
  ```

* `cascade-delete`
  Determines whether or not to perform cascade deletion of load balancers. Default: true.

* `flavor-id`
  The id of the loadbalancer flavor to use. Uses octavia default if not set.

* `availability-zone`
  The name of the loadbalancer availability zone to use. The Octavia availability zone capabilities will not be used if it is not set. The parameter will be ignored if the Octavia version doesn't support availability zones yet.

* `LoadBalancerClass "ClassName"`
  This is a config section including a set of config options. User can choose the `ClassName` by specifying the Service annotation `loadbalancer.edgecenter.com/class`. The following options are supported:

    * floating-network-id. The same with `floating-network-id` option above.
    * floating-subnet-id. The same with `floating-subnet-id` option above.
    * floating-subnet. The same with `floating-subnet` option above.
    * floating-subnet-tags. The same with `floating-subnet-tags` option above.
    * network-id. The same with `network-id` option above.
    * subnet-id. The same with `subnet-id` option above.
    * member-subnet-id. The same with `member-subnet-id` option above.

* `enable-ingress-hostname`

  Used with proxy protocol (set by annotation `loadbalancer.edgecenter.com/proxy-protocol: "true"`) by adding a dns suffix (nip.io) to the load balancer IP address. Default false.

  This option is currently a workaround for the issue https://github.com/kubernetes/ingress-nginx/issues/3996, should be removed or refactored after the Kubernetes [KEP-1860](https://github.com/kubernetes/enhancements/tree/master/keps/sig-network/1860-kube-proxy-IP-node-binding) is implemented.

* `ingress-hostname-suffix`

  The dns suffix to the load balancer IP address when using proxy protocol. Default nip.io

  This option is currently a workaround for the issue https://github.com/kubernetes/ingress-nginx/issues/3996, should be removed or refactored after the Kubernetes [KEP-1860](https://github.com/kubernetes/enhancements/tree/master/keps/sig-network/1860-kube-proxy-IP-node-binding) is implemented.

* `default-tls-container-ref`
  Reference to a tls container or secret. This option works with Octavia, when this option is set then the cloud provider will create an Octavia Listener of type TERMINATED_HTTPS for a TLS Terminated loadbalancer.

  Accepted format for tls container ref are `https://{keymanager_host}/v1/containers/{uuid}` and `https://{keymanager_host}/v1/secrets/{uuid}`.
  Check `container-store` parameter if you want to disable validation.

* `container-store`
  Optional. Used to specify the store of the tls-container-ref, e.g. "barbican" or "external" - other store will cause a warning log.
  Default value - `barbican` - existence of tls container ref would always be performed.

  If set to `external` format for tls container ref will not be validated.

* `max-shared-lb`
  The maximum number of Services that share a load balancer. Default: 2

* `provider-requires-serial-api-calls`
  Some Octavia providers do not support creating fully-populated loadbalancers using a single [API
  call](https://docs.openstack.org/api-ref/load-balancer/v2/?expanded=create-a-load-balancer-detail#creating-a-fully-populated-load-balancer).
  Setting this option to true will create loadbalancers using serial API calls which first create an unpopulated
  loadbalancer, then populate its listeners, pools and members. This is a compatibility option at the expense of
  increased load on the OpenStack API. Default: false 
