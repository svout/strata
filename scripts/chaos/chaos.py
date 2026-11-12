#!/usr/bin/env python3
"""Chaos run against the `make dev` stack on the fake PSP (SPEC-002 11a, AC4).

Kills the gateway mid-settlement, restarts it, and checks that every payment
it was settling ends terminal, settled once, with balanced postings. Run it
through `make chaos`. Standard library only (no new dependency); Docker and
psql are driven through subprocess, HTTP through urllib.

How a payment is held mid-settlement without changing the gateway: a psql
session takes a table lock, so the gateway's next statement on that table
waits for it.

  * LOCK ledger_entries IN EXCLUSIVE MODE: the fake PSP records the payment
    (it moves the money, moves = 1), then the gateway's payer posting waits.
    Killed here, the rail acted and the gateway never recorded it: the crash
    window ADR-123 is about.
  * LOCK fake_rail_operations IN ACCESS EXCLUSIVE MODE: the fake's first read
    waits. Killed here, the rail never acted.

The kill comes as soon as pg_stat_activity shows the requests waiting on the
lock, so the timing is decided by the script, not by a race. Each wait is
also bounded inside the gateway by its 3 s settle timeout
(payments.defaultSettleTimeout); a kill that lands later than that would be
after the gateway gave up, not mid-settlement, and the run is rejected.

ADR-117: the script reads payments, ledger_entries and fake_rail_operations
to check them, as a black-box test driver, and writes none of them. It also
reads goose_db_version (the schema check before the gateway starts) and
pg_stat_activity (who is waiting on a lock). Its two accounts come from
infra/compose/seed.sql (`make dev-seed`).
"""

from __future__ import annotations

import json
import os
import re
import subprocess
import sys
import threading
import time
import urllib.error
import urllib.request
import uuid
from dataclasses import dataclass
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]
COMPOSE = ["docker", "compose", "-f", "infra/compose/compose.yml"]
GATEWAY = "http://127.0.0.1:8080"
MIGRATIONS = ROOT / "gateway/internal/store/migrations"

# The accounts in infra/compose/seed.sql.
PAYER = "00000000-0000-4000-8000-00000000c4a1"
PAYEE = "00000000-0000-4000-8000-00000000c4a2"

# payments.defaultSettleTimeout: how long the gateway itself waits on one
# rail call or one write. The kill must land well inside it.
SETTLE_TIMEOUT_S = 3.0
KILL_WITHIN_S = 2.5
# Recovery: payments.defaultRecoverGrace (15 s) plus wire.go's recoverEvery
# (15 s) plus one pass, with room for a slow CI runner.
RECOVERY_DEADLINE_S = 90.0
# Extra creates held behind the fake-table lock, besides A. Each held request
# holds one gateway pool connection while it waits, and the pool is pgx's
# default (store.OpenPool): max(4, NumCPU), so 4 on a 4-vCPU CI runner. B, A
# and one burst hold 3 of those 4, whatever the runner; a fourth held request
# could block in pool.Acquire instead of on the lock, and never be counted.
BURST = 1


class ChaosError(Exception):
    """The run failed: an invariant broke or the scenario couldn't be set up."""


# --- psql ------------------------------------------------------------------


class Session:
    """A long-lived psql session in the postgres container.

    Each `docker compose exec` starts a new process in the container, and
    the polls run inside the 3 s window, so they reuse one open session
    instead (assumed, not measured: an exec costs far more than a query).
    Each query is followed by an \\echo marker, so the reader knows where its
    output ends.
    """

    _END = "__chaos_end__"

    def __init__(self) -> None:
        self._p = subprocess.Popen(
            [*COMPOSE, "exec", "-T", "postgres", "psql", "-X", "-q", "-At", "-F", "|",
             "-v", "ON_ERROR_STOP=1", "-U", "strata", "-d", "strata"],
            cwd=ROOT, stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.PIPE,
            text=True, bufsize=1,
        )

    def query(self, sql: str) -> list[list[str]]:
        assert self._p.stdin and self._p.stdout
        self._p.stdin.write(sql + "\n\\echo " + self._END + "\n")
        self._p.stdin.flush()
        rows = []
        while True:
            line = self._p.stdout.readline()
            if not line:
                err = self._p.stderr.read() if self._p.stderr else ""
                raise ChaosError(f"psql session ended: {err.strip()}")
            line = line.rstrip("\n")
            if line == self._END:
                return rows
            rows.append(line.split("|"))

    def value(self, sql: str) -> str:
        rows = self.query(sql)
        if len(rows) != 1 or len(rows[0]) != 1:
            raise ChaosError(f"expected one value from {sql!r}, got {rows!r}")
        return rows[0][0]

    def close(self) -> None:
        if self._p.poll() is None:
            assert self._p.stdin
            try:
                self._p.stdin.write("\\q\n")
                self._p.stdin.flush()
            except BrokenPipeError:
                pass
            try:
                self._p.wait(5)
            except subprocess.TimeoutExpired:
                self._p.kill()


