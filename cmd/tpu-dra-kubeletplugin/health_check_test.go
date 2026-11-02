/*
Copyright 2026 The Kubernetes Authors.

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

package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestNewTPUHealthCheckerClonesDevices(t *testing.T) {
	src := AllocatableDevices{
		"dev0": &AllocatableDevice{UUID: "dev0", allocatable: true},
	}
	hc := NewTPUHealthChecker(src, nil, "/dev", "v6e", nil)

	// Mutating the checker's copy must not affect the source map.
	d := hc.devices["dev0"]
	d.allocatable = false
	hc.devices["dev0"] = d

	if !src["dev0"].allocatable {
		t.Error("expected source device to be unchanged after mutating checker copy")
	}
}

func TestDeviceExists(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"0", "1"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte{}, 0600); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
	}
	hc := NewTPUHealthChecker(AllocatableDevices{}, nil, dir, "v6e", nil)

	tests := []struct {
		name        string
		deviceNodes []string
		want        bool
	}{
		{name: "one node present", deviceNodes: []string{"0"}, want: true},
		{name: "one node missing", deviceNodes: []string{"missing"}, want: false},
		{name: "all nodes of a chip present", deviceNodes: []string{"0", "1"}, want: true},
		{name: "one node of a chip missing", deviceNodes: []string{"0", "missing"}, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := hc.deviceExists(AllocatableDevice{name: tt.deviceNodes[0], deviceNodes: tt.deviceNodes})
			if err != nil {
				t.Fatalf("deviceExists: %v", err)
			}
			if got != tt.want {
				t.Errorf("deviceExists(%v) = %v, want %v", tt.deviceNodes, got, tt.want)
			}
		})
	}
}
