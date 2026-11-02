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
	"slices"
	"strings"
	"testing"

	"github.com/opencontainers/runtime-spec/specs-go"
	"k8s.io/dynamic-resource-allocation/kubeletplugin"
	cdiapi "tags.cncf.io/container-device-interface/pkg/cdi"
	cdispec "tags.cncf.io/container-device-interface/specs-go"
)

func newTestCDIHandler(t *testing.T) (*CDIHandler, string) {
	t.Helper()
	dir := t.TempDir()
	cdi, err := NewCDIHandler(&Config{flags: &Flags{cdiRoot: dir}})
	if err != nil {
		t.Fatalf("NewCDIHandler: %v", err)
	}
	return cdi, dir
}

func TestGetClaimDevices(t *testing.T) {
	cdi, _ := newTestCDIHandler(t)

	got := cdi.GetClaimDevices("claim-uid", []string{"dev0", "dev1"})

	want := []string{
		cdiKind + "=" + cdiCommonDeviceName,
		cdiKind + "=claim-uid-dev0",
		cdiKind + "=claim-uid-dev1",
	}
	if len(got) != len(want) {
		t.Fatalf("got %d devices, want %d: %v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("device[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestCreateAndDeleteClaimSpecFile(t *testing.T) {
	cdi, dir := newTestCDIHandler(t)

	devices := PreparedDevices{
		{Device: kubeletplugin.Device{DeviceName: "dev0"}, ContainerEdits: &cdiapi.ContainerEdits{}},
	}
	if err := cdi.CreateClaimSpecFile("claim-uid", devices, map[string]string{"TPU_TOPOLOGY": "1x1x1"}); err != nil {
		t.Fatalf("CreateClaimSpecFile: %v", err)
	}

	if countSpecFiles(t, dir) == 0 {
		t.Fatal("expected a spec file to be written")
	}

	if err := cdi.DeleteClaimSpecFile("claim-uid"); err != nil {
		t.Fatalf("DeleteClaimSpecFile: %v", err)
	}
	if n := countSpecFiles(t, dir); n != 0 {
		t.Errorf("expected spec files removed, found %d", n)
	}
}

func TestCreateClaimSpecFileInjectsEnvsAndDeviceNodes(t *testing.T) {
	cdi, dir := newTestCDIHandler(t)
	if err := cdi.CreateCommonSpecFile(nil, nil); err != nil {
		t.Fatalf("CreateCommonSpecFile: %v", err)
	}

	devices := PreparedDevices{
		{
			Device: kubeletplugin.Device{DeviceName: "2"},
			ContainerEdits: &cdiapi.ContainerEdits{
				ContainerEdits: &cdispec.ContainerEdits{
					DeviceNodes: []*cdispec.DeviceNode{
						{Path: "/dev/vfio/2", HostPath: "/dev/vfio/2", Type: "c", Major: 240, Minor: 2, Permissions: "mrw"},
						{Path: "/dev/vfio/3", HostPath: "/dev/vfio/3", Type: "c", Major: 240, Minor: 3, Permissions: "mrw"},
						{Path: "/dev/vfio/vfio", HostPath: "/dev/vfio/vfio", Type: "c", Major: 10, Minor: 196, Permissions: "mrw"},
					},
				},
			},
		},
	}
	claimEnvs := map[string]string{
		"TPU_ACCELERATOR_TYPE":      "tpu7x-2",
		"TPU_TOPOLOGY":              "1x1x1",
		"TPU_CHIPS_PER_HOST_BOUNDS": "1,1,1",
	}
	if err := cdi.CreateClaimSpecFile("claim-uid", devices, claimEnvs); err != nil {
		t.Fatalf("CreateClaimSpecFile: %v", err)
	}

	readerCache, err := cdiapi.NewCache(cdiapi.WithSpecDirs(dir), cdiapi.WithAutoRefresh(false))
	if err != nil {
		t.Fatalf("NewCache: %v", err)
	}
	if err := readerCache.Refresh(); err != nil {
		t.Fatalf("Refresh: %v", err)
	}

	ociSpec := &specs.Spec{Process: &specs.Process{}, Linux: &specs.Linux{}}
	unresolved, err := readerCache.InjectDevices(ociSpec, cdi.GetClaimDevices("claim-uid", []string{"2"})...)
	if err != nil {
		t.Fatalf("InjectDevices: %v (unresolved=%v)", err, unresolved)
	}
	for _, wantEnv := range []string{
		"DRA_RESOURCE_DRIVER_NAME=" + DriverName,
		"TPU_ACCELERATOR_TYPE=tpu7x-2",
		"TPU_TOPOLOGY=1x1x1",
		"TPU_CHIPS_PER_HOST_BOUNDS=1,1,1",
		"TPU_DEVICE_2_RESOURCE_CLAIM=claim-uid",
	} {
		if !slices.Contains(ociSpec.Process.Env, wantEnv) {
			t.Errorf("injected OCI env %v missing %q", ociSpec.Process.Env, wantEnv)
		}
	}
	var gotDevPaths []string
	for _, d := range ociSpec.Linux.Devices {
		gotDevPaths = append(gotDevPaths, d.Path)
	}
	wantDevPaths := []string{"/dev/vfio/2", "/dev/vfio/3", "/dev/vfio/vfio"}
	if !slices.Equal(gotDevPaths, wantDevPaths) {
		t.Errorf("injected OCI devices = %v, want %v", gotDevPaths, wantDevPaths)
	}
}

func TestCreateCommonSpecFile(t *testing.T) {
	cdi, dir := newTestCDIHandler(t)

	if err := cdi.CreateCommonSpecFile(map[string]string{"FOO": "bar"}, nil); err != nil {
		t.Fatalf("CreateCommonSpecFile: %v", err)
	}
	if countSpecFiles(t, dir) == 0 {
		t.Error("expected a common spec file to be written")
	}
}

func countSpecFiles(t *testing.T, dir string) int {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	n := 0
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".json") || strings.HasSuffix(e.Name(), ".yaml") {
			n++
		}
	}
	return n
}
