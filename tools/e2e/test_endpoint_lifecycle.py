"""Fail-closed offline checks for the bounded historical F2/F4 runner."""

import copy
import unittest
from pathlib import Path
from unittest.mock import Mock, patch

from containerlab import LabError
from endpoint_lifecycle import (
    EndpointLifecycle, TRANSITIONS, confirmed, device_identity, retired_ledger, same_foreign,
)


class EndpointLifecycleTests(unittest.TestCase):
    def test_remote1_preallocation_preserves_strict_identity_checks(self):
        for namespace, ifindex in (("new", 8), ("old", 8), ("new", 6)):
            with self.subTest(namespace=namespace, ifindex=ifindex):
                fixture = EndpointLifecycle(Path("/tmp/not-created-r5"), "unit")
                fixture.frr_bridge_mac = "02:00:00:00:00:01"
                fixture.remote1_identity = Mock(side_effect=[
                    {"namespace": "old", "device": {"ifindex": 6}},
                    {"namespace": namespace, "device": {"ifindex": ifindex}}])
                receipt = {"Endpoint": {"MAC": "02:00:00:01:00:65"}, "Incarnation": 1,
                           "Status": "applied", "FDB": {"ACK": True, "Status": "confirmed"},
                           "Neighbor": {"ACK": True, "Status": "confirmed"}}
                fixture.fresh_import = Mock(side_effect=[
                    {"Snapshot": {"Authoritative": True}, "Receipts": [receipt]},
                    {"Snapshot": {"Authoritative": True},
                     "Receipts": [{**receipt, "Incarnation": 2}]}])
                for name in ("save_state", "export_continuity", "start_phase", "witnesses",
                             "wait", "source_absent", "probes", "stop_phase", "converge_pair"):
                    setattr(fixture, name, Mock())
                fixture.fr = Mock()
                with patch("endpoint_lifecycle.lab.write"):
                    if namespace == "old" or ifindex == 6:
                        with self.assertRaises(LabError):
                            fixture.remote1()
                    else:
                        fixture.remote1()
                commands = fixture.fr.call_args_list
                self.assertEqual(commands[0].args, ("ip", "netns", "add", "evpnwork-next"))
                self.assertEqual(commands[1].args, ("ip", "netns", "del", "evpnwork"))
                self.assertIn("mount --bind /run/netns/evpnwork-next /run/netns/evpnwork",
                              commands[2].args[2])

    def test_confirmed_requires_complete_applied_exact_receipts(self):
        receipt = {"Endpoint": {"MAC": "owned"}, "Status": "applied",
                   "FDB": {"ACK": True, "Status": "confirmed"},
                   "Neighbor": {"ACK": True, "Status": "confirmed"}}
        status = {"Snapshot": {"Authoritative": True}, "Receipts": [receipt]}
        self.assertEqual(confirmed(status, "owned"), receipt)
        variants = [{**status, "Error": "incomplete"},
                    {**status, "Snapshot": {"Authoritative": False}},
                    {**status, "Receipts": []}, {**status, "Receipts": [receipt, receipt]}]
        for field in ("FDB", "Neighbor"):
            for change in ({"ACK": False}, {"Status": "uncertain"}):
                bad = copy.deepcopy(status)
                bad["Receipts"][0][field].update(change)
                variants.append(bad)
        for bad in variants:
            with self.assertRaises(LabError):
                confirmed(bad, "owned")

    def test_recreation_requires_durable_exact_current_retirement(self):
        state = {"Generation": 2, "Incarnation": "new", "Phase": "retired", "Used": ["old", "new"]}
        envelope = {"Record": {"Pending": None, "State": state}}
        self.assertEqual(retired_ledger(envelope, 2, "new"), state)
        for field, value in (("Phase", "retiring"), ("Phase", "active"), ("Generation", 1),
                             ("Incarnation", "old"), ("Used", ["old"])):
            bad = copy.deepcopy(envelope)
            bad["Record"]["State"][field] = value
            with self.assertRaises(LabError):
                retired_ledger(bad, 2, "new")
        with self.assertRaises(LabError):
            retired_ledger({"Record": {"Pending": {"Operation": "withdraw"}, "State": state}}, 2, "new")

    def test_foreign_object_replacement_value_or_device_change_fails(self):
        original = {"uuid": "foreign", "fdb": {"dst": "foreign", "ifindex": 42}, "neighbor": "permanent"}
        same_foreign(original, copy.deepcopy(original))
        for field, value in (("uuid", "replacement"), ("fdb", {"dst": "foreign", "ifindex": 43}),
                             ("neighbor", "owned")):
            with self.assertRaises(LabError):
                same_foreign(original, {**original, field: value})

    def test_historical_matrix_includes_both_sides_of_every_transition(self):
        self.assertEqual(set(TRANSITIONS), {"F1-a", "F1-b", "F2-r1", "F2-r2", "F3-a", "F3-b"})

    def test_device_identity_excludes_only_observational_bridge_counters(self):
        bridge = {"ifindex": 4, "ifname": "br", "address": "original", "master": None,
                  "linkinfo": {"info_kind": "bridge", "info_data": {"gc_timer": 12, "stp_state": 0}}}
        changed = copy.deepcopy(bridge)
        changed["linkinfo"]["info_data"]["gc_timer"] = 11
        same_foreign(device_identity(bridge), device_identity(changed))
        for field, value in (("ifindex", 5), ("address", "replacement")):
            with self.assertRaises(LabError):
                same_foreign(device_identity(bridge), device_identity({**bridge, field: value}))
        changed["linkinfo"]["info_data"]["stp_state"] = 1
        with self.assertRaises(LabError):
            same_foreign(device_identity(bridge), device_identity(changed))

    def test_foreign_checks_surround_probes_even_for_unaffected_domains(self):
        fixture = EndpointLifecycle(Path("/tmp/not-created-r5"), "unit")
        self.assertIsInstance(fixture.remote2, str)
        self.assertTrue(callable(fixture.lifecycle_remote2))
        fixture.foreign_expected = {"witness": True}
        fixture.witnesses = Mock()
        fixture.rt = Mock(return_value="3 packets transmitted, 3 received")
        fixture.probes("retired", ("g",))
        self.assertEqual([c.args[0] for c in fixture.witnesses.call_args_list],
                         ["retired-before-probes", "retired-after-probes"])

    def test_docker_cleanup_rejects_replaced_foreign_identity(self):
        fixture = EndpointLifecycle(Path("/tmp/not-created-r5"), "unit")
        fixture.docker_witness = {"container": "foreign", "containerID": "original",
                                  "network": "foreign-net", "networkID": "original-network"}
        fixture.command = Mock(return_value='[{"Id":"replacement","Config":{"Labels":{}}}]')
        with self.assertRaises(LabError):
            fixture.cleanup_docker_witness()
        self.assertEqual(fixture.command.call_count, 1)


if __name__ == "__main__":
    unittest.main()
