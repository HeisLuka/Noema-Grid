package api

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/zcag/tela/backend/internal/semantic"
)

func (s *Server) registerSemanticMCPTools(server *mcp.Server) {
	no, yes := false, true
	mcp.AddTool(server, &mcp.Tool{
		Name:        "semanticize_page",
		Title:       "Preview semanticization",
		Description: "Read a Tela page and produce a signed, read-only semantic candidate graph (entities, claims, exact source regions, relations and gaps). Writes nothing. The returned preview_token and candidates must be passed unchanged to commit_semanticization after explicit review/selection.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: &yes, DestructiveHint: &no, OpenWorldHint: &no},
	}, s.mcpSemanticizePage)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "semantic_resolve_entities",
		Title:       "Suggest existing entity matches",
		Description: "Read-only advisory entity resolution for candidates returned by semanticize_page. Returns exact normalized-alias matches of the same entity kind as none, single_exact_alias, or ambiguous_exact_alias. It never chooses or merges an identity; use the suggestions to build explicit entity_choices for commit_semanticization.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: &yes, DestructiveHint: &no, OpenWorldHint: &no},
	}, s.mcpSemanticResolveEntities)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "commit_semanticization",
		Title:       "Commit semanticization",
		Description: "Persist an explicitly accepted subset of a signed semanticize_page preview. Requires editor/owner access, explicit entity identity choices, and an idempotency_key. Rejects stale/tampered previews and writes provenance + canonical claims in one transaction.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: &no, IdempotentHint: true, DestructiveHint: &no, OpenWorldHint: &no},
	}, s.mcpCommitSemanticization)
}

type mcpSemanticizePageIn struct {
	SpaceID int64                       `json:"space_id" jsonschema:"Tela space id containing the page"`
	PageID  int64                       `json:"page_id" jsonschema:"Tela page id to semanticize"`
	Profile semantic.ExtractionProfile `json:"profile" jsonschema:"extraction profile: genealogy, research_dense, technical, or argumentative_sparse"`
}

type mcpSemanticizePageOut struct {
	Preview semantic.Preview `json:"preview"`
}

func (s *Server) mcpSemanticizePage(ctx context.Context, req *mcp.CallToolRequest, in mcpSemanticizePageIn) (*mcp.CallToolResult, mcpSemanticizePageOut, error) {
	u, k := mcpIdentity(req)
	if u == nil {
		return mcpUnauthErr(), mcpSemanticizePageOut{}, nil
	}
	preview, ae := s.semanticPreviewCore(ctx, u, k, in.SpaceID, in.PageID, in.Profile, s.semanticExtractor())
	if ae != nil {
		return mcpErr(ae), mcpSemanticizePageOut{}, nil
	}
	return nil, mcpSemanticizePageOut{Preview: preview}, nil
}

type mcpSemanticResolveEntitiesIn struct {
	SpaceID  int64                      `json:"space_id" jsonschema:"Tela space id containing the semantic graph"`
	Entities []semantic.CandidateEntity `json:"entities" jsonschema:"entity candidates returned by semanticize_page"`
}

type mcpSemanticResolveEntitiesOut struct {
	Hints []semantic.EntityResolutionHint `json:"hints"`
}

func (s *Server) mcpSemanticResolveEntities(ctx context.Context, req *mcp.CallToolRequest, in mcpSemanticResolveEntitiesIn) (*mcp.CallToolResult, mcpSemanticResolveEntitiesOut, error) {
	u, k := mcpIdentity(req)
	if u == nil {
		return mcpUnauthErr(), mcpSemanticResolveEntitiesOut{}, nil
	}
	// membershipCore adds the PAT space-scope ceiling before the semantic service
	// independently checks live space_access.
	if _, ae := s.membershipCore(ctx, u, k, in.SpaceID); ae != nil {
		return mcpErr(ae), mcpSemanticResolveEntitiesOut{}, nil
	}
	hints, err := semantic.NewService(s.DB).ResolveEntityCandidateHints(ctx, u.ID, in.SpaceID, in.Entities)
	if err != nil {
		return mcpErr(semanticAPIError(err)), mcpSemanticResolveEntitiesOut{}, nil
	}
	return nil, mcpSemanticResolveEntitiesOut{Hints: hints}, nil
}

type mcpCommitSemanticizationIn struct {
	SpaceID        int64                            `json:"space_id" jsonschema:"Tela space id the signed preview belongs to"`
	PreviewToken   string                           `json:"preview_token" jsonschema:"signed preview_token returned by semanticize_page"`
	Candidates     semantic.CandidateSet            `json:"candidates" jsonschema:"the exact candidate payload returned by semanticize_page"`
	AcceptedKeys   []string                         `json:"accepted_keys" jsonschema:"candidate claim/relation/gap keys explicitly accepted for persistence"`
	EntityChoices  map[string]semantic.EntityChoice `json:"entity_choices" jsonschema:"explicit identity choice for every referenced entity candidate"`
	IdempotencyKey string                           `json:"idempotency_key" jsonschema:"stable retry key for this semantic commit"`
}

type mcpCommitSemanticizationOut struct {
	Result semantic.CommitResult `json:"result"`
}

func (s *Server) mcpCommitSemanticization(ctx context.Context, req *mcp.CallToolRequest, in mcpCommitSemanticizationIn) (*mcp.CallToolResult, mcpCommitSemanticizationOut, error) {
	u, k := mcpIdentity(req)
	if u == nil {
		return mcpUnauthErr(), mcpCommitSemanticizationOut{}, nil
	}
	if ae := mcpRequireWrite(k); ae != nil {
		return mcpErr(ae), mcpCommitSemanticizationOut{}, nil
	}
	result, ae := s.semanticCommitCore(ctx, u, k, in.SpaceID, semantic.CommitInput{
		PreviewToken:   in.PreviewToken,
		Candidates:     in.Candidates,
		AcceptedKeys:   in.AcceptedKeys,
		EntityChoices:  in.EntityChoices,
		IdempotencyKey: in.IdempotencyKey,
	})
	if ae != nil {
		return mcpErr(ae), mcpCommitSemanticizationOut{}, nil
	}
	return nil, mcpCommitSemanticizationOut{Result: result}, nil
}
