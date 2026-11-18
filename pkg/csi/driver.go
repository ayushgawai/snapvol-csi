// Package csi implements CSI Identity/Controller/Node over gRPC.
// In dev mode volumes are local directories; NodeStage/Publish bind-mount
// them (falling back to a symlink where mounts are not permitted).
package csi

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	csi "github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/ayushgawai/snapvol-csi/pkg/backend"
	"github.com/ayushgawai/snapvol-csi/pkg/obs"
)

const Name = "csi.snapvol.dev"

type Driver struct {
	csi.UnimplementedIdentityServer
	csi.UnimplementedControllerServer
	csi.UnimplementedNodeServer
	b      backend.Backend
	nodeID string
	log    *slog.Logger
	mu     sync.Mutex
	staged map[string]string // volumeID -> source path
}

func New(b backend.Backend, nodeID string) *Driver {
	if nodeID == "" {
		nodeID, _ = os.Hostname()
	}
	return &Driver{b: b, nodeID: nodeID, log: slog.Default(), staged: map[string]string{}}
}

func (d *Driver) Run(endpoint string) error {
	proto, addr, ok := strings.Cut(endpoint, "://")
	if !ok {
		proto, addr = "unix", endpoint
	}
	if proto == "unix" {
		_ = os.Remove(addr)
		if err := os.MkdirAll(filepath.Dir(addr), 0o755); err != nil {
			return err
		}
	}
	lis, err := net.Listen(proto, addr)
	if err != nil {
		return err
	}
	srv := grpc.NewServer()
	csi.RegisterIdentityServer(srv, d)
	csi.RegisterControllerServer(srv, d)
	csi.RegisterNodeServer(srv, d)
	d.log.Info("csi listening", "endpoint", endpoint, "node", d.nodeID)
	return srv.Serve(lis)
}

// ---- Identity ----

func (d *Driver) GetPluginInfo(context.Context, *csi.GetPluginInfoRequest) (*csi.GetPluginInfoResponse, error) {
	return &csi.GetPluginInfoResponse{Name: Name, VendorVersion: "0.1.0"}, nil
}

func (d *Driver) GetPluginCapabilities(context.Context, *csi.GetPluginCapabilitiesRequest) (*csi.GetPluginCapabilitiesResponse, error) {
	return &csi.GetPluginCapabilitiesResponse{Capabilities: []*csi.PluginCapability{
		{Type: &csi.PluginCapability_Service_{Service: &csi.PluginCapability_Service{Type: csi.PluginCapability_Service_CONTROLLER_SERVICE}}},
		{Type: &csi.PluginCapability_Service_{Service: &csi.PluginCapability_Service{Type: csi.PluginCapability_Service_VOLUME_ACCESSIBILITY_CONSTRAINTS}}},
	}}, nil
}

func (d *Driver) Probe(context.Context, *csi.ProbeRequest) (*csi.ProbeResponse, error) {
	return &csi.ProbeResponse{}, nil
}

// ---- Controller ----

func caps() []*csi.VolumeCapability {
	return []*csi.VolumeCapability{{
		AccessType: &csi.VolumeCapability_Mount{Mount: &csi.VolumeCapability_MountVolume{}},
		AccessMode: &csi.VolumeCapability_AccessMode{Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER},
	}}
}

func (d *Driver) ControllerGetCapabilities(context.Context, *csi.ControllerGetCapabilitiesRequest) (*csi.ControllerGetCapabilitiesResponse, error) {
	mk := func(t csi.ControllerServiceCapability_RPC_Type) *csi.ControllerServiceCapability {
		return &csi.ControllerServiceCapability{Type: &csi.ControllerServiceCapability_Rpc{Rpc: &csi.ControllerServiceCapability_RPC{Type: t}}}
	}
	return &csi.ControllerGetCapabilitiesResponse{Capabilities: []*csi.ControllerServiceCapability{
		mk(csi.ControllerServiceCapability_RPC_CREATE_DELETE_VOLUME),
		mk(csi.ControllerServiceCapability_RPC_PUBLISH_UNPUBLISH_VOLUME),
		mk(csi.ControllerServiceCapability_RPC_CREATE_DELETE_SNAPSHOT),
		mk(csi.ControllerServiceCapability_RPC_LIST_SNAPSHOTS),
		mk(csi.ControllerServiceCapability_RPC_LIST_VOLUMES),
		mk(csi.ControllerServiceCapability_RPC_GET_CAPACITY),
		mk(csi.ControllerServiceCapability_RPC_EXPAND_VOLUME),
		mk(csi.ControllerServiceCapability_RPC_CLONE_VOLUME),
	}}, nil
}

