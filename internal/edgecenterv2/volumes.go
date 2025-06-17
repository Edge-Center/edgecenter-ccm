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
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	edgecloud "github.com/Edge-Center/edgecentercloud-go/v2"
	edgecloudutil "github.com/Edge-Center/edgecentercloud-go/v2/util"
	"github.com/prometheus/client_golang/prometheus"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/util/wait"
	cloudprovider "k8s.io/cloud-provider"
	"k8s.io/klog/v2"

	"ec-ccm/internal/util/metadata"
	k8svolume "ec-ccm/internal/volume"
	volumeutil "ec-ccm/internal/volume/util"
)

const (
	waitSeconds              = 5 * time.Minute
	operationFinishInitDelay = 1 * time.Second
	operationFinishFactor    = 1.1
	operationFinishSteps     = 15

	LabelZoneFailureDomain = "failure-domain.beta.kubernetes.io/zone"
	LabelZoneRegion        = "failure-domain.beta.kubernetes.io/region"
)

type volumeService interface {
	createVolume(ctx context.Context, opts volumeCreateOpts) (string, string, error)
	getVolume(ctx context.Context, volumeID string) (Volume, error)
	deleteVolume(ctx context.Context, volumeName string) error
	expandVolume(ctx context.Context, volumeID string, newSize int, volumeStatus string) error
}

// Volumes is a Volumes implementation for cinder v2
type Volumes struct {
	client *edgecloud.Client
	opts   BlockStorageOpts
}

// Volume stores information about a single volume
type Volume struct {
	// ID of the instance, to which this volume is attached. "" if not attached
	AttachedServerID string
	// Device file path
	AttachedDevice string
	// AvailabilityZone is which availability zone the volume is in
	AvailabilityZone string
	// Unique identifier for the volume.
	ID string
	// Human-readable display name for the volume.
	Name string
	// Current status of the volume.
	Status string
	// Volume size in GB
	Size int
}

type volumeCreateOpts struct {
	Size         int
	Availability string
	Name         string
	VolumeType   string
	Metadata     map[string]string
}

// implements PVLabeler.
var _ cloudprovider.PVLabeler = (*Edgecenter)(nil)

const (
	volumeAvailableStatus = "available"
	volumeInUseStatus     = "in-use"
	volumeDeletedStatus   = "deleted"
	volumeErrorStatus     = "error"

	// On some environments, we need to query the metadata service in order
	// to locate disks. We'll use the Newton version, which includes device
	// metadata.
	newtonMetadataVersion = "2016-06-30"
)

func (v *Volumes) createVolume(ctx context.Context, opts volumeCreateOpts) (string, string, error) {
	startTime := time.Now()

	typeName := edgecloud.VolumeType(opts.VolumeType)
	if err := typeName.IsValid(); err != nil {
		typeName = edgecloud.VolumeTypeStandard
	}

	createOpts := &edgecloud.VolumeCreateRequest{
		Name:     opts.Name,
		Size:     opts.Size,
		Source:   edgecloud.VolumeSourceNewVolume,
		TypeName: typeName,
	}

	task, err := edgecloudutil.ExecuteAndExtractTaskResult(ctx, v.client.Volumes.Create, createOpts, v.client, waitSeconds)
	if err != nil {
		return "", "", fmt.Errorf("create volume task failed with opts: %v. Error: %w", createOpts, err)
	}

	vlm, _, err := v.client.Volumes.Get(ctx, task.Volumes[0])
	if err != nil {
		return "", "", fmt.Errorf("cannot get volume with ID: %s. Error: %w", task.Volumes[0], err)
	}

	timeTaken := time.Since(startTime).Seconds()
	recordEdgecenterOperationMetric("create_volume", timeTaken, err)
	if err != nil {
		return "", "", err
	}

	if vlm.Status != volumeAvailableStatus {
		return "", "", fmt.Errorf("unaccessible volume %v state after creation: %s", vlm, vlm.Status)
	}

	return vlm.ID, vlm.AvailabilityZone, nil
}

