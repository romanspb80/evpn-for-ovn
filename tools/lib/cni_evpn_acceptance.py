#!/usr/bin/env python3
"""Fail-closed route and OVN cleanup evidence checks for Type-2 acceptance."""

import argparse
from argparse import Namespace
from contextlib import redirect_stderr, redirect_stdout
import io
import ipaddress
import json
from pathlib import Path
import re
import sys

from cni_invocation_identity import (
    CniIdentityError,
    parse_invocation_identity,
    read_call,
)
from cni_ovn_diagnostics import _read_rows, _uuid_members, classify_object


class EvidenceError(ValueError):
    """Raised when required runtime evidence is missing or malformed."""


def _read_json(path, description):
    try:
        return json.loads(Path(path).read_text(encoding="utf-8"))
    except (OSError, UnicodeDecodeError, json.JSONDecodeError) as error:
        raise EvidenceError(f"{description} is unreadable or malformed: {error}") from error


def capture_endpoint_identity(args):
    try:
        env, return_code = read_call(args.call_dir)
    except CniIdentityError as error:
        raise EvidenceError(f"CNI ADD evidence is incomplete: {error}") from error

    if return_code != "0" or env.get("CNI_COMMAND") != "ADD":
        raise EvidenceError("endpoint identity is not from a successful CNI ADD")
    try:
        cni_identity = parse_invocation_identity(env)
        config = _read_json(Path(args.call_dir, "config"), "CNI ADD config")
        result = _read_json(Path(args.call_dir, "stdout"), "CNI ADD result")
    except (CniIdentityError, OSError, UnicodeDecodeError, ValueError) as error:
        raise EvidenceError(f"CNI ADD evidence is incomplete: {error}") from error

    if cni_identity["K8S_POD_UID"] != args.uid:
        raise EvidenceError("CNI ADD UID does not match the requested Pod")
    if cni_identity.get("K8S_POD_NAMESPACE") != args.pod_namespace:
        raise EvidenceError("CNI ADD namespace does not match the requested Pod")
    if cni_identity.get("K8S_POD_NAME") != args.pod_name:
        raise EvidenceError("CNI ADD name does not match the requested Pod")
    try:
        configured_mac = config["macAddress"].lower()
        configured_ip = str(ipaddress.ip_interface(config["ipAddress"]).ip)
        interfaces = result["interfaces"]
        addresses = result["ips"]
        result_macs = [item["mac"].lower() for item in interfaces if item.get("name") == "net1"]
        result_ips = [
            str(ipaddress.ip_interface(item["address"]).ip)
            for item in addresses
            if item.get("interface") == 0
        ]
    except (KeyError, TypeError, ValueError) as error:
        raise EvidenceError(f"CNI endpoint identity is malformed: {error}") from error
    if (re.fullmatch(r"(?:[0-9a-f]{2}:){5}[0-9a-f]{2}", configured_mac) is None or
            result_macs != [configured_mac] or result_ips != [configured_ip]):
        raise EvidenceError("CNI ADD config and successful result disagree on MAC/IP")
    if not args.rd or args.vni <= 0:
        raise EvidenceError("route distinguisher and VNI must be valid")

    tables = {
        "northbound": _read_rows(args.nb_lsps, ["_uuid", "name", "external_ids"]),
        "southbound": _read_rows(args.sb_bindings, ["_uuid", "logical_port"]),
        "interfaces": _read_rows(args.ovs_interfaces, ["_uuid", "name", "external_ids"]),
        "ports": _read_rows(args.ovs_ports, ["_uuid", "name", "interfaces"]),
    }
    lsps = [
        row for row in tables["northbound"]
        if isinstance(row["external_ids"], dict)
        and row["external_ids"].get("evpn.framework/workload-uid") == args.uid
    ]
    if len(lsps) != 1:
        raise EvidenceError(f"expected one UID-owned NB LSP, found {len(lsps)}")
    lsp = lsps[0]
    if not lsp["_uuid"] or not lsp["name"]:
        raise EvidenceError("UID-owned NB LSP is missing UUID or logical-port name")

    bindings = [row for row in tables["southbound"] if row["logical_port"] == lsp["name"]]
    if len(bindings) != 1 or not bindings[0]["_uuid"]:
        raise EvidenceError(f"expected one exact SB Port_Binding, found {len(bindings)}")

    interfaces = [
        row for row in tables["interfaces"]
        if isinstance(row["external_ids"], dict)
        and row["external_ids"].get("evpn.framework/workload-uid") == args.uid
        and row["external_ids"].get("iface-id") == lsp["name"]
    ]
    if len(interfaces) != 1 or not interfaces[0]["_uuid"]:
        raise EvidenceError(f"expected one UID-owned OVS Interface, found {len(interfaces)}")
    interface = interfaces[0]

    ports = [
        row for row in tables["ports"]
        if interface["_uuid"] in _uuid_members(row["interfaces"])
    ]
    if len(ports) != 1 or not ports[0]["_uuid"]:
        raise EvidenceError(f"expected one OVS Port for the owned Interface, found {len(ports)}")

    identity = {
        "uid": args.uid,
        "podNamespace": cni_identity["K8S_POD_NAMESPACE"],
        "podName": cni_identity["K8S_POD_NAME"],
        "mac": configured_mac,
        "ip": configured_ip,
        "rd": args.rd,
        "vni": args.vni,
        "logicalPort": lsp["name"],
        "lspUuid": lsp["_uuid"],
        "portBindingUuid": bindings[0]["_uuid"],
        "interfaceName": interface["name"],
        "interfaceUuid": interface["_uuid"],
        "portName": ports[0]["name"],
        "portUuid": ports[0]["_uuid"],
    }
    Path(args.output).write_text(json.dumps(identity, indent=2, sort_keys=True) + "\n")
    print(json.dumps(identity, sort_keys=True))


