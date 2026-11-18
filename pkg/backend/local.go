package backend

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Local implements Backend on a local directory (dev mode / single node).
// Each volume is a directory; a snapshot is a full copy of it.
// ponytail: full-copy snapshots, O(volume size). Upgrade path: reflink
// (FICLONE) or overlay/symlink trees on filesystems that support them.
type Local struct {
	mu    sync.Mutex
	root  string
	vols  map[string]*Volume
	snaps map[string]*Snapshot
}

func NewLocal(root string) (*Local, error) {
	l := &Local{root: root, vols: map[string]*Volume{}, snaps: map[string]*Snapshot{}}
	for _, d := range []string{"volumes", "snapshots"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			return nil, err
		}
	}
	if err := l.load(); err != nil {
		return nil, err
	}
	return l, nil
}

func idFor(name string) string { return name } // name is the id: CSI names are unique & idempotency falls out

func (l *Local) metaPath(kind, id string) string {
	return filepath.Join(l.root, kind, id+".json")
}

func (l *Local) load() error {
	for kind, loadOne := range map[string]func([]byte) error{
		"volumes": func(b []byte) error {
			var v Volume
			e := json.Unmarshal(b, &v)
			if e == nil {
				l.vols[v.ID] = &v
			}
			return e
		},
		"snapshots": func(b []byte) error {
			var s Snapshot
			e := json.Unmarshal(b, &s)
			if e == nil {
				l.snaps[s.ID] = &s
			}
			return e
		},
	} {
		entries, err := os.ReadDir(filepath.Join(l.root, kind))
		if err != nil {
			return err
		}
		for _, e := range entries {
			if strings.HasSuffix(e.Name(), ".json") {
				b, err := os.ReadFile(filepath.Join(l.root, kind, e.Name()))
				if err != nil {
					return err
				}
				if err := loadOne(b); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func (l *Local) save(kind string, v any, id string) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return os.WriteFile(l.metaPath(kind, id), b, 0o644)
}

func (l *Local) CreateVolume(name string, size int64, sourceSnapshotID string) (*Volume, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if size <= 0 {
		size = 1 << 30
	}
	if existing, ok := l.vols[idFor(name)]; ok {
		if existing.SizeBytes != size || existing.SourceSnap != sourceSnapshotID {
			return nil, ErrAlreadyExists
		}
		cp := *existing
		return &cp, nil
	}
	path := filepath.Join(l.root, "volumes", idFor(name))
	if sourceSnapshotID != "" {
		snap, ok := l.snaps[sourceSnapshotID]
		if !ok {
			return nil, fmt.Errorf("snapshot %s: %w", sourceSnapshotID, ErrNotFound)
		}
		if err := copyTree(snap.Path, path); err != nil {
			return nil, err
		}
	} else if err := os.MkdirAll(path, 0o755); err != nil {
		return nil, err
	}
	v := &Volume{ID: idFor(name), Name: name, SizeBytes: size, Path: path, SourceSnap: sourceSnapshotID, CreatedAt: time.Now().UnixNano()}
	if err := l.save("volumes", v, v.ID); err != nil {
		return nil, err
	}
	l.vols[v.ID] = v
	cp := *v
	return &cp, nil
}

func (l *Local) GetVolume(id string) (*Volume, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	v, ok := l.vols[id]
	if !ok {
		return nil, ErrNotFound
	}
	cp := *v
	return &cp, nil
}

func (l *Local) GetVolumeByName(name string) (*Volume, error) { return l.GetVolume(idFor(name)) }

func (l *Local) ListVolumes() []*Volume {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]*Volume, 0, len(l.vols))
	for _, v := range l.vols {
		cp := *v
		out = append(out, &cp)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func (l *Local) DeleteVolume(id string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	v, ok := l.vols[id]
	if !ok {
		return nil // idempotent
	}
	if err := os.RemoveAll(v.Path); err != nil {
		return err
	}
	_ = os.Remove(l.metaPath("volumes", id))
	delete(l.vols, id)
	return nil
}

func (l *Local) ExpandVolume(id string, size int64) (*Volume, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	v, ok := l.vols[id]
	if !ok {
		return nil, ErrNotFound
	}
	if size < v.SizeBytes {
		return nil, fmt.Errorf("cannot shrink volume from %d to %d", v.SizeBytes, size)
	}
	v.SizeBytes = size // local dirs have no real capacity; recorded size is the quota accounting unit
	if err := l.save("volumes", v, v.ID); err != nil {
		return nil, err
	}
	cp := *v
	return &cp, nil
}

func (l *Local) CreateSnapshot(name, srcID string) (*Snapshot, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if existing, ok := l.snaps[idFor(name)]; ok {
		if existing.SourceVol != srcID {
			return nil, ErrAlreadyExists
		}
		cp := *existing
		return &cp, nil
	}
	src, ok := l.vols[srcID]
	if !ok {
		return nil, fmt.Errorf("volume %s: %w", srcID, ErrNotFound)
	}
	path := filepath.Join(l.root, "snapshots", idFor(name))
	if err := copyTree(src.Path, path); err != nil {
		return nil, err
	}
	s := &Snapshot{ID: idFor(name), Name: name, SourceVol: srcID, SizeBytes: src.SizeBytes, Path: path, CreatedAt: time.Now().UnixNano(), ReadyToUse: true}
	if err := l.save("snapshots", s, s.ID); err != nil {
		return nil, err
	}
	l.snaps[s.ID] = s
	cp := *s
	return &cp, nil
}

func (l *Local) GetSnapshot(id string) (*Snapshot, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	s, ok := l.snaps[id]
	if !ok {
		return nil, ErrNotFound
	}
	cp := *s
	return &cp, nil
}

func (l *Local) GetSnapshotByName(name string) (*Snapshot, error) { return l.GetSnapshot(idFor(name)) }
func (l *Local) ListSnapshots() []*Snapshot {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]*Snapshot, 0, len(l.snaps))
	for _, s := range l.snaps {
		cp := *s
		out = append(out, &cp)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func (l *Local) DeleteSnapshot(id string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	s, ok := l.snaps[id]
	if !ok {
		return nil
	}
	if err := os.RemoveAll(s.Path); err != nil {
		return err
	}
	_ = os.Remove(l.metaPath("snapshots", id))
	delete(l.snaps, id)
	return nil
}

func (l *Local) TotalBytes() int64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	var n int64
	for _, v := range l.vols {
		n += v.SizeBytes
	}
	return n
}

func copyTree(src, dst string) error {
	return filepath.Walk(src, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(target, info.Mode())
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		in, err := os.Open(p)
		if err != nil {
			return err
		}
		defer in.Close()
		out, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, info.Mode())
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(out, in)
		closeErr := out.Close()
		if copyErr != nil {
			return copyErr
		}
		return closeErr
	})
}