def latest_migration(directory: Path) -> int:
    """The highest version among NNNNN_name.sql files in directory."""
    versions = [int(m.group(1)) for f in directory.iterdir() if (m := re.fullmatch(r"(\d+)_.+\.sql", f.name))]
    if not versions:
        raise ChaosError(f"no migrations in {directory}")
    return max(versions)


# --- invariants ------------------------------------------------------------

# One row per payment the run created. payer_ref is payments.PayerRef's
# format ("payer_" + the id); rail_ref is rail.Ref's ("pay_" + the id). moves
# is -1 when the fake PSP has no record of the payment.
INVARIANT_SQL = """
SELECT p.id, p.state, p.rail, COALESCE(p.rail_ref, ''),
  (SELECT count(*) FROM ledger_entries e WHERE e.payment_id = p.id AND e.rail_ref = 'payer_' || p.id),
  (SELECT count(*) FROM ledger_entries e WHERE e.payment_id = p.id AND e.rail_ref IS DISTINCT FROM 'payer_' || p.id),
  (SELECT count(*) FROM (
     SELECT 1 FROM ledger_entries e WHERE e.payment_id = p.id GROUP BY e.rail_ref
     HAVING sum(CASE e.direction WHEN 'credit' THEN e.amount ELSE -e.amount END) <> 0) u),
  COALESCE(f.moves, -1)
FROM payments p LEFT JOIN fake_rail_operations f ON f.ref = p.rail_ref
WHERE p.id = ANY('{%s}'::uuid[])
ORDER BY p.id;
"""


@dataclass(frozen=True)
class PaymentRow:
    id: str
    state: str
    rail: str
    rail_ref: str
    payer_legs: int
    other_legs: int
    unbalanced: int
    moves: int


def parse_rows(rows: list[list[str]]) -> list[PaymentRow]:
    out = []
    for r in rows:
        if len(r) != 8:
            raise ChaosError(f"unexpected invariant row {r!r}")
        out.append(PaymentRow(r[0], r[1], r[2], r[3], int(r[4]), int(r[5]), int(r[6]), int(r[7])))
    return out


# payments.state's CHECK; refunded can't happen in this run.
STATES = ("created", "settling", "settled", "failed")


def check(rows: list[PaymentRow], *, final: bool) -> list[str]:
    """Every way rows break AC4's invariants on the fake PSP, as messages.

    final: after the recovery deadline, when nothing may still be settling.
    Before it (right after the restart) settling is allowed, but only with
    the recovery record Recover needs: the rail reference.
    """
    bad = []
    for r in rows:
        who = f"payment {r.id} ({r.state})"
        if r.state not in STATES:
            bad.append(f"{who}: unexpected state, want one of {', '.join(STATES)}")
        if r.rail != "fake":
            bad.append(f"{who}: on rail {r.rail!r}, want 'fake'")
        if r.state == "created":
            bad.append(f"{who}: a rail payment left created")
        if r.state == "settling" and final:
            bad.append(f"{who}: still settling after the recovery deadline")
        # Past created, the payment carries its one deterministic reference
        # (ADR-120): what Recover settles by, and the only reference the fake
        # PSP may hold for it. A different one would be a second operation.
        if r.state != "created" and r.rail_ref != "pay_" + r.id:
            bad.append(f"{who}: rail_ref {r.rail_ref!r}, want its recovery record 'pay_{r.id}'")
        # Settled iff the payer posting exists (ADR-123), and the fake rail
        # posts nothing of its own.
        want_payer = 2 if r.state == "settled" else 0
        if r.payer_legs != want_payer:
            bad.append(f"{who}: {r.payer_legs} payer legs, want {want_payer}")
        if r.other_legs != 0:
            bad.append(f"{who}: {r.other_legs} entries outside the payer posting, want 0")
        if r.unbalanced != 0:
            bad.append(f"{who}: {r.unbalanced} unbalanced postings")
        # The money moves once at most (ADR-120), and a settled payment moved it.
        if r.moves > 1:
            bad.append(f"{who}: the fake PSP moved the money {r.moves} times")
        if r.state == "settled" and r.moves != 1:
            bad.append(f"{who}: settled, but the fake PSP moved the money {r.moves} times, want 1")
        if r.state == "failed" and r.moves > 0:
            bad.append(f"{who}: failed, but the fake PSP moved the money")
    return bad


