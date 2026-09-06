package rag

import (
	"context"
	"database/sql"
	"errors"
	"strings"
)

// ErrChunkNotFound is returned when a chunk id doesn't exist OR the caller can't
// access its page. The two are deliberately indistinguishable so a probing
// caller can't learn that a chunk exists in a space they can't read.
var ErrChunkNotFound = errors.New("chunk not found")

// ChunkRead is the full text of one chunk plus its citation. Unlike a search
// Hit (which carries a truncated snippet), this returns the complete section
// content — the chunk-granularity read between snippet and whole-page get_page.
// For a file chunk (SourceKind "file") Title is the file name, FileID/FileName/
// Hash identify the attachment, PageID is its PARENT page (0 = root), and
// DownloadURL / ShareURL are filled by the API layer (see enrichFileChunk) —
// bytes to fetch, and the file page to show a person.
type ChunkRead struct {
	ChunkID     int64  `json:"chunk_id"`
	SourceKind  string `json:"source_kind"` // "page" | "file"
	PageID      int64  `json:"page_id"`
	SpaceID     int64  `json:"space_id"`
	Title       string `json:"title"`
	HeadingPath string `json:"heading_path"`
	Content     string `json:"content"`
	UpdatedAt   string `json:"updated_at"`

	// File-source only.
	FileID      int64  `json:"file_id,omitempty"`
	FileName    string `json:"file_name,omitempty"`
	DownloadURL string `json:"download_url,omitempty"`
	ShareURL    string `json:"share_url,omitempty"`
	Hash        string `json:"-"` // carrier for the API layer's URL builds
}

// ReadChunk returns one chunk's full content, authorized through the LIVE source
// row joined to space_access (the anti-leak invariant — same as search). The
// chunk id routes to page_chunks or file_chunks by its range (fileChunkIDBase).
// spaceID, when non-nil, additionally narrows to that space (space-scoped bearer
// keys). Returns ErrChunkNotFound when the chunk doesn't exist or is out of scope.
func (s *Service) ReadChunk(ctx context.Context, userID, chunkID int64, spaceID *int64) (*ChunkRead, error) {
	if IsFileChunk(chunkID) {
		return s.readFileChunk(ctx, userID, chunkID, spaceID)
	}
	qb := &queryBuilder{}
	cid := qb.arg(chunkID)
	uid := qb.arg(userID)
	query := `
		SELECT pc.id, pc.page_id, p.space_id, pc.heading_path, pc.content, p.title, p.updated_at
		  FROM page_chunks pc
		  JOIN pages p ON p.id = pc.page_id AND p.deleted_at IS NULL
		  JOIN ` + accessibleSpacesSQL(uid) + ` sm
		    ON sm.space_id = p.space_id
		 WHERE pc.id = ` + cid
	if spaceID != nil {
		query += ` AND p.space_id = ` + qb.arg(*spaceID)
	}

	c := ChunkRead{SourceKind: "page"}
	err := s.db.QueryRowContext(ctx, query, qb.args...).Scan(
		&c.ChunkID, &c.PageID, &c.SpaceID, &c.HeadingPath, &c.Content, &c.Title, &c.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrChunkNotFound
	}
	if err != nil {
		return nil, err
	}
	return &c, nil
}

