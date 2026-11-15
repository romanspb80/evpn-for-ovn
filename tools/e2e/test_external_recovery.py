import tempfile
import copy
import unittest
from unittest.mock import Mock, patch
from pathlib import Path

from external_recovery import (
    CASES, RecoveryBaseline, complete_probe, rejected_issue, stable_objects, validate_injected_rib,
)
from containerlab import LabError


class RecoveryTests(unittest.TestCase):
    def test_live_seam_retains_exact_control_and_actual_source(self):
        with tempfile.TemporaryDirectory() as directory:
            runner = RecoveryBaseline(Path(directory), "test-cluster")
            runner.b_rib = Mock(return_value={})
            with patch("external_gobgp.ThreeDomainBaseline.bput") as publish:
                for mode in ("incomplete", "conflict"):
                    runner.bput("observation", mode + "\n", True)
                    self.assertEqual((Path(directory) / ("r2-" + mode + "-control.txt")).read_text(),
                                     mode + "\n")
                self.assertEqual(publish.call_count, 2)
                self.assertEqual(runner.b_rib.call_count, 2)
                with self.assertRaises(LabError):
                    runner.bput("observation", "unrecognized\n", True)
                self.assertEqual(publish.call_count, 2)

    def test_injected_path_checks_actual_encoding_and_provenance(self):
        config = {"RouterID": "198.18.20.3", "Endpoint": {
            "MACAddress": "02:00:00:02:ee:01", "IPAddresses": ["10.100.2.210"],
            "RouteDistinguisher": "198.18.20.3:3100", "VNI": 2000,
            "LocalVTEP": "198.18.20.98", "RouteTargets": ["64513:2000"]}}
        path = {"nlri": {"type": 2, "value": {
            "mac": "02:00:00:02:ee:01", "ip": "10.100.2.210",
            "rd": {"type": 1, "admin": "198.18.20.3", "assigned": 3100},
            "labels": [2000], "etag": 0, "esi": "single-homed"}},
            "attrs": [{"type": 14, "nexthop": "198.18.20.98", "afi": 25, "safi": 70},
                      {"type": 16, "value": [{"subtype": 2, "value": "64513:2000"},
                                             {"tunnel_type": 8}]}],
            "peer-address": "198.18.20.3"}
        self.assertEqual(validate_injected_rib({"route": [path]}, config), path)
        for field, value in (("VNI", 3000), ("LocalVTEP", "198.18.20.4"),
                             ("RouteTargets", ["64512:1000"])):
            changed = copy.deepcopy(config)
            changed["Endpoint"][field] = value
            with self.subTest(field=field), self.assertRaises(LabError):
                validate_injected_rib({"route": [path]}, changed)
        with self.assertRaises(LabError):
            validate_injected_rib({"route": [path, path]}, config)

    def test_rejection_waits_for_authoritative_matching_issue(self):
        status = {"Snapshot": {"Authoritative": True, "Issues": None}}
        self.assertFalse(rejected_issue(status, "denied"))
        status["Snapshot"]["Issues"] = [{"Reason": "denied"}]
        self.assertTrue(rejected_issue(status, "denied"))
        self.assertFalse(rejected_issue(status, "different"))
        status["Snapshot"]["Authoritative"] = False
        self.assertFalse(rejected_issue(status, "denied"))

    def test_convergence_retains_both_command_results(self):
        with tempfile.TemporaryDirectory() as directory:
            runner = RecoveryBaseline(Path(directory), "test-cluster")
            runner.pods = {"a": "ovn-a"}
            runner.frr = "remote1"
            runner.command = Mock(return_value="3 packets transmitted, 3 received, 0% packet loss")
            runner.converge_pair("a", "test-convergence")
            self.assertEqual(runner.command.call_count, 2)
            self.assertTrue((Path(directory) / "test-convergence-convergence.json").exists())
            for call in runner.command.call_args_list:
                self.assertFalse(call.kwargs["check"])

    def test_convergence_requires_all_probes_not_just_ping_success(self):
        self.assertTrue(complete_probe("3 packets transmitted, 3 received, 0% packet loss"))
        self.assertTrue(complete_probe("3 packets transmitted, 3 packets received, 0% packet loss"))
        self.assertFalse(complete_probe("3 packets transmitted, 1 received, 66% packet loss"))
        self.assertFalse(complete_probe("3 packets transmitted, 0 received, 100% packet loss"))
        self.assertFalse(complete_probe("namespace unavailable"))

    def test_action_and_assertion_order_are_exact(self):
        runner = RecoveryBaseline(Path("/tmp/not-created"), "test-cluster")
        self.assertEqual(len(runner.external_actions()) + 12, len(runner.cases))
        names = list(runner.cases)
        self.assertLess(names.index(CASES[0]), names.index("projector_uncertainty_conflict"))
        self.assertIn("G1_A_restart_recovery", CASES)
        self.assertIn("G2_B_restart_recovery", CASES)
        self.assertEqual(runner.scenarios["G1"]["status"], "PLANNED")
        self.assertEqual(runner.scenarios["E1"]["status"], "PLANNED")

    def test_recovered_kernel_identity_is_not_reduced_to_mac(self):
        receipt = {"Endpoint": {"MAC": "02:00:00:00:00:01"},
                   "FDB": {"ACK": True, "Observed": {"Ifindex": 7, "VTEP": "192.0.2.1"}}}
        changed = {**receipt, "FDB": {"ACK": True, "Observed": {"Ifindex": 8, "VTEP": "192.0.2.1"}}}
        self.assertNotEqual(stable_objects([receipt]), stable_objects([changed]))

    def test_supporting_pass_never_implies_full_milestone(self):
        with tempfile.TemporaryDirectory() as directory:
            runner = RecoveryBaseline(Path(directory), "test-cluster")
            runner.finish("PASS")
            import json
            result = json.loads((Path(directory) / "recovery-scenarios.json").read_text())
            self.assertFalse(result["fullMilestoneAccepted"])
            self.assertEqual(result["scenarios"]["H1"]["status"], "BLOCKED")

    def test_failed_attempt_cannot_accept_tentative_scenario(self):
        with tempfile.TemporaryDirectory() as directory:
            runner = RecoveryBaseline(Path(directory), "test-cluster")
            runner.scenarios["E3"] = {"status": "PASS", "detail": "supporting packets"}
            runner.finish("FAIL")
            self.assertEqual(runner.scenarios["E3"]["status"], "BLOCKED")


if __name__ == "__main__":
    unittest.main()
