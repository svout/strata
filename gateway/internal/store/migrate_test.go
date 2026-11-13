// External test package: testutil imports store (to migrate test databases),
// so an in-package test importing testutil would be an import cycle.
package store_test

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/pressly/goose/v3/lock"

	"github.com/svout/strata/gateway/internal/idem"
	"github.com/svout/strata/gateway/internal/store"
	"github.com/svout/strata/gateway/internal/testutil"
)

var schemaTables = []string{"accounts", "payments", "ledger_entries", "idempotency_keys", "fake_rail_operations"}

// schemaRelations is every table and index the migrations create; to_regclass
// (tableExists) finds indexes as well as tables.
var schemaRelations = slices.Concat(schemaTables, []string{"idempotency_keys_created_at_idx", "ledger_entries_rail_ref_leg_key", "payments_settling_idx", "accounts_clearing_owner_key"})

// PostgreSQL SQLSTATE codes the schema's constraints raise.
const (
	notNullViolation    = "23502"
	foreignKeyViolation = "23503"
	uniqueViolation     = "23505"
	checkViolation      = "23514"
)

func TestUpAppliesSchema(t *testing.T) {
	t.Parallel()
	pool := testutil.Postgres(t) // already migrated with store.Up
	ctx := testContext(t)

	var version int64
	if err := pool.QueryRow(ctx, "SELECT max(version_id) FROM goose_db_version").Scan(&version); err != nil {
		t.Fatalf("read goose version: %v", err)
	}
	if version != store.LatestVersion() {
		t.Errorf("schema version = %d, want %d", version, store.LatestVersion())
	}
	// 00007: keyed by the hash, and the CHECK that ties the hash to the key.
	for name, want := range map[string]string{
		"idempotency_keys_pkey":             "PRIMARY KEY (key_hash)",
		"idempotency_keys_key_hash_matches": "CHECK ((key_hash = sha256(",
	} {
		var def string
		if err := pool.QueryRow(ctx, "SELECT pg_get_constraintdef(oid) FROM pg_constraint WHERE conname = $1", name).Scan(&def); err != nil || !strings.HasPrefix(def, want) {
			t.Errorf("%s = %q (%v), want %s...", name, def, err, want)
		}
	}
	var indexdef string
	if err := pool.QueryRow(ctx, "SELECT indexdef FROM pg_indexes WHERE indexname = 'idempotency_keys_created_at_idx'").Scan(&indexdef); err != nil || !strings.HasSuffix(indexdef, "(created_at)") {
		t.Errorf("idempotency_keys_created_at_idx = %q (%v), want an index on (created_at)", indexdef, err)
	}
	for _, rel := range schemaRelations {
		if !tableExists(ctx, t, pool, rel) {
			t.Errorf("%s missing after Up", rel)
		}
	}
	// 00005 seeds exactly one clearing account per rail.
	if got := clearingOwners(ctx, t, pool); !slices.Equal(got, []string{"rail:fake", "rail:internal"}) {
		t.Errorf("seeded clearing owners = %v, want [rail:fake rail:internal]", got)
	}

	// Up is idempotent: a second run has nothing to apply and must not fail.
	if err := store.Up(ctx, sqlDB(t, pool)); err != nil {
		t.Errorf("second Up: %v", err)
	}
}

func TestDownRemovesSchemaAndUpRestoresIt(t *testing.T) {
	t.Parallel()
	pool := testutil.Postgres(t)
	ctx := testContext(t)
	db := sqlDB(t, pool)

	if err := store.Down(ctx, db); err != nil {
		t.Fatalf("Down: %v", err)
	}
	for _, rel := range schemaRelations {
		if tableExists(ctx, t, pool, rel) {
			t.Errorf("%s still exists after Down", rel)
		}
	}

	if err := store.Up(ctx, db); err != nil {
		t.Fatalf("Up after Down: %v", err)
	}
	for _, rel := range schemaRelations {
		if !tableExists(ctx, t, pool, rel) {
			t.Errorf("%s missing after Up following Down", rel)
		}
	}
	// 00005's Down removed its seeds, so Up reseeds them once, not twice.
	if got := clearingOwners(ctx, t, pool); !slices.Equal(got, []string{"rail:fake", "rail:internal"}) {
		t.Errorf("clearing owners after Down and Up = %v, want [rail:fake rail:internal]", got)
	}
}