// readFileChunk is ReadChunk's file branch: authorize through the LIVE space_files
// row joined to space_access, citing the file (name + parent page) rather than a
// phantom page. Same not-found-vs-forbidden indistinguishability as the page path.
func (s *Service) readFileChunk(ctx context.Context, userID, chunkID int64, spaceID *int64) (*ChunkRead, error) {
	qb := &queryBuilder{}
	cid := qb.arg(chunkID)
	uid := qb.arg(userID)
	query := `
		SELECT fc.id, fc.space_file_id, sf.space_id, sf.parent_page_id, fc.heading_path, fc.content, sf.name, sf.content_hash, sf.updated_at
		  FROM file_chunks fc
		  JOIN space_files sf ON sf.id = fc.space_file_id AND sf.deleted_at IS NULL
		  JOIN ` + accessibleSpacesSQL(uid) + ` sm
		    ON sm.space_id = sf.space_id
		 WHERE fc.id = ` + cid
	if spaceID != nil {
		query += ` AND sf.space_id = ` + qb.arg(*spaceID)
	}

	c := ChunkRead{SourceKind: "file"}
	var parent sql.NullInt64
	err := s.db.QueryRowContext(ctx, query, qb.args...).Scan(
		&c.ChunkID, &c.FileID, &c.SpaceID, &parent, &c.HeadingPath, &c.Content, &c.FileName, &c.Hash, &c.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrChunkNotFound
	}
	if err != nil {
		return nil, err
	}
	c.PageID = parent.Int64 // 0 = space root
	c.Title = c.FileName
	return &c, nil
}

