package rag

import (
	"context"
	"database/sql"
	"log/slog"
	"sort"
	"strconv"
	"strings"
)

// Hit is one ranked chunk result. Carries everything a caller needs to cite the
// source: page id + heading path (and the API layer adds the in-app URL).
// UpdatedAt surfaces freshness so an agent/UI can warn on stale facts.
//
// A chunk's source is a page OR a file (SourceKind). For a file hit, Title is the
// file name, FileID/FileName/Hash identify the attachment, and PageID is its
// PARENT page (0 = space root). DownloadURL and ShareURL are left empty here and
// filled by the API layer (rag can't build tela URLs without importing api) —
// see enrichFileCitations. DownloadURL fetches the bytes; ShareURL is the file
// page to show a person.
type Hit struct {
	ChunkID     int64   `json:"chunk_id"`
	SourceKind  string  `json:"source_kind"` // "page" | "file"
	PageID      int64   `json:"page_id"`
	SpaceID     int64   `json:"space_id"`
	Title       string  `json:"title"`
	HeadingPath string  `json:"heading_path"`
	Snippet     string  `json:"snippet"`
	Score       float64 `json:"score"`
	UpdatedAt   string  `json:"updated_at"`

	// File-source only.
	FileID      int64  `json:"file_id,omitempty"`
	FileName    string `json:"file_name,omitempty"`
	DownloadURL string `json:"download_url,omitempty"`
	ShareURL    string `json:"share_url,omitempty"`
	Hash        string `json:"-"` // carrier for the API layer's URL builds
}

// fileChunkIDBase is file_chunks.id's identity floor (2^40, see migration 0036):
// a chunk id at or above it is a FILE chunk, below it a PAGE chunk. The two id
// spaces never collide, so a bare chunk id routes to the right table by range.
const fileChunkIDBase = 1 << 40

// IsFileChunk reports whether a chunk id belongs to file_chunks (vs page_chunks).
func IsFileChunk(chunkID int64) bool { return chunkID >= fileChunkIDBase }

// rrfK is the standard Reciprocal Rank Fusion constant. Larger = flatter
// weighting across ranks; 60 is the well-trodden default and needs no score
// calibration between the lexical and vector rankers.
const rrfK = 60

// Search runs hybrid retrieval scoped to what userID can access — their
// space_access plus every public space (see accessibleSpacesSQL). mode is
// "hybrid" (default), "semantic", or "lexical".
// spaceID, when non-nil, narrows to a single space.
func (s *Service) Search(ctx context.Context, userID int64, q string, spaceID *int64, limit int, mode string) ([]Hit, error) {
	q = strings.TrimSpace(q)
	if q == "" {
		return []Hit{}, nil
	}
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	// Pull a deeper candidate pool from each ranker than we return, so fusion has
	// room to reorder AND the second-stage reranker sees a relevant doc that fused
	// past the shallow ranks (the floor must clear rerankCandidates, else a doc the
	// cross-encoder would rank #1 never reaches it — the cross-space-needle case).
	pool := limit * 4
	if pool < 60 {
		pool = 60
	}

	var lex, vec []int64
	if mode != "semantic" {
		ids, err := s.lexicalRank(ctx, userID, q, spaceID, pool)
		if err != nil {
			return nil, err
		}
		lex = ids
	}
	if mode != "lexical" {
		ids, err := s.vectorRank(ctx, userID, q, spaceID, pool)
		if err != nil {
			return nil, err
		}
		vec = ids
	}

	// Reciprocal Rank Fusion: each list contributes 1/(k + rank) to a chunk's
	// score; chunks ranked highly by both rankers rise to the top.
	score := map[int64]float64{}
	order := []int64{}
	add := func(ids []int64) {
		for rank, id := range ids {
			if _, seen := score[id]; !seen {
				order = append(order, id)
			}
			score[id] += 1.0 / float64(rrfK+rank+1)
		}
	}
	add(lex)
	add(vec)

	sort.SliceStable(order, func(i, j int) bool { return score[order[i]] > score[order[j]] })

	// Optional second-stage rerank: re-score the top fused candidates with a
	// cross-encoder, then trim. Lexical-only mode skips it (no semantic intent to
	// resolve). Best-effort — a rerank failure falls back to the RRF order.
	if s.RerankEnabled() && mode != "lexical" && len(order) > 1 {
		cand := order
		if len(cand) > rerankCandidates {
			cand = cand[:rerankCandidates]
		}
		if reordered, err := s.rerankIDs(ctx, userID, q, cand, spaceID, score); err != nil {
			slog.Warn("rag: rerank failed, using fused order", "err", err)
		} else {
			order = append(reordered, order[len(cand):]...)
		}
	}

	if len(order) > limit {
		order = order[:limit]
	}
	return s.hydrate(ctx, order, score)
}

