package edgecenter

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"

	edgecloud "github.com/Edge-Center/edgecentercloud-go/v2"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	cloudprovider "k8s.io/cloud-provider"
)

// NodeRegionLabel optionally restricts instance lookups to a configured region.
// Configure it on the kubelet before registration, rather than patching it after join.
const NodeRegionLabel = "edgecenter.com/node-region"

// NodeRegionOpts adds a node-only API scope. Load balancers remain in [Edgecenter].
type NodeRegionOpts struct {
	RegionID  int `gcfg:"region-id"`
	ProjectID int `gcfg:"project-id"`
}

type nodeRegion struct {
	name   string
	client *edgecloud.Client
}

type nodeRegionResolver struct {
	regions []nodeRegion // default first, then named regions in deterministic order
}

func newNodeRegionResolver(cfg Config, defaultClient *edgecloud.Client) (*nodeRegionResolver, error) {
	r := &nodeRegionResolver{regions: []nodeRegion{{name: "default", client: defaultClient}}}
	names := make([]string, 0, len(cfg.NodeRegion))
	for name := range cfg.NodeRegion {
		names = append(names, name)
	}
	sort.Strings(names)
	scopes := map[string]bool{fmt.Sprintf("%d/%d", cfg.Edgecenter.ProjectID, cfg.Edgecenter.RegionID): true}
	for _, name := range names {
		opts := cfg.NodeRegion[name]
		if name == "" || name == "default" || opts == nil || opts.RegionID <= 0 || opts.ProjectID < 0 {
			return nil, fmt.Errorf("invalid NodeRegion %q: use a non-default name and positive region-id", name)
		}
		regional := cfg.Edgecenter
		regional.RegionID = opts.RegionID
		if opts.ProjectID != 0 {
			regional.ProjectID = opts.ProjectID
		}
		key := fmt.Sprintf("%d/%d", regional.ProjectID, regional.RegionID)
		if scopes[key] {
			return nil, fmt.Errorf("duplicate project/region scope in NodeRegion %q", name)
		}
		scopes[key] = true
		client, err := NewEdgecenterClient(regional, "edgecenter-cloud-controller-manager", userAgentData...)
		if err != nil {
			return nil, fmt.Errorf("NodeRegion %q: %w", name, err)
		}
		client.HTTPClient.Timeout = defaultClient.HTTPClient.Timeout
		r.regions = append(r.regions, nodeRegion{name: name, client: client})
	}
	return r, nil
}

// resolve never interprets an API failure (including 403) as a missing VM.
// Without an explicit label, every configured scope must be checked: a default-
// region miss alone is not evidence that a cross-region node has been deleted.
func (r *nodeRegionResolver) resolve(ctx context.Context, node *corev1.Node) (*edgecloud.Instance, nodeRegion, error) {
	if node == nil {
		return nil, nodeRegion{}, fmt.Errorf("node is nil")
	}
	regions := r.regions
	if name, explicit := node.Labels[NodeRegionLabel]; explicit {
		regions = nil
		for _, region := range r.regions {
			if region.name == name {
				regions = []nodeRegion{region}
				break
			}
		}
		if len(regions) == 0 {
			return nil, nodeRegion{}, fmt.Errorf("node %s has unknown %s=%q", node.Name, NodeRegionLabel, name)
		}
	}
	var id string
	if node.Spec.ProviderID != "" {
		var err error
		id, err = instanceIDFromProviderID(node.Spec.ProviderID)
		if err != nil {
			return nil, nodeRegion{}, err
		}
	}
	var found *edgecloud.Instance
	var foundRegion nodeRegion
	for _, region := range regions {
		var instance *edgecloud.Instance
		var err error
		if id != "" {
			var response *edgecloud.Response
			instance, response, err = region.client.Instances.Get(ctx, id)
			if err != nil && response != nil && response.Response != nil && response.StatusCode == http.StatusNotFound {
				continue
			}
		} else {
			instance, err = getInstanceByName(ctx, region.client.Instances, types.NodeName(node.Name))
			if errors.Is(err, ErrNotFound) {
				continue
			}
		}
		if err != nil {
			return nil, nodeRegion{}, fmt.Errorf("resolve node %s in region %q: %w", node.Name, region.name, err)
		}
		if instance == nil || instance.ID == "" {
			return nil, nodeRegion{}, fmt.Errorf("invalid instance response for node %s in region %q", node.Name, region.name)
		}
		if found != nil {
			return nil, nodeRegion{}, fmt.Errorf("ambiguous node %s: found in regions %q and %q; set %s", node.Name, foundRegion.name, region.name, NodeRegionLabel)
		}
		found, foundRegion = instance, region
	}
	if found == nil {
		return nil, nodeRegion{}, cloudprovider.InstanceNotFound
	}
	return found, foundRegion, nil
}

func (i *Instances) regionResolver() *nodeRegionResolver {
	if i.regions != nil {
		return i.regions
	}
	return &nodeRegionResolver{regions: []nodeRegion{{name: "default", client: i.client}}}
}

func metadataRegion(instance *edgecloud.Instance, region nodeRegion) string {
	if instance.Region != "" {
		return instance.Region
	}
	if region.name != "default" {
		return region.name
	}
	return ""
}

// filterLoadBalancerNodes applies the same scope restriction to pool members and
// security groups. Cross-region backends are deliberately not supported here.
func (r *nodeRegionResolver) filterLoadBalancerNodes(ctx context.Context, nodes []*corev1.Node) ([]*corev1.Node, error) {
	var local []*corev1.Node
	for _, node := range nodes {
		_, region, err := r.resolve(ctx, node)
		if err != nil {
			return nil, err
		}
		if region.name == "default" {
			local = append(local, node)
		}
	}
	return local, nil
}
