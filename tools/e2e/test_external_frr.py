"""Offline checks for the bounded combined FRR fixture."""

import copy
import json
from pathlib import Path
import struct
import tempfile
import unittest
from unittest.mock import patch

import containerlab as lab
import external_frr as external
import test_geneve_baseline as geneve_tests


class ExternalTests(unittest.TestCase):
    def test_root_owned_node_key_is_checked_without_chmod(self):
        key = Path("/private/node.key")
        certificate = Path("/private/node.pem")
        ca = Path("/private/ca.pem")
        with patch.object(Path, "stat", side_effect=PermissionError), \
                patch.object(Path, "chmod", side_effect=AssertionError("must not chmod")), \
                patch.object(lab, "command") as command:
            command.return_value.stdout = "regular file:600\n"
            lab.verify_node_certificate(key, certificate, ca, "unit-frr")
            self.assertEqual(command.call_args_list[0].args[0],
                             ["sudo", "-n", "stat", "--format=%F:%a", "--", str(key)])
            self.assertEqual(command.call_args_list[1].args[0],
                             ["sudo", "-n", "openssl", "verify", "-purpose", "sslserver",
                              "-verify_hostname", "unit-frr", "-CAfile", str(ca),
                              str(certificate)])
            command.return_value.stdout = "regular file:644\n"
            with self.assertRaisesRegex(lab.LabError, "0600"):
                lab.verify_node_certificate(key, certificate, ca, "unit-frr")

    def test_node_key_creation_umask_is_restrictive(self):
        result = lab.command(["sh", "-c", "umask"], umask=0o077)
        self.assertEqual(result.stdout.strip(), "0077")

    def test_reservation_has_two_workers_and_one_frr(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory)
            state = lab.init(path, "geneve-frr", "unit", "external-unit", "unit-frr")
            self.assertEqual(len(state["kind_nodes"]), 3)
            self.assertEqual(state["containers"], state["kind_nodes"] + ["unit-frr"])
            self.assertEqual(state["subnet"], "198.18.10.0/24")
            self.assertEqual(state["geneve_network"], "external-unit-geneve")
            topology = json.loads((path / "lab.clab.yaml").read_text())
            self.assertEqual(topology["topology"]["nodes"]["unit-frr"]["mgmt-ipv4"], "198.18.10.3")
            self.assertEqual(lab.load(path), state)

    def test_external_profile_requires_frr(self):
        with tempfile.TemporaryDirectory() as directory:
            with self.assertRaisesRegex(lab.LabError, "requires exactly"):
                lab.init(Path(directory), "geneve-frr", "unit", "external-unit")

    def test_inventory_profile_reserves_one_external_peer_and_two_workers(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory)
            state = lab.init(path, "cni-inventory-frr", "unit", "inventory-unit", "unit-frr")
            self.assertEqual(state["containers"], state["kind_nodes"] + ["unit-frr"])
            self.assertEqual(state["subnet"], "198.18.30.0/24")
            topology = json.loads((path / "lab.clab.yaml").read_text())
            self.assertEqual(topology["topology"]["nodes"]["unit-frr"]["mgmt-ipv4"], "198.18.30.4")
            self.assertEqual(lab.load(path), state)
        with tempfile.TemporaryDirectory() as directory:
            with self.assertRaisesRegex(lab.LabError, "requires exactly"):
                lab.init(Path(directory), "cni-inventory-frr", "unit", "inventory-unit")

    def test_geneve_network_id_replacement_blocks_cleanup(self):
        state = {"cluster": None, "network": "external", "resources": {
            "containers": {}, "network": None, "geneve_network": "old"},
            "geneve_network": "geneve"}
        with patch.object(lab, "inventory", return_value={
                "containers": {}, "network": None, "geneve_network": "new"}), \
                patch.object(lab, "inspect", side_effect=lambda kind, name: (
                    {"Id": "new", "Containers": {}} if name == "geneve" else None)):
            with self.assertRaisesRegex(lab.LabError, "receipt/membership changed"):
                lab.verify_owned(state)

    def test_foreign_geneve_attachment_blocks_cleanup(self):
        state = {"cluster": None, "network": "external", "resources": {
            "containers": {}, "network": None, "geneve_network": "exact"},
            "geneve_network": "geneve"}
        with patch.object(lab, "inventory", return_value={
                "containers": {}, "network": None, "geneve_network": "exact"}), \
                patch.object(lab, "inspect", side_effect=lambda kind, name: (
                    {"Id": "exact", "Containers": {"foreign": {}}} if name == "geneve" else None)):
            with self.assertRaisesRegex(lab.LabError, "receipt/membership changed"):
                lab.verify_owned(state)

    @staticmethod
    def packets(vni=1000, duplicate=False, peer_as_vtep=False):
        frames = []
        for source, destination, smac, dmac, sip, dip in (
            ("198.18.10.2", "198.18.10.4", external.LOCAL_MAC, external.REMOTE_MAC,
             external.LOCAL_IP, external.REMOTE_IP),
            ("198.18.10.4", "198.18.10.2", external.REMOTE_MAC, external.LOCAL_MAC,
             external.REMOTE_IP, external.LOCAL_IP)):
            if peer_as_vtep:
                source = source.replace("198.18.10.4", "198.18.10.3")
                destination = destination.replace("198.18.10.4", "198.18.10.3")
            for kind in (0, 8):
                for sequence in range(1, 4):
                    icmp = struct.pack("!BBHHH", kind, 0, 0, 123, 1 if duplicate else sequence)
                    inner = (bytes.fromhex(dmac.replace(":", "")) +
                             bytes.fromhex(smac.replace(":", "")) + b"\x08\x00" +
                             geneve_tests.GeneveTests.ipv4(sip, dip, icmp, 1))
                    vxlan = b"\x08\x00\x00\x00" + vni.to_bytes(3, "big") + b"\x00" + inner
                    udp = struct.pack("!HHHH", 40000, 4789, len(vxlan) + 8, 0) + vxlan
                    frames.append(b"\x00" * 12 + b"\x08\x00" +
                                  geneve_tests.GeneveTests.ipv4(source, destination, udp, 17))
        return frames

    def test_both_directions_attributed(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "external.pcap"
            geneve_tests.GeneveTests.pcap(path, self.packets())
            self.assertEqual(len(external.decode_vxlan(path)["packets"]), 12)

    def test_wrong_vni_duplicate_and_peer_next_hop_rejected(self):
        for kwargs, message in (({"vni": 2000}, "wrong VXLAN"),
                                ({"duplicate": True}, "missing attributed"),
                                ({"peer_as_vtep": True}, "missing attributed")):
            with self.subTest(kwargs=kwargs), tempfile.TemporaryDirectory() as directory:
                path = Path(directory) / "external.pcap"
                geneve_tests.GeneveTests.pcap(path, self.packets(**kwargs))
                with self.assertRaisesRegex(lab.LabError, message):
                    external.decode_vxlan(path)

    @staticmethod
    def rib():
        return {"remote": [{"peer-address": "198.18.10.3", "nlri": {"type": 2, "value": {
            "mac": external.REMOTE_MAC, "ip": external.REMOTE_IP,
            "rd": {"type": 1, "admin": "198.18.10.3", "assigned": 1000},
            "etag": 0, "esi": "single-homed", "labels": [1000]}},
            "attrs": [{"type": 14, "nexthop": "198.18.10.4", "afi": 25, "safi": 70},
                      {"type": 16, "value": [{"value": "64512:1000"}, {"tunnel_type": 8}]}]}]}

    def test_raw_route_attributes_and_peer_vtep_distinction(self):
        external.validate_raw_rib(self.rib(), require_local=False)
        wrong = copy.deepcopy(self.rib())
        wrong["remote"][0]["attrs"][0]["nexthop"] = "198.18.10.3"
        with self.assertRaisesRegex(lab.LabError, "MP_REACH"):
            external.validate_raw_rib(wrong, require_local=False)

    def test_route_outside_identity_rejected(self):
        wrong = self.rib()
        wrong["remote"][0]["nlri"]["value"]["mac"] = "02:00:00:fa:00:01"
        with self.assertRaisesRegex(lab.LabError, "unexpected Type-2"):
            external.validate_raw_rib(wrong, require_local=False)


if __name__ == "__main__":
    unittest.main()