// TestLatestVersion: the version derived from the embedded file names is
// the one goose itself would migrate to (its own list of sources), and the
// one a database migrated by store.Up reports.
func TestLatestVersion(t *testing.T) {
	t.Parallel()
	pool := testutil.Postgres(t)
	p, err := goose.NewProvider(goose.DialectPostgres, sqlDB(t, pool), os.DirFS("migrations"))
	if err != nil {
		t.Fatal(err)
	}
	var want int64
	for _, src := range p.ListSources() {
		want = max(want, src.Version)
	}
	if got := store.LatestVersion(); got != want || want < 7 {
		t.Errorf("LatestVersion = %d, want %d (goose's sources, at least 7)", got, want)
	}
	if got, err := store.SchemaVersion(testContext(t), sqlDB(t, pool)); err != nil || got != want {
		t.Errorf("a migrated database's SchemaVersion = %d, %v; want %d", got, err, want)
	}
}

// SchemaVersion reads goose's version without migrating anything: the
// highest version recorded (store.LatestVersion after Up, 4 once the rows
// above it are gone), and 0 with no version table, which it must not create
// (the gateway calls it and never migrates).
func TestSchemaVersion(t *testing.T) {
	t.Parallel()
	pool := testutil.Postgres(t)
	ctx := testContext(t)
	db := sqlDB(t, pool)

	read := func(want int64) {
		t.Helper()
		if got, err := store.SchemaVersion(ctx, db); err != nil || got != want {
			t.Errorf("SchemaVersion = %d, %v; want %d", got, err, want)
		}
	}
	read(store.LatestVersion())
	// A database migrated to 4 only, as the gateway's startup sees it before
	// 00005: the version is the highest row.
	mustExec(ctx, t, pool, "DELETE FROM goose_db_version WHERE version_id >= 5")
	read(4)
	mustExec(ctx, t, pool, "DROP TABLE goose_db_version")
	read(0)
	if tableExists(ctx, t, pool, "goose_db_version") {
		t.Error("SchemaVersion created goose_db_version; it must only read")
	}
}

// An unreachable database gives ErrSchemaVersion with fixed text: nothing
// from the DSN (AC8).
func TestSchemaVersion_UnreachableHidesDSN(t *testing.T) {
	t.Parallel()
	db, err := sql.Open("pgx", "postgres://leakuser:leakpass@127.0.0.1:1/leakdb?connect_timeout=1")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	_, err = store.SchemaVersion(testContext(t), db)
	if !errors.Is(err, store.ErrSchemaVersion) {
		t.Fatalf("SchemaVersion = %v, want ErrSchemaVersion", err)
	}
	for _, s := range []string{"leakuser", "leakpass", "leakdb", "127.0.0.1"} {
		if strings.Contains(err.Error(), s) {
			t.Errorf("error %q contains %q", err, s)
		}
	}
}

// Down past 00005 refuses to discard postings: once a ledger entry
// references a seeded clearing account, deleting it fails on the foreign key
// and the migration rolls back, leaving the schema at version 5. (Down runs
// newest first, so 00007 and 00006 have already been rolled back by then;
// only 00005's own transaction is undone.)
func TestDownKeepsClearingAccountsWithEntries(t *testing.T) {
	t.Parallel()
	pool := testutil.Postgres(t)
	ctx := testContext(t)

	// The payment is payer -> payee, as under ADR-123; only the ledger entry
	// touches the clearing account. (A payment *to* the clearing account would
	// make Down fail on payments' payee foreign key instead, and this test
	// would pass without proving anything about postings.)
	payer := insertID(ctx, t, pool, `INSERT INTO accounts (kind, currency) VALUES ('user', 'GBP') RETURNING id::text`)
	payee := insertID(ctx, t, pool, `INSERT INTO accounts (kind, currency) VALUES ('merchant', 'GBP') RETURNING id::text`)
	clearing := insertID(ctx, t, pool, `SELECT id::text FROM accounts WHERE owner = 'rail:internal'`)
	payment := insertID(ctx, t, pool,
		`INSERT INTO payments (payer_account_id, payee_account_id, amount, currency, rail)
		 VALUES ($1, $2, 100, 'GBP', 'internal') RETURNING id::text`, payer, payee)
	mustExec(ctx, t, pool,
		`INSERT INTO ledger_entries (payment_id, account_id, direction, amount) VALUES ($1, $2, 'debit', 100), ($1, $3, 'credit', 100)`,
		payment, payer, clearing)

	err := store.Down(ctx, sqlDB(t, pool))
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != foreignKeyViolation || pgErr.ConstraintName != "ledger_entries_account_id_fkey" {
		t.Fatalf("Down with entries on a seeded clearing account = %v, want a foreign key violation on ledger_entries_account_id_fkey", err)
	}
	var version int64
	if err := pool.QueryRow(ctx, "SELECT max(version_id) FROM goose_db_version").Scan(&version); err != nil || version != 5 {
		t.Errorf("schema version after failed Down = %d (%v), want 5", version, err)
	}
	if got := clearingOwners(ctx, t, pool); len(got) != 2 {
		t.Errorf("clearing owners after failed Down = %v, want both still there", got)
	}
	// Down drops the CHECK before it reaches the failing DELETE, so its
	// survival shows the whole migration rolled back, not just the DELETE.
	var checks int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM pg_constraint WHERE conname = 'payments_rail_check'").Scan(&checks); err != nil || checks != 1 {
		t.Errorf("payments_rail_check after failed Down: %d (%v), want 1", checks, err)
	}
}

