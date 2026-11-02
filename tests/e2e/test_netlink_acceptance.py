import ipaddress
import importlib.util
import json
import struct
import subprocess
import tempfile
import unittest
from pathlib import Path
from unittest import mock


ROOT = Path(__file__).resolve().parents[2]
SCRIPT = ROOT / "tools/e2e/netlink-acceptance.py"
SPEC = importlib.util.spec_from_file_location("netlink_acceptance", SCRIPT)
assert SPEC and SPEC.loader
ACCEPTANCE = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(ACCEPTANCE)

LOCAL_IP = "198.18.1.101"
REMOTE_IP = "198.18.1.102"
LOCAL_MAC = "02:00:00:00:00:01"
REMOTE_MAC = "02:00:00:00:00:02"
LOCAL_VTEP = "198.18.0.4"
REMOTE_VTEP = "198.18.0.2"


def mac_bytes(value):
    return bytes.fromhex(value.replace(":", ""))


def ethernet_frame(source_mac, destination_mac, ether_type, payload):
    return (
        mac_bytes(destination_mac)
        + mac_bytes(source_mac)
        + struct.pack("!H", ether_type)
        + payload
    )


def icmp_frame(source_ip, destination_ip, source_mac, destination_mac, icmp_type):
    icmp = struct.pack("!BBHHH", icmp_type, 0, 0, 7, 1) + b"x"
    ip_header = struct.pack(
        "!BBHHHBBH4s4s",
        0x45,
        0,
        20 + len(icmp),
        1,
        0,
        64,
        1,
        0,
        ipaddress.ip_address(source_ip).packed,
        ipaddress.ip_address(destination_ip).packed,
    )
    return ethernet_frame(
        source_mac, destination_mac, 0x0800, ip_header + icmp
    )


def arp_frame(sender_ip, sender_mac, target_ip):
    arp = struct.pack(
        "!HHBBH6s4s6s4s",
        1,
        0x0800,
        6,
        4,
        1,
        mac_bytes(sender_mac),
        ipaddress.ip_address(sender_ip).packed,
        b"\x00" * 6,
        ipaddress.ip_address(target_ip).packed,
    )
    return ethernet_frame(sender_mac, "ff:ff:ff:ff:ff:ff", 0x0806, arp)


def vxlan_frame(
    inner,
    source_vtep=LOCAL_VTEP,
    destination_vtep=REMOTE_VTEP,
    vni=1000,
):
    vxlan = b"\x08\x00\x00\x00" + vni.to_bytes(3, "big") + b"\x00"
    udp_payload = vxlan + inner
    udp = struct.pack("!HHHH", 49152, 4789, 8 + len(udp_payload), 0)
    ip_header = struct.pack(
        "!BBHHHBBH4s4s",
        0x45,
        0,
        20 + len(udp) + len(udp_payload),
        2,
        0,
        64,
        17,
        0,
        ipaddress.ip_address(source_vtep).packed,
        ipaddress.ip_address(destination_vtep).packed,
    )
    return ethernet_frame(
        "02:00:00:00:00:fe",
        "02:00:00:00:00:fd",
        0x0800,
        ip_header + udp + udp_payload,
    )


def write_pcap(path, frames):
    with path.open("wb") as stream:
        stream.write(b"\xd4\xc3\xb2\xa1")
        stream.write(struct.pack("<HHIIII", 2, 4, 0, 0, 65535, 1))
        for index, frame in enumerate(frames, start=1):
            stream.write(struct.pack("<IIII", 100 + index, 0, len(frame), len(frame)))
            stream.write(frame)


