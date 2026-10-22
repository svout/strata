// External test package: rail imports ledger, so a test that needs the rail
// names can't live in package ledger without an import cycle.
package ledger_test

import (
	"testing"

	"github.com/svout/strata/gateway/internal/ledger"
	"github.com/svout/strata/gateway/internal/rail"
	"github.com/svout/strata/gateway/internal/testutil"
)

// Migration 00005 writes the rail names as SQL literals (the seeded owners
// 'rail:internal' and 'rail:fake', and the payments.rail CHECK). This ties
// them to the Go constants the adapters report: rename a constant without a
// migration and this fails here, not as a gateway that won't start.
func TestEveryRailHasAClearingAccount(t *testing.T) {
	t.Parallel()
	pool := testutil.Postgres(t)
	payer, payee := testutil.NewAccount(t, pool, "user"), testutil.NewAccount(t, pool, "merchant")
	for _, name := range []string{rail.InternalName, rail.FakeName} {
		if _, err := ledger.ClearingAccount(t.Context(), pool, name); err != nil {
			t.Errorf("ClearingAccount(%q) = %v, want the account migration 00005 seeds", name, err)
		}
		// payments.rail is the adapter's Name(), so the CHECK must accept it.
		if _, err := pool.Exec(t.Context(),
			"INSERT INTO payments (payer_account_id, payee_account_id, amount, currency, rail) VALUES ($1, $2, 100, 'GBP', $3)",
			payer, payee, name); err != nil {
			t.Errorf("payment on rail %q: %v, want accepted by payments_rail_check", name, err)
		}
	}
}