func errCode(err error) error {
	switch {
	case errors.Is(err, backend.ErrNotFound):
		return status.Error(codes.NotFound, err.Error())
	case errors.Is(err, backend.ErrAlreadyExists):
		return status.Error(codes.AlreadyExists, err.Error())
	case errors.Is(err, backend.ErrNotImplemented):
		return status.Error(codes.Unimplemented, err.Error())
	default:
		return status.Error(codes.Internal, err.Error())
	}
}

func sizeOf(req *csi.CreateVolumeRequest) int64 {
	if cr := req.GetCapacityRange(); cr != nil && cr.GetRequiredBytes() > 0 {
		return cr.GetRequiredBytes()
	}
	return 1 << 30
}

func (d *Driver) CreateVolume(_ context.Context, req *csi.CreateVolumeRequest) (resp *csi.CreateVolumeResponse, err error) {
	defer obs.Observe("CreateVolume", &err)()
	if req.GetName() == "" {
		return nil, status.Error(codes.InvalidArgument, "name required")
	}
	if len(req.GetVolumeCapabilities()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "volume_capabilities required")
	}
	var src string
	if content := req.GetVolumeContentSource(); content != nil {
		switch s := content.GetType().(type) {
		case *csi.VolumeContentSource_Snapshot:
			src = s.Snapshot.GetSnapshotId()
		case *csi.VolumeContentSource_Volume:
			// Clone from volume: snapshot it under the hood, then restore.
			snap, e := d.b.CreateSnapshot("clone-"+req.GetName(), s.Volume.GetVolumeId())
			if e != nil {
				return nil, errCode(e)
			}
			src = snap.ID
		}
	}
	v, err := d.b.CreateVolume(req.GetName(), sizeOf(req), src)
	if err != nil {
		return nil, errCode(err)
	}
	obs.Volumes.Set(float64(len(d.b.ListVolumes())))
	return &csi.CreateVolumeResponse{Volume: &csi.Volume{
		VolumeId: v.ID, CapacityBytes: v.SizeBytes, VolumeContext: map[string]string{"path": v.Path},
		AccessibleTopology: []*csi.Topology{{Segments: map[string]string{"topology.csi.snapvol.dev/node": d.nodeID}}},
		ContentSource:      req.GetVolumeContentSource(),
	}}, nil
}

func (d *Driver) DeleteVolume(_ context.Context, req *csi.DeleteVolumeRequest) (resp *csi.DeleteVolumeResponse, err error) {
	defer obs.Observe("DeleteVolume", &err)()
	if req.GetVolumeId() == "" {
		return nil, status.Error(codes.InvalidArgument, "volume_id required")
	}
	if err := d.b.DeleteVolume(req.GetVolumeId()); err != nil {
		return nil, errCode(err)
	}
	return &csi.DeleteVolumeResponse{}, nil
}

func invalid(msg string) error { return status.Error(codes.InvalidArgument, msg) }

func (d *Driver) ControllerPublishVolume(_ context.Context, req *csi.ControllerPublishVolumeRequest) (resp *csi.ControllerPublishVolumeResponse, err error) {
	defer obs.Observe("ControllerPublishVolume", &err)()
	if req.GetVolumeId() == "" {
		return nil, invalid("volume_id required")
	}
	if req.GetNodeId() == "" {
		return nil, invalid("node_id required")
	}
	if req.GetVolumeCapability() == nil {
		return nil, invalid("volume_capability required")
	}
	v, err := d.b.GetVolume(req.GetVolumeId())
	if err != nil {
		return nil, errCode(err)
	}
	if req.GetNodeId() != "" && req.GetNodeId() != d.nodeID {
		return nil, status.Errorf(codes.NotFound, "node %s not found", req.GetNodeId())
	}
	// NFS export path in prod; local backing path in dev mode.
	return &csi.ControllerPublishVolumeResponse{PublishContext: map[string]string{"path": v.Path}}, nil
}

func (d *Driver) ControllerUnpublishVolume(_ context.Context, req *csi.ControllerUnpublishVolumeRequest) (*csi.ControllerUnpublishVolumeResponse, error) {
	if req.GetVolumeId() == "" {
		return nil, invalid("volume_id required")
	}
	if req.GetNodeId() != "" && req.GetNodeId() != d.nodeID {
		if _, err := d.b.GetVolume(req.GetVolumeId()); err != nil {
			return nil, errCode(err)
		}
		return nil, status.Errorf(codes.NotFound, "node %s not found", req.GetNodeId())
	}
	return &csi.ControllerUnpublishVolumeResponse{}, nil
}