// lexicalRank returns chunk ids ranked by ts_rank_cd over the generated
// content_tsv, scoped by space_access (and optionally a single space). The pool
// is the UNION of page chunks (joined to the LIVE pages row) and file chunks
// (joined to the LIVE space_files row) — one ranked list across both sources.
// The permission join is always against the live source row, never a chunk copy
// — the anti-leak invariant.
func (s *Service) lexicalRank(ctx context.Context, userID int64, q string, spaceID *int64, limit int) ([]int64, error) {
	qb := &queryBuilder{}
	uid := qb.arg(userID)
	qry := qb.arg(q)
	// OR-recall: rewrite plainto_tsquery's AND-conjunction to OR, so a doc is a
	// lexical candidate if it contains ANY query term, not all of them. A
	// conversational query ("what's the X response time and Y price?") carries
	// filler/co-occurring words absent from a relevant doc; AND-matching silently
	// zeroed that doc out of the lexical pool, leaving vector-only ranking to fend
	// off a large overlapping corpus (the buried-needle case). ts_rank_cd still
	// ranks by how many terms matched + proximity, and the reranker supplies
	// precision downstream — so recall here is the right tradeoff. Same OR trick as
	// HubPages. Empty/stopword-only queries → empty tsquery → no lexical rows (vector
	// still runs).
	tsq := `replace(plainto_tsquery('english', ` + qry + `)::text, '&', '|')::tsquery`
	var pageSp, fileSp string
	if spaceID != nil {
		sp := qb.arg(*spaceID)
		pageSp, fileSp = ` AND p.space_id = `+sp, ` AND sf.space_id = `+sp
	}
	lim := qb.arg(limit)
	pen := strconv.FormatFloat(publicRankPenalty, 'f', -1, 64)
	// Soft-demote public-only chunks: members rank at full ts_rank, public at a
	// fraction, so a member match wins a near-tie but a strong public match survives.
	demoted := `(r * CASE WHEN is_member = 1 THEN 1.0 ELSE ` + pen + ` END)`
	sql := `
		SELECT id FROM (
			SELECT pc.id AS id, ts_rank_cd(pc.content_tsv, ` + tsq + `) AS r, sm.is_member AS is_member
			  FROM page_chunks pc
			  JOIN pages p ON p.id = pc.page_id AND p.deleted_at IS NULL
			  JOIN ` + accessibleSpacesSQL(uid) + ` sm
			    ON sm.space_id = p.space_id
			 WHERE pc.content_tsv @@ ` + tsq + pageSp + `
			UNION ALL
			SELECT fc.id AS id, ts_rank_cd(fc.content_tsv, ` + tsq + `) AS r, sm.is_member AS is_member
			  FROM file_chunks fc
			  JOIN space_files sf ON sf.id = fc.space_file_id AND sf.deleted_at IS NULL
			  JOIN ` + accessibleSpacesSQL(uid) + ` sm
			    ON sm.space_id = sf.space_id
			 WHERE fc.content_tsv @@ ` + tsq + fileSp + `
		) u ORDER BY ` + demoted + ` DESC LIMIT ` + lim
	return s.queryIDs(ctx, sql, qb.args...)
}

// queryEmbedder is the optional asymmetric-retrieval capability: an embedder
// that distinguishes a search query from a passage. The Ollama embedder
// implements it (instruction-prefixed queries); test fakes don't, and degrade to
// symmetric Embed. Kept off the core Embedder interface so injecting a plain
// fake stays a one-method job.
type queryEmbedder interface {
	EmbedQuery(ctx context.Context, query string) ([]float32, error)
}

// embedQuery embeds a search query, using the asymmetric instruction-prefixed
// path when the active embedder supports it, else a plain symmetric Embed.
func (s *Service) embedQuery(ctx context.Context, q string) ([]float32, error) {
	if qe, ok := s.emb.(queryEmbedder); ok {
		return qe.EmbedQuery(ctx, q)
	}
	return s.emb.Embed(ctx, q)
}

// vectorRank embeds the query and returns chunk ids ranked by cosine distance
// (pgvector <=>, ascending = closest), scoped by space_access. The pool UNIONs
// page chunks and file chunks (each ACL-joined to its live source row). No ANN
// index yet — an exact scan over the (small) permitted candidate set.
func (s *Service) vectorRank(ctx context.Context, userID int64, q string, spaceID *int64, limit int) ([]int64, error) {
	vec, err := s.embedQuery(ctx, q)
	if err != nil {
		return nil, err
	}
	qb := &queryBuilder{}
	uid := qb.arg(userID)
	qvec := qb.arg(vecLiteral(vec)) + `::vector`
	var pageSp, fileSp string
	if spaceID != nil {
		sp := qb.arg(*spaceID)
		pageSp, fileSp = ` AND p.space_id = `+sp, ` AND sf.space_id = `+sp
	}
	lim := qb.arg(limit)
	pen := strconv.FormatFloat(publicDistPenalty, 'f', -1, 64)
	// Soft-demote public-only chunks: scale up their cosine distance (lower = better)
	// so a member chunk wins a near-tie, while a much-closer public chunk still leads.
	demoted := `(d * CASE WHEN is_member = 1 THEN 1.0 ELSE ` + pen + ` END)`
	sql := `
		SELECT id FROM (
			SELECT pc.id AS id, (pc.embedding <=> ` + qvec + `) AS d, sm.is_member AS is_member
			  FROM page_chunks pc
			  JOIN pages p ON p.id = pc.page_id AND p.deleted_at IS NULL
			  JOIN ` + accessibleSpacesSQL(uid) + ` sm
			    ON sm.space_id = p.space_id
			 WHERE pc.embedding IS NOT NULL` + pageSp + `
			UNION ALL
			SELECT fc.id AS id, (fc.embedding <=> ` + qvec + `) AS d, sm.is_member AS is_member
			  FROM file_chunks fc
			  JOIN space_files sf ON sf.id = fc.space_file_id AND sf.deleted_at IS NULL
			  JOIN ` + accessibleSpacesSQL(uid) + ` sm
			    ON sm.space_id = sf.space_id
			 WHERE fc.embedding IS NOT NULL` + fileSp + `
		) u ORDER BY ` + demoted + ` ASC LIMIT ` + lim
	return s.queryIDs(ctx, sql, qb.args...)
}

