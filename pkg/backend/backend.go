// Package backend abstracts the volume store. LocalFS is implemented;
// cloud file services are stubs behind the same interface.
package backend

import (
	"errors"
	"fmt"
)

var (
	ErrNotFound       = errors.New("not found")
	ErrAlreadyExists  = errors.New("already exists with different parameters")
	ErrQuotaExceeded  = errors.New("quota exceeded")
	ErrNotImplemented = errors.New("backend not implemented")
)

type Volume struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	SizeBytes  int64  `json:"size_bytes"`
	Path       string `json:"path"` // host path / NFS export path
	SourceSnap string `json:"source_snapshot,omitempty"`
	CreatedAt  int64  `json:"created_at"` // unix nano
}

type Snapshot struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	SourceVol  string `json:"source_volume_id"`
	SizeBytes  int64  `json:"size_bytes"`
	Path       string `json:"path"`
	CreatedAt  int64  `json:"created_at"`
	ReadyToUse bool   `json:"ready_to_use"`
}

type Backend interface {
	CreateVolume(name string, size int64, sourceSnapshotID string) (*Volume, error)
	GetVolume(id string) (*Volume, error)
	GetVolumeByName(name string) (*Volume, error)
	ListVolumes() []*Volume
	DeleteVolume(id string) error
	ExpandVolume(id string, size int64) (*Volume, error)
	CreateSnapshot(name, sourceVolumeID string) (*Snapshot, error)
	GetSnapshot(id string) (*Snapshot, error)
	GetSnapshotByName(name string) (*Snapshot, error)
	ListSnapshots() []*Snapshot
	DeleteSnapshot(id string) error
	TotalBytes() int64
}

// New returns a backend by name: local (default), fsx, azurefiles, gcpfilestore.
// ponytail: cloud backends are credential/SDK stubs; wire the vendor SDK in
// each stub when deploying against that cloud.
func New(kind, root string) (Backend, error) {
	switch kind {
	case "", "local":
		return NewLocal(root)
	case "fsx":
		return stub{kind: "AWS FSx for NetApp ONTAP/Lustre"}, nil
	case "azurefiles":
		return stub{kind: "Azure Files"}, nil
	case "gcpfilestore":
		return stub{kind: "GCP Filestore"}, nil
	default:
		return nil, fmt.Errorf("unknown backend %q", kind)
	}
}

// stub stands in for a cloud file service until its SDK is wired up.
type stub struct{ kind string }

func (s stub) err() error                                          { return fmt.Errorf("%s: %w", s.kind, ErrNotImplemented) }
func (s stub) CreateVolume(string, int64, string) (*Volume, error) { return nil, s.err() }
func (s stub) GetVolume(string) (*Volume, error)                   { return nil, s.err() }
func (s stub) GetVolumeByName(string) (*Volume, error)             { return nil, s.err() }
func (s stub) ListVolumes() []*Volume                              { return nil }
func (s stub) DeleteVolume(string) error                           { return s.err() }
func (s stub) ExpandVolume(string, int64) (*Volume, error)         { return nil, s.err() }
func (s stub) CreateSnapshot(string, string) (*Snapshot, error)    { return nil, s.err() }
func (s stub) GetSnapshot(string) (*Snapshot, error)               { return nil, s.err() }
func (s stub) GetSnapshotByName(string) (*Snapshot, error)         { return nil, s.err() }
func (s stub) ListSnapshots() []*Snapshot                          { return nil }
func (s stub) DeleteSnapshot(string) error                         { return s.err() }
func (s stub) TotalBytes() int64                                   { return 0 }
