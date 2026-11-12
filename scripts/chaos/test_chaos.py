"""Tests for chaos.py's pure parts: no Docker, no database (they run in `make test`).

The scenario itself is `make chaos`. What can be checked without a stack is
the invariant checker, which decides whether a run passes: each test breaks
one invariant in otherwise good rows and expects it to be reported.
"""

import tempfile
import unittest
from dataclasses import replace
from pathlib import Path

from chaos import chaos

ID = "11111111-1111-4111-8111-111111111111"
SETTLED = chaos.PaymentRow(ID, "settled", "fake", "pay_" + ID, payer_legs=2, other_legs=0, unbalanced=0, moves=1)
SETTLING_ACTED = replace(SETTLED, state="settling", payer_legs=0)  # the rail moved it, the gateway didn't record it
SETTLING_UNTOUCHED = replace(SETTLING_ACTED, moves=-1)  # the rail has no record
FAILED = replace(SETTLED, state="failed", payer_legs=0, moves=0)


class CheckTest(unittest.TestCase):
    def assertCaught(self, row, want, *, final=True):
        bad = chaos.check([row], final=final)
        self.assertTrue(any(want in b for b in bad), f"want a message containing {want!r}, got {bad}")

    def test_good_rows_pass(self):
        self.assertEqual(chaos.check([SETTLED, FAILED], final=True), [])
        self.assertEqual(chaos.check([SETTLED, SETTLING_ACTED, SETTLING_UNTOUCHED, FAILED], final=False), [])

    def test_settling_is_a_failure_after_the_deadline(self):
        self.assertCaught(SETTLING_ACTED, "still settling", final=True)

    def test_settling_needs_its_recovery_record(self):
        self.assertCaught(replace(SETTLING_UNTOUCHED, rail_ref=""), "want its recovery record", final=False)

    def test_settling_under_another_reference(self):
        other = "pay_22222222-2222-4222-8222-222222222222"
        self.assertCaught(replace(SETTLING_ACTED, rail_ref=other), "want its recovery record", final=False)

    def test_settled_under_another_reference(self):
        # Recovery paying again under a fresh reference and saving that one.
        other = "pay_22222222-2222-4222-8222-222222222222"
        self.assertCaught(replace(SETTLED, rail_ref=other), "want its recovery record")

    def test_unexpected_state(self):
        self.assertCaught(replace(FAILED, state="refunded"), "unexpected state")

    def test_created_rail_payment(self):
        self.assertCaught(replace(SETTLING_UNTOUCHED, state="created"), "left created", final=False)

    def test_settled_without_payer_posting(self):
        self.assertCaught(replace(SETTLED, payer_legs=0), "0 payer legs, want 2")

    def test_payer_posting_twice(self):
        self.assertCaught(replace(SETTLED, payer_legs=4), "4 payer legs, want 2")

    def test_posted_without_settling(self):
        self.assertCaught(replace(SETTLING_ACTED, payer_legs=2), "2 payer legs, want 0", final=False)

    def test_fake_rail_posts_nothing_itself(self):
        self.assertCaught(replace(SETTLED, other_legs=2), "outside the payer posting")

    def test_unbalanced_posting(self):
        self.assertCaught(replace(SETTLED, unbalanced=1), "unbalanced")

    def test_money_moved_twice(self):
        self.assertCaught(replace(SETTLED, moves=2), "moved the money 2 times")

    def test_settled_but_never_moved(self):
        self.assertCaught(replace(SETTLED, moves=-1), "want 1")

    def test_failed_but_moved(self):
        self.assertCaught(replace(FAILED, moves=1), "failed, but the fake PSP moved the money")

    def test_wrong_rail(self):
        self.assertCaught(replace(SETTLED, rail="internal"), "want 'fake'")


class RailRecordsTest(unittest.TestCase):
    def test_one_record_per_payment_passes(self):
        self.assertEqual(chaos.check_rail_records(3, 3, want=3), [])

    def test_a_second_operation_under_another_reference(self):
        bad = chaos.check_rail_records(4, 4, want=3)
        self.assertTrue(any("4 records" in b for b in bad), bad)

    def test_money_moved_twice_under_one_record(self):
        bad = chaos.check_rail_records(3, 4, want=3)
        self.assertTrue(any("moved money 4 times" in b for b in bad), bad)


B_ID = "33333333-3333-4333-8333-333333333333"
B_IN_WINDOW = replace(SETTLING_ACTED, id=B_ID, rail_ref="pay_" + B_ID)


class ExpectationsTest(unittest.TestCase):
    def test_after_kill_good(self):
        self.assertEqual(chaos.expect_after_kill([B_IN_WINDOW, SETTLING_UNTOUCHED], B_ID), [])

    def test_after_kill_b_not_in_the_crash_window(self):
        bad = chaos.expect_after_kill([replace(B_IN_WINDOW, moves=-1), SETTLING_UNTOUCHED], B_ID)
        self.assertTrue(any("payment B" in b for b in bad), bad)

    def test_after_kill_a_reached_the_rail(self):
        bad = chaos.expect_after_kill([B_IN_WINDOW, SETTLING_ACTED], B_ID)
        self.assertTrue(any("want settling with no rail record" in b for b in bad), bad)

    def test_after_recovery_good(self):
        self.assertEqual(chaos.expect_after_recovery([SETTLED]), [])

    def test_after_recovery_failed_is_wrong_on_the_fake(self):
        bad = chaos.expect_after_recovery([FAILED])
        self.assertTrue(any("want settled" in b for b in bad), bad)


class ParseTest(unittest.TestCase):
    def test_parse_rows(self):
        rows = chaos.parse_rows([[ID, "settled", "fake", "pay_" + ID, "2", "0", "0", "1"]])
        self.assertEqual(rows, [SETTLED])

    def test_parse_rejects_a_short_row(self):
        with self.assertRaises(chaos.ChaosError):
            chaos.parse_rows([[ID, "settled"]])


class LatestMigrationTest(unittest.TestCase):
    def test_highest_numbered_sql_file(self):
        with tempfile.TemporaryDirectory() as d:
            for name in ("00001_a.sql", "00007_b.sql", "00010_c.sql", "README.md", "00099_x.sql.bak"):
                (Path(d) / name).write_text("")
            self.assertEqual(chaos.latest_migration(Path(d)), 10)

    def test_no_migrations(self):
        with tempfile.TemporaryDirectory() as d, self.assertRaises(chaos.ChaosError):
            chaos.latest_migration(Path(d))

    def test_the_real_directory(self):
        # Guards the path constant: the scenario's schema check depends on it.
        self.assertGreaterEqual(chaos.latest_migration(chaos.MIGRATIONS), 7)


if __name__ == "__main__":
    unittest.main()
