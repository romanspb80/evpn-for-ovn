#!/usr/bin/env python3
"""Offline tests only: synthetic packets, retained-format flows, and mocked shell commands."""

import contextlib
import importlib.util
import io
import json
import struct
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch


LIB = Path(__file__).resolve().parent
ROOT = LIB.parents[1]
SPEC = importlib.util.spec_from_file_location("vxlan_verify", LIB / "cni-vxlan-pcap-verify.py")
VERIFY = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(VERIFY)

# Exact D2.2e table 55 egress action string (cookie 0xe551c06d, priority 50).
LOAD = (
    "load:0xc6120002->NXM_NX_TUN_IPV4_SRC[],"
    "load:0xc6120004->NXM_NX_TUN_IPV4_DST[],"
    "load:0x3e8->NXM_NX_TUN_ID[0..23],output:1"
)
SET_FIELD = (
    "set_field:198.18.0.2->tun_src,set_field:198.18.0.4->tun_dst,"
    "set_field:0x3e8->tun_id,output:1"
)


def inner_frame(source, destination, icmp_type, sequence, identifier=198, payload=b"payload"):
    remote = source == "10.244.240.12"
    src_mac = bytes.fromhex("0a58f001000c" if remote else "0a58f000000a")
    dst_mac = bytes.fromhex("0a58f000000a" if remote else "0a58f001000c")
    icmp = struct.pack("!BBHHH", icmp_type, 0, 0, identifier, sequence) + payload
    ip = struct.pack("!BBHHHBBH4s4s", 0x45, 0, 20 + len(icmp), 0, 0, 64, 1, 0,
                     VERIFY.ipaddress.IPv4Address(source).packed,
                     VERIFY.ipaddress.IPv4Address(destination).packed)
    return dst_mac + src_mac + b"\x08\x00" + ip + icmp


def write_pcap(path, packets):
    raw = struct.pack("<IHHIIII", 0xa1b2c3d4, 2, 4, 0, 0, 256, 1)
    for timestamp, frame in packets:
        raw += struct.pack("<IIII", timestamp, 0, len(frame), len(frame)) + frame
    path.write_bytes(raw)