class NetlinkAcceptanceHelpersTest(unittest.TestCase):
    def test_local_endpoint_addresses_are_deterministic_and_on_link(self):
        run_id = "12345678-1234-5678-1234-567812345678"
        gateway, endpoint = ACCEPTANCE.local_endpoint_addresses(run_id)
        self.assertEqual(
            (gateway, endpoint),
            ACCEPTANCE.local_endpoint_addresses(run_id),
        )
        self.assertEqual(
            int(ipaddress.ip_address(endpoint)) - int(ipaddress.ip_address(gateway)),
            1,
        )

    def test_local_endpoint_interface_uses_endpoint_identity_not_bridge_identity(self):
        endpoint = {"mac": LOCAL_MAC, "ip": LOCAL_IP}
        commands = ACCEPTANCE.local_endpoint_setup_commands(
            "br-e2e",
            "02:00:00:00:00:09",
            endpoint,
            "198.18.1.1",
            [REMOTE_IP],
        )
        self.assertIn(
            [
                "ip",
                "link",
                "set",
                "dev",
                ACCEPTANCE.LOCAL_ENDPOINT_INTERFACE,
                "address",
                LOCAL_MAC,
            ],
            commands,
        )
        self.assertIn(
            ["ip", "link", "set", "dev", "br-e2e", "address", "02:00:00:00:00:09"],
            commands,
        )
        self.assertIn(
            [
                "ip",
                "address",
                "add",
                f"{LOCAL_IP}/24",
                "dev",
                ACCEPTANCE.LOCAL_ENDPOINT_INTERFACE,
            ],
            commands,
        )
        self.assertIn(
            [
                "ip",
                "route",
                "replace",
                f"{REMOTE_IP}/32",
                "via",
                "198.18.1.1",
                "dev",
                ACCEPTANCE.LOCAL_ENDPOINT_INTERFACE,
                "src",
                LOCAL_IP,
            ],
            commands,
        )

    def test_local_endpoint_interface_identity_matches_configured_mac_and_ip(self):
        endpoint = {"mac": LOCAL_MAC, "ip": LOCAL_IP}
        document = [
            {
                "ifname": ACCEPTANCE.LOCAL_ENDPOINT_INTERFACE,
                "address": LOCAL_MAC.upper(),
                "addr_info": [{"local": LOCAL_IP, "prefixlen": 24}],
            }
        ]
        matches, _ = ACCEPTANCE.local_endpoint_interface_matches(endpoint, document)
        self.assertTrue(matches)
        wrong_mac = json.loads(json.dumps(document))
        wrong_mac[0]["address"] = REMOTE_MAC
        self.assertFalse(
            ACCEPTANCE.local_endpoint_interface_matches(endpoint, wrong_mac)[0]
        )

    def test_export_classification_distinguishes_supported_route_stages(self):
        supported = {"exitCode": 0}
        unsupported = {"exitCode": 1}
        self.assertEqual(
            ACCEPTANCE.classify_local_route_export(
                False, supported, False, supported, False, False
            ),
            "Route absent from GoBGP Local RIB",
        )
        self.assertEqual(
            ACCEPTANCE.classify_local_route_export(
                True, supported, False, supported, False, False
            ),
            "Route absent from GoBGP export",
        )
        self.assertEqual(
            ACCEPTANCE.classify_local_route_export(
                True, supported, True, supported, False, True
            ),
            "Route accepted into the FRR EVPN route table",
        )
        self.assertEqual(
            ACCEPTANCE.classify_local_route_export(
                True, supported, True, supported, True, False
            ),
            "Route exported and received by FRR but absent from accepted routes",
        )
        self.assertEqual(
            ACCEPTANCE.classify_local_route_export(
                True, unsupported, False, unsupported, False, False
            ),
            "Insufficient evidence to determine the export/import boundary",
        )

    def test_gobgp_route_record_detection_requires_exact_mac_ip_path(self):
        endpoint = {"mac": LOCAL_MAC, "ip": LOCAL_IP}
        exact = json.dumps(
            {
                "route": {
                    "nlri": {
                        "mac": LOCAL_MAC,
                        "ip": LOCAL_IP,
                    },
                    "attrs": [{"type": 3, "value": "198.18.0.4"}],
                }
            }
        )
        wrong_pair = json.dumps(
            {
                "route": {
                    "nlri": {
                        "mac": LOCAL_MAC,
                        "ip": REMOTE_IP,
                    },
                    "attrs": [{"type": 3, "value": "198.18.0.4"}],
                }
            }
        )
        self.assertTrue(ACCEPTANCE.gobgp_route_records_present(exact, endpoint))
        self.assertFalse(ACCEPTANCE.gobgp_route_records_present(wrong_pair, endpoint))

    def test_fixture_addresses_avoid_peer_vteps_and_endpoints(self):
        subnet = "198.18.1.0/24"
        default_remote_vtep = ACCEPTANCE.allocate_fixture_addresses(
            subnet,
            [
                "198.18.1.1",
                "198.18.1.2",
                "198.18.1.3",
                "198.18.1.4",
                "198.18.1.101",
                "198.18.1.102",
            ],
            2,
        )
        self.assertEqual(default_remote_vtep, ["198.18.1.5", "198.18.1.6"])
        reserved = [
            "198.18.1.1",
            "198.18.1.2",
            "198.18.1.3",
            "198.18.1.5",
            "198.18.1.101",
            "198.18.1.102",
        ]
        allocated = ACCEPTANCE.allocate_fixture_addresses(subnet, reserved, 2)
        self.assertEqual(allocated, ["198.18.1.4", "198.18.1.6"])
        self.assertTrue(
            set(map(ipaddress.ip_address, allocated)).isdisjoint(
                map(ipaddress.ip_address, reserved)
            )
        )

    def test_fixture_endpoints_may_use_owned_secondary_subnet(self):
        parameters = {
            "subnet": "198.18.74.0/24",
            "endpoint_subnet": "198.19.67.0/24",
            "endpoints": [
                {"name": "primary", "ip": "198.18.74.101"},
                {"name": "secondary", "ip": "198.19.67.2"},
            ],
        }
        ACCEPTANCE.validate_fixture_endpoint_addresses(parameters)
        parameters["endpoints"][1]["ip"] = "198.20.1.2"
        with self.assertRaisesRegex(
            ACCEPTANCE.AcceptanceError, "outside the owned endpoint subnets"
        ):
            ACCEPTANCE.validate_fixture_endpoint_addresses(parameters)

    def test_exact_fdb_requires_mac_vtep_self_and_permanent(self):
        good = "02:00:00:00:00:01 dev vx-e2e dst 198.18.1.2 self permanent"
        self.assertTrue(
            ACCEPTANCE.exact_fdb(
                good, "02:00:00:00:00:01", "198.18.1.2", "vx-e2e"
            )
        )
        self.assertFalse(
            ACCEPTANCE.exact_fdb(
                good, "02:00:00:00:00:02", "198.18.1.2", "vx-e2e"
            )
        )
        self.assertFalse(
            ACCEPTANCE.exact_fdb(
                "02:00:00:00:00:01 dev vx-e2e dst 198.18.1.9 self permanent",
                "02:00:00:00:00:01",
                "198.18.1.2",
                "vx-e2e",
            )
        )
        self.assertFalse(
            ACCEPTANCE.exact_fdb(
                "00:00:00:00:00:00 dev vx-e2e dst 198.18.1.2 self permanent",
                "02:00:00:00:00:01",
                "198.18.1.2",
                "vx-e2e",
            )
        )
        self.assertFalse(
            ACCEPTANCE.exact_fdb(
                good, "02:00:00:00:00:01", "198.18.1.2", "vx-other"
            )
        )

    def test_exact_fdb_accepts_observed_scoped_text_and_json_schemas(self):
        observed_text = (
            "42:ab:b5:6c:f9:70 dst 198.19.184.4 self permanent"
        )
        self.assertTrue(
            ACCEPTANCE.exact_fdb(
                observed_text,
                "42:ab:b5:6c:f9:70",
                "198.19.184.4",
                "vx-e2e",
            )
        )
        observed_json = json.dumps(
            [
                {
                    "mac": "02:00:00:00:00:01",
                    "dev": "vx-e2e",
                    "dst": "198.18.1.2",
                    "self": True,
                    "state": "permanent",
                }
            ]
        )
        self.assertTrue(
            ACCEPTANCE.exact_fdb(
                observed_json, "02:00:00:00:00:01", "198.18.1.2", "vx-e2e"
            )
        )
        wrong_device = observed_json.replace("vx-e2e", "vx-other")
        self.assertFalse(
            ACCEPTANCE.exact_fdb(
                wrong_device, "02:00:00:00:00:01", "198.18.1.2", "vx-e2e"
            )
        )
        wrong_flags = observed_json.replace('"self": true', '"self": false')
        self.assertFalse(
            ACCEPTANCE.exact_fdb(
                wrong_flags, "02:00:00:00:00:01", "198.18.1.2", "vx-e2e"
            )
        )
        self.assertFalse(
            ACCEPTANCE.exact_fdb(
                observed_text, "02:00:00:00:00:01", "198.18.1.2", ""
            )
        )

    def test_exact_neighbor_requires_ip_mac_and_permanent_state(self):
        good = "198.18.1.101 lladdr 02:00:00:00:00:01 PERMANENT"
        self.assertTrue(
            ACCEPTANCE.exact_neighbor(
                good, "198.18.1.101", "02:00:00:00:00:01"
            )
        )
        self.assertFalse(
            ACCEPTANCE.exact_neighbor(
                good, "198.18.1.102", "02:00:00:00:00:01"
            )
        )
        self.assertFalse(
            ACCEPTANCE.exact_neighbor(
                "198.18.1.101 lladdr 02:00:00:00:00:02 PERMANENT",
                "198.18.1.101",
                "02:00:00:00:00:01",
            )
        )

    def test_status_aggregation_is_fail_closed(self):
        required = ["route", "forward", "withdraw"]
        gates = [{"name": name, "status": "PASS"} for name in required]
        self.assertEqual(ACCEPTANCE.overall_status(gates, required, "PASS"), "PASS")
        for missing_status in ("FAIL", "SKIP", "NOT RUN"):
            gates[-1]["status"] = missing_status
            self.assertEqual(
                ACCEPTANCE.overall_status(gates, required, "PASS"), "FAIL"
            )
        self.assertEqual(ACCEPTANCE.overall_status(gates[:2], required, "PASS"), "FAIL")
        self.assertEqual(
            ACCEPTANCE.overall_status(
                [{"name": name, "status": "PASS"} for name in required],
                required,
                "FAIL",
            ),
            "FAIL",
        )

    def test_vxlan_pcap_matches_both_vtep_directions_and_vni(self):
        forward = ACCEPTANCE.expected_ping_packets(
            {"ip": REMOTE_IP, "mac": REMOTE_MAC}, LOCAL_IP, LOCAL_MAC, False
        )
        reverse = ACCEPTANCE.expected_ping_packets(
            {"ip": REMOTE_IP, "mac": REMOTE_MAC}, LOCAL_IP, LOCAL_MAC, True
        )
        with tempfile.TemporaryDirectory() as directory:
            pcap = Path(directory) / "forward.pcap"
            write_pcap(
                pcap,
                [
                    vxlan_frame(
                        icmp_frame(*packet[:2], packet[3], packet[4], packet[2]),
                        source_vtep=(
                            LOCAL_VTEP if packet[0] == LOCAL_IP else REMOTE_VTEP
                        ),
                        destination_vtep=(
                            REMOTE_VTEP if packet[0] == LOCAL_IP else LOCAL_VTEP
                        ),
                    )
                    for packet in forward
                ],
            )
            result = ACCEPTANCE.summarize_pcap(
                pcap,
                "vxlan",
                forward,
                LOCAL_VTEP,
                REMOTE_VTEP,
                1000,
                LOCAL_IP,
                REMOTE_IP,
            )
            self.assertEqual(result["status"], "PASS")
            self.assertEqual(len(result["matchedExpectedPackets"]), 2)

            pcap = Path(directory) / "reverse.pcap"
            write_pcap(
                pcap,
                [
                    vxlan_frame(
                        icmp_frame(
                            *packet[:2],
                            packet[3],
                            packet[4],
                            packet[2],
                        ),
                        source_vtep=(
                            LOCAL_VTEP if packet[0] == LOCAL_IP else REMOTE_VTEP
                        ),
                        destination_vtep=(
                            REMOTE_VTEP if packet[0] == LOCAL_IP else LOCAL_VTEP
                        ),
                    )
                    for packet in reverse
                ],
            )
            result = ACCEPTANCE.summarize_pcap(
                pcap,
                "vxlan",
                reverse,
                LOCAL_VTEP,
                REMOTE_VTEP,
                1000,
                LOCAL_IP,
                REMOTE_IP,
            )
            self.assertEqual(result["status"], "PASS")
            self.assertEqual(len(result["matchedExpectedPackets"]), 2)

    def test_vxlan_pcap_rejects_wrong_vni_or_vtep_attribution(self):
        expected = ACCEPTANCE.expected_ping_packets(
            {"ip": REMOTE_IP, "mac": REMOTE_MAC}, LOCAL_IP, LOCAL_MAC, False
        )
        inner = icmp_frame(
            *expected[0][:2],
            expected[0][3],
            expected[0][4],
            expected[0][2],
        )
        for label, frame in (
            ("wrong-vni", vxlan_frame(inner, vni=1001)),
            (
                "wrong-vtep",
                vxlan_frame(inner, source_vtep=REMOTE_VTEP, destination_vtep=LOCAL_VTEP),
            ),
        ):
            with self.subTest(label=label), tempfile.TemporaryDirectory() as directory:
                pcap = Path(directory) / f"{label}.pcap"
                write_pcap(pcap, [frame])
                result = ACCEPTANCE.summarize_pcap(
                    pcap,
                    "vxlan",
                    expected,
                    LOCAL_VTEP,
                    REMOTE_VTEP,
                    1000,
                    LOCAL_IP,
                    REMOTE_IP,
                )
                self.assertEqual(result["status"], "FAIL")

    def test_vxlan_pcap_rejects_malformed_extra_packet_even_with_expected_traffic(self):
        expected = ACCEPTANCE.expected_ping_packets(
            {"ip": REMOTE_IP, "mac": REMOTE_MAC}, LOCAL_IP, LOCAL_MAC, False
        )
        valid_frames = [
            vxlan_frame(
                icmp_frame(*packet[:2], packet[3], packet[4], packet[2]),
                source_vtep=(
                    LOCAL_VTEP if packet[0] == LOCAL_IP else REMOTE_VTEP
                ),
                destination_vtep=(
                    REMOTE_VTEP if packet[0] == LOCAL_IP else LOCAL_VTEP
                ),
            )
            for packet in expected
        ]
        with tempfile.TemporaryDirectory() as directory:
            pcap = Path(directory) / "malformed-extra.pcap"
            write_pcap(pcap, [*valid_frames, b"\x00"])
            result = ACCEPTANCE.summarize_pcap(
                pcap,
                "vxlan",
                expected,
                LOCAL_VTEP,
                REMOTE_VTEP,
                1000,
                LOCAL_IP,
                REMOTE_IP,
            )
        self.assertEqual(result["status"], "FAIL")
        self.assertEqual(len(result["matchedExpectedPackets"]), 2)
        self.assertIn("truncated Ethernet frame", result["decodeErrors"][0])

    def test_vxlan_pcap_ignores_complete_ipv6_control_frames_but_rejects_truncation(self):
        expected = ACCEPTANCE.expected_ping_packets(
            {"ip": REMOTE_IP, "mac": REMOTE_MAC}, LOCAL_IP, LOCAL_MAC, False
        )
        expected_frames = [
            vxlan_frame(
                icmp_frame(*packet[:2], packet[3], packet[4], packet[2]),
                source_vtep=(
                    LOCAL_VTEP if packet[0] == LOCAL_IP else REMOTE_VTEP
                ),
                destination_vtep=(
                    REMOTE_VTEP if packet[0] == LOCAL_IP else LOCAL_VTEP
                ),
            )
            for packet in expected
        ]
        ipv6_header = struct.pack(
            "!IHBB16s16s",
            6 << 28,
            8,
            58,
            255,
            b"\x00" * 16,
            b"\xff" * 16,
        )
        ipv6_control = ethernet_frame(
            LOCAL_MAC,
            "33:33:00:00:00:16",
            0x86DD,
            ipv6_header + b"\x8f\x00\x00\x00\x00\x00\x00\x00",
        )
        truncated_ipv6 = ethernet_frame(
            LOCAL_MAC,
            "33:33:00:00:00:16",
            0x86DD,
            ipv6_header,
        )
        with tempfile.TemporaryDirectory() as directory:
            pcap = Path(directory) / "ipv6-control.pcap"
            write_pcap(pcap, [*expected_frames, vxlan_frame(ipv6_control)])
            result = ACCEPTANCE.summarize_pcap(
                pcap,
                "vxlan",
                expected,
                LOCAL_VTEP,
                REMOTE_VTEP,
                1000,
                LOCAL_IP,
                REMOTE_IP,
            )
            self.assertEqual(result["status"], "PASS")
            self.assertEqual(result["decodeErrors"], [])

            pcap = Path(directory) / "truncated-ipv6.pcap"
            write_pcap(pcap, [*expected_frames, vxlan_frame(truncated_ipv6)])
            result = ACCEPTANCE.summarize_pcap(
                pcap,
                "vxlan",
                expected,
                LOCAL_VTEP,
                REMOTE_VTEP,
                1000,
                LOCAL_IP,
                REMOTE_IP,
            )
        self.assertEqual(result["status"], "FAIL")
        self.assertIn("truncated inner IPv6 payload", result["decodeErrors"][0])

    def test_observed_gateway_arp_tunnel_does_not_count_as_endpoint_forwarding(self):
        endpoint = {"ip": "198.18.148.3", "mac": "42:ab:b5:6c:f9:70"}
        local_ip = "172.17.11.2"
        local_mac = "aa:c6:2f:d4:53:5e"
        expected = ACCEPTANCE.expected_ping_packets(
            endpoint, local_ip, local_mac, False
        )
        request = vxlan_frame(
            arp_frame(local_ip, local_mac, "172.17.11.1"),
            source_vtep="198.19.184.5",
            destination_vtep="198.19.184.4",
        )
        with tempfile.TemporaryDirectory() as directory:
            pcap = Path(directory) / "observed-gateway-arp.pcap"
            write_pcap(pcap, [request])
            result = ACCEPTANCE.summarize_pcap(
                pcap,
                "vxlan",
                expected,
                "198.19.184.5",
                "198.19.184.4",
                1000,
                local_ip,
                endpoint["ip"],
            )
        self.assertEqual(result["status"], "FAIL")
        self.assertEqual(result["arpPacketCount"], 1)
        self.assertEqual(result["matchedExpectedPackets"], [])
        self.assertEqual(result["decodeErrors"], [])
        self.assertEqual(result["arpPackets"][0]["outerSource"], "198.19.184.5")
        self.assertEqual(
            result["arpPackets"][0]["outerDestination"], "198.19.184.4"
        )

    def test_endpoint_pcap_asserts_bidirectional_icmp_and_retains_arp(self):
        expected = ACCEPTANCE.expected_ping_packets(
            {"ip": REMOTE_IP, "mac": REMOTE_MAC}, LOCAL_IP, LOCAL_MAC, False
        )
        frames = [
            arp_frame(LOCAL_IP, LOCAL_MAC, REMOTE_IP),
            *[
                icmp_frame(*packet[:2], packet[3], packet[4], packet[2])
                for packet in expected
            ],
        ]
        with tempfile.TemporaryDirectory() as directory:
            pcap = Path(directory) / "endpoint.pcap"
            write_pcap(pcap, frames)
            result = ACCEPTANCE.summarize_pcap(
                pcap,
                "endpoint",
                expected,
                LOCAL_VTEP,
                REMOTE_VTEP,
                0,
                LOCAL_IP,
                REMOTE_IP,
            )
        self.assertEqual(result["status"], "PASS")
        self.assertEqual(result["arpPacketCount"], 1)
        self.assertEqual(len(result["endpointPackets"]), 2)

    def test_endpoint_pcap_rejects_malformed_extra_packet(self):
        expected = ACCEPTANCE.expected_ping_packets(
            {"ip": REMOTE_IP, "mac": REMOTE_MAC}, LOCAL_IP, LOCAL_MAC, False
        )
        frames = [
            *[
                icmp_frame(*packet[:2], packet[3], packet[4], packet[2])
                for packet in expected
            ],
            b"\x00",
        ]
        with tempfile.TemporaryDirectory() as directory:
            pcap = Path(directory) / "malformed-extra-endpoint.pcap"
            write_pcap(pcap, frames)
            result = ACCEPTANCE.summarize_pcap(
                pcap,
                "endpoint",
                expected,
                LOCAL_VTEP,
                REMOTE_VTEP,
                0,
                LOCAL_IP,
                REMOTE_IP,
            )
        self.assertEqual(result["status"], "FAIL")
        self.assertEqual(len(result["matchedExpectedPackets"]), 2)
        self.assertIn("malformed or incomplete", result["decodeErrors"][0])

    def test_finish_pcap_capture_reads_stderr_and_handles_empty_optional_streams(self):
        with tempfile.TemporaryDirectory() as directory:
            pcap = Path(directory) / "capture.pcap"
            pcap.write_bytes(b"pcap")
            for remaining_stderr in ("tcpdump stopped", None):
                with self.subTest(remaining_stderr=remaining_stderr):
                    process = mock.Mock()
                    process.poll.return_value = 0
                    process.returncode = 0
                    process.communicate.return_value = (None, remaining_stderr)
                    pcap_file = pcap.open("wb")
                    capture = {
                        "process": process,
                        "pcapFile": pcap_file,
                        "pcapPath": pcap,
                        "stderrLines": ["tcpdump started"],
                        "remotePid": None,
                    }
                    result = ACCEPTANCE.finish_pcap_capture("handler", capture)
                    self.assertIn("tcpdump started", result["stderr"])
                    if remaining_stderr:
                        self.assertIn(remaining_stderr, result["stderr"])
                    self.assertEqual(result["exitCode"], 0)

    def test_start_pcap_capture_failure_reads_stderr_from_communicate(self):
        with tempfile.TemporaryDirectory() as directory:
            process = mock.Mock()
            process.poll.return_value = 1
            process.returncode = 1
            process.communicate.return_value = (None, "tcpdump failed")
            with (
                mock.patch.object(ACCEPTANCE, "EVIDENCE", Path(directory)),
                mock.patch.object(
                    ACCEPTANCE, "container_exec", return_value="net:[123]"
                ),
                mock.patch.object(ACCEPTANCE.subprocess, "Popen", return_value=process),
            ):
                capture = ACCEPTANCE.start_pcap_capture(
                    "handler", "failed-capture", "eth0", "udp port 4789"
                )
        self.assertEqual(capture["status"], "FAIL")
        self.assertIn("tcpdump failed", capture["stderr"])

    def test_capture_result_fails_closed_for_start_and_decode_failures(self):
        passed = {
            "exitCode": 0,
            "signalExitCode": 0,
            "stopError": None,
            "startError": None,
            "decode": {"status": "PASS"},
        }
        self.assertTrue(ACCEPTANCE.pcap_capture_passed(passed))
        for key, value in (
            ("exitCode", 1),
            ("signalExitCode", 1),
            ("stopError", "failed to stop capture"),
            ("startError", "tcpdump failed to start"),
        ):
            failed = {**passed, key: value}
            self.assertFalse(ACCEPTANCE.pcap_capture_passed(failed))
        self.assertFalse(
            ACCEPTANCE.pcap_capture_passed({**passed, "decode": {"status": "FAIL"}})
        )

    def test_frr_endpoint_inspection_selects_each_namespace_interface(self):
        for endpoint_name, endpoint_interface, bridge_interface in (
            ("secondary", "ep2", "sw2"),
            ("netlink-primary", "ep3", "sw3"),
        ):
            with self.subTest(endpoint=endpoint_name):
                endpoint = {
                    "name": endpoint_name,
                    "ip": REMOTE_IP,
                    "mac": REMOTE_MAC,
                }
                fixture = {"parameters": {"endpoints": [endpoint]}}
                command_outputs = [
                    json.dumps(
                        [{"ifname": endpoint_interface, "address": REMOTE_MAC}]
                    ),
                    json.dumps(
                        [
                            {
                                "ifname": endpoint_interface,
                                "addr_info": [{"local": REMOTE_IP}],
                            }
                        ]
                    ),
                    json.dumps(
                        [{"ifname": bridge_interface, "master": "br-e2e"}]
                    ),
                ]
                commands = []

                def fake_run(command, **_kwargs):
                    commands.append(command)
                    return subprocess.CompletedProcess(
                        command, 0, stdout=command_outputs.pop(0)
                    )

                with (
                    mock.patch.object(ACCEPTANCE, "RUN_DIR", Path("/unused")),
                    mock.patch.object(ACCEPTANCE, "REPORT", {"topology": {}}),
                    mock.patch.object(ACCEPTANCE, "write_json"),
                    mock.patch.object(
                        ACCEPTANCE, "fixture_container", return_value="owned-frr"
                    ),
                    mock.patch.object(
                        ACCEPTANCE.FIXTURE, "endpoint_namespace_pid", return_value=234
                    ),
                    mock.patch.object(ACCEPTANCE, "run", side_effect=fake_run),
                ):
                    details = ACCEPTANCE.verify_frr_endpoint(
                        fixture, "run-id", endpoint
                    )

                self.assertIn("endpoint namespace PID=234", details)
                self.assertIn("nsenter", commands[0])
                self.assertIn("234", commands[0])
                self.assertIn(endpoint_interface, commands[0])
                self.assertIn(endpoint_interface, commands[1])
                self.assertIn(bridge_interface, commands[2])
                self.assertNotIn("nsenter", commands[2])

    def test_record_routes_delegates_to_shared_origin_aware_observation(self):
        outputs = (
            ACCEPTANCE.FIXTURE.JsonCommandOutput({}, "gobgp-rib", ""),
            ACCEPTANCE.FIXTURE.JsonCommandOutput({}, "frr-rib", ""),
        )
        endpoint = {
            "name": "local",
            "mac": LOCAL_MAC,
            "ip": LOCAL_IP,
            "originator": "gobgp",
            "receiver": "frr",
        }
        with (
            mock.patch.object(
                ACCEPTANCE.FIXTURE, "route_outputs", return_value=outputs
            ),
            mock.patch.object(
                ACCEPTANCE.FIXTURE,
                "observe_route_pair",
                return_value=(True, True, ["GoBGP local route", "FRR exact route"], "exact JSON"),
            ) as observe,
        ):
            passed, detail = ACCEPTANCE.record_routes(
                Path("/unused"), {}, [endpoint], True
            )
        self.assertTrue(passed)
        self.assertIn("exact JSON", detail)
        observe.assert_called_once_with(
            Path("/unused"), {}, endpoint, outputs[0], outputs[1]
        )

    def test_record_routes_fails_closed_when_shared_route_validation_fails(self):
        outputs = (
            ACCEPTANCE.FIXTURE.JsonCommandOutput({}, "gobgp-rib", ""),
            ACCEPTANCE.FIXTURE.JsonCommandOutput({}, "frr-rib", ""),
        )
        endpoint = {
            "name": "secondary",
            "mac": REMOTE_MAC,
            "ip": REMOTE_IP,
            "originator": "frr",
            "receiver": "gobgp",
        }
        with (
            mock.patch.object(
                ACCEPTANCE.FIXTURE, "route_outputs", return_value=outputs
            ),
            mock.patch.object(
                ACCEPTANCE.FIXTURE,
                "observe_route_pair",
                side_effect=ACCEPTANCE.FIXTURE.FixtureError(
                    "wrong next hop or peer provenance"
                ),
            ),
        ):
            with self.assertRaisesRegex(
                ACCEPTANCE.FIXTURE.FixtureError, "wrong next hop"
            ):
                ACCEPTANCE.record_routes(Path("/unused"), {}, [endpoint], True)

    def test_image_references_require_immutable_digest(self):
        digest = "registry.example/image@sha256:" + "a" * 64
        self.assertEqual(ACCEPTANCE.require_digest(digest, "image"), digest)
        with self.assertRaises(ACCEPTANCE.AcceptanceError):
            ACCEPTANCE.require_digest("registry.example/image:latest", "image")

    def test_vxlan_identity_checks_vni_port_and_local_underlay_address(self):
        snapshot = {
            "link": json.dumps(
                [
                    {
                        "ifname": "vx-e2e",
                        "master": "br-e2e",
                        "linkinfo": {
                            "info_kind": "vxlan",
                            "info_data": {"id": 1000, "port": 4789},
                        },
                        "flags": ["UP"],
                    }
                ]
            ),
            "bridge_link": json.dumps(
                [
                    {
                        "ifname": "br-e2e",
                        "linkinfo": {"info_kind": "bridge"},
                        "flags": ["UP"],
                    }
                ]
            ),
            "underlay": json.dumps(
                [
                    {
                        "ifname": "eth0",
                        "addr_info": [
                            {"family": "inet", "local": "198.18.0.4/24"}
                        ],
                    }
                ]
            ),
        }
        self.assertTrue(
            ACCEPTANCE.link_configuration(snapshot, 1000, "198.18.0.4")
        )
        self.assertFalse(
            ACCEPTANCE.link_configuration(snapshot, 1001, "198.18.0.4")
        )
        self.assertFalse(
            ACCEPTANCE.link_configuration(snapshot, 1000, "198.18.0.9")
        )
        self.assertTrue(ACCEPTANCE.bridge_configuration(snapshot))
        dstport_snapshot = json.loads(json.dumps(snapshot))
        link = json.loads(dstport_snapshot["link"])
        link[0]["linkinfo"]["info_data"] = {"id": 1000, "dstport": 4789}
        dstport_snapshot["link"] = json.dumps(link)
        self.assertTrue(
            ACCEPTANCE.link_configuration(dstport_snapshot, 1000, "198.18.0.4")
        )
        for field, value in (
            ("info_kind", "dummy"),
            ("ifname", "vx-other"),
            ("master", "br-other"),
        ):
            invalid = json.loads(json.dumps(snapshot))
            link = json.loads(invalid["link"])
            if field == "info_kind":
                link[0]["linkinfo"][field] = value
            else:
                link[0][field] = value
            invalid["link"] = json.dumps(link)
            with self.subTest(field=field):
                self.assertFalse(
                    ACCEPTANCE.link_configuration(invalid, 1000, "198.18.0.4")
                )

    def test_overlay_capture_requires_both_vteps_and_udp_4789(self):
        capture = (
            "IP 198.18.0.4.49152 > 198.18.0.2.4789: "
            "UDP, length 142\n1 packet captured"
        )
        self.assertTrue(
            ACCEPTANCE.overlay_capture_matches(capture, "198.18.0.4", "198.18.0.2")
        )
        self.assertFalse(
            ACCEPTANCE.overlay_capture_matches(capture, "198.18.0.4", "198.18.0.9")
        )

    def test_dockerfile_contains_local_go_import_dependencies(self):
        dockerfile = (ROOT / "tools/e2e/netlink/Dockerfile").read_text()
        for copy_instruction in (
            "COPY pkg ./pkg",
            "COPY gen/ovsdb_mgmt ./gen/ovsdb_mgmt",
            "COPY services/ovsdb_mgmt/ovnnb ./services/ovsdb_mgmt/ovnnb",
            "COPY services/bgp_ad_handler ./services/bgp_ad_handler",
        ):
            with self.subTest(copy_instruction=copy_instruction):
                self.assertIn(copy_instruction, dockerfile)
        self.assertNotIn("apt-get", dockerfile)

    def test_dockerfile_declares_runtime_image_before_second_from(self):
        lines = (ROOT / "tools/e2e/netlink/Dockerfile").read_text().splitlines()
        runtime_arg = lines.index("ARG NETLINK_RUNTIME_IMAGE")
        runtime_from = lines.index("FROM ${NETLINK_RUNTIME_IMAGE}")
        self.assertLess(runtime_arg, lines.index("FROM ${GO_BUILD_IMAGE} AS builder"))
        self.assertLess(runtime_arg, runtime_from)

    def test_runner_forbids_host_network_and_broad_cleanup(self):
        source = SCRIPT.read_text()
        for forbidden in (
            "sudo ",
            "--network host",
            "--privileged",
            "docker system prune",
            "docker container prune",
            "docker rm -f",
        ):
            with self.subTest(forbidden=forbidden):
                self.assertNotIn(forbidden, source)
        self.assertIn("label=evpn.e2e.owner=", source)


if __name__ == "__main__":
    unittest.main()