func (v *Volumes) getVolume(ctx context.Context, volumeID string) (Volume, error) {
	var err error

	startTime := time.Now()
	defer func() {
		timeTaken := time.Since(startTime).Seconds()
		recordEdgecenterOperationMetric("get_volume", timeTaken, err)
	}()

	vlm, _, err := v.client.Volumes.Get(ctx, volumeID)
	if err != nil {
		return Volume{}, fmt.Errorf("error occurred getting volume by ID: %s, err: %v", volumeID, err)
	}

	volume := Volume{
		AvailabilityZone: vlm.AvailabilityZone,
		ID:               vlm.ID,
		Name:             vlm.Name,
		Status:           vlm.Status,
		Size:             vlm.Size,
	}

	if len(vlm.Attachments) > 0 {
		volume.AttachedServerID = vlm.Attachments[0].ServerID
		volume.AttachedDevice = vlm.Attachments[0].Device
	}

	return volume, nil
}

func (v *Volumes) deleteVolume(ctx context.Context, volumeID string) error {
	var err error

	startTime := time.Now()
	defer func() {
		timeTaken := time.Since(startTime).Seconds()
		recordEdgecenterOperationMetric("delete_volume", timeTaken, err)
	}()

	_, err = edgecloudutil.ExecuteAndExtractTaskResult(ctx, v.client.Volumes.Delete, volumeID, v.client, waitSeconds)
	if err != nil {
		return fmt.Errorf("cannot delete volume with ID: %s", volumeID)
	}

	return nil
}

func (v *Volumes) expandVolume(ctx context.Context, volumeID string, newSize int, volumeStatus string) error {
	if volumeStatus != volumeAvailableStatus {
		// cinder volume can not be expanded if cinder API is v2
		return fmt.Errorf("volume status is not 'available'")
	}

	var err error

	startTime := time.Now()
	defer func() {
		timeTaken := time.Since(startTime).Seconds()
		recordEdgecenterOperationMetric("expand_volume", timeTaken, err)
	}()

	opts := &edgecloud.VolumeExtendSizeRequest{
		Size: newSize,
	}

	taskResp, _, err := v.client.Volumes.Extend(ctx, volumeID, opts)
	if err != nil {
		return err
	}

	err = edgecloudutil.WaitForTaskComplete(ctx, v.client, taskResp.Tasks[0], waitSeconds)
	if err != nil {
		return err
	}

	return err
}

// AttachDisk attaches given cinder volume to the compute running kubelet
func (ec *Edgecenter) AttachDisk(ctx context.Context, instanceID, volumeID string) (string, error) {
	volume, err := ec.getVolume(ctx, volumeID)
	if err != nil {
		return "", err
	}

	if volume.AttachedServerID != "" {
		if instanceID == volume.AttachedServerID {
			klog.V(4).Infof("Disk %s is already attached to instance %s", volumeID, instanceID)
			return volume.ID, nil
		}

		nodeName, err := ec.GetNodeNameByID(ctx, volume.AttachedServerID)
		attachErr := fmt.Sprintf("disk %s path %s is attached to a different instance (%s)",
			volumeID, volume.AttachedDevice, volume.AttachedServerID)
		if err != nil {
			klog.Error(attachErr)
			return "", errors.New(attachErr)
		}

		// using volume.AttachedDevice may cause problems because cinder does not report device path correctly see issue #33128
		devicePath := volume.AttachedDevice
		danglingErr := volumeutil.NewDanglingError(attachErr, nodeName, devicePath)
		klog.V(2).Infof("Found dangling volume %s attached to node %s", volumeID, nodeName)
		return "", danglingErr
	}

	startTime := time.Now()
	defer func() {
		timeTaken := time.Since(startTime).Seconds()
		recordEdgecenterOperationMetric("attach_disk", timeTaken, err)
	}()

	opts := &edgecloud.VolumeAttachRequest{
		InstanceID: instanceID,
	}

	vlm, _, err := ec.client.Volumes.Attach(ctx, volumeID, opts)
	if err != nil {
		return "", fmt.Errorf("failed to attach %s volume to %s compute: %v", volumeID, instanceID, err)
	}

	klog.V(2).Infof("Successfully attached %s volume to %s compute", volumeID, instanceID)

	return vlm.ID, nil
}