func (d *Driver) ValidateVolumeCapabilities(_ context.Context, req *csi.ValidateVolumeCapabilitiesRequest) (*csi.ValidateVolumeCapabilitiesResponse, error) {
	if req.GetVolumeId() == "" {
		return nil, invalid("volume_id required")
	}
	if len(req.GetVolumeCapabilities()) == 0 {
		return nil, invalid("volume_capabilities required")
	}
	if _, err := d.b.GetVolume(req.GetVolumeId()); err != nil {
		return nil, errCode(err)
	}
	return &csi.ValidateVolumeCapabilitiesResponse{Confirmed: &csi.ValidateVolumeCapabilitiesResponse_Confirmed{VolumeCapabilities: req.GetVolumeCapabilities()}}, nil
}

func (d *Driver) ListVolumes(_ context.Context, req *csi.ListVolumesRequest) (*csi.ListVolumesResponse, error) {
	vols := d.b.ListVolumes()
	start := 0
	if req.GetStartingToken() != "" {
		n, err := parseToken(req.GetStartingToken())
		if err != nil {
			return nil, status.Error(codes.Aborted, "invalid starting_token")
		}
		start = n
	}
	if start > len(vols) {
		return nil, status.Error(codes.Aborted, "starting_token out of range")
	}
	end := len(vols)
	next := ""
	if req.GetMaxEntries() > 0 && start+int(req.GetMaxEntries()) < end {
		end = start + int(req.GetMaxEntries())
		next = itoa(end)
	}
	var entries []*csi.ListVolumesResponse_Entry
	for _, v := range vols[start:end] {
		entries = append(entries, &csi.ListVolumesResponse_Entry{Volume: &csi.Volume{VolumeId: v.ID, CapacityBytes: v.SizeBytes}})
	}
	return &csi.ListVolumesResponse{Entries: entries, NextToken: next}, nil
}

func parseToken(s string) (int, error) {
	var n int
	_, err := fmt.Sscan(s, &n)
	return n, err
}
func itoa(n int) string { return fmt.Sprintf("%d", n) }

func (d *Driver) GetCapacity(context.Context, *csi.GetCapacityRequest) (*csi.GetCapacityResponse, error) {
	return &csi.GetCapacityResponse{AvailableCapacity: 1 << 40}, nil // ponytail: dev-mode fixed 1TiB; report real fs stat in prod
}

func (d *Driver) CreateSnapshot(_ context.Context, req *csi.CreateSnapshotRequest) (resp *csi.CreateSnapshotResponse, err error) {
	defer obs.Observe("CreateSnapshot", &err)()
	if req.GetName() == "" || req.GetSourceVolumeId() == "" {
		return nil, status.Error(codes.InvalidArgument, "name and source_volume_id required")
	}
	s, err := d.b.CreateSnapshot(req.GetName(), req.GetSourceVolumeId())
	if err != nil {
		return nil, errCode(err)
	}
	return &csi.CreateSnapshotResponse{Snapshot: &csi.Snapshot{
		SnapshotId: s.ID, SourceVolumeId: s.SourceVol, SizeBytes: s.SizeBytes,
		CreationTime: timestamppb.New(time.Unix(0, s.CreatedAt)), ReadyToUse: true,
	}}, nil
}

func (d *Driver) DeleteSnapshot(_ context.Context, req *csi.DeleteSnapshotRequest) (*csi.DeleteSnapshotResponse, error) {
	if req.GetSnapshotId() == "" {
		return nil, status.Error(codes.InvalidArgument, "snapshot_id required")
	}
	if err := d.b.DeleteSnapshot(req.GetSnapshotId()); err != nil {
		return nil, errCode(err)
	}
	return &csi.DeleteSnapshotResponse{}, nil
}

