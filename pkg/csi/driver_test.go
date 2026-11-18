package csi

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	csi "github.com/container-storage-interface/spec/lib/go/csi"

	"github.com/ayushgawai/snapvol-csi/pkg/backend"
)

func testDriver(t *testing.T) *Driver {
	t.Helper()
	b, err := backend.NewLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return New(b, "test-node")
}

func TestDriver(t *testing.T) {
	ctx := context.Background()
	d := testDriver(t)

	cases := []struct {
		name string
		fn   func(t *testing.T)
	}{
		{"plugin info", func(t *testing.T) {
			info, err := d.GetPluginInfo(ctx, &csi.GetPluginInfoRequest{})
			if err != nil || info.GetName() != Name {
				t.Fatalf("%v %v", info, err)
			}
		}},
		{"create requires name and caps", func(t *testing.T) {
			if _, err := d.CreateVolume(ctx, &csi.CreateVolumeRequest{}); err == nil {
				t.Fatal("want error")
			}
		}},
		{"volume lifecycle + expand", func(t *testing.T) {
			req := &csi.CreateVolumeRequest{Name: "v1", VolumeCapabilities: caps(),
				CapacityRange: &csi.CapacityRange{RequiredBytes: 1 << 20}}
			v, err := d.CreateVolume(ctx, req)
			if err != nil {
				t.Fatal(err)
			}
			again, err := d.CreateVolume(ctx, req) // idempotent
			if err != nil || again.GetVolume().GetVolumeId() != v.GetVolume().GetVolumeId() {
				t.Fatalf("re-create: %v %v", again, err)
			}
			exp, err := d.ControllerExpandVolume(ctx, &csi.ControllerExpandVolumeRequest{
				VolumeId: v.GetVolume().GetVolumeId(), CapacityRange: &csi.CapacityRange{RequiredBytes: 2 << 20}})
			if err != nil || exp.GetCapacityBytes() != 2<<20 {
				t.Fatalf("expand: %v %v", exp, err)
			}
			if _, err := d.DeleteVolume(ctx, &csi.DeleteVolumeRequest{VolumeId: v.GetVolume().GetVolumeId()}); err != nil {
				t.Fatal(err)
			}
			if _, err := d.DeleteVolume(ctx, &csi.DeleteVolumeRequest{VolumeId: v.GetVolume().GetVolumeId()}); err != nil {
				t.Fatalf("delete not idempotent: %v", err)
			}
		}},
		{"snapshot clone restores data", func(t *testing.T) {
			v, err := d.CreateVolume(ctx, &csi.CreateVolumeRequest{Name: "src", VolumeCapabilities: caps()})
			if err != nil {
				t.Fatal(err)
			}
			bv, _ := d.b.GetVolume(v.GetVolume().GetVolumeId())
			if err := os.WriteFile(filepath.Join(bv.Path, "f"), []byte("data"), 0o644); err != nil {
				t.Fatal(err)
			}
			s, err := d.CreateSnapshot(ctx, &csi.CreateSnapshotRequest{Name: "snap", SourceVolumeId: bv.ID})
			if err != nil {
				t.Fatal(err)
			}
			clone, err := d.CreateVolume(ctx, &csi.CreateVolumeRequest{Name: "clone", VolumeCapabilities: caps(),
				VolumeContentSource: &csi.VolumeContentSource{Type: &csi.VolumeContentSource_Snapshot{
					Snapshot: &csi.VolumeContentSource_SnapshotSource{SnapshotId: s.GetSnapshot().GetSnapshotId()}}}})
			if err != nil {
				t.Fatal(err)
			}
			cv, _ := d.b.GetVolume(clone.GetVolume().GetVolumeId())
			got, err := os.ReadFile(filepath.Join(cv.Path, "f"))
			if err != nil || string(got) != "data" {
				t.Fatalf("clone data=%q err=%v", got, err)
			}
		}},
		{"stage and publish", func(t *testing.T) {
			v, err := d.CreateVolume(ctx, &csi.CreateVolumeRequest{Name: "pub", VolumeCapabilities: caps()})
			if err != nil {
				t.Fatal(err)
			}
			id := v.GetVolume().GetVolumeId()
			staging := filepath.Join(t.TempDir(), "staging")
			target := filepath.Join(t.TempDir(), "target")
			if _, err := d.ControllerPublishVolume(ctx, &csi.ControllerPublishVolumeRequest{VolumeId: id, NodeId: "test-node", VolumeCapability: caps()[0]}); err != nil {
				t.Fatal(err)
			}
			if _, err := d.NodeStageVolume(ctx, &csi.NodeStageVolumeRequest{VolumeId: id, StagingTargetPath: staging, VolumeCapability: caps()[0]}); err != nil {
				t.Fatal(err)
			}
			if _, err := d.NodePublishVolume(ctx, &csi.NodePublishVolumeRequest{VolumeId: id, StagingTargetPath: staging, TargetPath: target, VolumeCapability: caps()[0]}); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(target, "x"), []byte("y"), 0o644); err != nil {
				t.Fatalf("write via publish path: %v", err)
			}
			if _, err := d.NodeUnpublishVolume(ctx, &csi.NodeUnpublishVolumeRequest{VolumeId: id, TargetPath: target}); err != nil {
				t.Fatal(err)
			}
			if _, err := d.NodeUnstageVolume(ctx, &csi.NodeUnstageVolumeRequest{VolumeId: id, StagingTargetPath: staging}); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, c := range cases {
		t.Run(c.name, c.fn)
	}
}