_GOBGP_MACADV = re.compile(
    r"^\s*(?P<best>\*>?)\s*\[type:macadv\]\[rd:(?P<rd>[^\]]+)\]"
    r"\[etag:(?P<etag>\d+)\]\[mac:(?P<mac>(?:[0-9a-fA-F]{2}:){5}[0-9a-fA-F]{2})\]"
    r"(?:\[ip:(?P<ip>[0-9a-fA-F:.]+)\])?\s+\[(?P<vni>\d+)\]\s+"
    r"(?P<next_hop>[0-9a-fA-F:.]+)(?:[ \t]+.*)?$"
)
_FRR_ROUTE = re.compile(
    r"^BGP routing table entry for (?P<rd>[^:]+:\d+):\[2\]:\[0\]:\[48\]:"
    r"\[(?P<mac>(?:[0-9a-fA-F]{2}:){5}[0-9a-fA-F]{2})\]"
    r"(?::\[32\]:\[(?P<ip>[0-9a-fA-F:.]+)\])?$"
)
_FRR_ROUTE_TYPE = re.compile(
    r"^BGP routing table entry for [^:]+:\d+:\[(?P<route_type>\d+)\]:"
)
_FRR_PATHS = re.compile(r"^Paths: \((\d+) available(?:,.*)?\)$")


def parse_gobgp_routes(text):
    if not text.strip() or not re.search(r"(?m)^\s*Network\s+Labels\s+Next Hop\b", text):
        raise EvidenceError("GoBGP RIB is empty or missing its expected table header")
    routes = []
    malformed = []
    for line in text.splitlines():
        if "[type:macadv]" not in line:
            continue
        match = _GOBGP_MACADV.fullmatch(line)
        if not match:
            malformed.append(line)
            continue
        route = match.groupdict()
        route["mac"] = route["mac"].lower()
        route["ip"] = route["ip"] or ""
        route["vni"] = int(route["vni"])
        try:
            if route["ip"]:
                ipaddress.ip_address(route["ip"])
            ipaddress.ip_address(route["next_hop"])
        except ValueError as error:
            raise EvidenceError("GoBGP RIB contains an invalid Type-2 IP or next hop") from error
        routes.append(route)
    if malformed or not routes:
        raise EvidenceError("GoBGP RIB has malformed or no parseable MAC/IP Type-2 routes")
    return routes


