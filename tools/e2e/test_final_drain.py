"""H1 negative gates use the same validator as the live pre-destruction barrier."""

import hashlib
import copy
from contextlib import nullcontext
import json
from pathlib import Path
import unittest
from tempfile import TemporaryDirectory
from unittest.mock import Mock, patch

from containerlab import LabError
from final_drain import FinalDrain, check_barrier
import endpoint_lifecycle


def envelope(state, sequence=1):
    record = {"Domain": {"owner": "original"}, "Sequence": sequence, "State": state, "Pending": None}
    return {"Record": record, "SHA256": hashlib.sha256(
        json.dumps(record, separators=(",", ":")).encode()).hexdigest()}


def evidence():
    before = {"observedAt": 1, "processes": {"a": "original"}, "foreign": {"tuple": "foreign"},
              "generation": 2, "incarnation": "current",
              "endpoints": {s: {"endpointID": s, "networkID": s, "attachmentGeneration": 2}
                            for s in ("a", "b")}, "owners": {}}
    after = {"observedAt": 2, "leaseHeld": True, "processes": before["processes"],
             "foreign": before["foreign"], "ovs": {"tunnels": [], "endpointPorts": [],
             "endpointFlows": []}, "bindings": [], "southboundOwned": [],
             "remote1Kernel": {"fdb": [], "neighbors": [], "devices": ["original"]},
             "owners": {}, "frrRIB": "{}",
             "ledger": envelope({"Phase": "retired", "Generation": 2, "Incarnation": "current"}),
             "policies": {}, "fences": {}}
    before["remote1Kernel"] = {"devices": ["original"]}
    for side in ("a", "b", "r2"):
        state = {"Receipts": {}, "Quarantine": "", "Quarantines": {"foreign-slot": "retained"},
                 "Snapshot": {"Authoritative": True, "Endpoints": []},
                 "Controller": "original", "Target": "original", "Devices": {"index": 1}}
        before["owners"][side] = {"export": envelope({"Receipts": [{"Owned": {"PathUUID": side}}]}),
            "import": envelope({**state, "Receipts": {"owned": {
                "Endpoint": {"MAC": "owned", "IP": "owned"}, "Status": "applied",
                "FDB": {"ACK": True, "Status": "confirmed"},
                "Neighbor": {"ACK": True, "Status": "confirmed"}}}}),
            "status": {"Time": "1"}, "speaker": {"PID": 42}}
        after["owners"][side] = {"export": envelope({"Receipts": []}, 2),
            "import": envelope(state, 2), "exportStatus": {"OwnershipUnverified": False,
            "Applied": [], "Desired": [], "RecoveryState": "RUNNING"}, "status": {
            "Snapshot": {"Authoritative": True, "Endpoints": []}, "Receipts": [],
            "RecoveryState": "RECOVERY_PARTIAL", "Time": "3"}, "freshTimes": ["2", "3"],
            "speaker": {"PID": 42}, "kernel": {"fdb": [], "neighbors": []}, "rib": {}, "error": ""}
    for side in ("a", "b"):
        after["policies"][side] = {"revision": 3, "endpoints": [], "tombstones": [
            {"reason": "deleted", "record": before["endpoints"][side]}]}
        after["fences"][side] = envelope({"revision": 3, "endpoints": [
            {"networkID": side, "endpointID": side, "active": False, "retiredGeneration": 2},
            {"networkID": "non-owner", "endpointID": "rejected-B2", "active": False,
             "highestGeneration": 1, "retiredGeneration": 0}]})
    return before, after


