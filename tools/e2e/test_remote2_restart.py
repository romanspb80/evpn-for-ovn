"""Offline checks for the bounded live R3 runner."""

import unittest
from pathlib import Path
import json

from remote2_restart import Remote2Restarts, foreign_paths


class RestartTests(unittest.TestCase):
    def test_pinned_gobgp_json_session_enum(self):
        fixture = Remote2Restarts(Path("/tmp/not-created"), "unit-cluster")
        for state in range(1, 7):
            fixture.peer = lambda label, state=state: json.dumps({
                "conf": {"neighbor_address": "198.18.20.3"}, "state": {"session_state": state}})
            self.assertEqual(fixture.peer_established("unit"), state == 6)

    def test_readiness_rejects_empty_incomplete_and_global_blockers(self):
        fixture = Remote2Restarts(Path("/tmp/not-created"), "unit-cluster")
        healthy = {"Error": "", "Time": "2",
                   "Snapshot": {"Authoritative": True, "Endpoints": [{"MAC": "owned"}]},
                   "Export": {"OwnershipUnverified": False, "Applied": [{}]},
                   "Receipts": [{"FDB": {"ACK": True, "Status": "confirmed"},
                                 "Neighbor": {"ACK": True, "Status": "confirmed"}}],
                   "RecoveryState": "RUNNING"}

        def wait(path, predicate, label, remote=False):
            if remote:
                for field, value in (("Receipts", None), ("Receipts", []),
                                     ("RecoveryState", "RECOVERY_BLOCKED"), ("Error", "uncertain")):
                    self.assertFalse(predicate({**healthy, field: value}))
                self.assertFalse(predicate({**healthy, "Snapshot": {"Authoritative": False}}))
                self.assertFalse(predicate({**healthy, "Export": {"OwnershipUnverified": True}}))
                status = {**healthy, "Time": "3" if label.endswith("second") else "2"}
                self.assertTrue(predicate(status))
                return status
            return healthy
        fixture.bwait = wait
        self.assertEqual(fixture.ready("unit", "1")["Time"], "3")

    def test_foreign_bgp_comparison_ignores_only_age(self):
        route = {"nlri": {"prefix": "203.0.113.222/32"}, "attrs": [{"type": 3}], "age": 1}
        self.assertEqual(foreign_paths({"prefix": [route]}),
                         foreign_paths({"prefix": [{**route, "age": 2}]}))
        self.assertNotEqual(foreign_paths({"prefix": [route]}),
                            foreign_paths({"prefix": [{**route, "attrs": []}]}))
