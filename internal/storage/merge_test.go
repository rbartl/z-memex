package storage

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"

	"gitlab.com/zollneritsch/memex/internal/memory"
)

// These tests write to whatever database MEMEX_TEST_POSTGRES points at, so they
// only run when it is set — and it must name a throwaway copy, never the real
// one. Create one with:
//
//	docker exec memex-postgres-1 psql -U memex -d postgres \
//	    -c "CREATE DATABASE memex_test TEMPLATE memex"
func testStore(t *testing.T) *Postgres {
	t.Helper()
	url := os.Getenv("MEMEX_TEST_POSTGRES")
	if url == "" {
		t.Skip("MEMEX_TEST_POSTGRES not set")
	}
	pg, err := NewPostgres(context.Background(), url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pg.Close)
	return pg
}

// seedMemory writes a memory the relations can point at; relations.memory_id is
// a real foreign key, so the edges need one to hang from.
func seedMemory(t *testing.T, pg *Postgres, content string) *memory.Memory {
	t.Helper()
	m := &memory.Memory{
		ID:          uuid.New(),
		Content:     content,
		ContentHash: uuid.NewString(),
		Source:      "merge_test",
		Importance:  3,
		CreatedAt:   time.Now(),
		UpdatedAt:   time.Now(),
	}
	if err := pg.SaveMemory(context.Background(), m, make([]float32, 768)); err != nil {
		t.Fatalf("save memory: %v", err)
	}
	t.Cleanup(func() { _ = pg.DeleteMemory(context.Background(), m.ID) })
	return m
}

func TestMergeEntitiesFoldsEdgesAndRecordsAlias(t *testing.T) {
	pg := testStore(t)
	ctx := context.Background()

	m := seedMemory(t, pg, "merge test fixture")

	// Two names for one person, plus a third entity both are connected to.
	loser, err := pg.UpsertEntity(ctx, "Merge Test Max "+uuid.NewString()[:8], "person")
	if err != nil {
		t.Fatal(err)
	}
	winner, err := pg.UpsertEntity(ctx, "Merge Test Maximilian "+uuid.NewString()[:8], "person")
	if err != nil {
		t.Fatal(err)
	}
	org, err := pg.UpsertEntity(ctx, "Merge Test Org "+uuid.NewString()[:8], "organization")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for _, id := range []uuid.UUID{loser, winner, org} {
			_, _ = pg.pool.Exec(ctx, `DELETE FROM entities WHERE id = $1`, id)
		}
	})

	// Both names carry the same fact — after the merge it must be one edge.
	for _, from := range []uuid.UUID{loser, winner} {
		if err := pg.UpsertRelation(ctx, from, org, "works-at", "works-for", m.ID, 0.9); err != nil {
			t.Fatal(err)
		}
	}
	// An edge between the two names themselves must not survive as a self-edge.
	if err := pg.UpsertRelation(ctx, loser, winner, "same-as", "is known as", m.ID, 1); err != nil {
		t.Fatal(err)
	}
	if err := pg.LinkMemoryEntity(ctx, m.ID, loser); err != nil {
		t.Fatal(err)
	}

	if err := pg.MergeEntities(ctx, loser, winner); err != nil {
		t.Fatalf("merge: %v", err)
	}

	var stillThere int
	if err := pg.pool.QueryRow(ctx, `SELECT count(*) FROM entities WHERE id = $1`, loser).Scan(&stillThere); err != nil {
		t.Fatal(err)
	}
	if stillThere != 0 {
		t.Error("loser entity survived the merge")
	}

	var selfEdges int
	if err := pg.pool.QueryRow(ctx,
		`SELECT count(*) FROM relations WHERE from_entity = $1 AND to_entity = $1`, winner).Scan(&selfEdges); err != nil {
		t.Fatal(err)
	}
	if selfEdges != 0 {
		t.Errorf("merge produced %d self-edges", selfEdges)
	}

	var worksAt int
	if err := pg.pool.QueryRow(ctx,
		`SELECT count(*) FROM relations WHERE from_entity = $1 AND to_entity = $2 AND relationship = 'works-at'`,
		winner, org).Scan(&worksAt); err != nil {
		t.Fatal(err)
	}
	if worksAt != 1 {
		t.Errorf("works-at edges after merge = %d, want 1", worksAt)
	}

	var linked int
	if err := pg.pool.QueryRow(ctx,
		`SELECT count(*) FROM memory_entities WHERE memory_id = $1 AND entity_id = $2`, m.ID, winner).Scan(&linked); err != nil {
		t.Fatal(err)
	}
	if linked != 1 {
		t.Errorf("memory link moved to winner = %d, want 1", linked)
	}
}

// The point of recording an alias is that the next extraction of the old name
// resolves to the survivor instead of splitting the node again.
func TestMergedNameResolvesToWinnerOnUpsert(t *testing.T) {
	pg := testStore(t)
	ctx := context.Background()

	oldName := "Merge Alias Old " + uuid.NewString()[:8]
	loser, err := pg.UpsertEntity(ctx, oldName, "person")
	if err != nil {
		t.Fatal(err)
	}
	winner, err := pg.UpsertEntity(ctx, "Merge Alias New "+uuid.NewString()[:8], "person")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = pg.pool.Exec(ctx, `DELETE FROM entities WHERE id = ANY($1)`, []uuid.UUID{loser, winner})
	})

	if err := pg.MergeEntities(ctx, loser, winner); err != nil {
		t.Fatalf("merge: %v", err)
	}

	// Same name, different spelling — the alias is stored normalized.
	got, err := pg.UpsertEntity(ctx, "  "+oldName+"  ", "person")
	if err != nil {
		t.Fatal(err)
	}
	if got != winner {
		t.Errorf("upsert of merged-away name = %v, want winner %v", got, winner)
	}
}

func TestMergeEntitiesRejectsSelfMerge(t *testing.T) {
	pg := testStore(t)
	ctx := context.Background()

	id, err := pg.UpsertEntity(ctx, "Merge Self "+uuid.NewString()[:8], "concept")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = pg.pool.Exec(ctx, `DELETE FROM entities WHERE id = $1`, id) })

	if err := pg.MergeEntities(ctx, id, id); err == nil {
		t.Error("merging an entity into itself was allowed")
	}
}
