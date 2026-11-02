#!/usr/bin/env python3
"""Offline regressions for no-replacement Type-2 route and cleanup acceptance."""

import json
from pathlib import Path
import subprocess
import tempfile
import unittest

import cni_evpn_acceptance as acceptance


ROOT = Path(__file__).resolve().parents[2]
HELPER = ROOT / "tools/lib/cni-evpn-frr-interop-test.sh"

DELETED = {
    "uid": "pod-b-uid",
    "mac": "0a:58:f0:00:00:0b",
    "ip": "10.244.240.11",
    "rd": "198.18.0.2:1000",
    "vni": 1000,
    "logicalPort": "pod-b-port",
    "lspUuid": "lsp-b",
    "portBindingUuid": "pb-b",
    "interfaceName": "veth-b",
    "interfaceUuid": "iface-b",
    "portName": "port-b",
    "portUuid": "ovs-port-b",
}
SURVIVOR = {
    **DELETED,
    "uid": "pod-a-uid",
    "mac": "0a:58:f0:00:00:0a",
    "ip": "10.244.240.10",
    "logicalPort": "pod-a-port",
    "lspUuid": "lsp-a",
    "portBindingUuid": "pb-a",
    "interfaceName": "veth-a",
    "interfaceUuid": "iface-a",
    "portName": "port-a",
    "portUuid": "ovs-port-a",
}
REMOTE = {
    "rd": "198.18.0.3:1000",
    "mac": "0a:58:f0:01:00:0c",
    "ip": "10.244.240.12",
    "vni": 1000,
    "next_hop": "198.18.0.4",
}
PEER = "198.18.0.2"


def _gobgp_route(identity, ip, next_hop=None):
    mac = identity["mac"]
    rd = identity["rd"]
    vni = identity["vni"]
    route = f"[type:macadv][rd:{rd}][etag:0][mac:{mac}]"
    if ip:
        route += f"[ip:{ip}]"
    return f"*> {route} [{vni}] {next_hop or PEER}"


def _frr_block(identity, ip, peer=None, paths=1):
    route = (
        f"BGP routing table entry for {identity['rd']}:[2]:[0]:[48]:"
        f"[{identity['mac']}]"
    )
    if ip:
        route += f":[32]:[{ip}]"
    if paths:
        path_text = (
            f"Paths: ({paths} available, best #1)\n"
            f"  Route [2]:[0]:[48]:[{identity['mac']}] VNI {identity.get('vni', 1000)}\n"
            f"  Local\n"
            f"    {peer or PEER} from {peer or PEER} ({peer or PEER})\n"
        )
    else:
        path_text = "Paths: (0 available, no best path)\n  Not advertised to any peer\n"
    return f"{route}\n{path_text}"


def _frr_multicast_block():
    return (
        "BGP routing table entry for 198.18.0.3:1000:[3]:[0]:[32]:[198.18.0.4]\n"
        "Paths: (1 available, best #1)\n"
        "  Route [3]:[0]:[32]:[198.18.0.4] VNI 1000\n"
    )


def valid_route_evidence():
    gobgp = [
        _gobgp_route(SURVIVOR, ""),
        _gobgp_route(SURVIVOR, SURVIVOR["ip"]),
        _gobgp_route(REMOTE, REMOTE["ip"], REMOTE["next_hop"]),
    ]
    frr = [
        _frr_block(DELETED, "", paths=0),
        _frr_block(DELETED, DELETED["ip"], paths=0),
        _frr_block(SURVIVOR, ""),
        _frr_block(SURVIVOR, SURVIVOR["ip"]),
        _frr_block(REMOTE, REMOTE["ip"], peer="198.18.0.4"),
        _frr_multicast_block(),
    ]
    return "Network Labels Next Hop\n" + "\n".join(gobgp) + "\n", "".join(frr)


def _ovsdb(headings, rows):
    return {"headings": headings, "data": rows}


def _write_json(directory, name, document):
    path = Path(directory) / name
    path.write_text(json.dumps(document), encoding="utf-8")
    return str(path)


