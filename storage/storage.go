package storage

import (
	"context"
	"errors"
	"time"

	"agent-wayfinder/extractor"
	"agent-wayfinder/graph"
)

var (
	ErrWorkspaceNotFound    = errors.New("storage workspace not found")
	ErrGraphVersionNotFound = errors.New("storage graph version not found")
	ErrGraphVersionPruned   = errors.New("storage graph version pruned")
	ErrInvalidRequest       = errors.New("storage invalid request")
)

type GraphVersion uint64

type OpenSnapshotRequest struct {
	Workspace string
	// Version selects one published version. Nil selects the version current when opening begins.
	Version *GraphVersion
}

type Snapshot struct {
	Workspace   string
	Version     GraphVersion
	PublishedAt time.Time
}

type SnapshotOpener interface {
	OpenSnapshot(context.Context, OpenSnapshotRequest) (Snapshot, error)
}

type PublishRequest struct {
	Workspace                   string
	Update                      extractor.GraphUpdate
	WorkspaceFacts              graph.Facts
	ReplacedWorkspaceFactOwners []string
	Measurement                 func(PublishMeasurement)
	SQLiteWriteMeasurement      func(PublishMeasurement)
}

type PublishMeasurement struct {
	Name          string
	Duration      time.Duration
	NotApplicable bool
}

const (
	PublicationPreparationMeasurement = "publication_preparation"
	SQLiteWriteMeasurement            = "sqlite_write"
	CommitMeasurement                 = "commit"
	StagedTransactionMeasurement      = "staged_transaction"
)

type Publisher interface {
	// Publish either returns an error without exposing partial facts or returns a snapshot that exposes the complete update.
	Publish(context.Context, PublishRequest) (Snapshot, error)
}

type ResolverStageSource struct {
	ProjectID  string
	Language   string
	SourcePath string
}

type ResolverStager interface {
	Snapshot() Snapshot
	StageResolverSources(context.Context, []ResolverStageSource) error
	ResolverProjectionPageReader
	ResolverTargetReader
	ResolverPackagePageReader
}

type StagedPublisher interface {
	// PublishStaged runs stage and publication in one transaction. The callback returns the complete request to publish.
	PublishStaged(context.Context, PublishRequest, func(context.Context, ResolverStager) (PublishRequest, error)) (Snapshot, error)
}

type CommitRequest struct {
	Measurement            func(PublishMeasurement)
	SQLiteWriteMeasurement func(PublishMeasurement)
}

// ContributionSession accepts contribution writes and staged resolver reads before one final commit.
// A session either commits one complete graph version, or rolls back and exposes no partial facts.
type ContributionSession interface {
	ResolverProjectionPageReader
	ResolverTargetReader
	ResolverPackagePageReader
	StageSource(context.Context, string) error
	WriteContribution(context.Context, extractor.Contribution) error
	SealContributions(context.Context) error
	ReplaceContributionDependencies(context.Context, []extractor.Contribution) error
	WriteWorkspaceFacts(context.Context, graph.Facts) error
	SealWorkspaceFacts(context.Context) (FactCounts, error)
	Commit(context.Context, CommitRequest) (Snapshot, error)
	Rollback(context.Context) error
}

type ContributionSessionStore interface {
	BeginContributionSession(context.Context, string) (ContributionSession, error)
}

type PublishProgress struct {
	CompletedContributions int
	TotalContributions     int
	WrittenNodes           int
	TotalNodes             int
	WrittenEdges           int
	TotalEdges             int
}

type ProgressPublisher interface {
	PublishWithProgress(context.Context, PublishRequest, func(PublishProgress)) (Snapshot, error)
}

type AffectedSourcesRequest struct {
	Update extractor.GraphUpdate
}

type AffectedSourceFinder interface {
	AffectedSources(context.Context, Snapshot, AffectedSourcesRequest) ([]string, error)
}

type SourceContribution struct {
	SourcePath           string
	Metadata             extractor.Metadata
	Facts                graph.Facts
	UnresolvedReferences []extractor.UnresolvedReference
	SymbolReferences     []extractor.SymbolReference
	ExportedSurfaces     []extractor.ExportedSurface
	Dependencies         []extractor.Dependency
	Diagnostics          []extractor.Diagnostic
}

type SourceContributionReader interface {
	SourceContributions(context.Context, Snapshot) ([]SourceContribution, error)
}

type ResolverProjection = extractor.ResolverProjection

type ResolverProjectionReader interface {
	ResolverProjections(context.Context, Snapshot) ([]ResolverProjection, error)
}

type ResolverProjectionPageRequest struct {
	ProjectID       string
	Language        string
	AfterSourcePath string
	Limit           int
}