// ChunkContents returns the FULL section text for a set of chunk ids, keyed by
// chunk id, authorized through the live page row joined to space_access (the
// same anti-leak path as ReadChunk/Search). Out-of-scope or missing ids are
// silently omitted from the map — callers fall back to whatever they have. Used
// by "ask your docs" to ground the LLM on whole chunks instead of the truncated
// search snippet. One query, not N — safe to call with the top-k hit ids.
func (s *Service) ChunkContents(ctx context.Context, userID int64, ids []int64, spaceID *int64) (map[int64]string, error) {
	if len(ids) == 0 {
		return map[int64]string{}, nil
	}
	var pageIDs, fileIDs []int64
	for _, id := range ids {
		if IsFileChunk(id) {
			fileIDs = append(fileIDs, id)
		} else {
			pageIDs = append(pageIDs, id)
		}
	}
	out := make(map[int64]string, len(ids))
	collect := func(query string, args []any) error {
		rows, err := s.db.QueryContext(ctx, query, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id int64
			var content string
			if err := rows.Scan(&id, &content); err != nil {
				return err
			}
			out[id] = content
		}
		return rows.Err()
	}

	if len(pageIDs) > 0 {
		qb := &queryBuilder{}
		uid := qb.arg(userID)
		ph := make([]string, len(pageIDs))
		for i, id := range pageIDs {
			ph[i] = qb.arg(id)
		}
		query := `
			SELECT pc.id, pc.content
			  FROM page_chunks pc
			  JOIN pages p ON p.id = pc.page_id AND p.deleted_at IS NULL
			  JOIN ` + accessibleSpacesSQL(uid) + ` sm
			    ON sm.space_id = p.space_id
			 WHERE pc.id IN (` + strings.Join(ph, ",") + `)`
		if spaceID != nil {
			query += ` AND p.space_id = ` + qb.arg(*spaceID)
		}
		if err := collect(query, qb.args); err != nil {
			return nil, err
		}
	}

	if len(fileIDs) > 0 {
		qb := &queryBuilder{}
		uid := qb.arg(userID)
		ph := make([]string, len(fileIDs))
		for i, id := range fileIDs {
			ph[i] = qb.arg(id)
		}
		query := `
			SELECT fc.id, fc.content
			  FROM file_chunks fc
			  JOIN space_files sf ON sf.id = fc.space_file_id AND sf.deleted_at IS NULL
			  JOIN ` + accessibleSpacesSQL(uid) + ` sm
			    ON sm.space_id = sf.space_id
			 WHERE fc.id IN (` + strings.Join(ph, ",") + `)`
		if spaceID != nil {
			query += ` AND sf.space_id = ` + qb.arg(*spaceID)
		}
		if err := collect(query, qb.args); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// PageBodies returns the full markdown body for a set of page ids, keyed by page
// id, authorized through the same space_access anti-leak join as ChunkContents.
// Out-of-scope or missing ids are silently omitted. Used by "ask your docs" for
// parent-document retrieval: when a retrieved chunk points at a page, the ask
// path can feed the LLM the WHOLE page so an answer that lives in a table/list
// the chunker split (e.g. a "services using X" registry) survives intact.
func (s *Service) PageBodies(ctx context.Context, userID int64, ids []int64, spaceID *int64) (map[int64]string, error) {
	if len(ids) == 0 {
		return map[int64]string{}, nil
	}
	qb := &queryBuilder{}
	uid := qb.arg(userID)
	ph := make([]string, len(ids))
	for i, id := range ids {
		ph[i] = qb.arg(id)
	}
	query := `
		SELECT p.id, p.body
		  FROM pages p
		  JOIN ` + accessibleSpacesSQL(uid) + ` sm
		    ON sm.space_id = p.space_id
		 WHERE p.deleted_at IS NULL AND p.id IN (` + strings.Join(ph, ",") + `)`
	if spaceID != nil {
		query += ` AND p.space_id = ` + qb.arg(*spaceID)
	}
	rows, err := s.db.QueryContext(ctx, query, qb.args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[int64]string, len(ids))
	for rows.Next() {
		var id int64
		var body string
		if err := rows.Scan(&id, &body); err != nil {
			return nil, err
		}
		out[id] = body
	}
	return out, rows.Err()
}

// HubPage is a topical-hub candidate: a page and how many of its chunks match
// ANY query term (OR semantics), plus a representative chunk for fallback.
type HubPage struct {
	PageID  int64
	SpaceID int64
	Title   string
	ChunkID int64
	Count   int
}

// HubPages returns the topic-hub pages for a query: pages whose TITLE matches a
// query term, ranked by how many of their chunks also match. A wiki page titled
// after the subject ("Kafka") is that subject's hub — its whole body, often a
// registry TABLE the chunker split and a precision reranker buried, is the real
// answer to an aggregate question ("which projects use kafka"). Title match is
// the robust signal here: it beats AND-matching the full question (plainto_tsquery
// matches almost nothing), raw OR-count (the common words "project"/"use" swamp
// it), and TF-IDF (in this corpus "project" is rarer than "kafka", so rarity
// points the wrong way). Terms are OR-matched off plainto_tsquery (sanitised,
// stemmed; `'a' & 'b'` → `'a' | 'b'`). The ask path expands these; when no titled
// hub exists it returns nothing and the caller falls back to rank-based
// expansion. Same space_access anti-leak join. Empty queries return no rows.
func (s *Service) HubPages(ctx context.Context, userID int64, query string, spaceID *int64, limit int) ([]HubPage, error) {
	if limit <= 0 {
		limit = 8
	}
	qb := &queryBuilder{}
	uid := qb.arg(userID)
	orq := `replace(plainto_tsquery('english', ` + qb.arg(query) + `)::text, '&', '|')::tsquery`
	sql := `
		SELECT p.id, p.space_id, p.title,
		       (SELECT min(id) FROM page_chunks WHERE page_id = p.id),
		       count(pc.id)::int AS matches
		  FROM pages p
		  JOIN ` + accessibleSpacesSQL(uid) + ` sm
		    ON sm.space_id = p.space_id
		  LEFT JOIN page_chunks pc ON pc.page_id = p.id AND pc.content_tsv @@ ` + orq + `
		 WHERE p.deleted_at IS NULL
		   AND to_tsvector('english', p.title) @@ ` + orq
	if spaceID != nil {
		sql += ` AND p.space_id = ` + qb.arg(*spaceID)
	}
	sql += ` GROUP BY p.id, p.title ORDER BY matches DESC, p.id LIMIT ` + qb.arg(limit)

	rows, err := s.db.QueryContext(ctx, sql, qb.args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []HubPage
	for rows.Next() {
		var h HubPage
		if err := rows.Scan(&h.PageID, &h.SpaceID, &h.Title, &h.ChunkID, &h.Count); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}
