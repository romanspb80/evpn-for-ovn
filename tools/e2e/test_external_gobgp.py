"""Focused Domain2 topology, raw route and ownership-contract checks."""
import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import Mock, patch

import containerlab as lab
from external_gobgp import B_MAC, R2_MAC, ThreeDomainBaseline, validate_rib


def route(mac, remote=False):
    b = mac == B_MAC
    address = "198.18.20.2" if b else "198.18.20.3"
    return {"peer-address": address if (remote and b) or (not remote and not b) else "",
            "nlri": {"type": 2, "value": {"mac": mac, "ip": "10.100.2.10" if b else "10.100.2.101",
                     "rd": {"type": 1, "admin": address, "assigned": 2000},
                     "labels": [2000], "etag": 0, "esi": "single-homed"}},
            "attrs": [{"type": 14, "nexthop": "198.18.20.2" if b else "198.18.20.4",
                       "afi": 25, "safi": 70},
                      {"type": 16, "value": [{"value": "64513:2000"}, {"tunnel_type": 8}]}]}


class Domain2Tests(unittest.TestCase):
    def test_recreation_preserves_immutable_config_and_requires_retirement(self):
        fixture = ThreeDomainBaseline(Path("/tmp/not-created"), "unit-cluster")
        fixture.r_generation, fixture.r_incarnation = 1, "original-incarnation"
        fixture.rconfig = {"Endpoint": {"Incarnation": "original-incarnation"}}
        state = {"Phase": "active", "Generation": 1, "Incarnation": fixture.r_incarnation}
        fixture.rread = Mock(return_value={"Lifecycle": state})
        fixture.bput = Mock()
        with self.assertRaisesRegex(lab.LabError, "acknowledged"):
            fixture.recreate_r2_generation()
        fixture.bput.assert_not_called()
        state["Phase"] = "retired"
        fixture.recreate_r2_generation()
        request = fixture.bput.call_args.args
        self.assertEqual(request[0], "endpoint.json")
        self.assertEqual(request[1]["Generation"], 2)
        self.assertFalse(request[1]["Retired"])
        self.assertNotEqual(request[1]["Incarnation"], "original-incarnation")
        self.assertEqual(fixture.rconfig["Endpoint"]["Incarnation"], "original-incarnation")
        with self.assertRaisesRegex(lab.LabError, "current-generation"):
            fixture.recreate_r2_generation()

    def test_received_routes(self):
        for remote in (False, True):
            validate_rib({"routes": [route(B_MAC, remote), route(R2_MAC, remote)]}, remote)

    def test_peer_is_not_vtep(self):
        path = route(R2_MAC)
        path["attrs"][0]["nexthop"] = "198.18.20.3"
        with self.assertRaises(lab.LabError):
            validate_rib({"routes": [route(B_MAC), path]})

    def test_wrong_domain(self):
        for field, value in (("labels", [1000]), ("mac", "02:00:00:01:00:0a")):
            path = route(R2_MAC)
            path["nlri"]["value"][field] = value
            with self.assertRaises(lab.LabError):
                validate_rib({"routes": [route(B_MAC), path]})

    def test_exact_foreign_kernel_identity(self):
        with tempfile.TemporaryDirectory() as directory:
            fixture = object.__new__(ThreeDomainBaseline)
            fixture.directory = Path(directory)
            fdb = {"mac": "02:00:00:fb:00:01", "dst": "198.18.20.99",
                   "flags": ["self"], "state": "permanent"}
            neighbor = {"dst": "10.100.2.199", "lladdr": "02:00:00:fb:00:01",
                        "state": ["PERMANENT"]}
            def run(*args, **kwargs):
                return json.dumps([fdb if "fdb" in args else neighbor])
            fixture.r2 = run
            fixture.rt = run
            fixture.b_foreign()
            self.assertEqual(set(json.loads(
                (fixture.directory / "domain2-foreign-preserved.json").read_text())), {"b", "r2"})
            fdb["dst"] = "198.18.20.98"
            with self.assertRaisesRegex(lab.LabError, "FDB identity"):
                fixture.b_foreign()
            fdb["dst"] = "198.18.20.99"
            neighbor["lladdr"] = B_MAC
            with self.assertRaisesRegex(lab.LabError, "neighbor identity"):
                fixture.b_foreign()

    def test_b_configs_do_not_overwrite_a_evidence(self):
        with tempfile.TemporaryDirectory() as directory:
            fixture = object.__new__(ThreeDomainBaseline)
            fixture.directory = Path(directory)
            fixture.pods = {"b": "ovn-b"}
            fixture.k = lambda *args: None
            fixture.rt = lambda *args: None
            for name in ("export.json", "import.json", "runtime.json"):
                original = fixture.directory / name
                original.write_text('{"owner":"A"}')
                fixture.bput(name, {"owner": "B"})
                self.assertEqual(json.loads(original.read_text()), {"owner": "A"})
                self.assertEqual(json.loads((fixture.directory / f"b-{name}").read_text()), {"owner": "B"})

    def test_full_topology_reservation(self):
        with tempfile.TemporaryDirectory() as directory:
            state = lab.init(Path(directory), "geneve-full", "unit-cluster", "unit-net", "unit-frr")
            self.assertEqual(len(state["kind_nodes"]), 3)
            self.assertEqual(len(state["containers"]), 5)
            self.assertEqual(state["remote2"], "unit-cluster-remote2")
            topology = json.loads((Path(directory) / "lab.clab.yaml").read_text())
            self.assertIn(state["remote2"], topology["topology"]["nodes"])
            self.assertEqual(state["resources"]["remote2_network"], None)

    def test_remote2_network_receipt_and_foreign_attachment(self):
        with tempfile.TemporaryDirectory() as directory:
            state = lab.init(Path(directory), "geneve-full", "unit-cluster", "unit-net", "unit-frr")
            state["resources"]["remote2_network"] = "original"
            network = {"Id": "replacement", "Labels": {"evpn.e2e.owner": state["owner"]},
                       "IPAM": {"Config": [{"Subnet": "198.18.20.0/24"}]},
                       "Internal": True, "Containers": {}}
            def inspect(kind, name):
                return network if name == state["remote2_network"] else None
            with patch.object(lab, "inspect", side_effect=inspect):
                with self.assertRaises(lab.LabError):
                    lab.verify_owned(state)
                network["Id"] = "original"
                network["Containers"] = {"foreign": {}}
                with self.assertRaises(lab.LabError):
                    lab.verify_owned(state)