func (s *Service) queryIDs(ctx context.Context, query string, args ...any) ([]int64, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// hydrate fetches display fields for the fused id list and returns hits in fused
// order (the IN-clause result order is undefined, so we re-sort by score). The id
// list can mix page and file chunks; each table is hydrated separately (routed by
// the fileChunkIDBase id range) and merged. No re-ACL here — the rankers already
// scoped ids by space_access; hydrate joins only to pick up display fields.
func (s *Service) hydrate(ctx context.Context, ids []int64, score map[int64]float64) ([]Hit, error) {
	if len(ids) == 0 {
		return []Hit{}, nil
	}
	var pageIDs, fileIDs []int64
	for _, id := range ids {
		if IsFileChunk(id) {
			fileIDs = append(fileIDs, id)
		} else {
			pageIDs = append(pageIDs, id)
		}
	}
	byID := make(map[int64]Hit, len(ids))

	if len(pageIDs) > 0 {
		qb := &queryBuilder{}
		ph := make([]string, len(pageIDs))
		for i, id := range pageIDs {
			ph[i] = qb.arg(id)
		}
		rows, err := s.db.QueryContext(ctx, `
			SELECT pc.id, pc.page_id, p.space_id, pc.heading_path, pc.content, p.title, p.updated_at
			  FROM page_chunks pc
			  JOIN pages p ON p.id = pc.page_id AND p.deleted_at IS NULL
			 WHERE pc.id IN (`+strings.Join(ph, ",")+`)`, qb.args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var (
				h       Hit
				content string
			)
			if err := rows.Scan(&h.ChunkID, &h.PageID, &h.SpaceID, &h.HeadingPath, &content, &h.Title, &h.UpdatedAt); err != nil {
				rows.Close()
				return nil, err
			}
			h.SourceKind = "page"
			h.Snippet = snippet(content, 280)
			h.Score = score[h.ChunkID]
			byID[h.ChunkID] = h
		}
		if err := rows.Close(); err != nil {
			return nil, err
		}
	}

	if len(fileIDs) > 0 {
		qb := &queryBuilder{}
		ph := make([]string, len(fileIDs))
		for i, id := range fileIDs {
			ph[i] = qb.arg(id)
		}
		rows, err := s.db.QueryContext(ctx, `
			SELECT fc.id, fc.space_file_id, sf.space_id, sf.parent_page_id, fc.heading_path, fc.content, sf.name, sf.content_hash, sf.updated_at
			  FROM file_chunks fc
			  JOIN space_files sf ON sf.id = fc.space_file_id AND sf.deleted_at IS NULL
			 WHERE fc.id IN (`+strings.Join(ph, ",")+`)`, qb.args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var (
				h       Hit
				content string
				parent  sql.NullInt64
			)
			if err := rows.Scan(&h.ChunkID, &h.FileID, &h.SpaceID, &parent, &h.HeadingPath, &content, &h.FileName, &h.Hash, &h.UpdatedAt); err != nil {
				rows.Close()
				return nil, err
			}
			h.SourceKind = "file"
			h.PageID = parent.Int64 // 0 = space root
			h.Title = h.FileName
			h.Snippet = snippet(content, 280)
			h.Score = score[h.ChunkID]
			byID[h.ChunkID] = h
		}
		if err := rows.Close(); err != nil {
			return nil, err
		}
	}

	out := make([]Hit, 0, len(ids))
	for _, id := range ids {
		if h, ok := byID[id]; ok {
			out = append(out, h)
		}
	}
	return out, nil
}

// queryBuilder accumulates positional args and hands back $1, $2, … placeholders
// in order — so optional WHERE clauses can be appended without hand-counting.
type queryBuilder struct {
	args []any
}

func (b *queryBuilder) arg(v any) string {
	b.args = append(b.args, v)
	return "$" + strconv.Itoa(len(b.args))
}

// snippet returns a single-line preview of content, truncated to roughly n runes
// on a word boundary.
func snippet(content string, n int) string {
	content = strings.Join(strings.Fields(content), " ")
	if len(content) <= n {
		return content
	}
	cut := content[:n]
	if i := strings.LastIndexByte(cut, ' '); i > n/2 {
		cut = cut[:i]
	}
	return cut + "…"
}