// clearingOwners lists the owners of clearing accounts that have one, sorted.
func clearingOwners(ctx context.Context, t *testing.T, pool *pgxpool.Pool) []string {
	t.Helper()
	rows, err := pool.Query(ctx, "SELECT owner FROM accounts WHERE kind = 'clearing' AND owner IS NOT NULL ORDER BY owner")
	if err != nil {
		t.Fatalf("list clearing owners: %v", err)
	}
	owners, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("list clearing owners: %v", err)
	}
	return owners
}

// TestConcurrentUpIsSerialised starts several Up runs at once against an empty
// schema. goose's advisory lock lets one apply every version while the
// others wait and then find nothing pending, so all succeed, each version is
// recorded exactly once, and the lock is released afterwards.
func TestConcurrentUpIsSerialised(t *testing.T) {
	t.Parallel()
	pool := testutil.Postgres(t)
	ctx := testContext(t)
	db := sqlDB(t, pool)

	if err := store.Down(ctx, db); err != nil {
		t.Fatalf("Down: %v", err)
	}

	// 4 runs: each holds up to two connections (the lock session plus a
	// pending check), and the test pool is capped at 8.
	const runs = 4
	errs := make([]error, runs)
	var wg sync.WaitGroup
	for i := range runs {
		wg.Go(func() { errs[i] = store.Up(ctx, db) })
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Errorf("Up run %d: %v", i, err)
		}
	}

	for _, v := range []int{1, 2, 3} {
		var applied int
		if err := pool.QueryRow(ctx, "SELECT count(*) FROM goose_db_version WHERE version_id = $1", v).Scan(&applied); err != nil {
			t.Fatalf("count version rows: %v", err)
		}
		if applied != 1 {
			t.Errorf("goose_db_version rows for version %d = %d, want 1", v, applied)
		}
	}
	if n := advisoryLocks(ctx, t, pool); n != 0 {
		t.Errorf("%d advisory locks left in this database after Up, want 0", n)
	}
}

// TestUpWaitsForHeldLock holds goose's lock on another session, as a
// concurrent migrator would, and checks that Up applies nothing until the lock
// is released and then completes.
func TestUpWaitsForHeldLock(t *testing.T) {
	t.Parallel()
	pool := testutil.Postgres(t)
	ctx := testContext(t)
	db := sqlDB(t, pool)

	if err := store.Down(ctx, db); err != nil {
		t.Fatalf("Down: %v", err)
	}

	holder, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire holder connection: %v", err)
	}
	defer holder.Release()
	if _, err := holder.Exec(ctx, "SELECT pg_advisory_lock($1)", lock.DefaultLockID); err != nil {
		t.Fatalf("take goose lock: %v", err)
	}

	done := make(chan error, 1)
	go func() { done <- store.Up(ctx, db) }()

	// Longer than the 1 s lock retry period, so Up has polled at least twice.
	select {
	case err := <-done:
		t.Fatalf("Up returned (%v) while the lock was held", err)
	case <-time.After(2500 * time.Millisecond):
	}
	for _, table := range schemaTables {
		if tableExists(ctx, t, pool, table) {
			t.Errorf("table %s created while the lock was held", table)
		}
	}

	if _, err := holder.Exec(ctx, "SELECT pg_advisory_unlock($1)", lock.DefaultLockID); err != nil {
		t.Fatalf("release goose lock: %v", err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Up after the lock was released: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Up did not finish within 10 s of the lock being released")
	}
	for _, table := range schemaTables {
		if !tableExists(ctx, t, pool, table) {
			t.Errorf("table %s missing after Up", table)
		}
	}
}

