package index

import (
	"context"
	"fmt"
	"path/filepath"

	"agent-wayfinder/extractor"
	"agent-wayfinder/storage"
)

type CatalogRequest struct {
	Root                string
	ConfiguredRoots     []string
	Snapshot            storage.Snapshot
	PreviousSnapshot    storage.Snapshot
	ChangedPaths        []string
	CatalogWriteOptions CatalogWriteOptions
	Progress            func(CatalogProgress)
}

type CatalogProgress struct {
	CompletedUnits int
	TotalUnits     int
}

type CatalogResult struct {
	Snapshot storage.Snapshot
	Units    int
	Write    CatalogWriteResult
}

type catalogStore interface {
	storage.SnapshotOpener
	storage.SourceContributionReader
	storage.CatalogUnitCoverageReader
	storage.CatalogWriter
	storage.CatalogCopier
}

func Catalog(ctx context.Context, store catalogStore, request CatalogRequest) (CatalogResult, error) {
	if request.Root == "" {
		return CatalogResult{}, fmt.Errorf("catalog workspace: root is required")
	}
	if store == nil {
		return CatalogResult{}, fmt.Errorf("catalog workspace: catalog store is required")
	}

	root, err := filepath.Abs(request.Root)
	if err != nil {
		return CatalogResult{}, fmt.Errorf("catalog workspace: resolve root: %w", err)
	}
	snapshot := request.Snapshot
	if snapshot.Version == 0 {
		var err error
		snapshot, err = store.OpenSnapshot(ctx, storage.OpenSnapshotRequest{Workspace: root})
		if err != nil {
			return CatalogResult{}, fmt.Errorf("catalog workspace: open current snapshot: %w", err)
		}
	} else if snapshot.Workspace != root {
		return CatalogResult{}, fmt.Errorf("catalog workspace: snapshot workspace does not match root")
	}
	covered, err := store.CatalogUnitsCovered(ctx, snapshot)
	if err != nil {
		return CatalogResult{}, fmt.Errorf("catalog workspace: read snapshot catalog-unit coverage: %w", err)
	}
	if !covered {
		return CatalogResult{}, fmt.Errorf("catalog workspace: catalog units require a full re-index")
	}
	if request.PreviousSnapshot.Version != 0 {
		if err := store.CopyCatalog(ctx, request.PreviousSnapshot, snapshot); err != nil {
			return CatalogResult{}, fmt.Errorf("catalog workspace: copy prior catalog: %w", err)
		}
	}
	contributions, err := store.SourceContributions(ctx, snapshot)
	if err != nil {
		return CatalogResult{}, fmt.Errorf("catalog workspace: read snapshot sources: %w", err)
	}
	if request.PreviousSnapshot.Version != 0 && len(request.ChangedPaths) > 0 {
		changed := make(map[string]struct{}, len(request.ChangedPaths))
		for _, changedPath := range request.ChangedPaths {
			normalizedPath, err := normalizeChangedPath(root, changedPath)
			if err != nil {
				return CatalogResult{}, fmt.Errorf("catalog workspace: %w", err)
			}
			changed[normalizedPath] = struct{}{}
		}
		filtered := make([]storage.SourceContribution, 0, len(changed))
		for _, contribution := range contributions {
			if _, found := changed[contribution.SourcePath]; found {
				filtered = append(filtered, contribution)
			}
		}
		contributions = filtered
	}
	units := make([]extractor.CatalogUnit, 0)
	for _, contribution := range contributions {
		units = append(units, contribution.CatalogUnits...)
	}
	unitCount := len(units)
	reportCatalogProgress(request.Progress, CatalogProgress{TotalUnits: unitCount})
	writeResult, err := writeCatalogUnits(ctx, store, snapshot, units, request.CatalogWriteOptions)
	if err != nil {
		return CatalogResult{}, fmt.Errorf("catalog workspace: %w", err)
	}
	reportCatalogProgress(request.Progress, CatalogProgress{CompletedUnits: unitCount, TotalUnits: unitCount})
	return CatalogResult{Snapshot: snapshot, Units: unitCount, Write: writeResult}, nil
}

func reportCatalogProgress(callback func(CatalogProgress), progress CatalogProgress) {
	if callback != nil {
		callback(progress)
	}
}