def cleanup_files(directory, *, remaining_sb=False, malformed_sb=False):
    files = {
        "nb": _write_json(directory, "nb.json", _ovsdb(
            ["_uuid", "name", "external_ids"], []
        )),
        "sb": _write_json(directory, "sb.json", _ovsdb(
            ["_uuid", "logical_port"],
            [["pb-b", "pod-b-port"]] if remaining_sb else [],
        )),
        "interfaces": _write_json(directory, "interfaces.json", _ovsdb(
            ["_uuid", "name", "external_ids"], []
        )),
        "ports": _write_json(directory, "ports.json", _ovsdb(
            ["_uuid", "name", "interfaces"], []
        )),
        "bridges": _write_json(directory, "bridges.json", _ovsdb(
            ["name", "ports"], [["br-int", ["set", []]]]
        )),
    }
    if malformed_sb:
        Path(files["sb"]).write_text('{"headings":["_uuid"]}', encoding="utf-8")
    return files


class RouteTransitionTests(unittest.TestCase):
    def test_gobgp_cli_trailing_columns_are_accepted(self):
        routes = acceptance.parse_gobgp_routes(
            "Network Labels Next Hop Age Attrs\n"
            "*> [type:macadv][rd:198.18.0.3:1000][etag:0]"
            "[mac:0a:58:f0:01:00:0c][ip:10.244.240.12] [1000] "
            "198.18.0.4 00:01:49 [{Origin: i} {Extcomms: [VXLAN]}]\n"
        )
        self.assertEqual(len(routes), 1)
        self.assertEqual(routes[0]["mac"], REMOTE["mac"])
        self.assertEqual(routes[0]["ip"], REMOTE["ip"])
        self.assertEqual(routes[0]["next_hop"], REMOTE["next_hop"])
        self.assertEqual(routes[0]["vni"], REMOTE["vni"])

    def test_successful_no_replacement_route_transition(self):
        gobgp, frr = valid_route_evidence()
        result = acceptance.evaluate_route_transition(
            DELETED, SURVIVOR, gobgp, frr, PEER,
            REMOTE["rd"], REMOTE["mac"], REMOTE["ip"], REMOTE["next_hop"], 1000,
        )
        self.assertTrue(result["ok"], result["problems"])
        self.assertEqual(result["routes"]["deleted"]["gobgpVariants"], [])
        self.assertEqual(result["routes"]["deleted"]["frrPeerVariants"], [])

    def test_deleted_pod_route_remaining_advertised_fails(self):
        gobgp, frr = valid_route_evidence()
        gobgp += _gobgp_route(DELETED, "") + "\n"
        frr += _frr_block(DELETED, "")
        result = acceptance.evaluate_route_transition(
            DELETED, SURVIVOR, gobgp, frr, PEER,
            REMOTE["rd"], REMOTE["mac"], REMOTE["ip"], REMOTE["next_hop"], 1000,
        )
        self.assertFalse(result["ok"])
        self.assertIn("deleted Pod Type-2 route remains in GoBGP RIB", result["problems"])
        self.assertIn(
            "deleted Pod Type-2 route remains active in FRR from the GoBGP peer",
            result["problems"],
        )

    def test_missing_survivor_route_fails(self):
        gobgp, frr = valid_route_evidence()
        gobgp = "\n".join(line for line in gobgp.splitlines() if SURVIVOR["mac"] not in line)
        frr = "".join(
            block for block in frr.split("BGP routing table entry for ")
            if SURVIVOR["mac"] not in block
        )
        frr = frr.replace(
            f"{REMOTE['rd']}:[2]", f"BGP routing table entry for {REMOTE['rd']}:[2]", 1
        )
        result = acceptance.evaluate_route_transition(
            DELETED, SURVIVOR, gobgp, frr, PEER,
            REMOTE["rd"], REMOTE["mac"], REMOTE["ip"], REMOTE["next_hop"], 1000,
        )
        self.assertFalse(result["ok"])
        self.assertIn("survivor Type-2 routes are absent, duplicated, or not advertised", result["problems"])

    def test_empty_or_malformed_gobgp_and_frr_evidence_fails_closed(self):
        gobgp, frr = valid_route_evidence()
        malformed_gobgp = (
            "Network Labels Next Hop\n"
            "*> [type:macadv][rd:198.18.0.2:1000][etag:0][mac:bad]\n"
        )
        for value in ("", "Network Labels Next Hop\nnot-a-route\n", malformed_gobgp):
            with self.subTest(source="GoBGP", value=value):
                with self.assertRaises(acceptance.EvidenceError):
                    acceptance.evaluate_route_transition(
                        DELETED, SURVIVOR, value, frr, PEER,
                        REMOTE["rd"], REMOTE["mac"], REMOTE["ip"], REMOTE["next_hop"], 1000,
                    )
        malformed_frr = (
            "BGP routing table entry for 198.18.0.2:1000:[2]:[0]:[48]:[not-a-mac]\n"
            "Paths: (1 available, best #1)\n"
        )
        for value in ("", "no BGP route entries\n", malformed_frr):
            with self.subTest(source="FRR", value=value):
                with self.assertRaises(acceptance.EvidenceError):
                    acceptance.evaluate_route_transition(
                        DELETED, SURVIVOR, gobgp, value, PEER,
                        REMOTE["rd"], REMOTE["mac"], REMOTE["ip"], REMOTE["next_hop"], 1000,
                    )