func (d *Driver) ListSnapshots(_ context.Context, req *csi.ListSnapshotsRequest) (*csi.ListSnapshotsResponse, error) {
	var snaps []*backend.Snapshot
	for _, s := range d.b.ListSnapshots() {
		if req.GetSnapshotId() != "" && s.ID != req.GetSnapshotId() {
			continue
		}
		if req.GetSourceVolumeId() != "" && s.SourceVol != req.GetSourceVolumeId() {
			continue
		}
		snaps = append(snaps, s)
	}
	start := 0
	if req.GetStartingToken() != "" {
		n, err := parseToken(req.GetStartingToken())
		if err != nil || n > len(snaps) {
			return nil, status.Error(codes.Aborted, "invalid starting_token")
		}
		start = n
	}
	end, next := len(snaps), ""
	if req.GetMaxEntries() > 0 && start+int(req.GetMaxEntries()) < end {
		end = start + int(req.GetMaxEntries())
		next = itoa(end)
	}
	var out []*csi.ListSnapshotsResponse_Entry
	for _, s := range snaps[start:end] {
		out = append(out, &csi.ListSnapshotsResponse_Entry{Snapshot: &csi.Snapshot{
			SnapshotId: s.ID, SourceVolumeId: s.SourceVol, SizeBytes: s.SizeBytes,
			CreationTime: timestamppb.New(time.Unix(0, s.CreatedAt)), ReadyToUse: s.ReadyToUse,
		}})
	}
	return &csi.ListSnapshotsResponse{Entries: out, NextToken: next}, nil
}

func (d *Driver) ControllerExpandVolume(_ context.Context, req *csi.ControllerExpandVolumeRequest) (resp *csi.ControllerExpandVolumeResponse, err error) {
	defer obs.Observe("ControllerExpandVolume", &err)()
	if req.GetVolumeId() == "" {
		return nil, invalid("volume_id required")
	}
	if req.GetCapacityRange() == nil {
		return nil, invalid("capacity_range required")
	}
	size := req.GetCapacityRange().GetRequiredBytes()
	v, err := d.b.ExpandVolume(req.GetVolumeId(), size)
	if err != nil {
		return nil, errCode(err)
	}
	return &csi.ControllerExpandVolumeResponse{CapacityBytes: v.SizeBytes, NodeExpansionRequired: false}, nil
}

// ---- Node ----

func (d *Driver) NodeGetInfo(context.Context, *csi.NodeGetInfoRequest) (*csi.NodeGetInfoResponse, error) {
	return &csi.NodeGetInfoResponse{
		NodeId:             d.nodeID,
		MaxVolumesPerNode:  100,
		AccessibleTopology: &csi.Topology{Segments: map[string]string{"topology.csi.snapvol.dev/node": d.nodeID}},
	}, nil
}

func (d *Driver) NodeGetCapabilities(context.Context, *csi.NodeGetCapabilitiesRequest) (*csi.NodeGetCapabilitiesResponse, error) {
	mk := func(t csi.NodeServiceCapability_RPC_Type) *csi.NodeServiceCapability {
		return &csi.NodeServiceCapability{Type: &csi.NodeServiceCapability_Rpc{Rpc: &csi.NodeServiceCapability_RPC{Type: t}}}
	}
	return &csi.NodeGetCapabilitiesResponse{Capabilities: []*csi.NodeServiceCapability{
		mk(csi.NodeServiceCapability_RPC_STAGE_UNSTAGE_VOLUME),
		mk(csi.NodeServiceCapability_RPC_GET_VOLUME_STATS),
		mk(csi.NodeServiceCapability_RPC_EXPAND_VOLUME),
	}}, nil
}

// bindMount tries a real bind mount; falls back to a symlink (dev/test, unprivileged).
func bindMount(src, dst string) error {
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return err
	}
	if out, err := exec.Command("mount", "--bind", src, dst).CombinedOutput(); err == nil {
		_ = out
		return nil
	}
	_ = os.Remove(dst)
	return os.Symlink(src, dst)
}

func unmount(dst string) {
	_ = exec.Command("umount", dst).Run()
	if fi, err := os.Lstat(dst); err == nil && fi.Mode()&os.ModeSymlink != 0 {
		_ = os.Remove(dst)
		return
	}
	_ = os.RemoveAll(dst)
}

