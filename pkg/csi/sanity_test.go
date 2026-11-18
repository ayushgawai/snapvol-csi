package csi

import (
	"path/filepath"
	"testing"

	"github.com/kubernetes-csi/csi-test/v5/pkg/sanity"

	"github.com/ayushgawai/snapvol-csi/pkg/backend"
)

// CSI sanity suite against the driver in dev (local backend) mode.
func TestSanity(t *testing.T) {
	dir := t.TempDir()
	b, err := backend.NewLocal(filepath.Join(dir, "data"))
	if err != nil {
		t.Fatal(err)
	}
	endpoint := "unix://" + filepath.Join(dir, "csi.sock")
	go func() { _ = New(b, "sanity-node").Run(endpoint) }()

	config := sanity.NewTestConfig()
	config.Address = endpoint
	config.StagingPath = filepath.Join(dir, "staging")
	config.TargetPath = filepath.Join(dir, "target")
	config.TestVolumeSize = 1 << 20
	config.TestVolumeParameters = map[string]string{}
	config.TestSnapshotParameters = map[string]string{}
	sanity.Test(t, config)
}