class FlowTests(unittest.TestCase):
    def matches(self, actions):
        return VERIFY.tunnel_output_matches(actions, "198.18.0.2", "198.18.0.4", 1000, 1)

    def test_recorded_load_format(self):
        self.assertTrue(self.matches(LOAD))

    def test_existing_set_formats(self):
        for action in (SET_FIELD, SET_FIELD.replace("0x3e8", "1000"),
                       SET_FIELD.replace("set_field:0x3e8->tun_id", "set_tunnel:1000"),
                       SET_FIELD.replace("set_field:0x3e8->tun_id", "set_tunnel:0x3e8")):
            with self.subTest(action=action):
                self.assertTrue(self.matches(action))

    def test_wrong_load_values_and_fields(self):
        for old, new in (("0xc6120002", "0xc6120003"), ("0xc6120004", "0xc6120005"),
                         ("0x3e8", "0x3e9"), ("output:1", "output:10"),
                         (",output:1", ""), ("NXM_NX_TUN_ID", "NXM_NX_REG0"),
                         ("[0..23]", "[0..7]"),
                         ("NXM_NX_TUN_IPV4_SRC[]", "NXM_NX_TUN_IPV4_SRC[8..31]")):
            with self.subTest(old=old, new=new):
                self.assertFalse(self.matches(LOAD.replace(old, new)))

    def test_wrong_existing_set_values(self):
        for old, new in (("198.18.0.2", "198.18.0.20"), ("198.18.0.4", "198.18.0.40"),
                         ("0x3e8", "0x3e80"), ("output:1", "output:11")):
            with self.subTest(old=old):
                self.assertFalse(self.matches(SET_FIELD.replace(old, new)))

    def test_action_order_overwrites_and_nested_output(self):
        self.assertFalse(self.matches("output:1," + LOAD.removesuffix(",output:1")))
        self.assertFalse(self.matches(LOAD.replace(",output:1",
            ",load:0xc6120005->NXM_NX_TUN_IPV4_DST[],output:1")))
        self.assertFalse(self.matches(LOAD.replace(",output:1", ",clone(output:1)")))
        self.assertTrue(self.matches(LOAD + ",resubmit(,56)"))

    def test_flow_counters_require_matched_actions_and_present_counter(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "flows.txt"
            for packets, expected in (("n_packets=3,", 3), ("n_packets=0,", 0), ("", 0)):
                path.write_text(f"table=55,{packets}actions={LOAD}\n")
                total, lines = VERIFY.matching_flow_packets(
                    path, "198.18.0.2", "198.18.0.4", 1000, 1)
                self.assertEqual(total, expected)
                self.assertEqual(bool(lines), bool(packets))


class PacketTests(unittest.TestCase):
    def setUp(self):
        self.manifest = {
            "localIp": "10.244.240.10", "remoteIp": "10.244.240.12",
            "localMac": "0a:58:f0:00:00:0a", "remoteMac": "0a:58:f0:01:00:0c",
            "localVtep": "198.18.0.2", "remoteVtep": "198.18.0.4", "vni": 1000,
            "udpPort": 4789, "markers": {"pod-to-frr": "d22b3b3d00010203"},
            "stimuli": {"frr-to-pod": {"startedAt": 20, "completedAt": 25}},
        }

    def test_pcap_reader_rejects_truncated_record_and_ipv4_payload(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "truncated.pcap"
            header = b"\xd4\xc3\xb2\xa1" + struct.pack(
                "<HHIIII", 2, 4, 0, 0, 65535, 1
            )
            path.write_bytes(
                header + struct.pack("<IIII", 1, 0, 4, 8) + b"abcd"
            )
            with self.assertRaisesRegex(ValueError, "packet truncated"):
                VERIFY.read_pcap(path)

        truncated_frame = inner_frame(
            self.manifest["localIp"], self.manifest["remoteIp"], 8, 1
        )[:-1]
        self.assertIsNone(VERIFY.inner_packet(truncated_frame))

    def test_unmarked_reverse_is_direction_and_time_scoped(self):
        for source, destination, kind, expected in (
            ("10.244.240.12", "10.244.240.10", 8, "frr-to-pod"),
            ("10.244.240.10", "10.244.240.12", 0, "frr-to-pod"),
            ("10.244.240.10", "10.244.240.12", 8, None),
            ("10.244.240.12", "10.244.240.10", 0, None),
            ("10.244.240.99", "10.244.240.10", 8, None),
        ):
            inner = VERIFY.inner_packet(inner_frame(source, destination, kind, 1))
            self.assertEqual(VERIFY.packet_label(inner, 22, {}, self.manifest), expected)
            self.assertIsNone(VERIFY.packet_label(inner, 19, {}, self.manifest))
            self.assertIsNone(VERIFY.packet_label(inner, 26, {}, self.manifest))

    def test_missing_stimulus_and_legacy_marker_do_not_invent_reverse(self):
        inner = VERIFY.inner_packet(inner_frame("10.244.240.12", "10.244.240.10", 8, 0))
        self.assertEqual(VERIFY.packet_label(inner, 22, {}, self.manifest), "frr-to-pod")
        self.assertIsNone(VERIFY.packet_label(inner, 22, {}, {}))
        self.assertIsNone(VERIFY.packet_label(inner, 22, {"frr-to-pod": b"absent"}, self.manifest))
        inner["icmp"]["sequence"] = 3
        self.assertIsNone(VERIFY.packet_label(inner, 22, {}, self.manifest))

    def test_payload_fingerprint_and_forward_marker(self):
        payload = bytes.fromhex(self.manifest["markers"]["pod-to-frr"])
        frame = inner_frame("10.244.240.10", "10.244.240.12", 8, 1, payload=payload)
        events, *_ = VERIFY.marker_events([{"frame": frame, "timestamp": 10}], "inner",
                                         {"pod-to-frr": payload}, self.manifest)
        self.assertEqual(events[0]["marker"], "pod-to-frr")
        self.assertEqual(events[0]["icmp"]["payloadSha256"],
                         VERIFY.hashlib.sha256(payload).hexdigest())

    def replay(self, directory, before_count=0, after_count=3, mutate=None, verify=True):
        root = Path(directory)
        frames = []
        for reverse in (False, True):
            source, destination = self.manifest["localIp"], self.manifest["remoteIp"]
            if reverse:
                source, destination = destination, source
            sequences = (0, 1, 2) if reverse else (1, 2, 3)
            for sequence in sequences:
                for kind, src, dst in ((8, source, destination), (0, destination, source)):
                    payload = (b"unmarked-reverse" if reverse else
                               bytes.fromhex(self.manifest["markers"]["pod-to-frr"]) + b"suffix")
                    frames.append((22 if reverse else 10,
                                   inner_frame(src, dst, kind, sequence,
                                               identifier=199 if reverse else 198, payload=payload)))
        pcap = root / "inner.pcap"
        write_pcap(pcap, frames)
        vxlan_frames = []
        for timestamp, frame in frames:
            inner = VERIFY.inner_packet(frame)
            src = self.manifest["localVtep"] if inner["sourceIp"] == self.manifest["localIp"] else self.manifest["remoteVtep"]
            dst = self.manifest["remoteVtep"] if src == self.manifest["localVtep"] else self.manifest["localVtep"]
            vxlan = b"\x08\x00\x00\x00\x00\x03\xe8\x00" + frame
            udp = struct.pack("!HHHH", 40000, 4789, 8 + len(vxlan), 0) + vxlan
            ip = struct.pack("!BBHHHBBH4s4s", 0x45, 0, 20 + len(udp), 0, 0, 64, 17, 0,
                             VERIFY.ipaddress.IPv4Address(src).packed,
                             VERIFY.ipaddress.IPv4Address(dst).packed)
            vxlan_frames.append((timestamp, bytes(12) + b"\x08\x00" + ip + udp))
        vxlan_pcap = root / "vxlan.pcap"
        write_pcap(vxlan_pcap, vxlan_frames)
        manifest = {**self.manifest, "captures": {
            name: {"path": str(vxlan_pcap if kind == "vxlan" else pcap), "kind": kind}
            for name, kind in (("pod-facing", "inner"), ("worker-underlay", "vxlan"),
                               ("frr-underlay", "vxlan"), ("frr-vxlan", "inner"),
                               ("remote-workload", "inner"))
        }}
        manifest["ovs"] = {"ofport": 1}
        for name, text in {
            "tunnelConfig": 'type=vxlan\noptions={dst_port="4789", key=flow, remote_ip=flow}\n',
            "flowsBefore": f"table=55,n_packets={before_count},actions={LOAD}\n",
            "flowsAfter": f"table=55,n_packets={after_count},actions={LOAD}\n",
            "portStatsBefore": "port 1: rx pkts=1, tx pkts=1,\n",
            "portStatsAfter": "port 1: rx pkts=7, tx pkts=7,\n",
        }.items():
            path = root / name
            path.write_text(text)
            manifest["ovs"][name] = str(path)
        if mutate:
            mutate(manifest, pcap, vxlan_pcap)
        manifest_path = root / "manifest.json"
        manifest_path.write_text(json.dumps(manifest))
        if not verify:
            return manifest_path
        output = root / "result.json"
        with patch.object(sys, "argv", ["verify", "--manifest", str(manifest_path),
                                       "--output", str(output)]), \
             contextlib.redirect_stdout(io.StringIO()), contextlib.redirect_stderr(io.StringIO()):
            rc = VERIFY.main()
        return rc, json.loads(output.read_text())

    def test_complete_synthetic_correlation(self):
        with tempfile.TemporaryDirectory() as directory:
            rc, result = self.replay(directory)
            self.assertEqual((rc, result["status"]), (0, "PASS"), result["errors"])

    def test_counter_missing_unchanged_or_decreased_stays_partial(self):
        for before, after in ((3, 3), (4, 3)):
            with tempfile.TemporaryDirectory() as directory:
                rc, result = self.replay(directory, before, after)
                self.assertEqual(rc, 1)
                self.assertIn("OVS remote-VTEP/VNI forwarding-flow counter did not advance",
                              result["errors"])
        def remove_counter(manifest, *_):
            Path(manifest["ovs"]["flowsAfter"]).write_text(f"actions={LOAD}\n")
        with tempfile.TemporaryDirectory() as directory:
            rc, result = self.replay(directory, mutate=remove_counter)
            self.assertEqual(rc, 1)
            self.assertEqual(result["status"], "PARTIAL")

    def test_missing_reverse_cannot_be_repaired_by_ovs(self):
        def remove_stimulus(manifest, *_):
            manifest.pop("stimuli")
        with tempfile.TemporaryDirectory() as directory:
            rc, result = self.replay(directory, mutate=remove_stimulus)
            self.assertTrue(result["ovs"]["egressFlowValid"])
            self.assertEqual((rc, result["status"]), (1, "PARTIAL"))
            self.assertTrue(all(c["missing"] for c in result["captures"].values()))

    def test_missing_or_empty_capture_file_cannot_pass_packet_path_acceptance(self):
        for empty in (False, True):
            with self.subTest(empty=empty), tempfile.TemporaryDirectory() as directory:
                def remove_capture(manifest, *_):
                    path = Path(directory) / "required-pod-facing.pcap"
                    if empty:
                        path.touch()
                    manifest["captures"]["pod-facing"]["path"] = str(path)

                rc, result = self.replay(directory, mutate=remove_capture)
                self.assertEqual((rc, result["status"]), (1, "PARTIAL"))
                self.assertTrue(
                    any("pod-facing:" in error for error in result["errors"]),
                    result["errors"],
                )

    def test_missing_sequence_three_is_not_a_parser_success(self):
        def drop_last_forward_pair(manifest, pcap, _):
            raw, records = VERIFY.read_pcap(pcap)
            kept = [record for record in records if not (
                record["timestamp"] == 10 and
                VERIFY.inner_packet(record["frame"])["icmp"]["sequence"] == 3)]
            data = raw[:24]
            for record in kept:
                frame = record["frame"]
                data += struct.pack("<IIII", int(record["timestamp"]), 0, len(frame), len(frame)) + frame
            pcap.write_bytes(data)
        with tempfile.TemporaryDirectory() as directory:
            rc, result = self.replay(directory, mutate=drop_last_forward_pair)
            self.assertEqual((rc, result["status"]), (1, "PARTIAL"))
            self.assertEqual(len(result["captures"]["pod-facing"]["missing"]), 2)

    def test_identifier_and_payload_mismatch_rejected(self):
        for offset in (14 + 20 + 4, 14 + 20 + 8 + 8):
            def corrupt(manifest, pcap, _, offset=offset):
                data = bytearray(pcap.read_bytes())
                data[24 + 16 + offset] ^= 1
                pcap.write_bytes(data)
            with tempfile.TemporaryDirectory() as directory:
                rc, result = self.replay(directory, mutate=corrupt)
                self.assertEqual((rc, result["status"]), (1, "PARTIAL"))
                self.assertTrue(any("mismatch" in error for error in result["errors"]))


class ShellTests(unittest.TestCase):
    def shell(self, script):
        return subprocess.run(["bash", "-c", script], text=True, capture_output=True, check=False)

    def test_actual_ping_command_construction(self):
        result = self.shell(f"""
set -eu
KIND_CLUSTER=cni-24b-offline
source '{LIB}/cni-evpn-frr-interop-test.sh'
interop_ping_command reverse
printf '%s\\n' "${{INTEROP_PING_COMMAND[@]}}"
""")
        self.assertEqual(result.returncode, 0, result.stderr)
        args = result.stdout.splitlines()
        self.assertEqual(args, ["timeout", "--kill-after=2s", "10s", "docker", "exec",
            "cni-24b-offline-frr", "ip", "netns", "exec", "evpnwork", "ping", "-n",
            "-I", "frr-work0", "-c", "3", "-W", "2", "-s", "56", "10.244.240.10"])
        self.assertNotIn("-p", args)
        self.assertNotIn("b22b3b3d04050607", result.stdout)
        forward = self.shell(f"""
set -eu
KIND_CLUSTER=cni-24b-offline
source '{LIB}/cni-evpn-frr-interop-test.sh'
interop_ping_command forward
printf '%s\\n' "${{INTEROP_PING_COMMAND[@]}}"
""")
        self.assertEqual(forward.returncode, 0, forward.stderr)
        self.assertIn("d22b3b3d00010203", forward.stdout)

    def test_cleanup_and_verification_status_independent(self):
        runner = (ROOT / "tools/run-cni-mvp-2.4b-multus.sh").read_text()
        cleanup = runner[runner.index("cleanup() {"):runner.index("\ntrap cleanup EXIT")]
        for original_rc, packet_failure, cleanup_failure, shutdown_failure, image_failure in (
            (7, 1, 0, 0, 0), (0, 1, 0, 0, 0), (0, 0, 1, 0, 0),
            (0, 0, 0, 1, 0), (0, 0, 0, 0, 0), (0, 0, 0, 0, 1),
            (7, 1, 0, 0, 1),
        ):
            with self.subTest(original_rc=original_rc, packet=packet_failure,
                              cleanup=cleanup_failure, shutdown=shutdown_failure,
                              image=image_failure), \
                 tempfile.TemporaryDirectory() as directory:
                script = f"""
set -euo pipefail
KIND_CLUSTER=cni-24b-offline
source '{LIB}/cni-evpn-frr-interop-test.sh'
EVIDENCE_DIR='{directory}'
EVPN_FRR_INTEROP_TEST=1 CREATED=1 KEEP_CLUSTER=0 WORKER=offline AGENT_IMAGE=offline KUBECONFIG=offline
INTEROP_CONTROL_PLANE_STATUS=PASS INTEROP_FORWARDING_STATE_STATUS=PASS
INTEROP_CAPTURE_STARTUP_STATUS=PASS INTEROP_FORWARD_ICMP_STATUS=PASS
INTEROP_REVERSE_ICMP_STATUS=PASS
INTEROP_PACKET_VERIFICATION_FAILURE={packet_failure}
INTEROP_PACKET_VERIFICATION_STATUS={'PARTIAL' if packet_failure else 'PASS'}
INTEROP_CAPTURE_SHUTDOWN_FAILURE={shutdown_failure}
INTEROP_CAPTURE_SHUTDOWN_STATUS={'FAIL' if shutdown_failure else 'PASS'}
INTEROP_CAPTURE_PREFLIGHT_STATUS=PASS
INTEROP_LIFECYCLE_STATUS=PASS
k() {{ :; }}
kind() {{ return {cleanup_failure}; }}
cleanup_cluster() {{ return {cleanup_failure}; }}
docker() {{ :; }}
copy_calls() {{ :; }}
interop_cleanup() {{ :; }}
test_image_cleanup() {{ return {image_failure}; }}
{cleanup}
trap cleanup EXIT
exit {original_rc}
"""
                result = self.shell(script)
                expected_rc = original_rc or int(bool(
                    packet_failure or cleanup_failure or shutdown_failure or image_failure))
                self.assertEqual(result.returncode, expected_rc, result.stderr)
                status = json.loads((Path(directory) / "interop-status.json").read_text())
                self.assertEqual(status["resource_cleanup_status"],
                                 "FAIL" if cleanup_failure or shutdown_failure or image_failure else "PASS")
                self.assertEqual(status["packet_verification_status"],
                                 "PARTIAL" if packet_failure else "PASS")
                self.assertEqual(status["overall_status"], "PARTIAL" if expected_rc == 0 else "FAIL")

    def test_unreached_phases_are_not_reported_as_pass(self):
        with tempfile.TemporaryDirectory() as directory:
            result = self.shell(f"""
set -eu
KIND_CLUSTER=cni-24b-offline
source '{LIB}/cni-evpn-frr-interop-test.sh'
EVIDENCE_DIR='{directory}'
interop_write_status 7 0
""")
            self.assertEqual(result.returncode, 0, result.stderr)
            status = json.loads((Path(directory) / "interop-status.json").read_text())
            self.assertEqual(status["capture_startup_status"], "NOT_RUN")
            self.assertEqual(status["packet_verification_status"], "NOT_RUN")
            self.assertEqual(status["resource_cleanup_status"], "PASS")
            self.assertEqual(status["overall_status"], "BLOCKED")

    def test_interface_filtered_forwarding_assertions(self):
        baseline = {
            "worker-fdb": [{"mac": "0a:58:f0:01:00:0c", "dst": "198.18.0.4",
                            "state": "permanent", "flags": ["self", "extern_learn"]}],
            "worker-neigh": [{"dst": "10.244.240.12", "lladdr": "0a:58:f0:01:00:0c",
                              "state": ["NOARP"]}],
            "frr-fdb": [{"mac": "0a:58:f0:00:00:0a", "dst": "198.18.0.2",
                         "state": "", "flags": ["self", "extern_learn"]}],
            "frr-neigh": [{"dst": "10.244.240.10", "lladdr": "0a:58:f0:00:00:0a",
                           "state": ["NOARP"]}],
        }
        cases = [(None, None, None)]
        for name, changes in (
            ("worker-fdb", {"dev": "wrong", "state": "", "flags": ["self"],
                            "dst": "198.18.0.3", "mac": "00:00:00:00:00:00"}),
            ("worker-neigh", {"dev": "wrong", "state": ["STALE"]}),
            ("frr-fdb", {"dev": "wrong", "flags": []}),
            ("frr-neigh", {"dev": "wrong", "state": ["FAILED"]}),
        ):
            cases.extend((name, key, value) for key, value in changes.items())
        for name, key, value in cases:
            with self.subTest(name=name, key=key), tempfile.TemporaryDirectory() as directory:
                data = json.loads(json.dumps(baseline))
                if name:
                    data[name][0][key] = value
                for source, entries in data.items():
                    (Path(directory) / (source + ".json")).write_text(json.dumps(entries))
                result = self.shell(f"""
set -euo pipefail
KIND_CLUSTER=cni-24b-offline
source '{LIB}/cni-evpn-frr-interop-test.sh'
EVIDENCE_DIR='{directory}'
LOCAL_MAC=0a:58:f0:00:00:0a
INTEROP_FRR_VXLAN_IFACE=vxlan1000
interop_exec_worker() {{
  case "$*" in
    'bridge -j fdb show dev evpnvx33') cat '{directory}/worker-fdb.json';;
    'ip -4 -j neigh show nud all dev evpnbr33') cat '{directory}/worker-neigh.json';;
    *) return 99;;
  esac
}}
interop_exec_frr() {{
  case "$*" in
    'bridge -j fdb show dev vxlan1000') cat '{directory}/frr-fdb.json';;
    'ip -4 -j neigh show nud all dev br1000') cat '{directory}/frr-neigh.json';;
    *) return 99;;
  esac
}}
interop_assert_forwarding_state
""")
                if name:
                    self.assertNotEqual(result.returncode, 0)
                    self.assertIn("AssertionError", result.stderr)
                else:
                    self.assertEqual(result.returncode, 0, result.stderr)


if __name__ == "__main__":
    unittest.main()
