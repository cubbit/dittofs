package runtime

import (
	"context"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/metadata/store/badger/badgertest"
)

func TestNew(t *testing.T) {
	rt := New(nil)
	setJournalRoot(t, rt)

	if rt == nil {
		t.Fatal("expected non-nil runtime")
	}

	if rt.storesSvc == nil {
		t.Error("expected stores service to be initialized")
	}
	if rt.sharesSvc == nil {
		t.Error("expected shares service to be initialized")
	}
	if rt.lifecycleSvc == nil {
		t.Error("expected lifecycle service to be initialized")
	}
}

func TestSetShutdownTimeout(t *testing.T) {
	rt := New(nil)
	setJournalRoot(t, rt)

	t.Run("set custom timeout does not panic", func(t *testing.T) {
		rt.SetShutdownTimeout(60 * time.Second)
		// Timeout is delegated to the lifecycle sub-service;
		// we verify it doesn't panic.
	})

	t.Run("zero uses default does not panic", func(t *testing.T) {
		rt.SetShutdownTimeout(0)
		// Zero is normalized to DefaultShutdownTimeout in sub-services.
	})
}

func TestRegisterMetadataStore(t *testing.T) {
	rt := New(nil)
	setJournalRoot(t, rt)
	metaStore := badgertest.NewInMemory(t)

	t.Run("register valid store", func(t *testing.T) {
		err := rt.RegisterMetadataStore("test-store", metaStore)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("duplicate registration fails", func(t *testing.T) {
		err := rt.RegisterMetadataStore("test-store", metaStore)
		if err == nil {
			t.Fatal("expected error for duplicate registration")
		}
	})

	t.Run("nil store fails", func(t *testing.T) {
		err := rt.RegisterMetadataStore("nil-store", nil)
		if err == nil {
			t.Fatal("expected error for nil store")
		}
	})

	t.Run("empty name fails", func(t *testing.T) {
		err := rt.RegisterMetadataStore("", metaStore)
		if err == nil {
			t.Fatal("expected error for empty name")
		}
	})
}

func TestGetMetadataStore(t *testing.T) {
	rt := New(nil)
	setJournalRoot(t, rt)
	metaStore := badgertest.NewInMemory(t)
	if err := rt.RegisterMetadataStore("test-store", metaStore); err != nil {
		t.Fatalf("RegisterMetadataStore failed: %v", err)
	}

	t.Run("existing store", func(t *testing.T) {
		store, err := rt.GetMetadataStore("test-store")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if store != metaStore {
			t.Error("returned store doesn't match registered store")
		}
	})

	t.Run("non-existing store", func(t *testing.T) {
		_, err := rt.GetMetadataStore("not-found")
		if err == nil {
			t.Fatal("expected error for non-existing store")
		}
	})
}

func TestListMetadataStores(t *testing.T) {
	rt := New(nil)
	setJournalRoot(t, rt)

	t.Run("empty list", func(t *testing.T) {
		names := rt.ListMetadataStores()
		if len(names) != 0 {
			t.Errorf("expected empty list, got %d items", len(names))
		}
	})

	t.Run("with registered stores", func(t *testing.T) {
		if err := rt.RegisterMetadataStore("store1", badgertest.NewInMemory(t)); err != nil {
			t.Fatalf("RegisterMetadataStore failed: %v", err)
		}
		if err := rt.RegisterMetadataStore("store2", badgertest.NewInMemory(t)); err != nil {
			t.Fatalf("RegisterMetadataStore failed: %v", err)
		}

		names := rt.ListMetadataStores()
		if len(names) != 2 {
			t.Errorf("expected 2 stores, got %d", len(names))
		}
	})
}

func TestCountMetadataStores(t *testing.T) {
	rt := New(nil)
	setJournalRoot(t, rt)

	if rt.CountMetadataStores() != 0 {
		t.Errorf("expected 0, got %d", rt.CountMetadataStores())
	}

	if err := rt.RegisterMetadataStore("store1", badgertest.NewInMemory(t)); err != nil {
		t.Fatalf("RegisterMetadataStore failed: %v", err)
	}
	if rt.CountMetadataStores() != 1 {
		t.Errorf("expected 1, got %d", rt.CountMetadataStores())
	}
}

func TestShareOperations(t *testing.T) {
	rt, bsID := newRuntimeWithBlockStore(t)
	ctx := context.Background()
	metaStore := badgertest.NewInMemory(t)
	if err := rt.RegisterMetadataStore("test-meta", metaStore); err != nil {
		t.Fatalf("RegisterMetadataStore failed: %v", err)
	}

	t.Run("add share", func(t *testing.T) {
		config := &ShareConfig{
			Name:          "/export",
			MetadataStore: "test-meta",
			BlockStoreID:  bsID,
		}

		err := rt.AddShare(ctx, config)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		if !rt.ShareExists("/export") {
			t.Error("share should exist after adding")
		}
	})

	t.Run("add share with empty name fails", func(t *testing.T) {
		config := &ShareConfig{
			Name:          "",
			MetadataStore: "test-meta",
			BlockStoreID:  bsID,
		}

		err := rt.AddShare(ctx, config)
		if err == nil {
			t.Fatal("expected error for empty name")
		}
	})

	t.Run("add duplicate share fails", func(t *testing.T) {
		config := &ShareConfig{
			Name:          "/export",
			MetadataStore: "test-meta",
			BlockStoreID:  bsID,
		}

		err := rt.AddShare(ctx, config)
		if err == nil {
			t.Fatal("expected error for duplicate share")
		}
	})

	t.Run("add share with non-existing metadata store fails", func(t *testing.T) {
		config := &ShareConfig{
			Name:          "/new-share",
			MetadataStore: "non-existing",
			BlockStoreID:  bsID,
		}

		err := rt.AddShare(ctx, config)
		if err == nil {
			t.Fatal("expected error for non-existing metadata store")
		}
	})

	t.Run("get share", func(t *testing.T) {
		share, err := rt.GetShare("/export")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if share.Name != "/export" {
			t.Errorf("expected name '/export', got %q", share.Name)
		}
	})

	t.Run("get non-existing share", func(t *testing.T) {
		_, err := rt.GetShare("/not-found")
		if err == nil {
			t.Fatal("expected error for non-existing share")
		}
	})

	t.Run("list shares", func(t *testing.T) {
		names := rt.ListShares()
		if len(names) != 1 {
			t.Errorf("expected 1 share, got %d", len(names))
		}
	})

	t.Run("count shares", func(t *testing.T) {
		if rt.CountShares() != 1 {
			t.Errorf("expected 1, got %d", rt.CountShares())
		}
	})

	t.Run("get root handle", func(t *testing.T) {
		handle, err := rt.GetRootHandle("/export")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if handle == nil {
			t.Error("expected non-nil root handle")
		}
	})

	t.Run("get root handle for non-existing share", func(t *testing.T) {
		_, err := rt.GetRootHandle("/not-found")
		if err == nil {
			t.Fatal("expected error for non-existing share")
		}
	})

	t.Run("update share", func(t *testing.T) {
		readOnly := true
		perm := "read"
		err := rt.UpdateShare("/export", &readOnly, &perm, nil, nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		share, _ := rt.GetShare("/export")
		if !share.ReadOnly {
			t.Error("expected ReadOnly to be true")
		}
		if share.DefaultPermission != "read" {
			t.Errorf("expected permission 'read', got %q", share.DefaultPermission)
		}
	})

	t.Run("update non-existing share fails", func(t *testing.T) {
		readOnly := true
		err := rt.UpdateShare("/not-found", &readOnly, nil, nil, nil)
		if err == nil {
			t.Fatal("expected error for non-existing share")
		}
	})

	t.Run("remove share", func(t *testing.T) {
		err := rt.RemoveShare("/export")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		if rt.ShareExists("/export") {
			t.Error("share should not exist after removal")
		}
	})

	t.Run("remove non-existing share fails", func(t *testing.T) {
		err := rt.RemoveShare("/not-found")
		if err == nil {
			t.Fatal("expected error for non-existing share")
		}
	})
}

func TestGetMetadataStoreForShare(t *testing.T) {
	rt, bsID := newRuntimeWithBlockStore(t)
	ctx := context.Background()
	metaStore := badgertest.NewInMemory(t)
	if err := rt.RegisterMetadataStore("test-meta", metaStore); err != nil {
		t.Fatalf("RegisterMetadataStore failed: %v", err)
	}

	config := &ShareConfig{
		Name:          "/export",
		MetadataStore: "test-meta",
		BlockStoreID:  bsID,
	}
	if err := rt.AddShare(ctx, config); err != nil {
		t.Fatalf("AddShare failed: %v", err)
	}

	t.Run("existing share", func(t *testing.T) {
		store, err := rt.GetMetadataStoreForShare("/export")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if store != metaStore {
			t.Error("returned store doesn't match expected store")
		}
	})

	t.Run("non-existing share", func(t *testing.T) {
		_, err := rt.GetMetadataStoreForShare("/not-found")
		if err == nil {
			t.Fatal("expected error for non-existing share")
		}
	})
}

func TestGetServices(t *testing.T) {
	rt := New(nil)
	setJournalRoot(t, rt)

	t.Run("get metadata service", func(t *testing.T) {
		svc := rt.GetMetadataService()
		if svc == nil {
			t.Error("expected non-nil metadata service")
		}
	})
}

func TestCloseMetadataStores(t *testing.T) {
	rt := New(nil)
	setJournalRoot(t, rt)

	// Register a memory store (which implements io.Closer via its Close method if any)
	metaStore := badgertest.NewInMemory(t)
	if err := rt.RegisterMetadataStore("test-store", metaStore); err != nil {
		t.Fatalf("RegisterMetadataStore failed: %v", err)
	}

	// CloseMetadataStores should not panic and should handle stores gracefully
	rt.CloseMetadataStores()

	// After close, stores are still in the map (we don't remove them)
	if rt.CountMetadataStores() != 1 {
		t.Error("stores should still be registered after close")
	}
}

func TestStore(t *testing.T) {
	rt := New(nil)
	setJournalRoot(t, rt)

	t.Run("nil store", func(t *testing.T) {
		if rt.Store() != nil {
			t.Error("expected nil store")
		}
	})
}