type ResolverProjectionPageReader interface {
	ResolverProjectionPage(context.Context, Snapshot, ResolverProjectionPageRequest) ([]ResolverProjection, error)
}

type ResolverTargetReader interface {
	ResolverTarget(context.Context, Snapshot, extractor.ResolverTargetRequest) (extractor.ResolverTarget, bool, error)
}

type ResolverPackagePageReader interface {
	ResolverPackagePage(context.Context, Snapshot, extractor.ResolverPackagePageRequest) ([]extractor.ResolverTarget, error)
}

type NodeLookupRequest struct {
	Text       string
	Kinds      []graph.NodeKind
	ProjectIDs []string
	Limit      int
}

type NodeMatch struct {
	Node  graph.Node
	Score float64
}

type LexicalSearchRequest struct {
	Text        string           `json:"text"`
	Phrases     []string         `json:"phrases,omitempty"`
	TokenGroups [][]string       `json:"tokenGroups,omitempty"`
	Kinds       []graph.NodeKind `json:"kinds,omitempty"`
	ProjectIDs  []string         `json:"projectIds,omitempty"`
	Limit       int              `json:"limit"`
}

type LexicalMatch struct {
	Node          graph.Node `json:"node"`
	Score         float64    `json:"score"`
	MatchedFields []string   `json:"matchedFields"`
}

type LexicalSearcher interface {
	SearchNodes(context.Context, Snapshot, LexicalSearchRequest) ([]LexicalMatch, error)
}

type LexicalIndexRebuilder interface {
	RebuildLexicalIndex(context.Context, string) error
}

type NodeLookup interface {
	LookupNodes(context.Context, Snapshot, NodeLookupRequest) ([]NodeMatch, error)
}

// ExactNodeLookup finds nodes whose ID, qualified name, or source path exactly matches the identifier.
type ExactNodeLookup interface {
	LookupExactNodes(context.Context, Snapshot, string) ([]NodeMatch, error)
}

type TraversalDirection string

const (
	TraverseIncoming TraversalDirection = "incoming"
	TraverseOutgoing TraversalDirection = "outgoing"
	TraverseBoth     TraversalDirection = "both"
)

type TraversalRequest struct {
	StartNodeIDs []string
	ProjectIDs   []string
	Direction    TraversalDirection
	Relations    []graph.RelationKind
	MaxDepth     int
	MaxNodes     int
}

type TruncationReason string

const (
	TruncatedByDepthLimit TruncationReason = "depth_limit"
	TruncatedByNodeLimit  TruncationReason = "node_limit"
)

type TraversalResult struct {
	Facts             graph.Facts
	TruncationReasons []TruncationReason
	ScopeBoundary     *graph.Node
}

func (result TraversalResult) Truncated() bool {
	return len(result.TruncationReasons) > 0
}

type Traverser interface {
	Traverse(context.Context, Snapshot, TraversalRequest) (TraversalResult, error)
}

type ExplainRequest struct {
	NodeID string
}

type Explanation struct {
	Node            graph.Node
	SupportingFacts graph.Facts
}

type Explainer interface {
	Explain(context.Context, Snapshot, ExplainRequest) (Explanation, error)
}

type ExportRequest struct {
	NodeKinds []graph.NodeKind
	Relations []graph.RelationKind
}

func (request ExportRequest) IsUnfiltered() bool {
	return len(request.NodeKinds) == 0 && len(request.Relations) == 0
}

type ExportSink interface {
	WriteNode(graph.Node) error
	WriteEdge(graph.Edge) error
}

type Exporter interface {
	Export(context.Context, Snapshot, ExportRequest, ExportSink) error
}

type FactCounts struct {
	Nodes int
	Edges int
}

type FactCounter interface {
	FactCounts(context.Context, Snapshot) (FactCounts, error)
}

type RollbackRequest struct {
	Workspace string
	Version   GraphVersion
}

type Rollbacker interface {
	Rollback(context.Context, RollbackRequest) (Snapshot, error)
}

type PruneRequest struct {
	Workspace     string
	BeforeVersion GraphVersion
}

type PruneResult struct {
	PrunedVersions int
}

type Pruner interface {
	Prune(context.Context, PruneRequest) (PruneResult, error)
}

type Store interface {
	SnapshotOpener
	Publisher
	AffectedSourceFinder
	SourceContributionReader
	ResolverProjectionReader
	ResolverProjectionPageReader
	ResolverTargetReader
	ResolverPackagePageReader
	NodeLookup
	LexicalSearcher
	Traverser
	Explainer
	Exporter
	FactCounter
	Rollbacker
	Pruner
}