def check_rail_records(records: int, moves: int, want: int) -> list[str]:
    """The fake PSP's records made during the run, against what AC4 allows.

    check() joins on each payment's own reference, so it can't see a second
    operation under another reference (recovery paying again with a fresh
    one, say). This counts every record the run made: want of them, each
    having moved the money once.
    """
    bad = []
    if records != want:
        bad.append(f"the fake PSP has {records} records from this run, want {want}")
    if moves != want:
        bad.append(f"the fake PSP moved money {moves} times in this run, want {want}")
    return bad


def expect_after_kill(rows: list[PaymentRow], b_id: str) -> list[str]:
    """Right after the kill: B is in the crash window, the rest never reached the rail."""
    bad = check(rows, final=False)
    for r in rows:
        if r.id == b_id:
            if (r.state, r.moves, r.payer_legs) != ("settling", 1, 0):
                bad.append(f"payment B {r.id}: {r.state}, moves {r.moves}, payer legs {r.payer_legs}; "
                           "want settling, 1, 0 (the rail acted and the gateway didn't record it)")
        elif (r.state, r.moves) != ("settling", -1):
            bad.append(f"payment {r.id}: {r.state}, moves {r.moves}; want settling with no rail record")
    return bad


def expect_after_recovery(rows: list[PaymentRow]) -> list[str]:
    """After the deadline: every payment settled (the fake PSP settles every retry)."""
    bad = check(rows, final=True)
    bad += [f"payment {r.id}: {r.state} after recovery, want settled (the fake PSP settles every retry)"
            for r in rows if r.state != "settled"]
    return bad


# --- the stack -------------------------------------------------------------


def run(cmd: list[str], *, env: dict[str, str] | None = None, capture: bool = False) -> str:
    res = subprocess.run(cmd, cwd=ROOT, env={**os.environ, **(env or {})}, text=True,
                         stdout=subprocess.PIPE if capture else None)
    if res.returncode != 0:
        raise ChaosError(f"{' '.join(cmd)} exited {res.returncode}")
    return res.stdout or ""


def make(target: str, **env: str) -> None:
    run(["make", "--no-print-directory", target], env=env)


def gateway_logs(tail: int = 200) -> str:
    return subprocess.run([*COMPOSE, "logs", "--no-color", "--no-log-prefix", "--tail", str(tail), "gateway"],
                          cwd=ROOT, text=True, capture_output=True).stdout


def create_payment(key: str, amount: int, timeout: float = 30.0) -> tuple[int, dict]:
    body = json.dumps({"amount": amount, "currency": "GBP",
                       "payer_account_id": PAYER, "payee_account_id": PAYEE}).encode()
    req = urllib.request.Request(GATEWAY + "/v1/payments", data=body, method="POST",
                                 headers={"Content-Type": "application/json", "Idempotency-Key": key})
    try:
        with urllib.request.urlopen(req, timeout=timeout) as resp:
            return resp.status, json.load(resp)
    except urllib.error.HTTPError as e:
        return e.code, {}


class HeldCreate:
    """A create sent on its own thread, expected to block until the kill."""

    def __init__(self, key: str, amount: int) -> None:
        self.key, self.amount = key, amount
        self.status: int | None = None  # set only if a response came back
        self.answered_at: float | None = None
        self.error: Exception | None = None
        self._t = threading.Thread(target=self._send, daemon=True)
        self._t.start()

    def _send(self) -> None:
        try:
            self.status, _ = create_payment(self.key, self.amount)
            self.answered_at = time.monotonic()
        except Exception as e:  # the kill drops the connection: expected
            self.error = e

    def join(self) -> None:
        self._t.join(35)


def lock_waiters(watch: Session, table: str) -> int:
    return int(watch.value(
        "SELECT count(*) FROM pg_stat_activity WHERE wait_event_type = 'Lock' "
        f"AND state = 'active' AND query ILIKE '%{table}%' AND application_name <> 'psql';"))


