//go:build integration

package specdata_test

import (
	"context"
	"fmt"
	"os"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/ory/dockertest/v4"

	"github.com/GreenOnGrey/hammurapi-core/internal/platform/postgres"
	"github.com/GreenOnGrey/hammurapi-core/internal/specdata"
)

var pool *pgxpool.Pool

// TestMain starts Postgres in Docker (dockertest) and applies migrations.
// Run with: go test -tags integration ./internal/specdata/
func TestMain(m *testing.M) {
	ctx := context.Background()
	dp, err := dockertest.NewPool(ctx, "", dockertest.WithMaxWait(2*time.Minute))
	if err != nil {
		fmt.Println("docker unavailable:", err)
		os.Exit(1)
	}
	res, err := dp.Run(ctx, "postgres", dockertest.WithTag("16"), dockertest.WithoutReuse(),
		dockertest.WithEnv([]string{"POSTGRES_PASSWORD=pw", "POSTGRES_DB=hammurapi"}))
	if err != nil {
		fmt.Println("start postgres:", err)
		_ = dp.Close(ctx)
		os.Exit(1)
	}
	url := fmt.Sprintf("postgres://postgres:pw@%s/hammurapi?sslmode=disable", res.GetHostPort("5432/tcp"))
	if err := dp.Retry(ctx, time.Minute, func() error {
		var err error
		if pool, err = postgres.Connect(ctx, url); err != nil {
			return err
		}
		return pool.Ping(ctx)
	}); err != nil {
		fmt.Println("postgres not ready:", err)
		_ = dp.Close(ctx)
		os.Exit(1)
	}
	if err := postgres.Migrate(ctx, url); err != nil {
		fmt.Println("migrate:", err)
		_ = dp.Close(ctx)
		os.Exit(1)
	}
	code := m.Run()
	pool.Close()
	_ = dp.Close(ctx) // removes all tracked containers
	os.Exit(code)
}

func seed(t *testing.T) (userA, userB, systemID uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	for _, u := range []*uuid.UUID{&userA, &userB} {
		name := uuid.NewString()[:8]
		if err := pool.QueryRow(ctx, `INSERT INTO users (provider_uid, username, display_name, agent_name, agent_tone)
			VALUES ($1,$1,$1,'Codex','business') RETURNING id`, name).Scan(u); err != nil {
			t.Fatal(err)
		}
	}
	key := fmt.Sprintf("D%d", time.Now().UnixNano()%1000000)
	if err := pool.QueryRow(ctx, `WITH d AS (INSERT INTO domains (key, name) VALUES ($1, 'D') RETURNING id)
		INSERT INTO systems (domain_id, key, name) SELECT id, 'SYS', 'S' FROM d RETURNING id`, key).Scan(&systemID); err != nil {
		t.Fatal(err)
	}
	return
}

// FEAT-02: 20 parallel creations get 20 unique consecutive numbers.
func TestNextNumberConcurrent(t *testing.T) {
	_, _, sys := seed(t)
	store := specdata.NewPG(pool)
	var mu sync.Mutex
	var got []int
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := store.InTx(context.Background(), func(tx specdata.Store) error {
				n, err := tx.NextNumber(context.Background(), sys)
				if err != nil {
					return err
				}
				time.Sleep(5 * time.Millisecond) // simulated git call while the row is locked
				mu.Lock()
				got = append(got, n)
				mu.Unlock()
				return nil
			})
			if err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	sort.Ints(got)
	for i, n := range got {
		if n != i+1 {
			t.Fatalf("numbers %v", got)
		}
	}
}

// FEAT-03: a rolled back creation does not spend the number.
func TestNumberNotSpentOnRollback(t *testing.T) {
	_, _, sys := seed(t)
	store := specdata.NewPG(pool)
	_ = store.InTx(context.Background(), func(tx specdata.Store) error {
		_, _ = tx.NextNumber(context.Background(), sys)
		return fmt.Errorf("git failed")
	})
	n, err := store.PeekNumber(context.Background(), sys)
	if err != nil || n != 0 {
		t.Fatalf("got %d %v", n, err)
	}
}

// LOCK-01 / LOCK-02: the lock belongs to the first editor.
func TestLocks(t *testing.T) {
	a, b, sys := seed(t)
	ctx := context.Background()
	store := specdata.NewPG(pool)
	f := &specdata.Feature{UniqueID: "L.OCK-" + uuid.NewString()[:4], SystemID: sys, Number: 1, Title: "t", Branch: "b", PRNumber: 1, PRURL: "u", CreatedBy: a}
	if err := store.InsertFeature(ctx, f); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := store.AcquireLock(ctx, f.ID, a); err != nil || !ok {
		t.Fatalf("A: %v %v", ok, err)
	}
	l, ok, err := store.AcquireLock(ctx, f.ID, b)
	if err != nil || ok || l.LockedBy != a {
		t.Fatalf("B took A's lock: %v %v %+v", ok, err, l)
	}
	if _, ok, _ := store.AcquireLock(ctx, f.ID, a); !ok {
		t.Fatal("A cannot extend")
	}
	// LOCK-03: an expired lock can be taken over.
	if _, err := pool.Exec(ctx, `UPDATE feature_locks SET expires_at = now() - interval '1 minute' WHERE feature_id = $1`, f.ID); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := store.AcquireLock(ctx, f.ID, b); !ok {
		t.Fatal("B cannot take an expired lock")
	}
}