def parse_frr_routes(text):
    if not text.strip():
        raise EvidenceError("FRR EVPN RIB is empty")
    blocks = []
    current = None
    saw_header = False
    for line in text.splitlines():
        if line.startswith("BGP routing table entry for "):
            saw_header = True
            if current is not None:
                blocks.append(current)
            route_type = _FRR_ROUTE_TYPE.match(line)
            if route_type is None:
                raise EvidenceError("FRR EVPN RIB has a malformed route header")
            is_type2 = route_type.group("route_type") == "2"
            match = _FRR_ROUTE.fullmatch(line)
            current = {
                "identity": match.groupdict() if match and is_type2 else None,
                "type2": is_type2,
                "paths": None,
                "text": [],
            }
        elif current is not None:
            if current["type2"]:
                current["text"].append(line)
            if current["type2"] and line.startswith("Paths:"):
                match = _FRR_PATHS.fullmatch(line)
                if not match:
                    raise EvidenceError("FRR EVPN RIB has malformed Paths evidence")
                current["paths"] = int(match.group(1))
    if current is not None:
        blocks.append(current)
    if not saw_header or not blocks:
        raise EvidenceError("FRR EVPN RIB has no parseable route entries")
    for block in blocks:
        if not block["type2"]:
            continue
        if block["identity"] is None or block["paths"] is None:
            raise EvidenceError("FRR EVPN RIB contains a malformed or incomplete route block")
        block["identity"]["mac"] = block["identity"]["mac"].lower()
        block["identity"]["ip"] = block["identity"]["ip"] or ""
        try:
            if block["identity"]["ip"]:
                ipaddress.ip_address(block["identity"]["ip"])
        except ValueError as error:
            raise EvidenceError("FRR EVPN RIB contains an invalid Type-2 IP") from error
        block["text"] = "\n".join(block["text"])
    return blocks


def _identity_routes(routes, identity):
    return [
        route for route in routes
        if route["rd"] == identity["rd"]
        and route["mac"] == identity["mac"]
        and route["vni"] == identity["vni"]
        and route["etag"] == "0"
        and route["ip"] in ("", identity["ip"])
    ]


def _route_variants(identity):
    return {("", identity["mac"]), (identity["ip"], identity["mac"])}


def evaluate_route_transition(deleted, survivor, gobgp_text, frr_text, peer,
                              remote_rd, remote_mac, remote_ip, remote_vtep, vni):
    gobgp = parse_gobgp_routes(gobgp_text)
    frr = parse_frr_routes(frr_text)
    problems = []
    detail = {}

    def check_survivor(identity, name):
        matches = _identity_routes(gobgp, identity)
        variants = {(route["ip"], route["mac"]) for route in matches}
        good = (
            variants == _route_variants(identity)
            and len(matches) == 2
            and all(route["next_hop"] == peer for route in matches)
            and all(route["best"] == "*>" for route in matches)
        )
        frr_identity = {
            "rd": identity["rd"],
            "mac": identity["mac"],
            "ip": identity["ip"],
        }
        received = [
            block for block in frr
            if block["type2"]
            and block["identity"]["rd"] == frr_identity["rd"]
            and block["identity"]["mac"] == frr_identity["mac"]
            and block["identity"]["ip"] in ("", identity["ip"])
            and block["paths"] > 0
            and re.search(rf"(?m)^\s+\S+\s+from {re.escape(peer)}\s+\(", block["text"])
        ]
        frr_variants = {
            (block["identity"]["ip"], block["identity"]["mac"])
            for block in received
        }
        good = good and frr_variants == _route_variants(identity)
        good = good and len(received) == 2
        detail[name] = {
            "gobgpVariants": sorted(ip or "MAC-only" for ip, _ in variants),
            "frrPeerVariants": sorted(ip or "MAC-only" for ip, _ in frr_variants),
        }
        if not good:
            problems.append(f"{name} Type-2 routes are absent, duplicated, or not advertised")

    deleted_matches = _identity_routes(gobgp, deleted)
    deleted_frr_active = [
        block for block in frr
        if block["type2"]
        and block["identity"]["rd"] == deleted["rd"]
        and block["identity"]["mac"] == deleted["mac"]
        and block["identity"]["ip"] in ("", deleted["ip"])
        and block["paths"] > 0
        and re.search(rf"(?m)^\s+\S+\s+from {re.escape(peer)}\s+\(", block["text"])
    ]
    detail["deleted"] = {
        "gobgpVariants": sorted(route["ip"] or "MAC-only" for route in deleted_matches),
        "frrPeerVariants": sorted(
            block["identity"]["ip"] or "MAC-only" for block in deleted_frr_active
        ),
    }
    if deleted_matches:
        problems.append("deleted Pod Type-2 route remains in GoBGP RIB")
    if deleted_frr_active:
        problems.append("deleted Pod Type-2 route remains active in FRR from the GoBGP peer")

    check_survivor(survivor, "survivor")

    remote = [
        route for route in gobgp
        if route["rd"] == remote_rd and route["mac"] == remote_mac.lower()
        and route["ip"] == remote_ip and route["vni"] == vni
        and route["best"] == "*>"
    ]
    remote_frr = [
        block for block in frr
        if block["type2"]
        and block["identity"] == {"rd": remote_rd, "mac": remote_mac.lower(), "ip": remote_ip}
        and block["paths"] > 0
    ]
    if not remote or not any(route["next_hop"] == remote_vtep for route in remote):
        raise EvidenceError("GoBGP RIB is missing the active remote-route positive control")
    if not remote_frr:
        raise EvidenceError("FRR RIB is missing the active remote-route positive control")
    detail["remotePositiveControl"] = {"gobgp": len(remote), "frr": len(remote_frr)}
    return {"ok": not problems, "problems": problems, "routes": detail}


