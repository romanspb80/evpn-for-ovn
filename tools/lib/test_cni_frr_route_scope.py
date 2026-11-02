"""Offline check of the actual route-scope assertion's confirmed evidence input."""

import json
from pathlib import Path
import subprocess
import tempfile
import unittest


ROOT = Path(__file__).resolve().parents[2]
HELPER = ROOT / "tools/lib/cni-evpn-frr-interop-test.sh"


class RouteScopeTests(unittest.TestCase):
    def test_interop_uses_the_optional_exclusive_netlink_writer(self):
        helper = HELPER.read_text()
        runner = (ROOT / "tools/run-cni-mvp-2.4b-multus.sh").read_text()
        setup = helper[
            helper.index("interop_setup_config() {"):
            helper.index("\ninterop_capture_discover() {")
        ]
        provider = helper[
            helper.index("interop_setup_provider() {"):
            helper.index("\ninterop_setup_underlay() {")
        ]
        self.assertIn('"Enabled":true', setup)
        self.assertIn('"Netlink":{"Enabled":true,"ExclusiveWriter":true', setup)
        self.assertIn('"AllowedVTEPs":["${FRR_VTEP}"]', setup)
        self.assertIn(
            "REMOTE_FLAGS='--evpn-remote-config=/run/evpn-node-agent/remote-config.json",
            provider,
        )
        self.assertIn("${EXPORT_FLAGS} ${REMOTE_FLAGS}", runner)
        self.assertIn('nodeSelector: {kubernetes.io/hostname: "${WORKER}"}', runner)
        self.assertNotIn("bgp-ad-handler", runner)

    def test_packet_capture_uses_same_confirmed_snapshot(self):
        text = HELPER.read_text()
        start = text[text.index("interop_capture_start() {"):
                      text.index("\ninterop_capture_stop() {")]
        self.assertIn(
            'local phase="${1:-initial}" route_file="${EVIDENCE_DIR}/remote-status-initial-packet-path.json"',
            start)

    def exercise(self, authoritative, header="Advertised to non peer-group peers:",
                 advertised_peer="198.18.0.2", mac_only=True):
        text = HELPER.read_text()
        function = text[text.index("interop_assert_initial_route_scope() {"):
                        text.index("\ninterop_verify() {")]
        mac, ip, peer = "0a:58:f0:01:00:0c", "10.244.240.12", "198.18.0.3"
        with tempfile.TemporaryDirectory() as directory:
            p = Path(directory)
            prefix = f"[type:macadv][rd:{peer}:1000][etag:0][mac:{mac}]"
            (p / "gobgp-evpn-rib.txt").write_text(prefix + "\n" + prefix + f"[ip:{ip}]\n")
            route = f"BGP routing table entry for {peer}:1000:[2]:[0]:[48]:[{mac}]"
            (p / "frr-evpn-rib.txt").write_text("".join(
                item + "\nPaths: (1 available, best #1)\n"
                f"  {header}\n  {advertised_peer}\n"
                "  Local\n    198.18.0.2 from 198.18.0.2\n"
                for item in ((route,) if mac_only else ()) + (route + f":[32]:[{ip}]",)))
            (p / "frr-running-config.txt").write_text("advertise-all-vni\n")
            endpoint = {"MAC": mac, "IP": ip, "RemoteVTEP": "198.18.0.4", "VNI": 1000,
                        "Routes": [{"Route": {"IP": address, "MAC": mac, "Labels": [1000],
                                              "RouteTargets": ["64512:1000"],
                                              "NextHop": "198.18.0.4"}} for address in ("", ip)]}
            status = {"Snapshot": {"Authoritative": authoritative, "Endpoints": [endpoint]},
                      "Receipts": [{}]}
            (p / "remote-status-initial-packet-path.json").write_text(json.dumps(status))
            (p / "remote-status.json").write_text(json.dumps(
                {"Error": "older transitional snapshot", "Snapshot": {"Authoritative": False}}))
            return subprocess.run(
                ["bash", "-c", function + '\nEVIDENCE_DIR="$1"; FRR_PEER="$2"; VNI=1000; '
                 'REMOTE_MAC="$3"; REMOTE_IP="$4"; interop_assert_initial_route_scope',
                 "sh", directory, peer, mac, ip], capture_output=True)

    def test_confirmed_snapshot_not_older_pre_wait_copy(self):
        result = self.exercise(True)
        self.assertEqual(result.returncode, 0, result.stderr)

    def test_non_authoritative_confirmed_file_still_rejected(self):
        self.assertNotEqual(self.exercise(False).returncode, 0)

    def test_frr_107_advertisement_header(self):
        result = self.exercise(True, header="Advertised to peers:")
        self.assertEqual(result.returncode, 0, result.stderr)

    def test_unadvertised_route_with_matching_next_hop_is_rejected(self):
        self.assertNotEqual(self.exercise(True, header="Not advertised to any peer").returncode, 0)

    def test_other_advertised_peer_is_rejected(self):
        self.assertNotEqual(self.exercise(True, header="Advertised to peers:",
                                          advertised_peer="198.18.0.20").returncode, 0)

    def test_mac_ip_does_not_substitute_for_mac_only_route(self):
        self.assertNotEqual(self.exercise(True, header="Advertised to peers:",
                                          mac_only=False).returncode, 0)


if __name__ == "__main__":
    unittest.main()