def wait_for_waiters(watch: Session, table: str, n: int, timeout: float = 10.0) -> float:
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        if lock_waiters(watch, table) >= n:
            return time.monotonic()
        time.sleep(0.02)
    raise ChaosError(f"fewer than {n} requests waiting on {table} after {timeout:.0f} s")


# --- the scenario ----------------------------------------------------------


def start_stack(watch_factory=Session) -> None:
    run([*COMPOSE, "up", "-d", "--wait", "--wait-timeout", "120", "postgres", "redis"])
    # Migrate before the gateway starts, and never between the kill and the
    # restart: the restarted gateway must be the same binary on the same
    # schema. Until the minimum-schema-version readiness gate exists (the
    # step after 11), a gateway on an old schema reports ready and then 500s,
    # so check the version here rather than trust /readyz.
    make("migrate")
    s = watch_factory()
    try:
        have = int(s.value("SELECT COALESCE(max(version_id), 0) FROM goose_db_version WHERE is_applied;"))
    finally:
        s.close()
    want = latest_migration(MIGRATIONS)
    if have != want:
        raise ChaosError(f"database is at schema version {have}, the code's latest migration is {want}")
    make("dev-seed")
    make("dev", GATEWAY_ALLOW_FAKE_RAIL="1")
    if "fake rail registered" not in gateway_logs():
        raise ChaosError("the gateway didn't register the fake rail (GATEWAY_ALLOW_FAKE_RAIL=1 not passed through?)")


def ac4() -> None:
    run_id = uuid.uuid4().hex[:8]
    ledger_lock, fake_lock, watch = Session(), Session(), Session()
    started = watch.value("SELECT now();")
    try:
        # B: the rail acts, the gateway's payer posting waits.
        ledger_lock.query("BEGIN; LOCK TABLE ledger_entries IN EXCLUSIVE MODE;")
        b = HeldCreate(f"chaos-{run_id}-b", 1250)
        blocked_at = wait_for_waiters(watch, "ledger_entries", 1)
        # A and the burst: the rail's first read waits, so it never acts.
        fake_lock.query("BEGIN; LOCK TABLE fake_rail_operations IN ACCESS EXCLUSIVE MODE;")
        held = [HeldCreate(f"chaos-{run_id}-a{i}", 1000 + i) for i in range(1 + BURST)]
        wait_for_waiters(watch, "fake_rail_operations", len(held))
        # Still mid-settlement: every request is waiting on a lock right now.
        waiting = lock_waiters(watch, "ledger_entries") + lock_waiters(watch, "fake_rail_operations")
        # Timed before the kill command, so the CLI's own start-up doesn't
        # count against the window.
        killed_after = time.monotonic() - blocked_at
        run([*COMPOSE, "kill", "-s", "KILL", "gateway"])
    finally:
        # Releasing the locks lets each waiting statement finish, but the
        # client is gone, so nothing it was part of can commit.
        for s in (ledger_lock, fake_lock):
            try:
                s.query("ROLLBACK;")
            except ChaosError:
                pass
            s.close()
    try:
        for h in [b, *held]:
            h.join()
        if waiting != 1 + len(held):
            raise ChaosError(f"{waiting} requests were waiting at the kill, want {1 + len(held)}")
        if killed_after > KILL_WITHIN_S:
            raise ChaosError(f"the kill landed {killed_after:.2f} s after B blocked, past {KILL_WITHIN_S} s "
                             f"(the gateway's own timeout is {SETTLE_TIMEOUT_S:.0f} s): not mid-settlement, rerun")
        answered = [h.key for h in [b, *held] if h.status is not None]
        if answered:
            raise ChaosError(f"requests answered before the kill (the gateway timed out first): {answered}")

        ids = payment_ids(watch, started, [b.amount, *(h.amount for h in held)])
        b_id = ids[0]
        wait_for_gateway_backends_gone(watch)

        bad = expect_after_kill(rows_for(watch, ids), b_id)
        bad += check_rail_records(*rail_records(watch, started), want=1)
        if bad:
            raise ChaosError("after the kill:\n  " + "\n  ".join(bad))

        restarted = time.monotonic()
        make("dev", GATEWAY_ALLOW_FAKE_RAIL="1")
        after = wait_resolved(watch, ids)
        resolved_in = time.monotonic() - restarted
        bad = expect_after_recovery(after)
        bad += check_rail_records(*rail_records(watch, started), want=len(ids))
        total = int(watch.value(
            "SELECT COALESCE(sum(CASE direction WHEN 'credit' THEN amount ELSE -amount END), 0) FROM ledger_entries;"))
        if total != 0:
            bad.append(f"the whole ledger sums to {total}, want 0")
        if bad:
            raise ChaosError("after recovery:\n  " + "\n  ".join(bad))
    finally:
        watch.close()

    print(f"AC4 ok: killed {killed_after * 1000:.0f} ms after B blocked, with {waiting} requests waiting on locks "
          f"(B after the rail acted, {len(held)} before it did); after the restart all {len(ids)} payments "
          f"settled within {resolved_in:.0f} s, each moved once, every posting balanced, ledger sums to 0.")