// DetachDisk detaches given cinder volume from the compute running kubelet
func (ec *Edgecenter) DetachDisk(ctx context.Context, instanceID, volumeID string) error {
	volume, err := ec.getVolume(ctx, volumeID)
	if err != nil {
		return err
	}
	if volume.Status == volumeAvailableStatus {
		// "available" is fine since that means the volume is detached from instance already.
		klog.V(2).Infof("volume: %s has been detached from compute: %s ", volume.ID, instanceID)
		return nil
	}

	if volume.Status != volumeInUseStatus {
		return fmt.Errorf("can not detach volume %s, its status is %s", volume.Name, volume.Status)
	}

	if volume.AttachedServerID != instanceID {
		return fmt.Errorf("disk: %s has no attachments or is not attached to compute: %s", volume.Name, instanceID)
	}

	startTime := time.Now()
	defer func() {
		timeTaken := time.Since(startTime).Seconds()
		recordEdgecenterOperationMetric("detach_disk", timeTaken, err)
	}()

	opts := &edgecloud.VolumeDetachRequest{
		InstanceID: instanceID,
	}

	// This is a blocking call and effects kubelet's performance directly.
	// We should consider kicking it out into a separate routine, if it is bad.
	_, _, err = ec.client.Volumes.Detach(ctx, volumeID, opts)
	if err != nil {
		return fmt.Errorf("failed to delete volume %s from compute %s attached %v", volume.ID, instanceID, err)
	}

	klog.V(2).Infof("Successfully detached volume: %s from compute: %s", volume.ID, instanceID)

	return nil
}

// ExpandVolume expands the size of specific cinder volume (in GiB)
func (ec *Edgecenter) ExpandVolume(ctx context.Context, volumeID string, oldSize resource.Quantity, newSize resource.Quantity) (resource.Quantity, error) {
	volume, err := ec.getVolume(ctx, volumeID)
	if err != nil {
		return oldSize, err
	}

	// Cinder works with gigabytes, convert to GiB with rounding up
	volSizeGiB, err := volumeutil.RoundUpToGiBInt(newSize)
	if err != nil {
		return oldSize, err
	}
	newSizeQuant := resource.MustParse(fmt.Sprintf("%dGi", volSizeGiB))

	// if volume size equals to or greater than the newSize, return nil
	if volume.Size >= volSizeGiB {
		return newSizeQuant, nil
	}

	// Init a local thread safe copy of the Cinder ServiceClient
	volumes, err := ec.volumeService("")
	if err != nil {
		return oldSize, err
	}

	err = volumes.expandVolume(ctx, volumeID, volSizeGiB, volume.Status)
	if err != nil {
		return oldSize, err
	}
	return newSizeQuant, nil
}

// getVolume retrieves Volume by its ID.
func (ec *Edgecenter) getVolume(ctx context.Context, volumeID string) (Volume, error) {
	volumes, err := ec.volumeService("")
	if err != nil {
		return Volume{}, fmt.Errorf("unable to initialize cinder client for region: %s, err: %v", ec.region, err)
	}
	return volumes.getVolume(ctx, volumeID)
}

// CreateVolume creates a volume of given size (in GiB)
func (ec *Edgecenter) CreateVolume(ctx context.Context, name string, size int, vtype, availability string, tags *map[string]string) (string, string, string, bool, error) {
	volumes, err := ec.volumeService("")
	if err != nil {
		return "", "", "", ec.bsOpts.IgnoreVolumeAZ, fmt.Errorf("unable to initialize cinder client for region: %s, err: %v", ec.region, err)
	}

	opts := volumeCreateOpts{
		Name:         name,
		Size:         size,
		VolumeType:   vtype,
		Availability: availability,
	}
	if tags != nil {
		opts.Metadata = *tags
	}

	volumeID, volumeAZ, err := volumes.createVolume(ctx, opts)

	if err != nil {
		return "", "", "", ec.bsOpts.IgnoreVolumeAZ, fmt.Errorf("failed to create a %d GB volume: %w", size, err)
	}

	klog.Infof("Created volume %v in Availability Zone: %v Region: %v Ignore volume AZ: %v", volumeID, volumeAZ, ec.region, ec.bsOpts.IgnoreVolumeAZ)
	return volumeID, volumeAZ, ec.region, ec.bsOpts.IgnoreVolumeAZ, nil
}

