package semantic

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
)

var (
	ErrStalePreview        = errors.New("semantic: stale preview")
	ErrInvalidSelection    = errors.New("semantic: invalid selection")
	ErrIdempotencyConflict = errors.New("semantic: idempotency conflict")
	ErrCommitInProgress    = errors.New("semantic: commit in progress")
)

const semanticCommitTool = "semantic.commit"

type EntityChoice struct {
	ExistingEntityID *int64 `json:"existing_entity_id,omitempty"`
	CreateNew        bool   `json:"create_new,omitempty"`
}

type CommitInput struct {
	PreviewToken   string                  `json:"preview_token"`
	Candidates     CandidateSet            `json:"candidates"`
	AcceptedKeys   []string                `json:"accepted_keys"`
	EntityChoices  map[string]EntityChoice `json:"entity_choices"`
	IdempotencyKey string                  `json:"idempotency_key"`
}

type CommitResult struct {
	SpaceID         int64            `json:"space_id"`
	PageID          int64            `json:"page_id"`
	SourceID        int64            `json:"source_id"`
	SourceVersionID int64            `json:"source_version_id"`
	Entities        map[string]int64 `json:"entities,omitempty"`
	Claims          map[string]int64 `json:"claims,omitempty"`
	Regions         map[string]int64 `json:"regions,omitempty"`
	Relations       map[string]int64 `json:"relations,omitempty"`
	Gaps            map[string]int64 `json:"gaps,omitempty"`
	Replayed        bool             `json:"replayed,omitempty"`
}

type CommitService struct {
	db     *sql.DB
	signer *PreviewSigner
}

func NewCommitService(db *sql.DB, signer *PreviewSigner) (*CommitService, error) {
	if db == nil {
		return nil, errors.New("semantic commit: nil database")
	}
	if signer == nil {
		return nil, errors.New("semantic commit: nil signer")
	}
	return &CommitService{db: db, signer: signer}, nil
}

