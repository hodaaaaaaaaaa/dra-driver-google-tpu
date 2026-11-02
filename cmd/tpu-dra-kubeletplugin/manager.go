/*
 * Copyright The Kubernetes Authors.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package main

import (
	"fmt"
	"math/rand"
	"os"
	"path"

	"github.com/google/uuid"
	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/klog/v2"
	"k8s.io/utils/ptr"
	cdispec "tags.cncf.io/container-device-interface/specs-go"
)

const (
	// vfioContainerDevice is the vfio container node, /dev/vfio/vfio, that a
	// container needs next to the group nodes of a chip bound to vfio-pci.
	vfioContainerDevice = "vfio"
	// libtpuLogDir is the host path that libtpu writes its logs to. It is shared by
	// every claim on the node and tailed by the log collector sidecar.
	libtpuLogDir     = "/tmp/tpu_logs"
	DevicePluginPath = "/var/lib/kubelet/plugins/tpu.google.com"
	LogDir           = DevicePluginPath + "/logs"
)

type AllocatableDevices map[string]*AllocatableDevice

type AllocatableDevice struct {
	UUID string `json:"uuid"`
	name string
	// deviceNodes are the chip's device files inside the manager's
	// DevDirectory, all of which must be present for the chip to be healthy.
	deviceNodes   []string
	index         int
	tpuGen        string
	brand         string
	driverVersion string
	allocatable   bool
	// Properties of the TPU the chip belongs to, what a claim selects on.
	accelerator string
	chipCount   int
	topology    string
}

// tpuManager manages google tpu devices.
type tpuManager struct {
	DevDirectory string
	tpuGen       string
	// chips are the chips found on the host (see probeTPUHardware), one
	// published device each.
	chips        []tpuChip
	devices      AllocatableDevices
	tpuChipCount int
	nodeLabels   map[string]string
	envs         map[string]string
	commonMounts []*cdispec.Mount
	tpuLogDir    string
}

// NewTPUManager builds the manager for the chips found in devDirectory.
func NewTPUManager(nodeLabels map[string]string, devDirectory string, chips []tpuChip) (*tpuManager, error) {
	accelerator := nodeLabels[AcceleratorLabel]
	acceleratorCount := nodeLabels[AcceleratorCountLabel]
	topology := nodeLabels[TopologyLabel]
	enableICIResiliency := nodeLabels[ICIResiliency]

	// Get TPU generation (v4, v5, etc.) and chips per node count from node labels.
	tpuGen, err := AcceleratorGen(nodeLabels[AcceleratorLabel])
	if err != nil {
		return nil, err
	}

	klog.Infof("Accelerator count from node labels: %s", acceleratorCount)
	chipCount, err := ChipCount(acceleratorCount)
	if err != nil {
		return nil, fmt.Errorf("cannot determine the number of TPU chips of the node, set --tpu-chip-count or the %s label: %w", AcceleratorCountLabel, err)
	}

	// Initialize default node-level environment variables for full-node claims.
	envs, err := InitEnvs(InitEnvOptions{
		Accelerator:         accelerator,
		Topology:            topology,
		ChipCount:           chipCount,
		AcceleratorCount:    chipCount,
		RequestedChipCount:  chipCount,
		EnableICIResiliency: enableICIResiliency,
	})
	if err != nil {
		return nil, fmt.Errorf("error initializing environment variables: %w", err)
	}

	commonMounts := []*cdispec.Mount{{
		HostPath:      libtpuLogDir,
		ContainerPath: libtpuLogDir,
		Options:       []string{"rw", "nosuid", "nodev", "bind"},
	},
	}

	return &tpuManager{
		tpuGen:       tpuGen,
		DevDirectory: devDirectory,
		chips:        chips,
		tpuChipCount: chipCount,
		nodeLabels:   nodeLabels,
		envs:         envs,
		commonMounts: commonMounts,
		tpuLogDir:    libtpuLogDir,
	}, nil
}

func (d *AllocatableDevice) GetDevice() resourceapi.Device {
	device := resourceapi.Device{
		Name: d.name,
		Attributes: map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
			"index": {
				IntValue: ptr.To(int64(d.index)),
			},
			"uuid": {
				StringValue: ptr.To(d.UUID),
			},
			"tpuGen": {
				StringValue: ptr.To(d.tpuGen),
			},
			"brand": {
				StringValue: ptr.To(d.brand),
			},
			"accelerator": {
				StringValue: ptr.To(d.accelerator),
			},
			"chipCount": {
				IntValue: ptr.To(int64(d.chipCount)),
			},
		},
	}
	// A node of a multi host slice cannot always tell its topology.
	if d.topology != "" {
		device.Attributes["topology"] = resourceapi.DeviceAttribute{
			StringValue: ptr.To(d.topology),
		}
	}
	return device
}

func (tm *tpuManager) getTpuInfo(i int, chip tpuChip) *AllocatableDevice {
	uuid := tm.generateUUIDs(deviceUUIDSeed(os.Getenv("NODE_NAME"), chip.name))
	allocatableDevice := &AllocatableDevice{
		UUID:        uuid,
		name:        chip.name,
		deviceNodes: chip.deviceNodes,
		index:       i,
		// memoryBytes:   memory.Total, Question?
		tpuGen:        tm.tpuGen,
		brand:         "Google",
		driverVersion: "1.0.0",
		allocatable:   true,
		accelerator:   tm.nodeLabels[AcceleratorLabel],
		chipCount:     tm.tpuChipCount,
		topology:      tm.nodeLabels[TopologyLabel],
	}
	return allocatableDevice
}

// enumerateAllPossibleTpuDevices publishes one device per chip found on the
// local node.
func (tm *tpuManager) enumerateAllPossibleTpuDevices() (AllocatableDevices, error) {
	klog.Info("Enumerating all possible Tpu Devices")
	allocatableDevices := make(AllocatableDevices)
	for i, chip := range tm.chips {
		klog.Infof("Found Google TPU %q with device nodes %v in %s", chip.name, chip.deviceNodes, tm.DevDirectory)
		device := tm.getTpuInfo(i, chip)
		allocatableDevices[device.name] = device
	}
	if len(tm.chips) != tm.tpuChipCount {
		return nil, fmt.Errorf("found %d TPU chips in %s, the node is expected to have %d chips", len(tm.chips), tm.DevDirectory, tm.tpuChipCount)
	}
	klog.Infof("Number of devices discovered: %d", len(tm.chips))
	tm.devices = allocatableDevices
	return allocatableDevices, nil
}

// deviceUUIDSeed returns the seed used to derive a chip's UUID. TPUs expose no
// hardware UUID, so the UUID is derived deterministically from the node name and
// the chip's device file name (e.g. "accel0" or "0"), which is what identifies a
// chip on the host and is also the ResourceSlice device name. This makes the
// UUID unique per chip on a node and stable across plugin restarts.
func deviceUUIDSeed(nodeName, deviceName string) string {
	return nodeName + "/" + deviceName
}

// generateUUIDs derives a deterministic "tpu-<uuid>" string from seed.
func (tm *tpuManager) generateUUIDs(seed string) string {
	rand := rand.New(rand.NewSource(hash(seed)))

	charset := make([]byte, 16)
	rand.Read(charset)
	uuid, _ := uuid.FromBytes(charset)
	return "tpu-" + uuid.String()
}

func hash(s string) int64 {
	h := int64(0)
	for _, c := range s {
		h = 31*h + int64(c)
	}
	return h
}

// DeviceNodeContainerEdits returns the device nodes a container needs to use
// the chip published as deviceID: every device node of the chip and, for chips
// bound to vfio-pci, the vfio container node.
func (tm *tpuManager) DeviceNodeContainerEdits(deviceID string) []*cdispec.DeviceNode {
	var names []string
	for _, chip := range tm.chips {
		if chip.name == deviceID {
			names = append(names, chip.deviceNodes...)
		}
	}
	if len(names) == 0 {
		klog.Errorf("No device nodes known for TPU device %q", deviceID)
		return nil
	}
	if tm.DevDirectory == devDirectoryVfio {
		names = append(names, vfioContainerDevice)
	}
	deviceNodes := make([]*cdispec.DeviceNode, 0, len(names))
	for _, name := range names {
		deviceNodes = append(deviceNodes, &cdispec.DeviceNode{
			Path:        path.Join(tm.DevDirectory, name),
			HostPath:    path.Join(tm.DevDirectory, name),
			Permissions: "mrw",
		})
	}
	return deviceNodes
}

// ValidateTpuRequest ensures a claim requests a supported number of TPU chips on this node.
func (tm *tpuManager) ValidateTpuRequest(requestDeviceIds []string) error {
	n := len(requestDeviceIds)
	if n == tm.tpuChipCount {
		return nil
	}
	if n == 1 && tm.allowsSingleChipAllocation() {
		return nil
	}
	if tm.allowsSingleChipAllocation() && tm.tpuChipCount != 1 {
		return fmt.Errorf("invalid TPU chip count request (%d): must request either 1 chip or all %d chips on this node", n, tm.tpuChipCount)
	}
	return fmt.Errorf("invalid TPU chip count request (%d): you must request all %d chips on this node together", n, tm.tpuChipCount)
}

// allowsSingleChipAllocation reports whether this node supports allocating a
// single TPU chip to a ResourceClaim. Only single-host nodes of generations
// marked singleChipAllocatable in tpuGenerationFamilies allow 1-chip claims.
func (tm *tpuManager) allowsSingleChipAllocation() bool {
	if tm == nil || !supportsSingleChipAllocation(tm.tpuGen) {
		return false
	}
	if len(tm.nodeLabels) == 0 {
		return true
	}
	return isSingleHostNode(tm.nodeLabels)
}

// envsForClaim returns the TPU environment variables to inject for a claim
// that was allocated numChips chips on this node.
func (tm *tpuManager) envsForClaim(numChips int) (map[string]string, error) {
	if tm == nil {
		return nil, nil
	}
	if numChips == tm.tpuChipCount || len(tm.nodeLabels) == 0 {
		return tm.envs, nil
	}
	if numChips == 1 && tm.allowsSingleChipAllocation() {
		return InitEnvs(InitEnvOptions{
			Accelerator:         tm.nodeLabels[AcceleratorLabel],
			Topology:            "1x1x1",
			ChipCount:           1,
			AcceleratorCount:    1,
			RequestedChipCount:  1,
			EnableICIResiliency: tm.nodeLabels[ICIResiliency],
		})
	}
	return nil, fmt.Errorf("unsupported TPU chip count %d for node with %d chips", numChips, tm.tpuChipCount)
}
