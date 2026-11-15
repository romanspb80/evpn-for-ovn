import copy
import json
from pathlib import Path
import re
import tempfile
import unittest
from unittest.mock import Mock

from containerlab import LabError
from cross_domain import (
    CASES, CrossDomain, assert_clean, assert_foreign, assert_policy_views, exact_withdrawal, other_rt_pattern,
    require_reachable, strict_policy,
)


class CrossDomainTests(unittest.TestCase):
    def test_strict_rt_complement_including_unknown_domains(self):
        for allowed in ("64512:1000", "64513:2000"):
            pattern = re.compile(other_rt_pattern(allowed))
            self.assertIsNone(pattern.fullmatch(allowed))
            for other in ("64512:1000", "64513:2000", "1:1", "64512:10000",
                          "64512:100", "64512:1001", "64512:999", "198.18.10.3:1"):
                if other != allowed:
                    self.assertIsNotNone(pattern.fullmatch(other), other)
            for targets in ([], ["64512:1000", "64513:2000"], ["1:1"], [allowed, "1:1"]):
                accepted = allowed in targets and not any(pattern.fullmatch(rt) for rt in targets)
                self.assertFalse(accepted)

    def test_policy_requires_rt_and_rejects_other_before_allow(self):
        config = strict_policy("global:\n  config:\n    as: 64512\nneighbors:\n- config: {}\n", "64512:1000")
        self.assertIn("default-import-policy: reject-route", config)
        self.assertIn("default-export-policy: reject-route", config)
        self.assertLess(config.index("name: reject-any-other-rt"), config.index("name: require-allowed-rt"))
        with self.assertRaises(LabError):
            strict_policy("missing policy context", "64512:1000")
        for malformed in ("", "64512", "64512:1000:1", "RT:64512:1000", "64512:.*"):
            with self.assertRaises(LabError):
                other_rt_pattern(malformed)

    def test_incomplete_rib_receipt_desired_or_missing_denial_cannot_pass(self):
        status = {"Snapshot": {"Authoritative": True, "Issues": None, "Endpoints": []},
                  "Receipts": [], "Error": ""}
        assert_clean(status, "test-mac", "test-ip")
        variants = []
        for key, value in (("Authoritative", False), ("Endpoints", [{"MAC": "test-mac"}])):
            changed = copy.deepcopy(status)
            changed["Snapshot"][key] = value
            variants.append(changed)
        variants += [{**status, "Receipts": [{"foreign": "test-mac"}]},
                     {**status, "Error": "incomplete"},
                     {**status, "Snapshot": {**status["Snapshot"], "Issues": [{"Reason": "wrong"}]}}]
        for changed in variants:
            with self.assertRaises(LabError):
                assert_clean(changed, "test-mac", "test-ip")
        with self.assertRaises(LabError):
            assert_clean(status, "test-mac", "test-ip", "remote VTEP not allowlisted")

    def test_cleanup_cannot_succeed_on_tuple_match_or_other_incarnation(self):
        owned = {"PathUUID": "exact-uuid", "Incarnation": "current"}
        exact_withdrawal(owned, owned)
        for changed in ({**owned, "PathUUID": "other"}, {**owned, "Incarnation": "old"},
                        {**owned, "PathUUID": ""}):
            with self.assertRaises(LabError):
                exact_withdrawal(owned, changed)

    def test_mandatory_matrix_has_no_recovery_or_transit_cases(self):
        self.assertEqual(len(CASES), 8)
        for side in ("a", "b"):
            for case in ("single-rt", "wrong-rt", "dual-rt", "unauthorized-vtep"):
                self.assertIn(case + "-" + side, CASES)

    def test_policy_proof_requires_both_views_and_legitimate_control(self):
        assert_policy_views("injected", "legitimate", "injected", "legitimate")
        for rejected, accepted in (("", "legitimate"), ("injected", ""), ("injected legitimate", "legitimate"),
                                   ("injected", "legitimate injected")):
            with self.assertRaises(LabError):
                assert_policy_views(rejected, accepted, "injected", "legitimate")

    def test_unreachable_or_partial_underlay_control_blocks_negative(self):
        require_reachable("3 packets transmitted, 3 received")
        for output in ("3 packets transmitted, 0 received", "3 packets transmitted, 2 received",
                       "unreachable", ""):
            with self.assertRaises(LabError):
                require_reachable(output)

    def test_foreign_state_requires_exact_nonempty_permanent_witnesses(self):
        before = {"fdb": [{"mac": "sentinel", "dst": "foreign", "state": "permanent"}],
                  "neighbors": [{"lladdr": "sentinel", "dst": "ip", "state": ["PERMANENT"]}]}
        assert_foreign(before, before, "sentinel")
        for field in before:
            for entries in ([], [{**before[field][0], "state": "changed"}],
                            [{**before[field][0], "dst": "adopted"}]):
                after = {**before, field: entries}
                with self.assertRaises(LabError):
                    assert_foreign(before, after, "sentinel")

    def test_frr_learning_is_unicast_and_wrong_rt_preserves_encapsulation(self):
        with tempfile.TemporaryDirectory() as directory:
            fixture = CrossDomain(Path(directory), "unit")
            fixture.frr_bridge_mac = "02:00:00:01:fe:01"
            fixture.fr = Mock()
            fixture.vty = Mock()
            fixture.frr_add("wrong", {"Endpoint": {
                "IPAddresses": ["10.100.1.222"], "MACAddress": "02:00:00:01:ed:02",
                "LocalVTEP": "198.18.10.4", "RouteDistinguisher": "198.18.10.3:1000"}}, "wrong-rt")
            commands = fixture.vty.call_args_list[0].args
            self.assertIn("set extended-comm-list R4-LOCAL delete", commands)
            self.assertNotIn("set extcommunity none", commands)
            self.assertIn("match ip address prefix-list R4-INJECT", commands)
            script = fixture.fr.call_args.args[2]
            self.assertLess(script.index("ip neigh add 10.100.1.1"),
                            script.index("ping -n -c 2"))
            self.assertIn("lladdr 02:00:00:01:fe:01 dev r4-work nud permanent", script)
            self.assertLess(script.index("r4-work addrgenmode none"),
                            script.index("r4-work up"))

    def test_provenance_distinguishes_frr_from_attested_gobgp_owner(self):
        with tempfile.TemporaryDirectory() as directory:
            fixture = CrossDomain(Path(directory), "unit")
            fixture.owned_state = Mock(side_effect=LabError("stop before live actions"))
            with self.assertRaisesRegex(LabError, "stop before live"):
                fixture.case("a", "wrong-rt", 2)
            frr = json.loads((Path(directory) / "wrong-rt-a-expected.json").read_text())
            self.assertEqual(frr["ASN"], 64512)
            self.assertEqual(frr["RouterID"], "198.18.10.3")
            self.assertNotIn("Socket", frr)
            self.assertNotIn("NodeUID", frr)
            fixture.installation, fixture.r_node = "installation", "remote-node"
            with self.assertRaisesRegex(LabError, "stop before live"):
                fixture.case("b", "wrong-rt", 6)
            gobgp = json.loads((Path(directory) / "wrong-rt-b-expected.json").read_text())
            self.assertEqual(gobgp["ASN"], 64513)
            self.assertEqual(gobgp["NodeUID"], "remote-node")
            self.assertEqual(gobgp["Attestation"], "/run/evpn-remote2/r2-speaker.json")

    def test_freshness_alone_cannot_accept_transitional_incomplete_snapshot(self):
        with tempfile.TemporaryDirectory() as directory:
            fixture = CrossDomain(Path(directory), "unit")
            initial = {"Time": "1", "Snapshot": {"Authoritative": True, "Generation": 1}}
            incomplete = {"Time": "2", "Snapshot": {"Authoritative": False, "Generation": 2}}
            ready = {"Time": "3", "Snapshot": {"Authoritative": True, "Generation": 2}}
            stable = {**ready, "Time": "4"}
            fixture.import_status = Mock(side_effect=(initial, incomplete, ready, stable))

            def wait(predicate, _message):
                for _ in range(3):
                    if predicate():
                        return
                self.fail("complete stable observations did not converge")

            fixture.wait = wait
            self.assertEqual(fixture.fresh_import("b", "unit"), stable)
            self.assertEqual(fixture.import_status.call_count, 4)


if __name__ == "__main__":
    unittest.main()
