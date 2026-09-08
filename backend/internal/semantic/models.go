package semantic

import "encoding/json"

// Slot is one semantic argument of a claim. Exactly one of EntityID or Literal
// must be set. Role is namespaced by convention (for example subject, location,
// occupation, cause, effect).
type Slot struct {
	Role        string
	Ordinal     int
	EntityID    *int64
	Literal     json.RawMessage
	LiteralKind string
}

// ClaimDraft is the canonical, source-independent shape used to compute claim
// identity before persistence.
type ClaimDraft struct {
	SpaceID       int64
	CanonicalText string
	ClaimType     string
	Predicate     string
	ValidFrom     string
	ValidTo       string
	TimePrecision string
	TimeNote      string
	Qualifiers    json.RawMessage
	Slots         []Slot
	CreatedBy     *int64
}

// SourceDraft identifies one logical source. PageID, SpaceFileID and URI mirror
// sem_sources. The caller is responsible for choosing the correct Kind.
type SourceDraft struct {
	SpaceID     int64
	Kind        string
	PageID      *int64
	SpaceFileID *int64
	URI         *string
	Title       string
	Metadata    json.RawMessage
	CreatedBy   *int64
}

type SourceVersionDraft struct {
	ContentHash string
	ObservedAt  string
	MIME        *string
	ByteSize    *int64
	Metadata    json.RawMessage
}

type SourceRegionDraft struct {
	LocatorKind string
	Locator     json.RawMessage
	Excerpt     string
	ExcerptHash string
}

type ClaimInstanceDraft struct {
	OriginalText          string
	ProposedCanonicalText string
	Context               string
	Stance                string
	ExtractionConfidence  *float64
	CreatedBy             string
}

// ManualIngest is the Phase-0 transaction boundary: one immutable source
// version/region and one canonical claim instance are committed atomically.
type ManualIngest struct {
	Source   SourceDraft
	Version  SourceVersionDraft
	Region   SourceRegionDraft
	Claim    ClaimDraft
	Instance ClaimInstanceDraft
}

type IngestResult struct {
	SourceID        int64
	SourceVersionID int64
	SourceRegionID  int64
	ClaimID         int64
	ClaimInstanceID int64
	Fingerprint     string
}

// ClaimTrace is the minimum provenance chain needed by a projection cell to
// jump back from a claim to the exact immutable source region.
type ClaimTrace struct {
	ClaimID         int64
	CanonicalText   string
	Predicate       string
	Fingerprint     string
	InstanceID      int64
	Stance          string
	OriginalText    string
	SourceRegionID  int64
	LocatorKind     string
	Locator         json.RawMessage
	Excerpt         string
	SourceVersionID int64
	ContentHash     string
	SourceID        int64
	SourceKind      string
	SourceTitle     string
	SourceURI       *string
}