func (d *Driver) NodeStageVolume(_ context.Context, req *csi.NodeStageVolumeRequest) (resp *csi.NodeStageVolumeResponse, err error) {
	defer obs.Observe("NodeStageVolume", &err)()
	if req.GetVolumeId() == "" {
		return nil, invalid("volume_id required")
	}
	if req.GetStagingTargetPath() == "" {
		return nil, invalid("staging_target_path required")
	}
	if req.GetVolumeCapability() == nil {
		return nil, invalid("volume_capability required")
	}
	if _, e := d.b.GetVolume(req.GetVolumeId()); e != nil {
		return nil, errCode(e)
	}
	src := req.GetPublishContext()["path"]
	if src == "" {
		v, e := d.b.GetVolume(req.GetVolumeId())
		if e != nil {
			return nil, errCode(e)
		}
		src = v.Path
	}
	if _, e := os.Stat(src); e != nil {
		// Staging on a node that doesn't share the controller's fs: create placeholder.
		if e := os.MkdirAll(src, 0o755); e != nil {
			return nil, errCode(e)
		}
	}
	d.mu.Lock()
	d.staged[req.GetVolumeId()] = src
	d.mu.Unlock()
	if _, e := os.Lstat(req.GetStagingTargetPath()); e == nil {
		return &csi.NodeStageVolumeResponse{}, nil // already staged
	}
	if e := bindMount(src, req.GetStagingTargetPath()); e != nil {
		return nil, errCode(e)
	}
	return &csi.NodeStageVolumeResponse{}, nil
}

func (d *Driver) NodeUnstageVolume(_ context.Context, req *csi.NodeUnstageVolumeRequest) (*csi.NodeUnstageVolumeResponse, error) {
	if req.GetVolumeId() == "" {
		return nil, invalid("volume_id required")
	}
	if req.GetStagingTargetPath() == "" {
		return nil, invalid("staging_target_path required")
	}
	unmount(req.GetStagingTargetPath())
	d.mu.Lock()
	delete(d.staged, req.GetVolumeId())
	d.mu.Unlock()
	return &csi.NodeUnstageVolumeResponse{}, nil
}

func (d *Driver) NodePublishVolume(_ context.Context, req *csi.NodePublishVolumeRequest) (resp *csi.NodePublishVolumeResponse, err error) {
	defer obs.Observe("NodePublishVolume", &err)()
	if req.GetVolumeId() == "" {
		return nil, invalid("volume_id required")
	}
	if req.GetTargetPath() == "" {
		return nil, invalid("target_path required")
	}
	if req.GetStagingTargetPath() == "" {
		return nil, invalid("staging_target_path required")
	}
	if req.GetVolumeCapability() == nil {
		return nil, invalid("volume_capability required")
	}
	if _, e := os.Lstat(req.GetTargetPath()); e == nil {
		return &csi.NodePublishVolumeResponse{}, nil
	}
	src := req.GetStagingTargetPath()
	if src == "" {
		return nil, status.Error(codes.InvalidArgument, "staging_target_path required")
	}
	if e := bindMount(src, req.GetTargetPath()); e != nil {
		return nil, errCode(fmt.Errorf("publish: %w", e))
	}
	return &csi.NodePublishVolumeResponse{}, nil
}

func (d *Driver) NodeUnpublishVolume(_ context.Context, req *csi.NodeUnpublishVolumeRequest) (*csi.NodeUnpublishVolumeResponse, error) {
	if req.GetVolumeId() == "" {
		return nil, invalid("volume_id required")
	}
	if req.GetTargetPath() == "" {
		return nil, invalid("target_path required")
	}
	unmount(req.GetTargetPath())
	return &csi.NodeUnpublishVolumeResponse{}, nil
}

func (d *Driver) NodeGetVolumeStats(_ context.Context, req *csi.NodeGetVolumeStatsRequest) (*csi.NodeGetVolumeStatsResponse, error) {
	if req.GetVolumeId() == "" {
		return nil, invalid("volume_id required")
	}
	if req.GetVolumePath() == "" {
		return nil, invalid("volume_path required")
	}
	if _, err := d.b.GetVolume(req.GetVolumeId()); err != nil {
		return nil, errCode(err)
	}
	if _, err := os.Stat(req.GetVolumePath()); err != nil {
		return nil, status.Error(codes.NotFound, err.Error())
	}
	return &csi.NodeGetVolumeStatsResponse{Usage: []*csi.VolumeUsage{
		{Unit: csi.VolumeUsage_BYTES, Available: 1 << 40, Total: 1 << 40, Used: 0},
		{Unit: csi.VolumeUsage_INODES, Available: 1000000, Total: 1000000, Used: 0},
	}}, nil
}

func (d *Driver) NodeExpandVolume(_ context.Context, req *csi.NodeExpandVolumeRequest) (*csi.NodeExpandVolumeResponse, error) {
	if req.GetVolumeId() == "" {
		return nil, invalid("volume_id required")
	}
	if req.GetVolumePath() == "" {
		return nil, invalid("volume_path required")
	}
	if _, err := os.Stat(req.GetVolumePath()); err != nil {
		return nil, status.Error(codes.NotFound, err.Error())
	}
	return &csi.NodeExpandVolumeResponse{}, nil
}