def _rows(path, columns, label):
    try:
        return _read_rows(path, columns)
    except (OSError, UnicodeDecodeError, json.JSONDecodeError, ValueError) as error:
        raise EvidenceError(f"{label} query evidence is missing or invalid: {error}") from error


def _require_absent(path, object_kind, *, key_column=None, key_value=None,
                    map_column=None, map_key=None, map_value=None):
    output = io.StringIO()
    error_output = io.StringIO()
    arguments = Namespace(
        input=path,
        object_kind=object_kind,
        key_column=key_column,
        key_value=key_value,
        map_column=map_column,
        map_key=map_key,
        map_value=map_value,
        expect="absent",
    )
    with redirect_stdout(output), redirect_stderr(error_output):
        status = classify_object(arguments)
    if status != 0:
        detail = error_output.getvalue().strip() or output.getvalue().strip()
        raise EvidenceError(f"{object_kind} absence check failed: {detail}")
    result = json.loads(output.getvalue())
    if result.get("classification") != "OBJECT_ABSENT":
        raise EvidenceError(f"{object_kind} was not proven absent")
    return result


def verify_cleanup(identities, nb_lsps, sb_bindings, ovs_interfaces, ovs_ports, ovs_bridges):
    bridges = _rows(ovs_bridges, ["name", "ports"], "OVS Bridge")
    checks = []
    for identity in identities:
        uid = identity["uid"]
        logical_port = identity["logicalPort"]
        for column, value, label in (
            ("_uuid", identity["lspUuid"], "NB LSP UUID"),
            ("name", logical_port, "NB LSP logical port"),
        ):
            checks.append(_require_absent(nb_lsps, label, key_column=column, key_value=value))
        checks.append(_require_absent(
            nb_lsps, "NB LSP workload UID", map_column="external_ids",
            map_key="evpn.framework/workload-uid", map_value=uid,
        ))
        for column, value, label in (
            ("_uuid", identity["portBindingUuid"], "SB Port_Binding UUID"),
            ("logical_port", logical_port, "SB Port_Binding logical port"),
        ):
            checks.append(_require_absent(sb_bindings, label, key_column=column, key_value=value))
        for column, value, label in (
            ("_uuid", identity["interfaceUuid"], "OVS Interface UUID"),
            ("name", identity["interfaceName"], "OVS Interface name"),
        ):
            checks.append(_require_absent(
                ovs_interfaces, label, key_column=column, key_value=value
            ))
        checks.append(_require_absent(
            ovs_interfaces, "OVS Interface workload UID", map_column="external_ids",
            map_key="evpn.framework/workload-uid", map_value=uid,
        ))
        checks.append(_require_absent(
            ovs_interfaces, "OVS Interface iface-id", map_column="external_ids",
            map_key="iface-id", map_value=logical_port,
        ))
        for column, value, label in (
            ("_uuid", identity["portUuid"], "OVS Port UUID"),
            ("name", identity["portName"], "OVS Port name"),
        ):
            checks.append(_require_absent(ovs_ports, label, key_column=column, key_value=value))
        if any(identity["portUuid"] in _uuid_members(row["ports"]) for row in bridges):
            raise EvidenceError(f"OVS Bridge still references the owned Port for UID {uid}")
        checks.append({
            "classification": "OBJECT_ABSENT",
            "objectKind": "OVS Bridge port membership",
            "query": {"ports": identity["portUuid"]},
            "matches": [],
        })
    return {"ok": True, "checks": checks, "problems": []}


_CRASH_SIGNATURE = re.compile(
    r"SIGSEGV|segmentation fault|signal 11|core dumped|"
    r"(?:restarting|restart(?:ed|ing)?).{0,80}ovn-northd",
    re.IGNORECASE,
)