class FinalDrainTests(unittest.TestCase):
    def test_retired_recovery_requires_prior_drain_and_exact_owner_transition(self):
        before, after = evidence()
        before["processes"] = {s: {"agent": "1 10\nagent --original",
                                  "speaker": "2 20\nspeaker"} for s in ("a", "b")}
        after["processes"] = copy.deepcopy(before["processes"])
        drained = copy.deepcopy(after)
        recovery = {"drained": drained, "barrier": {"status": "PASS", "acceptedAt": 3},
                    "events": []}
        for index, side in enumerate(("a", "b")):
            replacement = "3 30\nagent --original"
            recovery["events"].append({"side": side,
                "beforeAgent": before["processes"][side]["agent"], "afterAgent": replacement,
                "stoppedAt": 4 + index * 2, "recoveredAt": 5 + index * 2})
            after["processes"][side]["agent"] = replacement
        after.update(observedAt=8, retiredRecovery=recovery)
        self.assertEqual(check_barrier(before, after)["status"], "PASS")
        for change in (
            lambda a: a["retiredRecovery"]["barrier"].update(status="FAIL"),
            lambda a: a["retiredRecovery"]["events"][0].update(stoppedAt=2),
            lambda a: a["retiredRecovery"]["events"][0].update(afterAgent="3 30\nother"),
            lambda a: a["processes"]["b"].update(speaker="new"),
            lambda a: a["retiredRecovery"]["drained"].update(leaseHeld=False),
        ):
            altered = copy.deepcopy(after)
            change(altered)
            with self.assertRaises(LabError):
                check_barrier(before, altered)

    def test_exact_drain_preserves_foreign_quarantine(self):
        before, after = evidence()
        self.assertEqual(check_barrier(before, after)["status"], "PASS")

    def test_negative_drain_gates(self):
        def mutate_record(after, owner, kind, change):
            record = after["owners"][owner][kind]["Record"]
            change(record)
            after["owners"][owner][kind] = envelope(record["State"], record["Sequence"])
            if record["Pending"]:
                after["owners"][owner][kind]["Record"]["Pending"] = record["Pending"]
                r = after["owners"][owner][kind]["Record"]
                after["owners"][owner][kind]["SHA256"] = hashlib.sha256(
                    json.dumps(r, separators=(",", ":")).encode()).hexdigest()

        changes = {
            "incomplete RIB": lambda a: a["owners"]["a"]["status"]["Snapshot"].update(Authoritative=False),
            "unacknowledged UUID": lambda a: a["owners"]["b"]["exportStatus"].update(OwnershipUnverified=True),
            "remaining receipt": lambda a: mutate_record(a, "a", "import",
                lambda r: r["State"].update(Receipts={"owned": {}})),
            "pending intent": lambda a: mutate_record(a, "r2", "export",
                lambda r: r.update(Pending={"Operation": "withdraw"})),
            "stale generation": lambda a: a.update(
                ledger=envelope({"Phase": "retired", "Generation": 1, "Incarnation": "stale"})),
            "missing journal": lambda a: a["owners"]["a"].pop("export"),
            "corrupt journal": lambda a: a["owners"]["a"]["export"].update(SHA256="corrupt"),
            "failed attestation": lambda a: a["owners"]["a"].update(speaker={"PID": 43}),
            "matching foreign tuple": lambda a: a["owners"]["a"]["kernel"]["fdb"].append(
                {"mac": "owned", "dst": "foreign"}),
            "global blocker": lambda a: mutate_record(a, "a", "import",
                lambda r: r["State"].update(Quarantine="uncertain")),
            "premature shutdown": lambda a: a.update(processes={}),
            "premature teardown": lambda a: a.update(leaseHeld=False),
            "owned OVS": lambda a: a["ovs"]["tunnels"].append(["owned"]),
            "owned SB FDB": lambda a: a["southboundOwned"].append(["owned"]),
            "FRR kernel": lambda a: a["remote1Kernel"]["fdb"].append({"mac": "02:00:00:01:00:0a"}),
            "stale observation": lambda a: a["owners"]["a"].update(freshTimes=["1", "3"]),
            "resurrection": lambda a: a["owners"]["b"]["status"]["Snapshot"]["Endpoints"].append("owned"),
            "foreign quarantine cleared": lambda a: mutate_record(a, "a", "import",
                lambda r: r["State"].update(Quarantines={})),
        }
        for name, change in changes.items():
            with self.subTest(name=name):
                before, after = evidence()
                change(after)
                with self.assertRaises((LabError, KeyError)):
                    check_barrier(before, after)

    def test_shutdown_and_coordinator_are_interlocked(self):
        fixture = FinalDrain(Path("/tmp/not-created-h1"), "unit")
        fixture.draining = True
        fixture.rt = Mock()
        self.assertFalse(fixture.teardown_allowed())
        with self.assertRaises(LabError):
            fixture.stop_process("speaker")
        with self.assertRaises(LabError):
            fixture.bstop("projector", True)
        fixture.rt.assert_not_called()

    def test_failed_barrier_prevents_shared_runner_teardown(self):
        with TemporaryDirectory() as root:
            path = Path(root) / "evidence"
            fixture = Mock()
            fixture.inputs.side_effect = LabError("injected failed barrier")
            fixture.ext_captures = fixture.captures = []
            fixture.docker_witness = None
            fixture.teardown_allowed.return_value = False
            with patch.object(endpoint_lifecycle.lab, "exclusive", return_value=nullcontext()), \
                    patch.object(endpoint_lifecycle.lab, "load", return_value={"phase": "ready"}), \
                    patch.object(endpoint_lifecycle.lab, "collect"), \
                    patch.object(endpoint_lifecycle.lab, "down") as down:
                # The retained topology exists before the injected failure.
                def factory(directory, cluster):
                    (directory / "lab.json").write_text("{}")
                    return fixture
                self.assertEqual(endpoint_lifecycle.run(path, "unit", factory), 1)
                down.assert_not_called()
                self.assertEqual(json.loads((path / "r5-result.json").read_text())[
                    "cases"]["cleanup"]["status"], "BLOCKED")

    def test_changed_frr_neighbor_is_not_deleted(self):
        fixture = FinalDrain(Path("/tmp/not-created-h1"), "unit")
        fixture.fr = Mock(return_value=json.dumps([
            {"dst": "10.100.1.101", "lladdr": "02:00:00:ff:ff:ff"}]))
        before = {"remote1Kernel": {"neighbors": [
            {"dst": "10.100.1.101", "lladdr": "02:00:00:01:00:65"}]}}
        with self.assertRaises(LabError):
            fixture.retire_remote1_neighbor(before)
        self.assertEqual(fixture.fr.call_count, 1)


if __name__ == "__main__":
    unittest.main()