// CommitPreview persists only an explicitly accepted, signed preview. Claims are
// selectable semantic roots; their signed instances and source regions are
// provenance dependencies and are committed with the accepted claim. Entity
// identity is never guessed: every referenced candidate entity needs an explicit
// create-new or reuse-existing choice.
func (s *CommitService) CommitPreview(ctx context.Context, userID int64, in CommitInput) (CommitResult, error) {
	if s == nil || s.db == nil || s.signer == nil {
		return CommitResult{}, errors.New("semantic commit: invalid service")
	}
	claims, err := s.signer.Verify(strings.TrimSpace(in.PreviewToken))
	if err != nil {
		return CommitResult{}, err
	}
	if err := validateCandidateSet(in.Candidates); err != nil {
		return CommitResult{}, err
	}
	candidateHash, err := CandidatePayloadHash(in.Candidates)
	if err != nil {
		return CommitResult{}, err
	}
	if !constantStringEqual(candidateHash, claims.CandidatePayloadHash) {
		return CommitResult{}, ErrInvalidPreviewToken
	}
	selection, err := prepareCommitSelection(in.Candidates, in.AcceptedKeys, in.EntityChoices)
	if err != nil {
		return CommitResult{}, err
	}
	if strings.TrimSpace(in.IdempotencyKey) == "" {
		return CommitResult{}, fmt.Errorf("%w: idempotency_key is required", ErrInvalidSelection)
	}

	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return CommitResult{}, fmt.Errorf("semantic commit: begin: %w", err)
	}
	defer tx.Rollback()

	if err := requireSpaceAccess(ctx, tx, userID, claims.SpaceID, true); err != nil {
		return CommitResult{}, err
	}
	if replay, ok, err := claimCommitIdempotency(ctx, tx, userID, strings.TrimSpace(in.IdempotencyKey)); err != nil {
		return CommitResult{}, err
	} else if ok {
		replay.Replayed = true
		return replay, nil
	}

	snapshot, err := readPageSnapshotTx(ctx, tx, claims.SpaceID, claims.PageID)
	if err != nil {
		return CommitResult{}, err
	}
	if !constantStringEqual(snapshot.ContentHash, claims.SourceContentHash) {
		return CommitResult{}, ErrStalePreview
	}

	result := CommitResult{
		SpaceID:   claims.SpaceID,
		PageID:    claims.PageID,
		Entities:  make(map[string]int64),
		Claims:    make(map[string]int64),
		Regions:   make(map[string]int64),
		Relations: make(map[string]int64),
		Gaps:      make(map[string]int64),
	}

	pageID := claims.PageID
	createdBy := userID
	result.SourceID, err = upsertSource(ctx, tx, SourceDraft{
		SpaceID:   claims.SpaceID,
		Kind:      "tela_page",
		PageID:    &pageID,
		Title:     snapshot.Title,
		CreatedBy: &createdBy,
	})
	if err != nil {
		return CommitResult{}, err
	}
	result.SourceVersionID, err = upsertSourceVersion(ctx, tx, claims.SpaceID, result.SourceID, SourceVersionDraft{
		ContentHash: snapshot.ContentHash,
	})
	if err != nil {
		return CommitResult{}, err
	}

	for _, key := range sortedSetKeys(selection.regionKeys) {
		region := selection.regions[key]
		if strings.TrimSpace(region.Excerpt) == "" || (!strings.Contains(snapshot.Body, region.Excerpt) && !strings.Contains(snapshot.Title, region.Excerpt)) {
			return CommitResult{}, fmt.Errorf("%w: region %q excerpt is not an exact span of the current page", ErrInvalidSelection, key)
		}
		excerptHash := sha256Hex(region.Excerpt)
		regionID, err := upsertSourceRegion(ctx, tx, claims.SpaceID, result.SourceVersionID, SourceRegionDraft{
			LocatorKind: region.LocatorKind,
			Locator:     region.Locator,
			Excerpt:     region.Excerpt,
			ExcerptHash: excerptHash,
		})
		if err != nil {
			return CommitResult{}, err
		}
		result.Regions[key] = regionID
	}

	for _, key := range sortedSetKeys(selection.entityKeys) {
		candidate := selection.entities[key]
		choice := in.EntityChoices[key]
		regionID := firstEntityRegionID(key, selection, result.Regions)
		entityID, err := resolveCommitEntity(ctx, tx, claims.SpaceID, userID, candidate, choice, regionID)
		if err != nil {
			return CommitResult{}, err
		}
		result.Entities[key] = entityID
	}

	for _, key := range sortedSetKeys(selection.claimKeys) {
		candidate := selection.claims[key]
		draft, err := candidateClaimDraft(claims.SpaceID, userID, candidate, result.Entities)
		if err != nil {
			return CommitResult{}, err
		}
		fingerprint, err := Fingerprint(draft)
		if err != nil {
			return CommitResult{}, fmt.Errorf("semantic commit claim %q: %w", key, err)
		}
		claimID, err := upsertClaim(ctx, tx, draft, fingerprint)
		if err != nil {
			return CommitResult{}, err
		}
		if err := replaceClaimSlots(ctx, tx, claims.SpaceID, claimID, draft.Slots); err != nil {
			return CommitResult{}, err
		}
		result.Claims[key] = claimID

		if len(candidate.Instances) == 0 {
			return CommitResult{}, fmt.Errorf("%w: accepted claim %q has no evidence instance", ErrInvalidSelection, key)
		}
		for _, instance := range candidate.Instances {
			regionID, ok := result.Regions[instance.RegionKey]
			if !ok {
				return CommitResult{}, fmt.Errorf("%w: claim %q missing committed region %q", ErrInvalidSelection, key, instance.RegionKey)
			}
			if strings.TrimSpace(instance.OriginalText) == "" {
				return CommitResult{}, fmt.Errorf("%w: claim %q has empty instance text", ErrInvalidSelection, key)
			}
			region := selection.regions[instance.RegionKey]
			if !strings.Contains(region.Excerpt, instance.OriginalText) {
				return CommitResult{}, fmt.Errorf("%w: claim %q instance text is not inside region %q", ErrInvalidSelection, key, instance.RegionKey)
			}
			if _, err := upsertClaimInstance(ctx, tx, claims.SpaceID, claimID, regionID, ClaimInstanceDraft{
				OriginalText:          instance.OriginalText,
				ProposedCanonicalText: instance.ProposedCanonicalText,
				Context:               instance.Context,
				Stance:                instance.Stance,
				ExtractionConfidence:  instance.ExtractionConfidence,
				CreatedBy:             semanticCommitTool,
			}); err != nil {
				return CommitResult{}, err
			}
		}
	}

	for _, key := range sortedSetKeys(selection.relationKeys) {
		relation := selection.relations[key]
		fromID := result.Claims[relation.FromClaimKey]
		toID := result.Claims[relation.ToClaimKey]
		var id int64
		err := tx.QueryRowContext(ctx, `
INSERT INTO sem_claim_relations(space_id, from_claim_id, to_claim_id, relation_type, reason, confidence, created_by)
VALUES ($1,$2,$3,$4,$5,$6,$7)
ON CONFLICT (from_claim_id, to_claim_id, relation_type)
DO UPDATE SET reason = EXCLUDED.reason, confidence = EXCLUDED.confidence
RETURNING id`, claims.SpaceID, fromID, toID, strings.TrimSpace(strings.ToLower(relation.RelationType)), relation.Reason, relation.Confidence, semanticCommitTool).Scan(&id)
		if err != nil {
			return CommitResult{}, wrap("commit claim relation", err)
		}
		result.Relations[key] = id
	}

	for _, key := range sortedSetKeys(selection.gapKeys) {
		gap := selection.gaps[key]
		var relatedClaimID, relatedEntityID any
		if gap.RelatedClaimKey != "" {
			relatedClaimID = result.Claims[gap.RelatedClaimKey]
		}
		if gap.RelatedEntityKey != "" {
			relatedEntityID = result.Entities[gap.RelatedEntityKey]
		}
		var id int64
		err := tx.QueryRowContext(ctx, `
INSERT INTO sem_gaps(space_id, question, importance, related_claim_id, related_entity_id, context, created_by)
VALUES ($1,$2,$3,$4,$5,$6::jsonb,$7)
RETURNING id`, claims.SpaceID, strings.TrimSpace(gap.Question), gap.Importance, relatedClaimID, relatedEntityID, defaultJSON(gap.Context), semanticCommitTool).Scan(&id)
		if err != nil {
			return CommitResult{}, wrap("commit gap", err)
		}
		result.Gaps[key] = id
	}

	encodedResult, err := json.Marshal(result)
	if err != nil {
		return CommitResult{}, fmt.Errorf("semantic commit: marshal result: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
UPDATE idempotency_keys
SET result = $1
WHERE user_id = $2 AND idem_key = $3 AND tool = $4`, string(encodedResult), userID, strings.TrimSpace(in.IdempotencyKey), semanticCommitTool); err != nil {
		return CommitResult{}, fmt.Errorf("semantic commit: store idempotency result: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return CommitResult{}, fmt.Errorf("semantic commit: commit transaction: %w", err)
	}
	return result, nil
}

type commitSelection struct {
	entities     map[string]CandidateEntity
	claims       map[string]CandidateClaim
	regions      map[string]CandidateRegion
	relations    map[string]CandidateRelation
	gaps         map[string]CandidateGap
	claimKeys    map[string]struct{}
	entityKeys   map[string]struct{}
	regionKeys   map[string]struct{}
	relationKeys map[string]struct{}
	gapKeys      map[string]struct{}
	entityRegion map[string]string
}

func prepareCommitSelection(candidates CandidateSet, accepted []string, choices map[string]EntityChoice) (commitSelection, error) {
	sel := commitSelection{
		entities:     make(map[string]CandidateEntity),
		claims:       make(map[string]CandidateClaim),
		regions:      make(map[string]CandidateRegion),
		relations:    make(map[string]CandidateRelation),
		gaps:         make(map[string]CandidateGap),
		claimKeys:    make(map[string]struct{}),
		entityKeys:   make(map[string]struct{}),
		regionKeys:   make(map[string]struct{}),
		relationKeys: make(map[string]struct{}),
		gapKeys:      make(map[string]struct{}),
		entityRegion: make(map[string]string),
	}
	for _, x := range candidates.Entities {
		sel.entities[x.Key] = x
	}
	for _, x := range candidates.Claims {
		sel.claims[x.Key] = x
	}
	for _, x := range candidates.Regions {
		sel.regions[x.Key] = x
	}
	for _, x := range candidates.Relations {
		sel.relations[x.Key] = x
	}
	for _, x := range candidates.Gaps {
		sel.gaps[x.Key] = x
	}

	for _, rawKey := range accepted {
		key := strings.TrimSpace(rawKey)
		if key == "" {
			return commitSelection{}, fmt.Errorf("%w: accepted_keys contains an empty key", ErrInvalidSelection)
		}
		switch {
		case hasKey(sel.claims, key):
			sel.claimKeys[key] = struct{}{}
		case hasKey(sel.relations, key):
			sel.relationKeys[key] = struct{}{}
		case hasKey(sel.gaps, key):
			sel.gapKeys[key] = struct{}{}
		default:
			return commitSelection{}, fmt.Errorf("%w: key %q is not a selectable claim/relation/gap", ErrInvalidSelection, key)
		}
	}
	if len(sel.claimKeys) == 0 {
		return commitSelection{}, fmt.Errorf("%w: at least one claim must be accepted", ErrInvalidSelection)
	}

	for key := range sel.claimKeys {
		claim := sel.claims[key]
		if len(claim.Instances) == 0 {
			return commitSelection{}, fmt.Errorf("%w: accepted claim %q has no evidence", ErrInvalidSelection, key)
		}
		for _, slot := range claim.Slots {
			if slot.EntityKey != nil {
				entityKey := strings.TrimSpace(*slot.EntityKey)
				sel.entityKeys[entityKey] = struct{}{}
			}
		}
		for _, instance := range claim.Instances {
			regionKey := strings.TrimSpace(instance.RegionKey)
			if _, ok := sel.regions[regionKey]; !ok {
				return commitSelection{}, fmt.Errorf("%w: accepted claim %q references unknown region %q", ErrInvalidSelection, key, regionKey)
			}
			sel.regionKeys[regionKey] = struct{}{}
			for _, slot := range claim.Slots {
				if slot.EntityKey == nil {
					continue
				}
				entityKey := strings.TrimSpace(*slot.EntityKey)
				if _, exists := sel.entityRegion[entityKey]; !exists {
					sel.entityRegion[entityKey] = regionKey
				}
			}
		}
	}

	for key := range sel.relationKeys {
		relation := sel.relations[key]
		if _, ok := sel.claimKeys[relation.FromClaimKey]; !ok {
			return commitSelection{}, fmt.Errorf("%w: relation %q requires accepted claim %q", ErrInvalidSelection, key, relation.FromClaimKey)
		}
		if _, ok := sel.claimKeys[relation.ToClaimKey]; !ok {
			return commitSelection{}, fmt.Errorf("%w: relation %q requires accepted claim %q", ErrInvalidSelection, key, relation.ToClaimKey)
		}
	}
	for key := range sel.gapKeys {
		gap := sel.gaps[key]
		if gap.RelatedClaimKey != "" {
			if _, ok := sel.claimKeys[gap.RelatedClaimKey]; !ok {
				return commitSelection{}, fmt.Errorf("%w: gap %q requires accepted claim %q", ErrInvalidSelection, key, gap.RelatedClaimKey)
			}
		}
		if gap.RelatedEntityKey != "" {
			if _, ok := sel.entities[gap.RelatedEntityKey]; !ok {
				return commitSelection{}, fmt.Errorf("%w: gap %q references unknown entity %q", ErrInvalidSelection, key, gap.RelatedEntityKey)
			}
			sel.entityKeys[gap.RelatedEntityKey] = struct{}{}
		}
	}

	for entityKey := range sel.entityKeys {
		candidate, ok := sel.entities[entityKey]
		if !ok {
			return commitSelection{}, fmt.Errorf("%w: referenced entity %q is missing", ErrInvalidSelection, entityKey)
		}
		if strings.TrimSpace(candidate.CanonicalName) == "" || !validEntityKind(strings.TrimSpace(strings.ToLower(candidate.Kind))) {
			return commitSelection{}, fmt.Errorf("%w: entity %q is invalid", ErrInvalidSelection, entityKey)
		}
		choice, ok := choices[entityKey]
		if !ok {
			return commitSelection{}, fmt.Errorf("%w: entity %q needs an explicit identity choice", ErrInvalidSelection, entityKey)
		}
		hasExisting := choice.ExistingEntityID != nil
		if hasExisting == choice.CreateNew {
			return commitSelection{}, fmt.Errorf("%w: entity %q needs exactly one of existing_entity_id or create_new", ErrInvalidSelection, entityKey)
		}
		if choice.ExistingEntityID != nil && *choice.ExistingEntityID == 0 {
			return commitSelection{}, fmt.Errorf("%w: entity %q has invalid existing id", ErrInvalidSelection, entityKey)
		}
	}
	for key := range choices {
		if _, needed := sel.entityKeys[key]; !needed {
			return commitSelection{}, fmt.Errorf("%w: entity choice %q is not used by the accepted graph", ErrInvalidSelection, key)
		}
	}
	return sel, nil
}

func candidateClaimDraft(spaceID, userID int64, c CandidateClaim, entityIDs map[string]int64) (ClaimDraft, error) {
	slots := make([]Slot, 0, len(c.Slots))
	for _, in := range c.Slots {
		slot := Slot{Role: in.Role, Ordinal: in.Ordinal, LiteralKind: in.LiteralKind}
		if in.EntityKey != nil {
			id, ok := entityIDs[strings.TrimSpace(*in.EntityKey)]
			if !ok {
				return ClaimDraft{}, fmt.Errorf("%w: claim %q unresolved entity %q", ErrInvalidSelection, c.Key, *in.EntityKey)
			}
			slot.EntityID = &id
		} else {
			slot.Literal = append(json.RawMessage(nil), in.Literal...)
			if strings.TrimSpace(in.LiteralKind) == "" {
				return ClaimDraft{}, fmt.Errorf("%w: claim %q literal slot %q has no literal_kind", ErrInvalidSelection, c.Key, in.Role)
			}
		}
		slots = append(slots, slot)
	}
	claimType := strings.TrimSpace(strings.ToLower(c.ClaimType))
	if claimType == "" {
		claimType = "fact"
	}
	createdBy := userID
	return ClaimDraft{
		SpaceID:       spaceID,
		CanonicalText: strings.TrimSpace(c.CanonicalText),
		ClaimType:     claimType,
		Predicate:     strings.TrimSpace(strings.ToLower(c.Predicate)),
		ValidFrom:     strings.TrimSpace(c.ValidFrom),
		ValidTo:       strings.TrimSpace(c.ValidTo),
		TimePrecision: strings.TrimSpace(c.TimePrecision),
		Qualifiers:    append(json.RawMessage(nil), c.Qualifiers...),
		Slots:         slots,
		CreatedBy:     &createdBy,
	}, nil
}

func resolveCommitEntity(ctx context.Context, tx *sql.Tx, spaceID, userID int64, candidate CandidateEntity, choice EntityChoice, sourceRegionID *int64) (int64, error) {
	kind := strings.TrimSpace(strings.ToLower(candidate.Kind))
	name := strings.TrimSpace(candidate.CanonicalName)
	var entityID int64
	if choice.ExistingEntityID != nil {
		var existingKind string
		err := tx.QueryRowContext(ctx, `
SELECT kind
FROM sem_entities
WHERE id = $1 AND space_id = $2 AND state = 'active'`, *choice.ExistingEntityID, spaceID).Scan(&existingKind)
		if errors.Is(err, sql.ErrNoRows) {
			return 0, fmt.Errorf("%w: existing entity %d is not active in this space", ErrInvalidSelection, *choice.ExistingEntityID)
		}
		if err != nil {
			return 0, wrap("resolve existing entity", err)
		}
		if existingKind != kind {
			return 0, fmt.Errorf("%w: existing entity %d kind %q does not match candidate kind %q", ErrInvalidSelection, *choice.ExistingEntityID, existingKind, kind)
		}
		entityID = *choice.ExistingEntityID
	} else {
		err := tx.QueryRowContext(ctx, `
INSERT INTO sem_entities(space_id, kind, canonical_name, created_by)
VALUES ($1,$2,$3,$4)
RETURNING id`, spaceID, kind, name, userID).Scan(&entityID)
		if err != nil {
			return 0, wrap("commit create entity", err)
		}
	}

	aliases := append([]string{name}, candidate.Aliases...)
	seenAliases := make(map[string]struct{}, len(aliases))
	for _, alias := range aliases {
		norm := NormalizeAlias(alias)
		if norm == "" {
			continue
		}
		if _, seen := seenAliases[norm]; seen {
			continue
		}
		seenAliases[norm] = struct{}{}
		if err := insertEntityAlias(ctx, tx, spaceID, entityID, norm, EntityAliasDraft{
			Alias:          strings.TrimSpace(alias),
			SourceRegionID: sourceRegionID,
		}, &userID); err != nil {
			return 0, err
		}
	}
	return entityID, nil
}

func firstEntityRegionID(entityKey string, sel commitSelection, regionIDs map[string]int64) *int64 {
	key := sel.entityRegion[entityKey]
	if key == "" {
		return nil
	}
	id, ok := regionIDs[key]
	if !ok {
		return nil
	}
	return &id
}

func claimCommitIdempotency(ctx context.Context, tx *sql.Tx, userID int64, key string) (CommitResult, bool, error) {
	res, err := tx.ExecContext(ctx, `
INSERT INTO idempotency_keys(user_id, idem_key, tool, result)
VALUES ($1,$2,$3,NULL)
ON CONFLICT (user_id, idem_key) DO NOTHING`, userID, key, semanticCommitTool)
	if err != nil {
		return CommitResult{}, false, fmt.Errorf("semantic commit: claim idempotency key: %w", err)
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return CommitResult{}, false, fmt.Errorf("semantic commit: idempotency rows affected: %w", err)
	}
	if rows == 1 {
		return CommitResult{}, false, nil
	}

	var tool string
	var stored sql.NullString
	if err := tx.QueryRowContext(ctx, `
SELECT tool, result
FROM idempotency_keys
WHERE user_id = $1 AND idem_key = $2`, userID, key).Scan(&tool, &stored); err != nil {
		return CommitResult{}, false, fmt.Errorf("semantic commit: read idempotency key: %w", err)
	}
	if tool != semanticCommitTool {
		return CommitResult{}, false, ErrIdempotencyConflict
	}
	if !stored.Valid {
		return CommitResult{}, false, ErrCommitInProgress
	}
	var replay CommitResult
	if err := json.Unmarshal([]byte(stored.String), &replay); err != nil {
		return CommitResult{}, false, fmt.Errorf("semantic commit: decode replay result: %w", err)
	}
	return replay, true, nil
}

func readPageSnapshotTx(ctx context.Context, tx *sql.Tx, spaceID, pageID int64) (PageSnapshot, error) {
	var title, body string
	// Hold a shared row lock until semantic persistence commits. A concurrent page
	// UPDATE/DELETE therefore cannot move the source after the stale-hash check.
	err := tx.QueryRowContext(ctx, `
SELECT title, body
FROM pages
WHERE id = $1 AND space_id = $2 AND deleted_at IS NULL
FOR SHARE`, pageID, spaceID).Scan(&title, &body)
	if errors.Is(err, sql.ErrNoRows) {
		return PageSnapshot{}, ErrNotFound
	}
	if err != nil {
		return PageSnapshot{}, fmt.Errorf("semantic commit: read page: %w", err)
	}
	return PageSnapshot{
		SpaceID:     spaceID,
		PageID:      pageID,
		Title:       title,
		Body:        body,
		ContentHash: PageSnapshotHash(title, body),
	}, nil
}

func sha256Hex(v string) string {
	sum := sha256.Sum256([]byte(v))
	return hex.EncodeToString(sum[:])
}

func constantStringEqual(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	var diff byte
	for i := 0; i < len(a); i++ {
		diff |= a[i] ^ b[i]
	}
	return diff == 0
}

func sortedSetKeys(in map[string]struct{}) []string {
	out := make([]string, 0, len(in))
	for key := range in {
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}

func hasKey[T any](m map[string]T, key string) bool {
	_, ok := m[key]
	return ok
}