def verify_runtime_health(process_text, northd_log, baseline_path):
    pids = {}
    for line in process_text.splitlines():
        fields = line.split(None, 7)
        if len(fields) < 8:
            continue
        try:
            pid = int(fields[1])
        except ValueError:
            continue
        command = fields[7].split(None, 1)[0].rsplit("/", 1)[-1]
        if command in ("ovn-northd", "ovn-controller"):
            pids.setdefault(command, []).append(pid)
    if any(len(pids.get(name, [])) != 1 for name in ("ovn-northd", "ovn-controller")):
        raise EvidenceError("northd/controller process is missing or duplicated")
    current = {name: values[0] for name, values in pids.items()}
    if _CRASH_SIGNATURE.search(northd_log):
        raise EvidenceError("northd crash or restart signature found in complete log")
    baseline = Path(baseline_path)
    if baseline.exists():
        old = _read_json(baseline, "northd PID baseline")
        if old != current:
            raise EvidenceError(f"northd/controller PID changed: before={old} after={current}")
    else:
        baseline.write_text(json.dumps(current, indent=2, sort_keys=True) + "\n")
    return {"ok": True, "pids": current}


def main():
    parser = argparse.ArgumentParser()
    commands = parser.add_subparsers(dest="action", required=True)

    capture = commands.add_parser("capture-identity")
    capture.add_argument("--call-dir", required=True)
    capture.add_argument("--uid", required=True)
    capture.add_argument("--pod-namespace", required=True)
    capture.add_argument("--pod-name", required=True)
    capture.add_argument("--rd", required=True)
    capture.add_argument("--vni", required=True, type=int)
    capture.add_argument("--nb-lsps", required=True)
    capture.add_argument("--sb-bindings", required=True)
    capture.add_argument("--ovs-interfaces", required=True)
    capture.add_argument("--ovs-ports", required=True)
    capture.add_argument("--output", required=True)

    routes = commands.add_parser("verify-route-transition")
    routes.add_argument("--deleted", required=True)
    routes.add_argument("--survivor", required=True)
    routes.add_argument("--gobgp", required=True)
    routes.add_argument("--frr", required=True)
    routes.add_argument("--peer", required=True)
    routes.add_argument("--remote-rd", required=True)
    routes.add_argument("--remote-mac", required=True)
    routes.add_argument("--remote-ip", required=True)
    routes.add_argument("--remote-vtep", required=True)
    routes.add_argument("--vni", required=True, type=int)

    cleanup = commands.add_parser("verify-cleanup")
    cleanup.add_argument("--identity", action="append", required=True)
    cleanup.add_argument("--nb-lsps", required=True)
    cleanup.add_argument("--sb-bindings", required=True)
    cleanup.add_argument("--ovs-interfaces", required=True)
    cleanup.add_argument("--ovs-ports", required=True)
    cleanup.add_argument("--ovs-bridges", required=True)

    health = commands.add_parser("verify-runtime-health")
    health.add_argument("--processes", required=True)
    health.add_argument("--northd-log", required=True)
    health.add_argument("--baseline-pids", required=True)

    args = parser.parse_args()
    try:
        if args.action == "capture-identity":
            capture_endpoint_identity(args)
            return 0
        if args.action == "verify-route-transition":
            report = evaluate_route_transition(
                _read_json(args.deleted, "deleted endpoint identity"),
                _read_json(args.survivor, "survivor endpoint identity"),
                Path(args.gobgp).read_text(encoding="utf-8"),
                Path(args.frr).read_text(encoding="utf-8"),
                args.peer, args.remote_rd, args.remote_mac, args.remote_ip,
                args.remote_vtep, args.vni,
            )
        elif args.action == "verify-cleanup":
            report = verify_cleanup(
                [_read_json(path, "endpoint identity") for path in args.identity],
                args.nb_lsps, args.sb_bindings, args.ovs_interfaces,
                args.ovs_ports, args.ovs_bridges,
            )
        else:
            process_text = Path(args.processes).read_text(encoding="utf-8")
            northd_log = Path(args.northd_log).read_text(encoding="utf-8")
            report = verify_runtime_health(process_text, northd_log, args.baseline_pids)
    except (
        EvidenceError, OSError, UnicodeDecodeError, json.JSONDecodeError, KeyError, TypeError
    ) as error:
        print(json.dumps({"ok": False, "error": str(error)}), file=sys.stderr)
        return 2
    print(json.dumps(report, indent=2, sort_keys=True))
    return 0 if report["ok"] else 1


if __name__ == "__main__":
    raise SystemExit(main())
