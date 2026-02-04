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

	edgecloud "github.com/Edge-Center/edgecentercloud-go/v2"
	cloudprovider "k8s.io/cloud-provider"
	"k8s.io/klog/v2"
)

var errNoRouterID = errors.New("router-id not set in cloud provider config")

// Routes implements the cloudprovider.Routes for Edgecenter clouds
type Routes struct {
	client         *edgecloud.Client
	opts           RouterOpts
	networkingOpts NetworkingOpts
}

// NewRoutes creates a new instance of Routes
func NewRoutes(client *edgecloud.Client, opts RouterOpts, networkingOpts NetworkingOpts) (cloudprovider.Routes, error) {
	if opts.RouterID == "" {
		return nil, errNoRouterID
	}

	return &Routes{
		client:         client,
		opts:           opts,
		networkingOpts: networkingOpts,
	}, nil
}

// ListRoutes lists all managed routes that belong to the specified clusterName
func (r *Routes) ListRoutes(ctx context.Context, clusterName string) ([]*cloudprovider.Route, error) {
	klog.V(4).Infof("ListRoutes(%v)", clusterName)
	var routes []*cloudprovider.Route
	return routes, nil
}

// CreateRoute creates the described managed route
func (r *Routes) CreateRoute(ctx context.Context, clusterName string, nameHint string, route *cloudprovider.Route) error {
	klog.V(4).Infof("CreateRoute(%v, %v, %v)", clusterName, nameHint, route)
	return nil
}

// DeleteRoute deletes the specified managed route
func (r *Routes) DeleteRoute(ctx context.Context, clusterName string, route *cloudprovider.Route) error {
	klog.V(4).Infof("DeleteRoute(%v, %v)", clusterName, route)
	return nil
}
