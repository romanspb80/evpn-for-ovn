"""Focused offline provisioning and attributed-packet checks."""

import json
from pathlib import Path
import struct
import tempfile
import unittest

import containerlab as lab
import geneve_baseline as baseline


class GeneveTests(unittest.TestCase):
    def test_two_worker_reservation(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory)
            state = lab.init(path, "geneve", "geneve-unit", "geneve-unit-network")
            self.assertEqual(state["kind_nodes"], [
                "geneve-unit-control-plane", "geneve-unit-worker", "geneve-unit-worker2"])
            self.assertEqual(state["containers"], state["kind_nodes"])
            self.assertIsNone(state["frr"])
            self.assertEqual(state["subnet"], "198.18.30.0/24")
            topology = json.loads((path / "lab.clab.yaml").read_text())
            self.assertEqual(list(topology["topology"]["nodes"]), ["geneve-unit"])
            self.assertEqual(topology["topology"]["nodes"]["geneve-unit"]["startup-config"],
                             str(path / "kind.yaml"))
            self.assertEqual(lab.load(path), state)
            (path / "kind.yaml").write_text("changed")
            with self.assertRaisesRegex(lab.LabError, "Kind configuration changed"):
                lab.load(path)

    def test_existing_cni_reservation_unchanged(self):
        with tempfile.TemporaryDirectory() as directory:
            state = lab.init(Path(directory), "cni", "geneve-unit", "geneve-unit-network")
            self.assertEqual(len(state["kind_nodes"]), 2)
            self.assertNotIn("kind_config_sha256", state)

    def test_isolated_cni_reserves_two_workers(self):
        with tempfile.TemporaryDirectory() as directory:
            state = lab.init(Path(directory), "cni-isolated", "isolated-unit",
                             "isolated-unit-network")
            self.assertEqual(state["kind_nodes"], [
                "isolated-unit-control-plane", "isolated-unit-worker",
                "isolated-unit-worker2"])
            self.assertEqual(state["subnet"], "198.18.30.0/24")
            self.assertIn("kind_config_sha256", state)
            self.assertEqual(state["frr"], None)

    def test_reject_external_peers(self):
        with tempfile.TemporaryDirectory() as directory:
            with self.assertRaisesRegex(lab.LabError, "external BGP"):
                lab.init(Path(directory), "geneve", "geneve-unit", "geneve-unit-network", "frr")

    @staticmethod
    def ipv4(source, destination, payload, protocol):
        import ipaddress
        return (struct.pack("!BBHHHBBH", 0x45, 0, 20 + len(payload), 0, 0, 64, protocol, 0)
                + ipaddress.IPv4Address(source).packed
                + ipaddress.IPv4Address(destination).packed + payload)

    @classmethod
    def packet(cls, source, destination, kind, key=42, port=6081, options=True, sequence=1):
        smac, sip, svtep = baseline.ENDPOINTS[source]
        dmac, dip, dvtep = baseline.ENDPOINTS[destination]
        icmp = struct.pack("!BBHHH", kind, 0, 0, 123, sequence) + b"geneve-test"
        inner = (bytes.fromhex(dmac.replace(":", "")) + bytes.fromhex(smac.replace(":", ""))
                 + b"\x08\x00" + cls.ipv4(sip, dip, icmp, 1))
        option = b"\x01\x02\x00\x00" if options else b""
        geneve = bytes([len(option) // 4, 0]) + b"\x65\x58" + key.to_bytes(3, "big") + b"\x00"
        payload = geneve + option + inner
        udp = struct.pack("!HHHH", 40000, port, len(payload) + 8, 0) + payload
        return b"\x00" * 12 + b"\x08\x00" + cls.ipv4(svtep, dvtep, udp, 17)

    @staticmethod
    def pcap(path, frames):
        data = b"\xd4\xc3\xb2\xa1" + struct.pack("<HHIIII", 2, 4, 0, 0, 65535, 1)
        for frame in frames:
            data += struct.pack("<IIII", 1, 0, len(frame), len(frame)) + frame
        path.write_bytes(data)

    def test_attributed_geneve(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "geneve.pcap"
            frames = [self.packet(s, d, kind, sequence=sequence)
                      for s, d in (("a", "b"), ("b", "a"))
                      for kind in (0, 8) for sequence in range(1, 4)]
            self.pcap(path, frames)
            report = baseline.decode_geneve(path, 42)
            self.assertEqual(len(report["packets"]), 12)

    def test_wrong_key_rejected(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "geneve.pcap"
            self.pcap(path, [self.packet("a", "b", 8, key=43)])
            with self.assertRaisesRegex(lab.LabError, "key differs"):
                baseline.decode_geneve(path, 42)

    def test_vxlan_port_cannot_satisfy_geneve(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "geneve.pcap"
            self.pcap(path, [self.packet("a", "b", 8, port=4789)])
            with self.assertRaisesRegex(lab.LabError, "external VXLAN"):
                baseline.decode_geneve(path, 42)

    def test_duplicate_packets_do_not_satisfy_probe_count(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "geneve.pcap"
            self.pcap(path, [self.packet("a", "b", 8)] * 3)
            with self.assertRaisesRegex(lab.LabError, "missing attributed"):
                baseline.decode_geneve(path, 42)


if __name__ == "__main__":
    unittest.main()
