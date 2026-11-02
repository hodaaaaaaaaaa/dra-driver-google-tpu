/*
 * Copyright 2026 The Kubernetes Authors.
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
	"reflect"
	"strings"
	"testing"

	resourceapi "k8s.io/api/resource/v1"
)

// singleNodeChips builds one chip per device name, each with that one device node.
func singleNodeChips(names ...string) []tpuChip {
	chips := make([]tpuChip, 0, len(names))
	for _, n := range names {
		chips = append(chips, tpuChip{name: n, deviceNodes: []string{n}})
	}
	return chips
}

func enumerate(t *testing.T, nodeName, tpuGen string, devNames ...string) AllocatableDevices {
	t.Helper()
	t.Setenv("NODE_NAME", nodeName)
	tm := &tpuManager{
		tpuGen:       tpuGen,
		DevDirectory: devDirectory,
		chips:        singleNodeChips(devNames...),
		tpuChipCount: len(devNames),
	}
	devs, err := tm.enumerateAllPossibleTpuDevices()
	if err != nil {
		t.Fatalf("enumerateAllPossibleTpuDevices: %v", err)
	}
	if len(devs) != len(devNames) {
		t.Fatalf("got %d devices, want %d", len(devs), len(devNames))
	}
	return devs
}

func TestDeviceUUIDsUniquePerChip(t *testing.T) {
	// Enumeration recognizes the accel<n> devices outside the fixed vfio path, so
	// the fake node exposes four accel-named chips.
	devs := enumerate(t, "node-a", "v4", "accel0", "accel1", "accel2", "accel3")
	seen := map[string]string{}
	for name, d := range devs {
		if !strings.HasPrefix(d.UUID, "tpu-") || len(d.UUID) != len("tpu-")+36 {
			t.Errorf("device %s: malformed uuid %q", name, d.UUID)
		}
		if prev, dup := seen[d.UUID]; dup {
			t.Errorf("devices %s and %s share uuid %s", prev, name, d.UUID)
		}
		seen[d.UUID] = name
	}
}

func TestDeviceUUIDsStableAcrossRestarts(t *testing.T) {
	names := []string{"accel0", "accel1", "accel2", "accel3"}
	first := enumerate(t, "node-a", "v4", names...)
	second := enumerate(t, "node-a", "v4", names...)
	for name, d := range first {
		if got := second[name].UUID; got != d.UUID {
			t.Errorf("device %s: uuid changed across enumerations: %s -> %s", name, d.UUID, got)
		}
	}
}

func TestDeviceUUIDsDifferAcrossNodes(t *testing.T) {
	names := []string{"accel0", "accel1"}
	a := enumerate(t, "node-a", "v4", names...)
	b := enumerate(t, "node-b", "v4", names...)
	for name, d := range a {
		if b[name].UUID == d.UUID {
			t.Errorf("device %s: same uuid %s on different nodes", name, d.UUID)
		}
	}
}

func TestHashDeterministic(t *testing.T) {
	if got1, got2 := hash("node-a"), hash("node-a"); got1 != got2 {
		t.Error("hash is not deterministic for the same input")
	}
	if hash("node-a") == hash("node-b") {
		t.Error("hash collided for distinct inputs")
	}
}

func TestGenerateUUIDsDeterministic(t *testing.T) {
	tm := &tpuManager{}
	a := tm.generateUUIDs("seed")
	b := tm.generateUUIDs("seed")
	if a != b {
		t.Errorf("generateUUIDs not deterministic: %q != %q", a, b)
	}
	if !strings.HasPrefix(a, "tpu-") {
		t.Errorf("expected tpu- prefix, got %q", a)
	}
	if tm.generateUUIDs("other") == a {
		t.Error("expected distinct seeds to produce distinct UUIDs")
	}
}

func TestGetDeviceTopologyOptional(t *testing.T) {
	with := (&AllocatableDevice{name: "d0", topology: "2x2"}).GetDevice()
	if _, ok := with.Attributes["topology"]; !ok {
		t.Error("expected topology attribute when topology is set")
	}

	without := (&AllocatableDevice{name: "d0"}).GetDevice()
	if _, ok := without.Attributes["topology"]; ok {
		t.Error("expected no topology attribute when topology is empty")
	}
	// Core attributes always present.
	for _, k := range []resourceapi.QualifiedName{"index", "uuid", "tpuGen", "brand", "accelerator", "chipCount"} {
		if _, ok := without.Attributes[k]; !ok {
			t.Errorf("missing expected attribute %q", k)
		}
	}
}

func TestDeviceNodeContainerEdits(t *testing.T) {
	tests := []struct {
		name   string
		tm     *tpuManager
		device string
		want   []string
	}{
		{
			name:   "v4 accel device",
			tm:     &tpuManager{DevDirectory: devDirectory, tpuGen: "v4", chips: singleNodeChips("accel0")},
			device: "accel0",
			want:   []string{"/dev/accel0"},
		},
		{
			name:   "v6e vfio device also gets the vfio container node",
			tm:     &tpuManager{DevDirectory: devDirectoryVfio, tpuGen: "v6e", chips: singleNodeChips("0", "1")},
			device: "1",
			want:   []string{"/dev/vfio/1", "/dev/vfio/vfio"},
		},
		{
			name: "v7x chip made of two vfio devices gets both and the container node",
			tm: &tpuManager{DevDirectory: devDirectoryVfio, tpuGen: "v7x", chips: []tpuChip{
				{name: "0", deviceNodes: []string{"0", "1"}},
			}},
			device: "0",
			want:   []string{"/dev/vfio/0", "/dev/vfio/1", "/dev/vfio/vfio"},
		},
		{
			name:   "unknown device gets nothing",
			tm:     &tpuManager{DevDirectory: devDirectoryVfio, tpuGen: "v6e", chips: singleNodeChips("0")},
			device: "7",
			want:   nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got []string
			for _, node := range tt.tm.DeviceNodeContainerEdits(tt.device) {
				if node.HostPath != node.Path {
					t.Errorf("device node %s is mounted at %s", node.HostPath, node.Path)
				}
				got = append(got, node.Path)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("DeviceNodeContainerEdits(%q) = %v, want %v", tt.device, got, tt.want)
			}
		})
	}
}

func TestValidateTpuRequest(t *testing.T) {
	tm := &tpuManager{tpuChipCount: 4, tpuGen: "v6e"}
	if err := tm.ValidateTpuRequest([]string{"a", "b", "c", "d"}); err != nil {
		t.Errorf("expected full-chip request to pass: %v", err)
	}
	if err := tm.ValidateTpuRequest([]string{"a"}); err == nil {
		t.Error("expected single-chip request on v6e 4-chip node to be rejected")
	}
	if err := tm.ValidateTpuRequest([]string{"a", "b"}); err == nil {
		t.Error("expected partial request to be rejected")
	}

	singleHostV7x := &tpuManager{
		tpuChipCount: 4,
		tpuGen:       "v7x",
		nodeLabels: map[string]string{
			AcceleratorLabel:      "tpu7x",
			AcceleratorCountLabel: "4",
			TopologyLabel:         "2x2x1",
		},
	}
	if err := singleHostV7x.ValidateTpuRequest([]string{"2"}); err != nil {
		t.Errorf("expected 1-chip request on single-host v7x 4-chip node to pass: %v", err)
	}
	if err := singleHostV7x.ValidateTpuRequest([]string{"0", "2", "4", "6"}); err != nil {
		t.Errorf("expected full 4-chip request on single-host v7x node to pass: %v", err)
	}
	if err := singleHostV7x.ValidateTpuRequest([]string{"0", "2"}); err == nil {
		t.Error("expected 2-of-4 partial request on v7x node to be rejected")
	}

	multiHostV7x := &tpuManager{
		tpuChipCount: 4,
		tpuGen:       "v7x",
		nodeLabels: map[string]string{
			AcceleratorLabel:      "tpu7x",
			AcceleratorCountLabel: "4",
			TopologyLabel:         "4x4x4",
		},
	}
	if err := multiHostV7x.ValidateTpuRequest([]string{"0"}); err == nil {
		t.Error("expected 1-chip request on multi-host v7x slice to be rejected")
	}
}

func TestEnvsForClaimSingleChipOnFourChipV7x(t *testing.T) {
	t.Setenv(NodeIPEnv, "10.0.0.9")
	labels := map[string]string{
		AcceleratorLabel:      "tpu7x",
		AcceleratorCountLabel: "4",
		TopologyLabel:         "2x2x1",
	}
	chips := []tpuChip{
		{name: "0", deviceNodes: []string{"0", "1"}},
		{name: "2", deviceNodes: []string{"2", "3"}},
		{name: "4", deviceNodes: []string{"4", "5"}},
		{name: "6", deviceNodes: []string{"6", "7"}},
	}
	tm, err := NewTPUManager(labels, devDirectoryVfio, chips)
	if err != nil {
		t.Fatalf("NewTPUManager: %v", err)
	}

	oneChipEnvs, err := tm.envsForClaim(1)
	if err != nil {
		t.Fatalf("envsForClaim(1): %v", err)
	}
	if oneChipEnvs["TPU_ACCELERATOR_TYPE"] != "tpu7x-2" ||
		oneChipEnvs["TPU_TOPOLOGY"] != "1x1x1" ||
		oneChipEnvs["TPU_CHIPS_PER_HOST_BOUNDS"] != "1,1,1" ||
		oneChipEnvs["TPU_HOST_BOUNDS"] != "1,1,1" ||
		oneChipEnvs["TPU_WORKER_ID"] != "0" ||
		oneChipEnvs["TPU_WORKER_HOSTNAMES"] != "localhost" ||
		oneChipEnvs["VBAR_CONTROL_SERVICE_URL"] != "10.0.0.9:8353" {
		t.Errorf("unexpected 1-chip envs: %v", oneChipEnvs)
	}

	allChipEnvs, err := tm.envsForClaim(4)
	if err != nil {
		t.Fatalf("envsForClaim(4): %v", err)
	}
	if allChipEnvs["TPU_ACCELERATOR_TYPE"] != "tpu7x-8" ||
		allChipEnvs["TPU_TOPOLOGY"] != "2x2x1" ||
		allChipEnvs["TPU_CHIPS_PER_HOST_BOUNDS"] != "2,2,1" {
		t.Errorf("unexpected 4-chip envs: %v", allChipEnvs)
	}
}

func TestEnumerateAllPossibleTpuDevices(t *testing.T) {
	t.Setenv("NODE_NAME", "node-a")
	// A v7x node with two chips of two vfio devices each publishes two devices,
	// named after each chip's first device node, in chip order.
	chips := []tpuChip{
		{name: "0", deviceNodes: []string{"0", "1"}},
		{name: "2", deviceNodes: []string{"2", "3"}},
	}
	tm := &tpuManager{DevDirectory: devDirectoryVfio, tpuChipCount: 2, tpuGen: "v7x", chips: chips}

	devices, err := tm.enumerateAllPossibleTpuDevices()
	if err != nil {
		t.Fatalf("enumerateAllPossibleTpuDevices: %v", err)
	}
	if len(devices) != 2 {
		t.Fatalf("expected 2 devices, got %d", len(devices))
	}
	for i, chip := range chips {
		d, ok := devices[chip.name]
		if !ok {
			t.Fatalf("no device named %q in %v", chip.name, devices)
		}
		if d.index != i {
			t.Errorf("device %s has index %d, want %d", chip.name, d.index, i)
		}
		if d.tpuGen != "v7x" || d.chipCount != 2 {
			t.Errorf("device %s = %+v, want tpuGen v7x and chipCount 2", chip.name, d)
		}
		if !reflect.DeepEqual(d.deviceNodes, chip.deviceNodes) {
			t.Errorf("device %s has device nodes %v, want %v", chip.name, d.deviceNodes, chip.deviceNodes)
		}
	}

	// The chip count from the node labels must match the chips found.
	tmBad := &tpuManager{DevDirectory: devDirectoryVfio, tpuChipCount: 4, tpuGen: "v7x", chips: chips}
	if _, err := tmBad.enumerateAllPossibleTpuDevices(); err == nil {
		t.Error("expected error when discovered chip count != expected chip count")
	}
}
