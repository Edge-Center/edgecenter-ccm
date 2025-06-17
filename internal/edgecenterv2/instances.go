/*
Copyright 2016 The Kubernetes Authors.

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
	"errors"
	"fmt"
	"regexp"
	"strings"

	edgecloud "github.com/Edge-Center/edgecentercloud-go/v2"
	edgecloudUtil "github.com/Edge-Center/edgecentercloud-go/v2/util"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	cloudprovider "k8s.io/cloud-provider"
	"k8s.io/klog/v2"

	"ec-ccm/internal/util/metadata"
)

// Instances encapsulates an implementation of Instances for Edgecenter.
type Instances struct {
	client         *edgecloud.Client
	opts           MetadataOpts
	networkingOpts NetworkingOpts
}

const (
	instanceShutoff = "SHUTOFF"
)

// Instances returns an implementation of Instances for Edgecenter.
func (ec *Edgecenter) Instances() (cloudprovider.Instances, bool) {
	klog.V(4).Info("edgecenter.Instances() called")

	return &Instances{
		client:         ec.client,
		opts:           ec.metadataOpts,
		networkingOpts: ec.networkingOpts,
	}, true
}

// CurrentNodeName implements Instances.CurrentNodeName
// Note this is *not* necessarily the same as hostname.
func (i *Instances) CurrentNodeName(_ context.Context, _ string) (types.NodeName, error) {
	md, err := metadata.Get(i.opts.SearchOrder)
	if err != nil {
		return "", err
	}
	return types.NodeName(md.Name), nil
}

// AddSSHKeyToAllInstances is not implemented for Edgecenter
func (i *Instances) AddSSHKeyToAllInstances(_ context.Context, _ string, _ []byte) error {
	return cloudprovider.NotImplemented
}

// NodeAddresses implements Instances.NodeAddresses
func (i *Instances) NodeAddresses(ctx context.Context, name types.NodeName) ([]v1.NodeAddress, error) {
	klog.V(4).Infof("NodeAddresses(%v) called", name)

	addrs, err := getAddressesByName(ctx, i.client.Instances, name, i.networkingOpts)
	if err != nil {
		return nil, err
	}

	klog.V(4).Infof("NodeAddresses(%v) => %v", name, addrs)
	return addrs, nil
}

// NodeAddressesByProviderID returns the node addresses of an instances with the specified unique providerID
// This method will not be called from the node that is requesting this ID. i.e. metadata service
// and other local methods cannot be used here
func (i *Instances) NodeAddressesByProviderID(ctx context.Context, providerID string) ([]v1.NodeAddress, error) {
	instanceID, err := instanceIDFromProviderID(providerID)
	if err != nil {
		return []v1.NodeAddress{}, err
	}

	instance, _, err := i.client.Instances.Get(ctx, instanceID)
	if err != nil {
		return []v1.NodeAddress{}, err
	}

	addresses, err := nodeAddresses(instance, i.networkingOpts)
	if err != nil {
		return []v1.NodeAddress{}, err
	}

	return addresses, nil
}

// InstanceExistsByProviderID returns true if the instance with the given provider id still exist.
// If false is returned with no error, the instance will be immediately deleted by the cloud controller_openstack manager.
func (i *Instances) InstanceExistsByProviderID(ctx context.Context, providerID string) (bool, error) {
	instanceID, err := instanceIDFromProviderID(providerID)
	if err != nil {
		return false, err
	}

	return edgecloudUtil.ResourceIsExist(ctx, i.client.Instances.Get, instanceID)
}

// InstanceShutdownByProviderID returns true if the instances is in safe state to detach volumes
func (i *Instances) InstanceShutdownByProviderID(ctx context.Context, providerID string) (bool, error) {
	instanceID, err := instanceIDFromProviderID(providerID)
	if err != nil {
		return false, err
	}

	instance, _, err := i.client.Instances.Get(ctx, instanceID)
	if err != nil {
		return false, err
	}

	// SHUTOFF is the only state where we can detach volumes immediately
	if instance.Status == instanceShutoff {
		return true, nil
	}
	return false, nil
}

// InstanceID returns the kubelet's cloud provider ID.
func (ec *Edgecenter) InstanceID() (string, error) {
	if len(ec.localInstanceID) == 0 {
		id, err := readInstanceID(ec.metadataOpts.SearchOrder)
		if err != nil {
			return "", err
		}
		ec.localInstanceID = id
	}
	return ec.localInstanceID, nil
}

// InstanceID returns the cloud provider ID of the specified instance.
func (i *Instances) InstanceID(ctx context.Context, name types.NodeName) (string, error) {
	instance, err := getInstanceByName(ctx, i.client.Instances, name)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return "", cloudprovider.InstanceNotFound
		}
		return "", err
	}
	// In the future it is possible to also return an endpoint as:
	// <endpoint>/<instanceid>
	return "/" + instance.ID, nil
}

// InstanceTypeByProviderID returns the cloudprovider instance type of the node with the specified unique providerID
// This method will not be called from the node that is requesting this ID. i.e. metadata service
// and other local methods cannot be used here
func (i *Instances) InstanceTypeByProviderID(ctx context.Context, providerID string) (string, error) {
	instanceID, err := instanceIDFromProviderID(providerID)
	if err != nil {
		return "", err
	}

	instance, _, err := i.client.Instances.Get(ctx, instanceID)
	if err != nil {
		return "", err
	}

	return srvInstanceType(instance)
}

// InstanceType returns the type of the specified instance.
func (i *Instances) InstanceType(ctx context.Context, name types.NodeName) (string, error) {
	instance, err := getInstanceByName(ctx, i.client.Instances, name)

	if err != nil {
		return "", err
	}

	return srvInstanceType(instance)
}

func srvInstanceType(instance *edgecloud.Instance) (string, error) {
	if instance.Flavor.FlavorID != "" {
		return instance.Flavor.FlavorID, nil
	}
	if instance.Flavor.FlavorName != "" {
		return instance.Flavor.FlavorName, nil
	}
	return "", fmt.Errorf("flavor name/id not found")
}

// If Instances.InstanceID or cloudprovider.GetInstanceProviderID is changed, the regexp should be changed too.
var providerIDRegexp = regexp.MustCompile(`^` + ProviderName + `:///([^/]+)$`)

// instanceIDFromProviderID splits a provider's id and return instanceID.
// A providerID is build out of '${ProviderName}:///${instance-id}'which contains ':///'.
// See cloudprovider.GetInstanceProviderID and Instances.InstanceID.
func instanceIDFromProviderID(providerID string) (instanceID string, err error) {

	// https://github.com/kubernetes/kubernetes/issues/85731
	if providerID != "" && !strings.Contains(providerID, "://") {
		providerID = ProviderName + "://" + providerID
	}

	matches := providerIDRegexp.FindStringSubmatch(providerID)
	if len(matches) != 2 {
		return "", fmt.Errorf("ProviderID \"%s\" didn't match expected format \"edgecenter:///InstanceID\"", providerID)
	}
	return matches[1], nil
}