def payment_ids(watch: Session, since: str, amounts: list[int]) -> list[str]:
    """The payment each held create made, in the amounts' order.

    The requests were killed, so no response carries the id. Each create in
    a run has its own amount, so the run's payments are found by payer,
    amount and creation time, in the payments table (no other package's
    table is read for it, ADR-117). A create killed before its transaction
    committed would leave none, and that fails here.
    """
    ids = []
    for amount in amounts:
        rows = watch.query(f"SELECT id FROM payments WHERE payer_account_id = '{PAYER}' "
                           f"AND amount = {amount} AND created_at >= '{since}';")
        if len(rows) != 1:
            raise ChaosError(f"{len(rows)} payments of {amount} since the run started, want 1")
        ids.append(rows[0][0])
    return ids


def rail_records(watch: Session, since: str) -> tuple[int, int]:
    """How many records the fake PSP made since the run started, and their moves."""
    (records, moves), = watch.query("SELECT count(*), COALESCE(sum(moves), 0) FROM fake_rail_operations "
                                    f"WHERE created_at >= '{since}';")
    return int(records), int(moves)


def rows_for(watch: Session, ids: list[str]) -> list[PaymentRow]:
    rows = parse_rows(watch.query(INVARIANT_SQL % ",".join(ids)))
    if len(rows) != len(ids):
        raise ChaosError(f"{len(rows)} of {len(ids)} payments found")
    return rows


def wait_for_gateway_backends_gone(watch: Session, timeout: float = 15.0) -> None:
    """Wait until PostgreSQL has noticed the dead gateway's connections."""
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        n = int(watch.value("SELECT count(*) FROM pg_stat_activity WHERE backend_type = 'client backend' "
                            "AND datname = 'strata' AND application_name <> 'psql';"))
        if n == 0:
            return
        time.sleep(0.2)
    raise ChaosError(f"the killed gateway's connections were still open after {timeout:.0f} s")


def wait_resolved(watch: Session, ids: list[str]) -> list[PaymentRow]:
    deadline = time.monotonic() + RECOVERY_DEADLINE_S
    while True:
        rows = rows_for(watch, ids)
        if all(r.state != "settling" for r in rows) or time.monotonic() > deadline:
            return rows
        time.sleep(1)


def main() -> int:
    os.chdir(ROOT)
    # Compose interpolates GO_VERSION for the gateway image on every command;
    # `make` exports it, so set it the same way when run directly.
    if not os.environ.get("GO_VERSION"):
        m = re.search(r"^go (\S+)$", (ROOT / "gateway/go.mod").read_text(), re.M)
        if m:
            os.environ["GO_VERSION"] = m.group(1)
    ok = False
    try:
        start_stack()
        ac4()
        ok = True
    except ChaosError as e:
        print(f"chaos: FAILED: {e}", file=sys.stderr)
        print("--- gateway logs (last 200 lines) ---", file=sys.stderr)
        print(gateway_logs(), file=sys.stderr)
    finally:
        # Leave the stack as `make dev` would: on the internal rail. Passed
        # empty explicitly, so a GATEWAY_ALLOW_FAKE_RAIL exported in the
        # caller's shell can't turn the fake back on.
        try:
            make("dev", GATEWAY_ALLOW_FAKE_RAIL="")
            if "fake rail registered" in gateway_logs():
                raise ChaosError("the restarted gateway registered the fake rail")
        except ChaosError as e:
            print(f"chaos: couldn't restart the stack on the internal rail: {e}", file=sys.stderr)
            ok = False
    return 0 if ok else 1


if __name__ == "__main__":
    sys.exit(main())