// 00007's backfill gives rows written before it the hash that idem looks
// keys up by: a completed key stored at version 6 is still found and
// replayed after the migration, not claimed a second time. This is the
// backfill-to-Go parity check; idem's TestKeyHash_MatchesSchemaCheck covers
// new rows.
//
// It drives goose itself to stop at version 6, which store's API doesn't
// offer (production only moves forward). It reads the same files from disk
// (go test runs in the package directory) rather than adding a test-only
// export of store's embedded FS.
func TestMigration7BackfillsKeyHash(t *testing.T) {
	t.Parallel()
	pool := testutil.Postgres(t)
	ctx := testContext(t)
	db := sqlDB(t, pool)
	p, err := goose.NewProvider(goose.DialectPostgres, db, os.DirFS("migrations"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.DownTo(ctx, 6); err != nil {
		t.Fatalf("down to 6: %v", err)
	}
	// 00007's Down restores the old key.
	var pkey string
	if err := pool.QueryRow(ctx, "SELECT pg_get_constraintdef(oid) FROM pg_constraint WHERE conname = 'idempotency_keys_pkey'").Scan(&pkey); err != nil || pkey != "PRIMARY KEY (scope, key)" {
		t.Errorf("primary key at version 6 = %q (%v), want PRIMARY KEY (scope, key)", pkey, err)
	}

	keys := []idem.Key{{Value: "old-1"}, {Scope: "agent-a", Value: "old-1"}, {Scope: "agent-ü", Value: "ключ-€"}}
	reqHash := idem.Hash(idem.Request{Method: "POST", Path: "/v1/payments", Body: []byte(`{}`)})
	for i, k := range keys {
		mustExec(ctx, t, pool,
			`INSERT INTO idempotency_keys (scope, key, request_hash, status, response_code, response_body) VALUES ($1, $2, $3, 'completed', 201, $4)`,
			k.Scope, k.Value, reqHash[:], fmt.Appendf(nil, `{"n":%d}`, i))
	}
	if err := store.Up(ctx, db); err != nil {
		t.Fatalf("up to 7: %v", err)
	}

	for i, k := range keys {
		var resp idem.Response
		var found bool
		err := store.Serializable(ctx, pool, func(tx pgx.Tx) error {
			var err error
			resp, found, err = idem.Claim(ctx, tx, k, reqHash)
			return err
		})
		if want := fmt.Sprintf(`{"n":%d}`, i); err != nil || !found || string(resp.Body) != want {
			t.Errorf("%+v after 00007: Claim = %s, found %v, %v; want the stored %s", k, resp.Body, found, err, want)
		}
	}
}

// TestSchemaConstraints inserts one valid row per table, then checks that each
// kind of bad row is rejected by the database itself with the expected
// SQLSTATE, independent of any Go validation added later.
func TestSchemaConstraints(t *testing.T) {
	t.Parallel()
	pool := testutil.Postgres(t)
	ctx := testContext(t)

	// Valid baseline rows. A clearing account made by hand may have no owner
	// (NULL); the ones 00005 seeds are owned by 'rail:<name>'.
	payer := insertID(ctx, t, pool, `INSERT INTO accounts (owner, kind, currency) VALUES ('user-1', 'user', 'GBP') RETURNING id::text`)
	payee := insertID(ctx, t, pool, `INSERT INTO accounts (owner, kind, currency) VALUES ('merchant-1', 'merchant', 'GBP') RETURNING id::text`)
	insertID(ctx, t, pool, `INSERT INTO accounts (kind, currency) VALUES ('clearing', 'GBP') RETURNING id::text`)
	payment := insertID(ctx, t, pool,
		`INSERT INTO payments (payer_account_id, payee_account_id, amount, currency, rail)
		 VALUES ($1, $2, 1250, 'GBP', 'internal') RETURNING id::text`, payer, payee)
	mustExec(ctx, t, pool,
		`INSERT INTO ledger_entries (payment_id, account_id, direction, amount) VALUES ($1, $2, 'debit', 1250), ($1, $3, 'credit', 1250)`,
		payment, payer, payee)
	mustExec(ctx, t, pool,
		`INSERT INTO idempotency_keys (key_hash, key, request_hash, status) VALUES ($1, 'key-1', $2, 'in_progress')`, keyHash("", "key-1"), hash(32))
	// The same key under another scope is a different record (P2 per-agent scoping).
	mustExec(ctx, t, pool,
		`INSERT INTO idempotency_keys (key_hash, scope, key, request_hash, status) VALUES ($1, 'agent-a', 'key-1', $2, 'in_progress')`, keyHash("agent-a", "key-1"), hash(32))
	// A completed key carries the response to replay.
	mustExec(ctx, t, pool,
		`INSERT INTO idempotency_keys (key_hash, key, request_hash, status, response_code, response_body) VALUES ($1, 'key-done', $2, 'completed', 201, $3)`,
		keyHash("", "key-done"), hash(32), []byte(`{"id":"p1"}`))

	// 00005 accepts as well as rejects: without these, a CHECK or index with
	// a typo that blocks everything would pass the rejection cases below.
	// Another kind may share a rail-shaped owner (the unique index is partial
	// on kind = 'clearing'), and both rails are valid on a payment.
	mustExec(ctx, t, pool, `INSERT INTO accounts (owner, kind, currency) VALUES ('rail:internal', 'merchant', 'GBP')`)
	for _, railName := range []string{"internal", "fake"} {
		mustExec(ctx, t, pool,
			`INSERT INTO payments (payer_account_id, payee_account_id, amount, currency, rail) VALUES ($1, $2, 100, 'GBP', $3)`,
			payer, payee, railName)
	}

	var state string
	if err := pool.QueryRow(ctx, `SELECT state FROM payments WHERE id = $1`, payment).Scan(&state); err != nil {
		t.Fatalf("read payment state: %v", err)
	}
	if state != "created" {
		t.Errorf("default payment state = %q, want created", state)
	}

	// Both failure reasons are accepted on a failed payment (00004).
	var failedPayment string
	for _, reason := range []string{"declined", "invalid_request"} {
		failedPayment = insertID(ctx, t, pool,
			`INSERT INTO payments (payer_account_id, payee_account_id, amount, currency, rail, state, failure_reason)
			 VALUES ($1, $2, 1250, 'GBP', 'internal', 'failed', $3) RETURNING id::text`, payer, payee, reason)
	}

	// 00006: the fake PSP's records. Each outcome is accepted, with a reason
	// exactly when declined, and a 255-character reference is the upper
	// bound, so a CHECK that blocked everything fails here.
	mustExec(ctx, t, pool,
		`INSERT INTO fake_rail_operations (ref, outcome, reason, moves) VALUES
		 ('pay_settled', 'settled', NULL, 1), ('pay_pending', 'pending', NULL, 0), ('pay_declined', 'declined', 'insufficient_funds', 0), ($1, 'settled', NULL, 1)`,
		strings.Repeat("r", 255))

	const missing = "00000000-0000-0000-0000-000000000000" // no row has this id

	tests := []struct {
		name     string
		sql      string
		args     []any
		wantCode string
	}{
		// accounts
		{"account bad kind", `INSERT INTO accounts (kind, currency) VALUES ('bank', 'GBP')`, nil, checkViolation},
		{"account non-GBP", `INSERT INTO accounts (kind, currency) VALUES ('user', 'USD')`, nil, checkViolation},
		{"account no kind", `INSERT INTO accounts (currency) VALUES ('GBP')`, nil, notNullViolation},
		// 00005: one clearing account per owner; other kinds may share one.
		{"account second clearing for a rail", `INSERT INTO accounts (owner, kind, currency) VALUES ('rail:internal', 'clearing', 'GBP')`, nil, uniqueViolation},

		// payments
		{"payment zero amount", `INSERT INTO payments (payer_account_id, payee_account_id, amount, currency, rail) VALUES ($1, $2, 0, 'GBP', 'internal')`, []any{payer, payee}, checkViolation},
		{"payment negative amount", `INSERT INTO payments (payer_account_id, payee_account_id, amount, currency, rail) VALUES ($1, $2, -5, 'GBP', 'internal')`, []any{payer, payee}, checkViolation},
		{"payment non-GBP", `INSERT INTO payments (payer_account_id, payee_account_id, amount, currency, rail) VALUES ($1, $2, 100, 'EUR', 'internal')`, []any{payer, payee}, checkViolation},
		{"payment bad state", `INSERT INTO payments (payer_account_id, payee_account_id, amount, currency, rail, state) VALUES ($1, $2, 100, 'GBP', 'internal', 'pending')`, []any{payer, payee}, checkViolation},
		{"payment no rail", `INSERT INTO payments (payer_account_id, payee_account_id, amount, currency) VALUES ($1, $2, 100, 'GBP')`, []any{payer, payee}, notNullViolation},
		// 00005: only the rails that exist (rail.InternalName, rail.FakeName).
		{"payment unknown rail", `INSERT INTO payments (payer_account_id, payee_account_id, amount, currency, rail) VALUES ($1, $2, 100, 'GBP', 'stripe')`, []any{payer, payee}, checkViolation},
		{"payment dangling payer", `INSERT INTO payments (payer_account_id, payee_account_id, amount, currency, rail) VALUES ($1, $2, 100, 'GBP', 'internal')`, []any{missing, payee}, foreignKeyViolation},
		{"payment to itself", `INSERT INTO payments (payer_account_id, payee_account_id, amount, currency, rail) VALUES ($1, $1, 100, 'GBP', 'internal')`, []any{payer}, checkViolation},
		{"payment dangling payee", `INSERT INTO payments (payer_account_id, payee_account_id, amount, currency, rail) VALUES ($1, $2, 100, 'GBP', 'internal')`, []any{payer, missing}, foreignKeyViolation},
		// 00004: failure_reason is one of two values, set exactly when failed.
		{"payment failed without reason", `INSERT INTO payments (payer_account_id, payee_account_id, amount, currency, rail, state) VALUES ($1, $2, 100, 'GBP', 'internal', 'failed')`, []any{payer, payee}, checkViolation},
		{"payment settled with reason", `INSERT INTO payments (payer_account_id, payee_account_id, amount, currency, rail, state, failure_reason) VALUES ($1, $2, 100, 'GBP', 'internal', 'settled', 'declined')`, []any{payer, payee}, checkViolation},
		{"payment bad failure reason", `INSERT INTO payments (payer_account_id, payee_account_id, amount, currency, rail, state, failure_reason) VALUES ($1, $2, 100, 'GBP', 'internal', 'failed', 'timeout')`, []any{payer, payee}, checkViolation},
		{"payment unfailed keeps reason", `UPDATE payments SET state = 'settled' WHERE id = $1`, []any{failedPayment}, checkViolation},

		// ledger_entries
		{"entry bad direction", `INSERT INTO ledger_entries (payment_id, account_id, direction, amount) VALUES ($1, $2, 'up', 100)`, []any{payment, payer}, checkViolation},
		{"entry zero amount", `INSERT INTO ledger_entries (payment_id, account_id, direction, amount) VALUES ($1, $2, 'debit', 0)`, []any{payment, payer}, checkViolation},
		{"entry no payment", `INSERT INTO ledger_entries (account_id, direction, amount) VALUES ($1, 'debit', 100)`, []any{payer}, notNullViolation},
		{"entry no direction", `INSERT INTO ledger_entries (payment_id, account_id, amount) VALUES ($1, $2, 100)`, []any{payment, payer}, notNullViolation},
		{"entry dangling payment", `INSERT INTO ledger_entries (payment_id, account_id, direction, amount) VALUES ($1, $2, 'debit', 100)`, []any{missing, payer}, foreignKeyViolation},
		{"entry dangling account", `INSERT INTO ledger_entries (payment_id, account_id, direction, amount) VALUES ($1, $2, 'debit', 100)`, []any{payment, missing}, foreignKeyViolation},

		// fake_rail_operations (00006)
		{"fake op duplicate ref", `INSERT INTO fake_rail_operations (ref, outcome, moves) VALUES ('pay_settled', 'settled', 1)`, nil, uniqueViolation},
		{"fake op empty ref", `INSERT INTO fake_rail_operations (ref, outcome, moves) VALUES ('', 'settled', 1)`, nil, checkViolation},
		{"fake op ref too long", `INSERT INTO fake_rail_operations (ref, outcome, moves) VALUES ($1, 'settled', 1)`, []any{strings.Repeat("r", 256)}, checkViolation},
		{"fake op bad outcome", `INSERT INTO fake_rail_operations (ref, outcome, moves) VALUES ('pay_x1', 'refunded', 0)`, nil, checkViolation},
		{"fake op declined without reason", `INSERT INTO fake_rail_operations (ref, outcome, moves) VALUES ('pay_x2', 'declined', 0)`, nil, checkViolation},
		{"fake op settled with reason", `INSERT INTO fake_rail_operations (ref, outcome, reason, moves) VALUES ('pay_x3', 'settled', 'declined', 1)`, nil, checkViolation},
		{"fake op empty reason", `INSERT INTO fake_rail_operations (ref, outcome, reason, moves) VALUES ('pay_x4', 'declined', '', 0)`, nil, checkViolation},
		{"fake op negative moves", `INSERT INTO fake_rail_operations (ref, outcome, moves) VALUES ('pay_x5', 'settled', -1)`, nil, checkViolation},
		{"fake op no moves", `INSERT INTO fake_rail_operations (ref, outcome) VALUES ('pay_x6', 'settled')`, nil, notNullViolation},
		{"fake op no outcome", `INSERT INTO fake_rail_operations (ref, moves) VALUES ('pay_x7', 0)`, nil, notNullViolation},
		{"fake op resolve keeps reason", `UPDATE fake_rail_operations SET outcome = 'settled' WHERE ref = 'pay_declined'`, nil, checkViolation},

		// idempotency_keys. Each row carries its key's correct key_hash
		// unless the case is about key_hash, so the constraint under test is
		// the one that fires (NOT NULL is checked before any CHECK).
		{"idempotency duplicate scope+key", `INSERT INTO idempotency_keys (key_hash, key, request_hash, status) VALUES ($1, 'key-1', $2, 'in_progress')`, []any{keyHash("", "key-1"), hash(32)}, uniqueViolation},
		{"idempotency empty key", `INSERT INTO idempotency_keys (key_hash, key, request_hash, status) VALUES ($1, '', $2, 'in_progress')`, []any{keyHash("", ""), hash(32)}, checkViolation},
		{"idempotency key too long", `INSERT INTO idempotency_keys (key_hash, key, request_hash, status) VALUES ($1, $2, $3, 'in_progress')`, []any{keyHash("", strings.Repeat("k", 256)), strings.Repeat("k", 256), hash(32)}, checkViolation},
		{"idempotency short hash", `INSERT INTO idempotency_keys (key_hash, key, request_hash, status) VALUES ($1, 'key-2', $2, 'in_progress')`, []any{keyHash("", "key-2"), hash(31)}, checkViolation},
		{"idempotency bad status", `INSERT INTO idempotency_keys (key_hash, key, request_hash, status) VALUES ($1, 'key-3', $2, 'done')`, []any{keyHash("", "key-3"), hash(32)}, checkViolation},
		{"idempotency completed without response", `INSERT INTO idempotency_keys (key_hash, key, request_hash, status) VALUES ($1, 'key-4', $2, 'completed')`, []any{keyHash("", "key-4"), hash(32)}, checkViolation},
		{"idempotency in_progress with response", `INSERT INTO idempotency_keys (key_hash, key, request_hash, status, response_code) VALUES ($1, 'key-5', $2, 'in_progress', 201)`, []any{keyHash("", "key-5"), hash(32)}, checkViolation},
		{"idempotency response code 99", `INSERT INTO idempotency_keys (key_hash, key, request_hash, status, response_code) VALUES ($1, 'key-6', $2, 'completed', 99)`, []any{keyHash("", "key-6"), hash(32)}, checkViolation},
		{"idempotency response code 600", `INSERT INTO idempotency_keys (key_hash, key, request_hash, status, response_code) VALUES ($1, 'key-7', $2, 'completed', 600)`, []any{keyHash("", "key-7"), hash(32)}, checkViolation},
		// 00007: the hash must be present and must be the row's own key's.
		{"idempotency no key_hash", `INSERT INTO idempotency_keys (key, request_hash, status) VALUES ('key-8', $1, 'in_progress')`, []any{hash(32)}, notNullViolation},
		{"idempotency another key's hash", `INSERT INTO idempotency_keys (key_hash, key, request_hash, status) VALUES ($1, 'key-9', $2, 'in_progress')`, []any{keyHash("", "key-1x"), hash(32)}, checkViolation},
		// The encoding without length prefixes, i.e. a drifted keyHash.
		{"idempotency unprefixed hash", `INSERT INTO idempotency_keys (key_hash, key, request_hash, status) VALUES (sha256('key-10'::bytea), 'key-10', $1, 'in_progress')`, []any{hash(32)}, checkViolation},
		// Same key, other scope: the scope is part of the hash.
		{"idempotency hash of another scope", `INSERT INTO idempotency_keys (key_hash, scope, key, request_hash, status) VALUES ($1, 'agent-b', 'key-11', $2, 'in_progress')`, []any{keyHash("", "key-11"), hash(32)}, checkViolation},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := pool.Exec(ctx, tt.sql, tt.args...)
			var pgErr *pgconn.PgError
			if !errors.As(err, &pgErr) {
				t.Fatalf("got err %v, want PostgreSQL error %s", err, tt.wantCode)
			}
			if pgErr.Code != tt.wantCode {
				t.Errorf("SQLSTATE = %s (%s), want %s", pgErr.Code, pgErr.Message, tt.wantCode)
			}
		})
	}

	// A 255-character key is the upper bound and must be accepted.
	mustExec(ctx, t, pool,
		`INSERT INTO idempotency_keys (key_hash, key, request_hash, status) VALUES ($1, $2, $3, 'in_progress')`,
		keyHash("", strings.Repeat("k", 255)), strings.Repeat("k", 255), hash(32))
}

func testContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// sqlDB wraps pool for goose, which works over database/sql.
func sqlDB(t *testing.T, pool *pgxpool.Pool) *sql.DB {
	t.Helper()
	db := stdlib.OpenDBFromPool(pool)
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// advisoryLocks counts advisory locks held in the test's own database. Advisory
// locks are scoped per database, and other parallel tests share the server.
func advisoryLocks(ctx context.Context, t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()
	var n int
	err := pool.QueryRow(ctx,
		`SELECT count(*) FROM pg_locks l JOIN pg_database d ON d.oid = l.database
		 WHERE l.locktype = 'advisory' AND d.datname = current_database()`).Scan(&n)
	if err != nil {
		t.Fatalf("count advisory locks: %v", err)
	}
	return n
}

func tableExists(ctx context.Context, t *testing.T, pool *pgxpool.Pool, table string) bool {
	t.Helper()
	var exists bool
	if err := pool.QueryRow(ctx, "SELECT to_regclass('public.' || $1) IS NOT NULL", table).Scan(&exists); err != nil {
		t.Fatalf("check table %s: %v", table, err)
	}
	return exists
}

func insertID(ctx context.Context, t *testing.T, pool *pgxpool.Pool, sql string, args ...any) string {
	t.Helper()
	var id string
	if err := pool.QueryRow(ctx, sql, args...).Scan(&id); err != nil {
		t.Fatalf("insert valid row: %v", err)
	}
	return id
}

func mustExec(ctx context.Context, t *testing.T, pool *pgxpool.Pool, sql string, args ...any) {
	t.Helper()
	if _, err := pool.Exec(ctx, sql, args...); err != nil {
		t.Fatalf("exec valid statement: %v", err)
	}
}

// hash returns n bytes standing in for a SHA-256 digest.
func hash(n int) []byte { return make([]byte, n) }

// keyHash is the key_hash 00007 requires for (scope, key): a test-side copy
// of idem's unexported keyHash, so fixtures can insert rows by hand. The
// table's CHECK verifies every row, so a wrong copy fails these tests rather
// than passing silently.
func keyHash(scope, key string) []byte {
	h := sha256.New()
	for _, f := range []string{scope, key} {
		h.Write(binary.BigEndian.AppendUint64(nil, uint64(len(f))))
		h.Write([]byte(f))
	}
	return h.Sum(nil)
}