// GetDevicePathBySerialID returns the path of an attached block storage volume, specified by its id.
func (ec *Edgecenter) GetDevicePathBySerialID(volumeID string) string {
	// Build a list of candidate device paths.
	// Certain Nova drivers will set the disk serial ID, including the Cinder volume id.
	candidateDeviceNodes := []string{
		// KVM
		fmt.Sprintf("virtio-%s", volumeID[:20]),
		// KVM #852
		fmt.Sprintf("virtio-%s", volumeID),
		// KVM virtio-scsi
		fmt.Sprintf("scsi-0QEMU_QEMU_HARDDISK_%s", volumeID[:20]),
		// KVM virtio-scsi #852
		fmt.Sprintf("scsi-0QEMU_QEMU_HARDDISK_%s", volumeID),
		// ESXi
		fmt.Sprintf("wwn-0x%s", strings.Replace(volumeID, "-", "", -1)),
	}

	files, _ := os.ReadDir("/dev/disk/by-id/")

	for _, f := range files {
		for _, c := range candidateDeviceNodes {
			if c == f.Name() {
				klog.V(4).Infof("Found disk attached as %q; full devicepath: %s\n", f.Name(), path.Join("/dev/disk/by-id/", f.Name()))
				return path.Join("/dev/disk/by-id/", f.Name())
			}
		}
	}

	klog.V(4).Infof("Failed to find device for the volumeID: %q by serial ID", volumeID)
	return ""
}

func (ec *Edgecenter) getDevicePathFromInstanceMetadata(volumeID string) string {
	// Nova Hyper-V hosts cannot override disk SCSI IDs. In order to locate
	// volumes, we're querying the metadata service. Note that the Hyper-V
	// driver will include device metadata for untagged volumes as well.
	//
	// We're avoiding using cached metadata (or the configdrive),
	// relying on the metadata service.
	instanceMetadata, err := metadata.Get(
		newtonMetadataVersion)

	if err != nil {
		klog.V(4).Infof(
			"Could not retrieve instance metadata. Error: %v", err)
		return ""
	}

	for _, device := range instanceMetadata.Devices {
		if device.Type == "disk" && device.Serial == volumeID {
			klog.V(4).Infof("Found disk metadata for volumeID %q. Bus: %q, Address: %q",
				volumeID, device.Bus, device.Address)

			diskPattern := fmt.Sprintf("/dev/disk/by-path/*-%s-%s", device.Bus, device.Address)
			diskPaths, err := filepath.Glob(diskPattern)
			if err != nil {
				klog.Errorf(
					"could not retrieve disk path for volumeID: %q. Error filepath.Glob(%q): %v",
					volumeID, diskPattern, err)
				return ""
			}

			if len(diskPaths) == 1 {
				return diskPaths[0]
			}

			klog.Errorf(
				"expecting to find one disk path for volumeID %q, found %d: %v",
				volumeID, len(diskPaths), diskPaths)
			return ""
		}
	}

	klog.V(4).Infof("Не удалось получить метаданные устройства для volumeID: %q", volumeID)
	return ""
}

