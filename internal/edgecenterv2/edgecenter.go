/*
Copyright 2014 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package edgecenter

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"reflect"
	"sort"
	"strings"
	"time"

	edgecloud "github.com/Edge-Center/edgecentercloud-go/v2"
	"github.com/spf13/pflag"
	"gopkg.in/gcfg.v1"
	apiv1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	netutil "k8s.io/apimachinery/pkg/util/net"
	certutil "k8s.io/client-go/util/cert"
	cloudprovider "k8s.io/cloud-provider"
	"k8s.io/klog/v2"

	v1helper "ec-ccm/internal/apis/core/v1/helper"
	"ec-ccm/internal/util/metadata"
	"ec-ccm/internal/version"
)

const (
	// ProviderName is the name of the edgecenter provider
	ProviderName = "edgecenter"

	// TypeHostName is the name type of edgecenter instance
	TypeHostName   = "hostname"
	defaultTimeOut = 60 * time.Second

	// NoBillingMeta is used to label resources as not being billing
	NoBillingMeta = "edgecenter_no_billing"

	InternalClusterNameMeta = "edgecenter_cluster_name"
)

// ErrNotFound is used to inform that the object is missing
var ErrNotFound = errors.New("failed to find object")

// ErrMultipleResults is used when we unexpectedly get back multiple results
var ErrMultipleResults = errors.New("multiple results where only one expected")

// ErrNoAddressFound is used when we cannot find an ip address for the host
var ErrNoAddressFound = errors.New("no address found for host")

// ErrIPv6SupportDisabled is used when one tries to use IPv6 Addresses when
// IPv6 support is disabled by config
var ErrIPv6SupportDisabled = errors.New("IPv6 support is disabled")

// userAgentData is used to add extra information to the edgecenter user-agent
var userAgentData []string

// AddExtraFlags is called by the main package to add component specific command line flags
func AddExtraFlags(fs *pflag.FlagSet) {
	fs.StringArrayVar(&userAgentData, "user-agent", nil, "Extra data to add to edgecenter user-agent. Use multiple times to add more than one component.")
}

// MyDuration is the encoding.TextUnmarshaler interface for time.Duration
type MyDuration struct {
	time.Duration
}

// UnmarshalText is used to convert from text to Duration
func (d *MyDuration) UnmarshalText(text []byte) error {
	res, err := time.ParseDuration(string(text))
	if err != nil {
		return err
	}
	d.Duration = res
	return nil
}

// LoadBalancer is used for creating and maintaining load balancers
type LoadBalancer struct {
	client     *edgecloud.Client
	opts       LoadBalancerOpts
	edgecenter EdgecenterOpts
}

// LoadBalancerOpts have the options to talk to Neutron LBaaSV2 or Octavia
type LoadBalancerOpts struct {
	LBVersion            string              `gcfg:"lb-version"`          // overrides autodetection. Only support v2.
	UseOctavia           bool                `gcfg:"use-octavia"`         // uses Octavia V2 service catalog endpoint
	SubnetID             string              `gcfg:"subnet-id"`           // overrides autodetection.
	NetworkID            string              `gcfg:"network-id"`          // If specified, will create virtual ip from a subnet in network which has available IP addresses
	FloatingNetworkID    string              `gcfg:"floating-network-id"` // If specified, will create floating ip for loadbalancer, or do not create floating ip.
	FloatingSubnetID     string              `gcfg:"floating-subnet-id"`  // If specified, will create floating ip for loadbalancer in this particular floating pool subnetwork.
	LBClasses            map[string]*LBClass // Predefined named Floating networks and subnets
	LBMethod             string              `gcfg:"lb-method"` // default to ROUND_ROBIN.
	LBProvider           string              `gcfg:"lb-provider"`
	CreateMonitor        bool                `gcfg:"create-monitor"`
	MonitorDelay         MyDuration          `gcfg:"monitor-delay"`
	MonitorTimeout       MyDuration          `gcfg:"monitor-timeout"`
	MonitorMaxRetries    uint                `gcfg:"monitor-max-retries"`
	ManageSecurityGroups bool                `gcfg:"manage-security-groups"`
	NodeSecurityGroupIDs []string            // Do not specify, get instance_types automatically when enable manage-security-groups. TODO(FengyunPan): move instance_types into cache
	InternalLB           bool                `gcfg:"internal-lb"` // default false
}

// LBClass defines the corresponding floating network, floating subnet or internal subnet ID
type LBClass struct {
	FloatingNetworkID string `gcfg:"floating-network-id,omitempty"`
	FloatingSubnetID  string `gcfg:"floating-subnet-id,omitempty"`
	SubnetID          string `gcfg:"subnet-id,omitempty"`
	NetworkID         string `gcfg:"network-id,omitempty"`
}

// BlockStorageOpts is used to talk to Cinder service
type BlockStorageOpts struct {
	BSVersion       string `gcfg:"bs-version"`        // overrides autodetection. apiv1 or v2. Defaults to auto
	TrustDevicePath bool   `gcfg:"trust-device-path"` // See Issue #33128
	IgnoreVolumeAZ  bool   `gcfg:"ignore-volume-az"`
}

// NetworkingOpts is used for networking settings
type NetworkingOpts struct {
	IPv6SupportDisabled bool   `gcfg:"ipv6-support-disabled"`
	PublicNetworkName   string `gcfg:"public-network-name"`
	InternalNetworkName string `gcfg:"internal-network-name"`
}

// RouterOpts is used for Neutron routes
type RouterOpts struct {
	RouterID string `gcfg:"router-id"` // required
}

// MetadataOpts is used for configuring how to talk to metadata service or config drive
type MetadataOpts struct {
	SearchOrder    string     `gcfg:"search-order"`
	RequestTimeout MyDuration `gcfg:"request-timeout"`
}

// Edgecenter is an implementation of cloud provider interface for Edgecenter.
type Edgecenter struct {
	client         *edgecloud.Client
	region         string
	cfg            Config
	lbOpts         LoadBalancerOpts
	bsOpts         BlockStorageOpts
	routeOpts      RouterOpts
	metadataOpts   MetadataOpts
	networkingOpts NetworkingOpts
	// InstanceID of the server where this Edgecenter object is instantiated.
	localInstanceID string
}

func (ec *Edgecenter) InstancesV2() (cloudprovider.InstancesV2, bool) {
	return nil, false
}

// EdgecenterOpts is used to talk to Edgecenter service
type EdgecenterOpts struct {
	ApiURL       string `gcfg:"api-url" mapstructure:"api-url"`
	ProjectID    int    `gcfg:"project-id" mapstructure:"project-id"`
	RegionID     int    `gcfg:"region-id" mapstructure:"region-id"`
	ApiToken     string `gcfg:"api-token" mapstructure:"api-token"`
	ClusterID    string `gcfg:"cluster-id" mapstructure:"cluster-id"`
	NoBillingTag bool   `gcfg:"no-billing-tag" mapstructure:"no-billing-tag"` // default false

	Region string `name:"os-region"`
	CAFile string `gcfg:"ca-file" mapstructure:"ca-file" name:"os-certAuthorityPath" value:"optional"`
	// Manila only options
	TLSInsecure string `name:"os-TLSInsecure" value:"optional" dependsOn:"os-certAuthority|os-certAuthorityPath" matches:"^true|false$"`
	// backward compatibility with the manila-csi-plugin
	CAFileContents string `name:"os-certAuthority" value:"optional"`
}

// Config is used to read and store information from the cloud configuration file
type Config struct {
	LoadBalancer      LoadBalancerOpts
	LoadBalancerClass map[string]*LBClass
	BlockStorage      BlockStorageOpts
	Route             RouterOpts
	Metadata          MetadataOpts
	Networking        NetworkingOpts
	Edgecenter        EdgecenterOpts
}

func LogCfg(cfg Config) {
	klog.V(5).Infof("Edgecenter ApiURL %s", cfg.Edgecenter.ApiURL)
	klog.V(5).Infof("Edgecenter ProjectID %d", cfg.Edgecenter.ProjectID)
	klog.V(5).Infof("Edgecenter RegionID %d", cfg.Edgecenter.RegionID)
}

func init() {
	RegisterMetrics()

	cloudprovider.RegisterCloudProvider(ProviderName, func(config io.Reader) (cloudprovider.Interface, error) {

		cfg, err := ReadConfig(config)
		if err != nil {
			klog.Infof("Error reading config: %v", err)
		}

		klog.Infof("Config after read %v", cfg)
		if err != nil {
			return nil, err
		}
		cloud, err := NewEdgecenter(cfg)
		if err != nil {
			klog.V(1).Infof("New edgecenter client created failed with config")
		}
		klog.Infof("### edgecenter client created")
		return cloud, err
	})
}

// ReadConfig reads values from the cloud.conf
func ReadConfig(config io.Reader) (Config, error) {
	if config == nil {
		return Config{}, fmt.Errorf("no Edgecenter cloud provider config file given")
	}
	var cfg Config
	err := gcfg.FatalOnly(gcfg.ReadInto(&cfg, config))

	if err != nil {
		klog.Infof("Error reading config: %v", err)
		return Config{}, err
	}

	klog.Infof("CONFIG %v", cfg.Edgecenter)

	klog.V(5).Infof("Config, loaded from the config file:")
	LogCfg(cfg)

	// Set the default values for search order if not set
	if cfg.Metadata.SearchOrder == "" {
		cfg.Metadata.SearchOrder = fmt.Sprintf("%s,%s", metadata.ConfigDriveID, metadata.MetadataID)
	}

	if cfg.Metadata.RequestTimeout == (MyDuration{}) {
		cfg.Metadata.RequestTimeout.Duration = defaultTimeOut
	}

	// ini file doesn't support maps, so we are reusing top level subsections
	// and copy the resulting map to corresponding loadbalancer section
	cfg.LoadBalancer.LBClasses = cfg.LoadBalancerClass

	return cfg, err
}

// caller is a tiny helper for conditional unwind logic
type caller bool

func newCaller() caller   { return true }
func (c *caller) disarm() { *c = false }

func (c *caller) call(f func()) {
	if *c {
		f()
	}
}

func readInstanceID(searchOrder string) (string, error) {
	// First, try to get data from metadata service because local
	// data might be changed by accident
	md, err := metadata.Get(searchOrder)
	if err == nil {
		return md.UUID, nil
	}

	// Try to find instance ID on the local filesystem (created by cloud-init)
	const instanceIDFile = "/var/lib/cloud/data/instance-id"
	idBytes, err := os.ReadFile(instanceIDFile)
	if err == nil {
		instanceID := string(idBytes)
		instanceID = strings.TrimSpace(instanceID)
		klog.V(3).Infof("Got instance id from %s: %s", instanceIDFile, instanceID)
		if instanceID != "" && instanceID != "iid-datasource-none" {
			return instanceID, nil
		}
	}

	return "", err
}

// check opts for Edgecenter
func checkEdgecenterOpts(edgecenterOpts *Edgecenter) error {
	lbOpts := edgecenterOpts.lbOpts

	// if need to create health monitor for Neutron LB,
	// monitor-delay, monitor-timeout and monitor-max-retries should be set.
	if lbOpts.CreateMonitor {
		if lbOpts.MonitorDelay == (MyDuration{}) {
			return fmt.Errorf("monitor-delay not set in cloud provider config")
		}
		if lbOpts.MonitorTimeout == (MyDuration{}) {
			return fmt.Errorf("monitor-timeout not set in cloud provider config")
		}
		if lbOpts.MonitorMaxRetries == uint(0) {
			return fmt.Errorf("monitor-max-retries not set in cloud provider config")
		}
	}
	return checkMetadataSearchOrder(edgecenterOpts.metadataOpts.SearchOrder)
}

// NewEdgecenterClient creates a new instance of the edgecenter client
func NewEdgecenterClient(cfg EdgecenterOpts, userAgent string, extraUserAgent ...string) (*edgecloud.Client, error) {

	if cfg.ApiToken == "" {
		err := errors.New("token is not set in configuration")
		klog.V(1).Infof("Token is not set in configuration")
		return nil, err
	}

	ua := userAgent + "/" + version.Version
	for _, data := range extraUserAgent {
		ua = data + " " + ua
	}

	opts := []edgecloud.ClientOpt{
		edgecloud.SetAPIKey(cfg.ApiToken),
		edgecloud.SetBaseURL(cfg.ApiURL),
		edgecloud.SetRegion(cfg.RegionID),
		edgecloud.SetProject(cfg.ProjectID),
		edgecloud.SetUserAgent(ua),
	}

	client, err := edgecloud.NewWithRetries(nil, opts...)
	if err != nil {
		return nil, fmt.Errorf("failed to create edgecenter client")
	}

	klog.V(4).Infof("Using user-agent %s", ua)

	var caPool *x509.CertPool
	if cfg.CAFile != "" {
		klog.Infof("### with CAFile")
		// read and parse CA certificate from file
		caPool, err = certutil.NewPool(cfg.CAFile)
		if err != nil {
			return nil, fmt.Errorf("failed to read and parse %s certificate: %s", cfg.CAFile, err)
		}
	} else if cfg.CAFileContents != "" {
		klog.Infof("### with CAFileContents")
		// parse CA certificate from the contents
		caPool = x509.NewCertPool()
		if ok := caPool.AppendCertsFromPEM([]byte(cfg.CAFileContents)); !ok {
			return nil, fmt.Errorf("failed to parse os-certAuthority certificate")
		}
	}

	if caPool != nil {
		klog.Infof("### with caPool")
		config := &tls.Config{}
		config.RootCAs = caPool
		config.InsecureSkipVerify = cfg.TLSInsecure == "true"
		client.HTTPClient.Transport = netutil.SetOldTransportDefaults(&http.Transport{TLSClientConfig: config})
	}

	return client, err
}

// NewEdgecenter creates a new instance of the edgecenter struct from a config struct
func NewEdgecenter(cfg Config) (*Edgecenter, error) {

	client, err := NewEdgecenterClient(cfg.Edgecenter, "edgecenter-cloud-controller-manager", userAgentData...)
	if err != nil {
		return nil, err
	}

	client.HTTPClient.Timeout = cfg.Metadata.RequestTimeout.Duration

	gcr := Edgecenter{
		client:         client,
		region:         cfg.Edgecenter.Region,
		cfg:            cfg,
		lbOpts:         cfg.LoadBalancer,
		bsOpts:         cfg.BlockStorage,
		routeOpts:      cfg.Route,
		metadataOpts:   cfg.Metadata,
		networkingOpts: cfg.Networking,
	}

	err = checkEdgecenterOpts(&gcr)
	if err != nil {
		return nil, err
	}

	return &gcr, nil
}

// Initialize passes a Kubernetes clientBuilder interface to the cloud provider
func (ec *Edgecenter) Initialize(_ cloudprovider.ControllerClientBuilder, _ <-chan struct{}) {
}

// GetNodeNameByID maps instance id to types.NodeName
func (ec *Edgecenter) GetNodeNameByID(ctx context.Context, instanceID string) (types.NodeName, error) {
	instance, _, err := ec.client.Instances.Get(ctx, instanceID)
	if err != nil {
		return "", err
	}
	return types.NodeName(strings.ToLower(instance.Name)), nil
}

func getInstanceByName(ctx context.Context, svc edgecloud.InstancesService, name types.NodeName) (*edgecloud.Instance, error) {
	opts := &edgecloud.InstanceListOptions{
		Name: string(name),
	}

	list, _, err := svc.List(ctx, opts)
	if err != nil {
		return nil, err
	}

	if len(list) == 0 {
		return nil, ErrNotFound
	}

	return &list[0], nil
}

func nodeAddresses(instance *edgecloud.Instance, networkingOpts NetworkingOpts) ([]apiv1.NodeAddress, error) {
	var addrs []apiv1.NodeAddress

	addresses := instance.Addresses

	var networks []string
	for k := range addresses {
		networks = append(networks, k)
	}
	sort.Strings(networks)

	for _, network := range networks {
		for _, props := range addresses[network] {
			if props.Address != nil && strings.HasPrefix(props.Address.String(), "100.") {
				continue
			}

			var addressType apiv1.NodeAddressType
			if props.Type == string(edgecloud.AddressTypeFloating) || network == networkingOpts.PublicNetworkName {
				addressType = apiv1.NodeExternalIP
			} else if networkingOpts.InternalNetworkName == "" || network == networkingOpts.InternalNetworkName {
				addressType = apiv1.NodeInternalIP
			} else {
				klog.V(5).Infof("Node '%s' address '%s' ignored due to 'internal-network-name' option", instance.Name, props.Address)
				continue
			}

			isIPv6 := props.Address.To4() == nil
			if !(isIPv6 && networkingOpts.IPv6SupportDisabled) {
				v1helper.AddToNodeAddresses(&addrs, apiv1.NodeAddress{
					Type:    addressType,
					Address: props.Address.String(),
				})
			}
		}
	}

	if hostname, ok := instance.Metadata[TypeHostName]; ok && hostname != "" {
		v1helper.AddToNodeAddresses(&addrs, apiv1.NodeAddress{
			Type:    apiv1.NodeHostName,
			Address: instance.Metadata[TypeHostName],
		})
	}

	return addrs, nil
}

func getAddressesByName(
	ctx context.Context,
	svc edgecloud.InstancesService,
	name types.NodeName,
	networkingOpts NetworkingOpts,
) ([]apiv1.NodeAddress, error) {
	instance, err := getInstanceByName(ctx, svc, name)
	if err != nil {
		return nil, err
	}
	return nodeAddresses(instance, networkingOpts)
}

// Clusters is a no-op
func (ec *Edgecenter) Clusters() (cloudprovider.Clusters, bool) {
	return nil, false
}

// ProviderName returns the cloud provider ID.
func (ec *Edgecenter) ProviderName() string {
	return ProviderName
}

// ScrubDNS filters DNS settings for pods.
func (ec *Edgecenter) ScrubDNS(nameServers, searches []string) ([]string, []string) {
	return nameServers, searches
}

// HasClusterID returns true if the cluster has a clusterID
func (ec *Edgecenter) HasClusterID() bool {
	return true
}

// LoadBalancer initializes a LbaasV2 object
func (ec *Edgecenter) LoadBalancer() (cloudprovider.LoadBalancer, bool) {
	klog.V(4).Info("edgecenter.LoadBalancer() called")

	if reflect.DeepEqual(ec.lbOpts, LoadBalancerOpts{}) {
		klog.V(4).Info("LoadBalancer section is empty/not defined in cloud-config")
		return nil, false
	}

	lbVersion := ec.lbOpts.LBVersion
	if lbVersion != "" && lbVersion != "v2" {
		klog.Warningf("Config error: currently only support LBaaS v2, unrecognised lb-version \"%v\"", lbVersion)
		return nil, false
	}

	klog.V(1).Info("Claiming to support LoadBalancer")

	return &LbaasV2{
		LoadBalancer{
			client:     ec.client,
			opts:       ec.lbOpts,
			edgecenter: ec.cfg.Edgecenter,
		},
	}, true
}

// Zones indicates that we support zones
func (ec *Edgecenter) Zones() (cloudprovider.Zones, bool) {
	klog.V(1).Info("Claiming to support Zones")
	return ec, true
}

// GetZone returns the current zone
func (ec *Edgecenter) GetZone(_ context.Context) (cloudprovider.Zone, error) {
	md, err := metadata.Get(ec.metadataOpts.SearchOrder)
	if err != nil {
		return cloudprovider.Zone{}, err
	}

	zone := cloudprovider.Zone{
		FailureDomain: md.AvailabilityZone,
		Region:        ec.region,
	}

	klog.V(4).Infof("Current zone is %v", zone)

	return zone, nil
}

// GetZoneByProviderID implements Zones.GetZoneByProviderID
// This is particularly useful in external cloud providers where the kubelet
// does not initialize node data.
func (ec *Edgecenter) GetZoneByProviderID(ctx context.Context, providerID string) (cloudprovider.Zone, error) {
	instanceID, err := instanceIDFromProviderID(providerID)
	if err != nil {
		return cloudprovider.Zone{}, err
	}

	instance, _, err := ec.client.Instances.Get(ctx, instanceID)
	if err != nil {
		return cloudprovider.Zone{}, err
	}

	zone := cloudprovider.Zone{
		Region: ec.region,
	}

	klog.V(4).Infof("The instance %s in zone %v", instance.Name, zone)

	return zone, nil
}

// GetZoneByNodeName implements Zones.GetZoneByNodeName
// This is particularly useful in external cloud providers where the kubelet
// does not initialize node data.
func (ec *Edgecenter) GetZoneByNodeName(ctx context.Context, nodeName types.NodeName) (cloudprovider.Zone, error) {
	instance, err := getInstanceByName(ctx, ec.client.Instances, nodeName)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return cloudprovider.Zone{}, cloudprovider.InstanceNotFound
		}
		return cloudprovider.Zone{}, err
	}

	zone := cloudprovider.Zone{
		Region: ec.region,
	}

	klog.V(4).Infof("The instance %s in zone %v", instance.Name, zone)

	return zone, nil
}

// Routes initializes routes support
func (ec *Edgecenter) Routes() (cloudprovider.Routes, bool) {
	klog.V(4).Info("edgecenter.Routes() called")

	r, err := NewRoutes(nil, ec.routeOpts, ec.networkingOpts)
	if err != nil {
		klog.Warningf("Error initialising Routes support: %v", err)
		return nil, false
	}

	klog.V(1).Info("Claiming to support Routes")
	return r, true
}

func (ec *Edgecenter) volumeService(_ string) (volumeService, error) {
	klog.V(3).Info("Using edgecenter block storage API")
	return &Volumes{client: ec.client, opts: ec.bsOpts}, nil
}

func checkMetadataSearchOrder(order string) error {
	if order == "" {
		return errors.New("invalid value in section [Metadata] with key `search-order`. Value cannot be empty")
	}

	elements := strings.Split(order, ",")
	if len(elements) > 2 {
		return errors.New("invalid value in section [Metadata] with key `search-order`. Value cannot contain more than 2 elements")
	}

	for _, id := range elements {
		id = strings.TrimSpace(id)
		switch id {
		case metadata.ConfigDriveID:
		case metadata.MetadataID:
		default:
			return fmt.Errorf("invalid element %q found in section [Metadata] with key `search-order`."+
				"Supported elements include %q and %q", id, metadata.ConfigDriveID, metadata.MetadataID)
		}
	}

	return nil
}
