package semantic

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/zcag/tela/backend/internal/testdb"
)

func TestCommitPreviewRejectsTrashedSourcePageAndRollsBackIdempotency(t *testing.T) {
	d := testdb.New(t)
	ctx := context.Background()

	var ownerID, spaceID, pageID int64
	if err := d.QueryRow(`INSERT INTO users(username,password_hash) VALUES ('commit-trash-owner','x') RETURNING id`).Scan(&ownerID); err != nil {
		t.Fatal(err)
	}
	if err := d.QueryRow(`INSERT INTO spaces(name,slug) VALUES ('Commit Trash Test','commit-trash-test') RETURNING id`).Scan(&spaceID); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(`INSERT INTO space_members(space_id,user_id,role) VALUES ($1,$2,'owner')`, spaceID, ownerID); err != nil {
		t.Fatal(err)
	}

	title := "Фёдор Воронов — удаляемый источник"
	body := "Домой Фёдор Воронов вернулся в 1948 году."
	if err := d.QueryRow(`INSERT INTO pages(space_id,title,body) VALUES ($1,$2,$3) RETURNING id`, spaceID, title, body).Scan(&pageID); err != nil {
		t.Fatal(err)
	}

	signer, err := NewPreviewSigner([]byte("0123456789abcdef0123456789abcdef"), 30*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	fixedNow := time.Date(2026, 9, 8, 14, 0, 0, 0, time.UTC)
	signer.now = func() time.Time { return fixedNow }

	candidates := returnHomeCandidates(body, "Фёдор Воронов вернулся домой в 1948 году", 1948)
	token := mustSignPreview(t, signer, spaceID, pageID, title, body, candidates)

	if _, err := d.Exec(`UPDATE pages SET deleted_at=tela_now() WHERE id=$1`, pageID); err != nil {
		t.Fatal(err)
	}

	commitSvc, err := NewCommitService(d, signer)
	if err != nil {
		t.Fatal(err)
	}
	_, err = commitSvc.CommitPreview(ctx, ownerID, CommitInput{
		PreviewToken:   token,
		Candidates:     candidates,
		AcceptedKeys:   []string{"c:return"},
		EntityChoices:  map[string]EntityChoice{"e:fedor": {CreateNew: true}},
		IdempotencyKey: "commit-trashed-page",
	})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("trashed source commit error = %v, want ErrNotFound", err)
	}

	assertSemanticCoreEmpty(t, d, spaceID)

	var idemCount int
	if err := d.QueryRow(`SELECT count(*) FROM idempotency_keys WHERE user_id=$1 AND idem_key='commit-trashed-page'`, ownerID).Scan(&idemCount); err != nil {
		t.Fatal(err)
	}
	if idemCount != 0 {
		t.Fatalf("failed commit left %d idempotency rows; transaction should roll back", idemCount)
	}
}
