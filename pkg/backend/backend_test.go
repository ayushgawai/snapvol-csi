package backend

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestLocal(t *testing.T) {
	cases := []struct {
		name string
		fn   func(t *testing.T, b *Local)
	}{
		{"create is idempotent", func(t *testing.T, b *Local) {
			v1, err := b.CreateVolume("v1", 100, "")
			if err != nil {
				t.Fatal(err)
			}
			v2, err := b.CreateVolume("v1", 100, "")
			if err != nil || v1.ID != v2.ID {
				t.Fatalf("re-create: %v %v", v2, err)
			}
			if _, err := b.CreateVolume("v1", 200, ""); !errors.Is(err, ErrAlreadyExists) {
				t.Fatalf("want AlreadyExists, got %v", err)
			}
		}},
		{"delete is idempotent", func(t *testing.T, b *Local) {
			if err := b.DeleteVolume("nope"); err != nil {
				t.Fatal(err)
			}
		}},
		{"snapshot then clone restores data", func(t *testing.T, b *Local) {
			v, _ := b.CreateVolume("src", 100, "")
			if err := os.WriteFile(filepath.Join(v.Path, "hello.txt"), []byte("snapvol"), 0o644); err != nil {
				t.Fatal(err)
			}
			s, err := b.CreateSnapshot("s1", v.ID)
			if err != nil {
				t.Fatal(err)
			}
			_ = os.WriteFile(filepath.Join(v.Path, "hello.txt"), []byte("changed"), 0o644)
			clone, err := b.CreateVolume("clone", 100, s.ID)
			if err != nil {
				t.Fatal(err)
			}
			got, err := os.ReadFile(filepath.Join(clone.Path, "hello.txt"))
			if err != nil || string(got) != "snapvol" {
				t.Fatalf("clone=%q err=%v", got, err)
			}
		}},
		{"snapshot of missing volume", func(t *testing.T, b *Local) {
			if _, err := b.CreateSnapshot("s", "ghost"); !errors.Is(err, ErrNotFound) {
				t.Fatalf("want NotFound, got %v", err)
			}
		}},
		{"expand rejects shrink", func(t *testing.T, b *Local) {
			v, _ := b.CreateVolume("v", 100, "")
			if _, err := b.ExpandVolume(v.ID, 50); err == nil {
				t.Fatal("want shrink error")
			}
			v2, err := b.ExpandVolume(v.ID, 200)
			if err != nil || v2.SizeBytes != 200 {
				t.Fatalf("expand: %v %v", v2, err)
			}
		}},
		{"state survives reload", func(t *testing.T, b *Local) {
			if _, err := b.CreateVolume("persist", 42, ""); err != nil {
				t.Fatal(err)
			}
			reloaded, err := NewLocal(b.root)
			if err != nil {
				t.Fatal(err)
			}
			v, err := reloaded.GetVolume("persist")
			if err != nil || v.SizeBytes != 42 {
				t.Fatalf("reload: %v %v", v, err)
			}
		}},
		{"stub backends are unimplemented", func(t *testing.T, b *Local) {
			for _, kind := range []string{"fsx", "azurefiles", "gcpfilestore"} {
				sb, err := New(kind, "")
				if err != nil {
					t.Fatal(err)
				}
				if _, err := sb.CreateVolume("x", 1, ""); !errors.Is(err, ErrNotImplemented) {
					t.Fatalf("%s: want NotImplemented, got %v", kind, err)
				}
			}
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			b, err := NewLocal(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			c.fn(t, b)
		})
	}
}