// GetDevicePath returns the path of an attached block storage volume, specified by its id.
func (ec *Edgecenter) GetDevicePath(volumeID string) (string, error) {
	backoff := wait.Backoff{
		Duration: operationFinishInitDelay,
		Factor:   operationFinishFactor,
		Steps:    operationFinishSteps,
	}

	var devicePath string
	err := wait.ExponentialBackoff(backoff, func() (bool, error) {
		devicePath = ec.GetDevicePathBySerialID(volumeID)
		if devicePath != "" {
			return true, nil
		}
		devicePath = ec.getDevicePathFromInstanceMetadata(volumeID)
		return devicePath != "", nil
	})

	if errors.Is(err, wait.ErrWaitTimeout) {
		return "", fmt.Errorf("failed to find device for the volumeID: %q within the alloted time", volumeID)
	}

	if devicePath == "" {
		return "", fmt.Errorf("device path was empty for volumeID: %q", volumeID)
	}

	return devicePath, nil
}

// DeleteVolume deletes a volume given volume name.
func (ec *Edgecenter) DeleteVolume(ctx context.Context, volumeID string) error {
	used, err := ec.diskIsUsed(ctx, volumeID)
	if err != nil {
		return err
	}
	if used {
		msg := fmt.Sprintf("Cannot delete the volume %q, it's still attached to a node", volumeID)
		return k8svolume.NewDeletedVolumeInUseError(msg)
	}

	volumes, err := ec.volumeService("")
	if err != nil {
		return fmt.Errorf("unable to initialize cinder client for region: %s, err: %v", ec.region, err)
	}

	return volumes.deleteVolume(ctx, volumeID)
}

// DiskIsAttached queries if a volume is attached to a compute instance
func (ec *Edgecenter) DiskIsAttached(ctx context.Context, instanceID, volumeID string) (bool, error) {
	if instanceID == "" {
		klog.Warningf("calling DiskIsAttached with empty instanceid: %s %s", instanceID, volumeID)
	}
	volume, err := ec.getVolume(ctx, volumeID)
	if err != nil {
		return false, err
	}

	return instanceID == volume.AttachedServerID, nil
}

// DisksAreAttached queries if a list of volumes are attached to a compute instance
func (ec *Edgecenter) DisksAreAttached(ctx context.Context, instanceID string, volumeIDs []string) (map[string]bool, error) {
	attached := make(map[string]bool)
	for _, volumeID := range volumeIDs {
		isAttached, err := ec.DiskIsAttached(ctx, instanceID, volumeID)
		if err != nil && !errors.Is(err, ErrNotFound) {
			attached[volumeID] = true
			continue
		}
		attached[volumeID] = isAttached
	}
	return attached, nil
}

// diskIsUsed returns true a disk is attached to any node.
func (ec *Edgecenter) diskIsUsed(ctx context.Context, volumeID string) (bool, error) {
	volume, err := ec.getVolume(ctx, volumeID)
	if err != nil {
		return false, err
	}
	return volume.AttachedServerID != "", nil
}

// GetLabelsForVolume implements PVLabeler.GetLabelsForVolume
func (ec *Edgecenter) GetLabelsForVolume(ctx context.Context, pv *v1.PersistentVolume) (map[string]string, error) {
	// Ignore if not Cinder.
	if pv.Spec.Cinder == nil {
		return nil, nil
	}

	// Ignore any volumes that are being provisioned
	if pv.Spec.Cinder.VolumeID == k8svolume.ProvisionedVolumeName {
		return nil, nil
	}

	// Get Volume
	volume, err := ec.getVolume(ctx, pv.Spec.Cinder.VolumeID)
	if err != nil {
		return nil, err
	}

	labels := map[string]string{
		LabelZoneFailureDomain: volume.AvailabilityZone,
		LabelZoneRegion:        ec.region,
	}
	klog.V(4).Infof("Том %s имеет метки %v", pv.Spec.Cinder.VolumeID, labels)

	return labels, nil
}

// recordEdgecenterOperationMetric records edgecenter operation metrics
func recordEdgecenterOperationMetric(operation string, timeTaken float64, err error) {
	if err != nil {
		edgecenterAPIRequestErrors.With(prometheus.Labels{"request": operation}).Inc()
	} else {
		edgecenterOperationsLatency.With(prometheus.Labels{"request": operation}).Observe(timeTaken)
	}
}