class CleanupAndHealthTests(unittest.TestCase):
    def test_successful_exact_cleanup(self):
        with tempfile.TemporaryDirectory() as directory:
            files = cleanup_files(directory)
            result = acceptance.verify_cleanup(
                [DELETED, SURVIVOR], files["nb"], files["sb"], files["interfaces"],
                files["ports"], files["bridges"],
            )
        self.assertTrue(result["ok"])
        self.assertEqual(result["problems"], [])
        self.assertEqual(
            sum(check["classification"] == "OBJECT_ABSENT" for check in result["checks"]),
            24,
        )

    def test_remaining_sb_port_binding_fails(self):
        with tempfile.TemporaryDirectory() as directory:
            files = cleanup_files(directory, remaining_sb=True)
            with self.assertRaises(acceptance.EvidenceError):
                acceptance.verify_cleanup(
                    [DELETED], files["nb"], files["sb"], files["interfaces"],
                    files["ports"], files["bridges"],
                )

    def test_failed_or_malformed_required_sb_query_fails(self):
        with tempfile.TemporaryDirectory() as directory:
            files = cleanup_files(directory, malformed_sb=True)
            with self.assertRaises(acceptance.EvidenceError):
                acceptance.verify_cleanup(
                    [DELETED], files["nb"], files["sb"], files["interfaces"],
                    files["ports"], files["bridges"],
                )

    def test_sync_timeout_aborts_before_final_cleanup_queries(self):
        self._run_shell_cleanup_mock("ovn-nb-sb-sync-final.txt", 124)

    def test_required_sb_query_failure_aborts_cleanup(self):
        self._run_shell_cleanup_mock("cleanup-sb-bindings.json", 1)

    def _run_shell_cleanup_mock(self, fail_file, return_code):
        source = HELPER.read_text(encoding="utf-8")
        function = source[
            source.index("interop_verify_final_ovn_cleanup() {"):
            source.index("\ninterop_verify_no_replacement_lifecycle() {")
        ]
        script = f"""
calls=""
interop_capture_northd_health() {{
  calls="${{calls}} health:$1"
  return 0
}}
interop_capture_diagnostic() {{
  calls="${{calls}} capture:$1"
  if [[ "$1" == "{fail_file}" ]]; then return {return_code}; fi
  return 0
}}
{function}
status=0
interop_verify_final_ovn_cleanup || status=$?
printf '%s\\n' "$calls"
exit "$status"
"""
        result = subprocess.run(
            ["bash", "-c", script],
            text=True,
            capture_output=True,
            check=False,
        )
        self.assertEqual(result.returncode, 1, result.stdout + result.stderr)
        self.assertIn("capture:" + fail_file, result.stdout + result.stderr)
        self.assertNotIn("health:after-final-sb-convergence", result.stdout + result.stderr)

    def test_northd_crash_restart_and_unhealthy_processes_fail(self):
        healthy = (
            "UID PID PPID C STIME TTY TIME CMD\n"
            "root 101 1 0 10:00 ? 00:00:00 /usr/sbin/ovn-northd --detach\n"
            "root 102 1 0 10:00 ? 00:00:00 /usr/sbin/ovn-controller --pidfile\n"
        )
        with tempfile.TemporaryDirectory() as directory:
            baseline = Path(directory) / "pids.json"
            self.assertTrue(acceptance.verify_runtime_health(healthy, "healthy log", baseline)["ok"])
            with self.assertRaises(acceptance.EvidenceError):
                acceptance.verify_runtime_health(healthy, "ovn-northd: SIGSEGV", baseline)
            restarted = healthy.replace("root 101", "root 201")
            with self.assertRaises(acceptance.EvidenceError):
                acceptance.verify_runtime_health(restarted, "healthy log", baseline)
            with self.assertRaises(acceptance.EvidenceError):
                acceptance.verify_runtime_health(
                    "UID PID PPID C STIME TTY TIME CMD\nroot 101 1 0 10:00 ? 00:00:00 ovn-northd\n",
                    "healthy log", baseline,
                )


if __name__ == "__main__":
    unittest.main()
