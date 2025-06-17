/*
Copyright 2017 The Kubernetes Authors.

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
	"github.com/prometheus/client_golang/prometheus"
	"k8s.io/klog/v2"
)

const (
	edgecenterSubsystem         = "edgecenter"
	edgecenterOperationKey      = "cloudprovider_edgecenter_api_request_duration_seconds"
	edgecenterOperationErrorKey = "cloudprovider_edgecenter_api_request_errors"
)

var (
	edgecenterOperationsLatency = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Subsystem: edgecenterSubsystem,
			Name:      edgecenterOperationKey,
			Help:      "Latency of edgecenter api call",
		},
		[]string{"request"},
	)

	edgecenterAPIRequestErrors = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Subsystem: edgecenterSubsystem,
			Name:      edgecenterOperationErrorKey,
			Help:      "Cumulative number of edgecenter Api call errors",
		},
		[]string{"request"},
	)
)

func RegisterMetrics() {
	if err := prometheus.Register(edgecenterOperationsLatency); err != nil {
		klog.V(5).Infof("unable to register for latency metrics")
	}
	if err := prometheus.Register(edgecenterAPIRequestErrors); err != nil {
		klog.V(5).Infof("unable to register for error metrics")
	}
}
