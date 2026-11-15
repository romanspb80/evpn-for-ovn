#!/usr/bin/env python3
"""Disposable shared-OVN CNI test with an Operator-managed Node-Agent."""

import argparse
from datetime import datetime, timezone
import hashlib
import ipaddress
import json
import os
import re
import select
import shlex
import shutil
import stat
import subprocess
import sys
import time
import uuid
from pathlib import Path

import containerlab as lab
import isolated_secondary_cni as fixture
import operator_ownership


INVENTORY_SCENARIO = "operator-inventory-type2-two-worker"
EXAMPLE = fixture.ROOT / "deploy/operator/examples/two-worker"
REMOTE_IP = "10.244.240.30"
REMOTE_MAC = "02:00:00:00:00:30"
REMOTE_VTEP = "198.18.30.4"
EXTERNAL_VXLAN_INTERFACE = "vxlan1000"
EXTERNAL_BRIDGE = "br1000"
EXTERNAL_VNI = 1000
VXLAN_DSTPORT = 4789
SOURCE_ARCHIVE_HASH_COMMAND = "sha256sum /tmp/evpn-vm-source.tar"
INVENTORY_CAPTURE_LIMIT = 12
INVENTORY_CAPTURE_MAX_SECONDS = 120
INVENTORY_EVIDENCE_ARTIFACTS = frozenset({
    "dataplane-forwarding.json",
    "inventory-capture-start.json",
    "inventory-capture-stop.json",
    "inventory-capture-stop-command.json",
    "inventory-packet-evidence.json",
    "inventory-forwarding-endpoints.json",
    "inventory-workload-captures.json",
    "inventory-worker-a-net1.pcap",
    "inventory-worker-a-net1-decoded.json",
    "inventory-worker-b-net1.pcap",
    "inventory-worker-b-net1-decoded.json",
    "inventory-worker-a-underlay.pcap",
    "inventory-worker-b-underlay.pcap",
    "inventory-frr-underlay.pcap",
    "inventory-frr-workload.pcap",
    "inventory-frr-workload-decoded.json",
    "ovs-dataplane-worker-a.json",
    "ovs-dataplane-worker-b.json",
    "ovs-dataplane-after-worker-a.json",
    "ovs-dataplane-after-worker-b.json",
})
INVENTORY_ENDPOINTS = (
    ("10.244.240.11", "02:00:00:00:00:11"),
    ("10.244.240.12", "02:00:00:00:00:12"),
)


def utc_timestamp():
    return datetime.now(timezone.utc).isoformat(timespec="microseconds").replace("+00:00", "Z")


class InventoryEvidenceError(fixture.IsolationError):
    def __init__(self, failure_class, message):
        super().__init__(message)
        self.failure_class = failure_class


def verify_inventory_source_provenance(provenance_path, archive_path, environment=None):
    if environment is None:
        environment = os.environ
    for path in (Path(provenance_path), Path(archive_path)):
        if path.is_symlink() or not path.is_file():
            raise InventoryEvidenceError("MISSING_OBSERVATION", "source provenance or archive is missing/unsafe")
    try:
        provenance = json.loads(Path(provenance_path).read_text())
    except (OSError, ValueError, TypeError) as error:
        raise InventoryEvidenceError("MISSING_OBSERVATION", "source provenance JSON is invalid") from error
    commit = str(environment.get("EVPN_SOURCE_COMMIT", "")).lower()
    archive_sha = str(environment.get("EVPN_SOURCE_ARCHIVE_SHA256", "")).lower()
    archive_format = str(environment.get("EVPN_SOURCE_ARCHIVE_FORMAT", ""))
    archive_command = str(environment.get("EVPN_SOURCE_ARCHIVE_COMMAND", ""))
    if not re.fullmatch(r"[0-9a-f]{40}", commit) or not re.fullmatch(r"[0-9a-f]{64}", archive_sha):
        raise InventoryEvidenceError("MISSING_OBSERVATION", "source commit/archive provenance is missing")
    expected = {
        "commit": commit,
        "archiveSHA256": archive_sha,
        "archiveFormat": "tar",
        "archiveCommand": f"git archive --format=tar {commit}",
    }
    if archive_format != "tar" or archive_command != expected["archiveCommand"] or provenance != expected:
        raise InventoryEvidenceError("ATTRIBUTION_UNCERTAIN", "source provenance does not match the guest receipt")
    with Path(archive_path).open("rb") as stream:
        observed_sha = hashlib.file_digest(stream, "sha256").hexdigest()
    if observed_sha != archive_sha:
        raise InventoryEvidenceError("ATTRIBUTION_UNCERTAIN", "uploaded source archive SHA256 mismatch")
    return expected


def parse_capture_statistics(stderr):
    patterns = {
        "captured": r"(\d+)\s+packets captured",
        "receivedByFilter": r"(\d+)\s+packets received by filter",
        "droppedByKernel": r"(\d+)\s+packets dropped by kernel",
    }
    statistics = {}
    for name, pattern in patterns.items():
        match = re.search(pattern, stderr, re.IGNORECASE)
        if not match:
            raise InventoryEvidenceError("INCOMPLETE_CAPTURE", f"capture statistics are missing {name}")
        statistics[name] = int(match.group(1))
    if statistics["droppedByKernel"] != 0:
        raise InventoryEvidenceError("INCOMPLETE_CAPTURE", "capture dropped packets in the kernel")
    return statistics


def validate_capture_statistics(statistics, expected_count):
    if (statistics["captured"] != expected_count or
            statistics["receivedByFilter"] < statistics["captured"] or
            statistics["droppedByKernel"] != 0):
        raise InventoryEvidenceError(
            "INCOMPLETE_CAPTURE", "capture statistics do not prove a complete bounded capture"
        )
    return True


def write_inventory_manifest_checksum(directory):
    manifest_path = Path(directory) / "inventory-evidence-manifest.json"
    checksum_path = Path(directory) / "inventory-evidence-manifest.sha256"
    if manifest_path.is_symlink() or not manifest_path.is_file():
        raise fixture.IsolationError("evidence manifest is missing or unsafe")
    if checksum_path.exists() or checksum_path.is_symlink():
        raise fixture.IsolationError("refusing a pre-existing evidence manifest checksum")
    with manifest_path.open("rb") as stream:
        digest = hashlib.file_digest(stream, "sha256").hexdigest()
    descriptor = os.open(
        checksum_path, os.O_WRONLY | os.O_CREAT | os.O_EXCL |
        getattr(os, "O_NOFOLLOW", 0), 0o600,
    )
    with os.fdopen(descriptor, "w") as stream:
        stream.write(f"{digest}  inventory-evidence-manifest.json\n")
    return digest


def parse_inventory_workload_snapshot(namespace, link_output, address_output,
                                      interface, expected_mac, expected_ip):
    if not isinstance(namespace, str) or not re.fullmatch(r"net:\[\d+\]", namespace):
        raise InventoryEvidenceError("ATTRIBUTION_UNCERTAIN", "workload network namespace identity is unavailable")
    counters = parse_linux_link_counters(link_output, interface)
    try:
        links = json.loads(link_output)
        addresses = json.loads(address_output)
    except (TypeError, json.JSONDecodeError) as error:
        raise InventoryEvidenceError("ATTRIBUTION_UNCERTAIN", "workload interface identity is invalid JSON") from error
    if (not isinstance(links, list) or len(links) != 1 or
            not isinstance(addresses, list) or len(addresses) != 1):
        raise InventoryEvidenceError("ATTRIBUTION_UNCERTAIN", "workload interface identity is ambiguous")
    link = links[0]
    mac = link.get("address", "").lower()
    ips = sorted(
        item.get("local") for item in addresses[0].get("addr_info", [])
        if isinstance(item, dict) and item.get("family") == "inet"
    )
    if mac != expected_mac.lower() or ips != [expected_ip]:
        raise InventoryEvidenceError("ATTRIBUTION_UNCERTAIN", "workload interface MAC/IP identity differs")
    return {
        "namespace": namespace,
        "interface": interface,
        "ifindex": counters["ifindex"],
        "mac": mac,
        "ips": ips,
        "counters": counters,
        "observedAt": utc_timestamp(),
        "observedMonotonicNs": time.monotonic_ns(),
    }


def inventory_workload_counter_delta(before, after):
    for key in ("namespace", "interface", "ifindex", "mac", "ips"):
        if before.get(key) != after.get(key):
            raise InventoryEvidenceError(
                "ATTRIBUTION_UNCERTAIN", f"workload interface {key} identity changed during forwarding"
            )
    delta = {"interface": before["interface"], "ifindex": before["ifindex"]}
    for direction in ("rx", "tx"):
        delta[direction] = {}
        for name, value in before["counters"][direction].items():
            difference = after["counters"][direction][name] - value
            if difference < 0:
                raise InventoryEvidenceError(
                    "COUNTER_MISMATCH",
                    f"workload {direction} {name} counter moved backwards"
                )
            delta[direction][name] = difference
    if delta["rx"]["packets"] <= 0 or delta["tx"]["packets"] <= 0:
        raise InventoryEvidenceError("COUNTER_MISMATCH", "workload interface RX/TX counters did not advance")
    return delta


def inventory_ovs_counter_delta(before, after, flow_before, flow_after):
    deltas = {
        "rxPackets": after["rxPackets"] - before["rxPackets"],
        "txPackets": after["txPackets"] - before["txPackets"],
        "remoteType2FlowPackets": flow_after - flow_before,
    }
    if any(value <= 0 for value in deltas.values()):
        raise InventoryEvidenceError(
            "COUNTER_MISMATCH",
            "OVS tunnel and remote-Type-2 flow counters did not advance "
            f"(rx={deltas['rxPackets']}, tx={deltas['txPackets']}, "
            f"flow={deltas['remoteType2FlowPackets']})",
        )
    return deltas


def verify_inventory_workload_packets(packets, endpoint):
    directions = {}
    identifiers = endpoint["icmpIdentifiers"]
    expected = (
        ("workerToExternal", endpoint["ip"], REMOTE_IP, 8, identifiers["workerToExternal"]),
        ("workerToExternalReply", REMOTE_IP, endpoint["ip"], 0, identifiers["workerToExternal"]),
        ("externalToWorker", REMOTE_IP, endpoint["ip"], 8, identifiers["externalToWorker"]),
        ("externalToWorkerReply", endpoint["ip"], REMOTE_IP, 0, identifiers["externalToWorker"]),
    )
    allowed = set()
    for label, source, destination, kind, identifier in expected:
        allowed.add((source, destination, kind, identifier))
        matches = [
            packet for packet in packets
            if packet["source"] == source and packet["destination"] == destination
            and packet["icmpType"] == kind and packet["icmpIdentifier"] == identifier
        ]
        identities = {(packet["icmpIdentifier"], packet["icmpSequence"]) for packet in matches}
        if identities != {(identifier, sequence) for sequence in (1, 2, 3)} or len(matches) != 3:
            raise InventoryEvidenceError(
                "ATTRIBUTION_UNCERTAIN",
                f"{endpoint['node']}: workload capture has incomplete {label} ICMP identities"
            )
        directions[label] = identities
    unexpected = [
        packet for packet in packets
        if (packet["source"], packet["destination"], packet["icmpType"],
            packet["icmpIdentifier"]) not in allowed
    ]
    if unexpected:
        raise InventoryEvidenceError(
            "ATTRIBUTION_UNCERTAIN",
            f"{endpoint['node']}: workload capture contains unrelated ICMP identities",
        )
    for request, reply in (("workerToExternal", "workerToExternalReply"),
                           ("externalToWorker", "externalToWorkerReply")):
        if directions[request] != directions[reply]:
            raise InventoryEvidenceError(
                "ATTRIBUTION_UNCERTAIN",
                f"{endpoint['node']}: workload capture request/reply identities differ"
            )
    return {
        "status": "PASS",
        "directions": {name: len(values) for name, values in directions.items()},
        "identities": {
            name: sorted([identifier, sequence] for identifier, sequence in values)
            for name, values in directions.items()
        },
    }


def verify_inventory_external_workload_packets(packets, endpoints):
    result = {}
    for endpoint in endpoints:
        identity = endpoint["node"]
        endpoint_packets = [
            packet for packet in packets
            if (packet["source"], packet["destination"]) in (
                (endpoint["ip"], REMOTE_IP), (REMOTE_IP, endpoint["ip"])
            )
        ]
        result[identity] = verify_inventory_workload_packets(endpoint_packets, endpoint)
    return result


def evidence_file_manifest(directory, names):
    root = Path(directory)
    if root.is_symlink() or not root.is_dir():
        raise fixture.IsolationError("evidence directory is missing or unsafe")
    artifacts = []
    for name in sorted(names):
        if (not isinstance(name, str) or name not in INVENTORY_EVIDENCE_ARTIFACTS or
                Path(name).name != name or name in ("kubeconfig", "config", "id_rsa")):
            raise fixture.IsolationError("evidence manifest contains an unsafe artifact path")
        path = root / name
        try:
            descriptor = os.open(path, os.O_RDONLY | getattr(os, "O_NOFOLLOW", 0))
        except OSError as error:
            raise fixture.IsolationError(f"evidence artifact is missing or unsafe: {name}") from error
        with os.fdopen(descriptor, "rb") as stream:
            metadata = os.fstat(stream.fileno())
            if not stat.S_ISREG(metadata.st_mode) or metadata.st_nlink != 1:
                raise fixture.IsolationError(f"evidence artifact is not a private regular file: {name}")
            digest = hashlib.sha256()
            for chunk in iter(lambda: stream.read(1024 * 1024), b""):
                digest.update(chunk)
        artifacts.append({"path": name, "bytes": metadata.st_size,
                          "sha256": digest.hexdigest()})
    return artifacts


def redact_inventory_evidence(value, key=""):
    """Redact structured credentials and sensitive log lines before persistence."""
    sensitive = re.compile(r"(password|passwd|secret|credential|private.?key|token|authorization|"
                           r"kubeconfig|client.?key|certificate.?data)", re.I)
    if sensitive.search(key) or key in ("env", "envFrom", "stringData", "binaryData"):
        return "[REDACTED]"
    if isinstance(value, dict):
        return {k: redact_inventory_evidence(v, k) for k, v in value.items()}
    if isinstance(value, list):
        return [redact_inventory_evidence(v) for v in value]
    if isinstance(value, str):
        try:
            parsed = json.loads(value)
        except (ValueError, TypeError):
            parsed = None
        if isinstance(parsed, (dict, list)):
            return redact_inventory_evidence(parsed)
        output, in_pem = [], False
        for line in value.splitlines(keepends=True):
            if "-----BEGIN " in line:
                in_pem = True
            if in_pem or sensitive.search(line) or re.search(
                    r"\bBearer\s+\S+|eyJ[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\.", line, re.I):
                output.append("[REDACTED sensitive line]\n")
            else:
                output.append(line)
            if "-----END " in line:
                in_pem = False
        return "".join(output)
    return value


def validate_external_vxlan_identity(links):
    if not isinstance(links, list) or len(links) != 1:
        raise fixture.IsolationError("external VXLAN interface identity is ambiguous")
    link = links[0]
    info = link.get("linkinfo", {})
    data = info.get("info_data", {})
    try:
        local = str(ipaddress.ip_address(data.get("local", "")))
    except ValueError as error:
        raise fixture.IsolationError("external VXLAN local VTEP is malformed") from error
    if (link.get("ifname") != EXTERNAL_VXLAN_INTERFACE or
            info.get("info_kind") != "vxlan" or
            str(data.get("id")) != str(EXTERNAL_VNI) or local != REMOTE_VTEP or
            int(data.get("port", -1)) != VXLAN_DSTPORT or
            link.get("master") != EXTERNAL_BRIDGE or "UP" not in link.get("flags", [])):
        raise fixture.IsolationError("external VXLAN interface/VNI identity differs")
    return {
        "interface": EXTERNAL_VXLAN_INTERFACE,
        "bridge": EXTERNAL_BRIDGE,
        "vni": EXTERNAL_VNI,
        "localVTEP": local,
        "destinationPort": VXLAN_DSTPORT,
    }


def classify_external_vxlan_fdb(rows, mac, vtep):
    if not isinstance(rows, list) or not isinstance(mac, str) or not isinstance(vtep, str):
        raise fixture.IsolationError("external VXLAN FDB observation is malformed")
    expected_mac = mac.lower()
    try:
        expected_vtep = str(ipaddress.ip_address(vtep))
    except ValueError as error:
        raise fixture.IsolationError("expected remote VTEP is malformed") from error
    matching = []
    bridge_rows = []
    for row in rows:
        if not isinstance(row, dict):
            raise fixture.IsolationError("external VXLAN FDB row is malformed")
        row_mac = row.get("mac")
        if not isinstance(row_mac, str):
            raise fixture.IsolationError("external VXLAN FDB MAC is malformed")
        if row_mac.lower() != expected_mac:
            continue
        if "master" in row:
            if row["master"] != EXTERNAL_BRIDGE:
                raise fixture.IsolationError("external VXLAN FDB has an unexpected bridge master")
            bridge_rows.append(row)
            continue
        if row.get("dev", EXTERNAL_VXLAN_INTERFACE) != EXTERNAL_VXLAN_INTERFACE:
            raise fixture.IsolationError("external VXLAN FDB row names an unexpected device")
        matching.append(row)
    if len(matching) > 1:
        raise fixture.IsolationError(f"external VXLAN has conflicting FDB entries for {expected_mac}")
    if not matching:
        return {"status": "MISSING", "entry": None, "bridgeMasterRows": bridge_rows}

    entry = matching[0]
    flags = entry.get("flags", [])
    if not isinstance(flags, list) or any(not isinstance(flag, str) for flag in flags):
        raise fixture.IsolationError("external VXLAN FDB flags are malformed")
    flag_set = set(flags)
    if len(flag_set) != len(flags) or entry.get("dst") != expected_vtep:
        raise fixture.IsolationError(f"external VXLAN has a conflicting FDB entry for {expected_mac}")
    state = entry.get("state")
    if flag_set == {"self", "extern_learn"} and state in (None, ""):
        ownership = "observed-externally-learned"
    elif flag_set == {"self"} and state == "permanent":
        ownership = "observed-existing"
    else:
        raise fixture.IsolationError(f"external VXLAN has malformed or ambiguous FDB state for {expected_mac}")
    return {
        "status": ownership,
        "entry": entry,
        "bridgeMasterRows": bridge_rows,
    }


def classify_external_workload_bridge_fdb(rows, mac):
    if not isinstance(rows, list) or not isinstance(mac, str):
        raise fixture.IsolationError("external workload bridge FDB observation is malformed")
    matches = []
    for row in rows:
        if not isinstance(row, dict) or not isinstance(row.get("mac"), str):
            raise fixture.IsolationError("external workload bridge FDB row is malformed")
        if row["mac"].lower() == mac.lower():
            matches.append(row)
    if len(matches) > 1:
        raise fixture.IsolationError("external bridge has conflicting workload FDB entries")
    if not matches:
        return {"status": "MISSING", "entry": None}
    entry = matches[0]
    interface = entry.get("ifname", entry.get("dev"))
    flags = entry.get("flags", [])
    state = entry.get("state", "")
    if (interface != "frr-host0" or entry.get("master") != EXTERNAL_BRIDGE or
            not isinstance(flags, list) or any(not isinstance(flag, str) for flag in flags) or
            state not in ("", "permanent") or
            (state == "" and flags != []) or
            (state == "permanent" and set(flags) not in (set(), {"master"}))):
        raise fixture.IsolationError("external bridge has a conflicting workload FDB entry")
    return {"status": "OBSERVED_EXISTING", "entry": entry}


def inventory_evidence_comparisons(core, publication, journals):
    inventory = (core.get("spec") or {}).get("workerInventory") or {}
    data = (publication or {}).get("data") or {}
    text = data.get("inventory.json")
    digest = hashlib.sha256(text.encode()).hexdigest() if isinstance(text, str) else None
    result = {"conditions": operator_ownership.inventory_condition_comparisons(core, publication),
              "publication": {"expectedRevision": inventory.get("revision"),
                              "observedRevision": data.get("revision"),
                              "observedDigest": data.get("sha256"), "computedDigest": digest},
              "workers": {}}
    workers = {w.get("nodeName"): w for w in inventory.get("workers", [])}
    worker_hashes = {}
    if isinstance(text, str) and text.count('"workers":[') == 1:
        try:
            offset = text.index('"workers":[') + len('"workers":[')
            published_workers = json.loads(text)["workers"]
            for worker in published_workers:
                decoded, end = json.JSONDecoder().raw_decode(text, offset)
                if decoded != worker:
                    break
                worker_hashes[worker["nodeName"]] = hashlib.sha256(text[offset:end].encode()).hexdigest()
                offset = end + 1
        except (ValueError, KeyError, TypeError):
            pass
    for node, entry in journals.items():
        comparison = {"available": entry.get("returncode") == 0}
        try:
            record = json.loads(entry.get("stdout", ""))["Record"]
            state = record["State"]
            if state is None:
                comparison.update(state="uncommitted", sequence=record.get("Sequence"),
                                  pending=record.get("Pending"), domain=record.get("Domain"))
                result["workers"][node] = comparison
                continue
            if not isinstance(state, dict):
                raise ValueError("invalid journal state")
            comparison.update(expectedRevision=inventory.get("revision"),
                              observedRevision=state.get("revision"), expectedDigest=digest,
                              observedDigest=state.get("digest"),
                              expectedNodeUID=workers.get(node, {}).get("nodeUID"),
                              observedNodeUID=state.get("nodeUID"),
                              expectedWorkerDigest=worker_hashes.get(node),
                              observedWorkerDigest=state.get("workerDigest"),
                              pending=record.get("Pending"), domain=record.get("Domain"))
        except (ValueError, KeyError, TypeError):
            comparison["parseError"] = "missing or invalid journal"
        result["workers"][node] = comparison
    return result


def parse_ovs_port_counters(output, port):
    lines = output.splitlines()
    for index, line in enumerate(lines):
        match = re.match(r"\s*port\s+(\d+)(?:\([^)]*\))?:\s+rx pkts=(\d+)", line)
        if not match or int(match.group(1)) != port:
            continue
        if index + 1 >= len(lines):
            break
        tx = re.search(r"\btx pkts=(\d+)", lines[index + 1])
        if not tx:
            break
        return {"rxPackets": int(match.group(2)), "txPackets": int(tx.group(1))}
    raise fixture.IsolationError(f"OVS port {port} counters are unavailable")


def parse_linux_link_counters(output, expected_interface):
    try:
        links = json.loads(output)
    except (TypeError, json.JSONDecodeError) as error:
        raise fixture.IsolationError(f"Linux counters for {expected_interface} are invalid JSON") from error
    if (not isinstance(links, list) or len(links) != 1 or
            not isinstance(links[0], dict) or links[0].get("ifname") != expected_interface):
        raise fixture.IsolationError(f"Linux counters for {expected_interface} have an ambiguous link identity")
    link = links[0]
    if (isinstance(link.get("ifindex"), bool) or not isinstance(link.get("ifindex"), int) or
            link["ifindex"] <= 0):
        raise fixture.IsolationError(f"Linux counters for {expected_interface} have an invalid ifindex")
    stats = link.get("stats64") or link.get("stats")
    if not isinstance(stats, dict):
        raise fixture.IsolationError(f"Linux counters for {expected_interface} are unavailable")
    result = {"ifname": expected_interface, "ifindex": link["ifindex"]}
    for direction in ("rx", "tx"):
        values = stats.get(direction)
        if not isinstance(values, dict):
            raise fixture.IsolationError(f"Linux {direction} counters for {expected_interface} are unavailable")
        counters = {}
        for name in ("packets", "bytes", "errors", "dropped"):
            value = values.get(name)
            if isinstance(value, bool) or not isinstance(value, int) or value < 0:
                raise fixture.IsolationError(
                    f"Linux {direction} {name} counter for {expected_interface} is invalid"
                )
            counters[name] = value
        result[direction] = counters
    return result


def parse_inventory_ping_result(output, returncode):
    match = re.search(
        r"(\d+) packets transmitted,\s*(\d+) (?:packets )?received,\s*(\d+)% packet loss",
        output,
    )
    transmitted, received, loss = (
        map(int, match.groups()) if match else (None, None, None)
    )
    return {
        "exitStatus": returncode,
        "transmitted": transmitted,
        "received": received,
        "lossPercent": loss,
        "status": "PASS"
        if returncode == 0 and transmitted == 3 and received == 3 and loss == 0
        else "FAIL",
    }


def verify_inventory_forwarding_captures(directory, endpoints):
    import external_frr

    evidence = {}
    frr_capture = directory / "inventory-frr-underlay.pcap"
    if frr_capture.is_symlink() or not frr_capture.is_file():
        raise fixture.IsolationError("external FRR VXLAN capture is unavailable")
    for endpoint in endpoints:
        node = endpoint["node"]
        capture = directory / endpoint["capture"]
        if (Path(endpoint["capture"]).name != endpoint["capture"] or
                capture.is_symlink() or not capture.is_file()):
            raise fixture.IsolationError(f"{node}: worker VXLAN capture is unavailable")
        common = {
            "vni": 1000,
            "local_vtep": endpoint["vtep"],
            "remote_vtep": REMOTE_VTEP,
            "local_mac": endpoint["mac"],
            "remote_mac": REMOTE_MAC,
            "local_ip": endpoint["ip"],
            "remote_ip": REMOTE_IP,
        }
        captures = {
            "worker": external_frr.decode_vxlan(capture, **common),
            "frr": external_frr.decode_vxlan(frr_capture, **common),
        }
        packets = captures["frr"]["packets"]
        identifiers = endpoint["icmpIdentifiers"]
        worker_identifier = identifiers["workerToExternal"]
        external_identifier = identifiers["externalToWorker"]
        directions = {
            "workerToExternal": {
                "requests": [
                    p for p in packets
                    if p["outerSource"] == endpoint["vtep"] and p["type"] == 8
                    and p["id"] == worker_identifier
                    and p["sourceIp"] == endpoint["ip"] and p["destinationIp"] == REMOTE_IP
                    and p["sourceMac"] == endpoint["mac"] and p["destinationMac"] == REMOTE_MAC
                ],
                "replies": [
                    p for p in packets
                    if p["outerSource"] == REMOTE_VTEP and p["type"] == 0
                    and p["id"] == worker_identifier
                    and p["sourceIp"] == REMOTE_IP and p["destinationIp"] == endpoint["ip"]
                    and p["sourceMac"] == REMOTE_MAC and p["destinationMac"] == endpoint["mac"]
                ],
            },
            "externalToWorker": {
                "requests": [
                    p for p in packets
                    if p["outerSource"] == REMOTE_VTEP and p["type"] == 8
                    and p["id"] == external_identifier
                    and p["sourceIp"] == REMOTE_IP and p["destinationIp"] == endpoint["ip"]
                    and p["sourceMac"] == REMOTE_MAC and p["destinationMac"] == endpoint["mac"]
                ],
                "replies": [
                    p for p in packets
                    if p["outerSource"] == endpoint["vtep"] and p["type"] == 0
                    and p["id"] == external_identifier
                    and p["sourceIp"] == endpoint["ip"] and p["destinationIp"] == REMOTE_IP
                    and p["sourceMac"] == endpoint["mac"] and p["destinationMac"] == REMOTE_MAC
                ],
            },
        }
        direction_expectations = (
            ("workerToExternal", endpoint["vtep"], REMOTE_VTEP, endpoint["mac"],
             REMOTE_MAC, endpoint["ip"], REMOTE_IP, 8, worker_identifier),
            ("workerToExternalReply", REMOTE_VTEP, endpoint["vtep"], REMOTE_MAC,
             endpoint["mac"], REMOTE_IP, endpoint["ip"], 0, worker_identifier),
            ("externalToWorker", REMOTE_VTEP, endpoint["vtep"], REMOTE_MAC,
             endpoint["mac"], REMOTE_IP, endpoint["ip"], 8, external_identifier),
            ("externalToWorkerReply", endpoint["vtep"], REMOTE_VTEP, endpoint["mac"],
             REMOTE_MAC, endpoint["ip"], REMOTE_IP, 0, external_identifier),
        )
        capture_identities = {}
        for role, capture_result in captures.items():
            observed = set()
            capture_packets = capture_result["packets"]
            for (
                label, outer_source, outer_destination, source_mac, destination_mac,
                source_ip, destination_ip, kind, identifier,
            ) in direction_expectations:
                matching = [
                    packet for packet in capture_packets
                    if (packet["outerSource"], packet["outerDestination"],
                        packet["sourceMac"], packet["destinationMac"],
                        packet["sourceIp"], packet["destinationIp"], packet["type"],
                        packet["id"]) ==
                       (outer_source, outer_destination, source_mac, destination_mac,
                        source_ip, destination_ip, kind, identifier)
                ]
                identities = {(packet["id"], packet["seq"]) for packet in matching}
                expected = {(identifier, sequence) for sequence in (1, 2, 3)}
                if len(matching) != 3 or identities != expected:
                    raise fixture.IsolationError(
                        f"{node}: {role} capture has incomplete {label} VXLAN packet identities"
                    )
                observed.update((label, *identity) for identity in identities)
            capture_identities[role] = observed
        if capture_identities["worker"] != capture_identities["frr"]:
            raise fixture.IsolationError(
                f"{node}: worker and FRR VXLAN capture packet identities differ"
            )
        evidence[node] = {
            "endpoint": {key: endpoint[key] for key in ("ip", "mac", "vtep")},
            "captures": captures,
            "directions": {
                name: {"requestCount": len(value["requests"]), "replyCount": len(value["replies"])}
                for name, value in directions.items()
            },
        }
    return {"status": "PASS", "vni": 1000, "remoteVtep": REMOTE_VTEP, "workers": evidence}


def verify_external_import_evidence(status, export, journal, fdb, neighbors, identity, installation, vtep):
    """Check one worker's attested speaker and acknowledged remote Type-2 state."""
    speaker = export.get("SpeakerProcess") or {}
    standalone = speaker.get("standalone") or {}
    if (export.get("Error") or not speaker.get("PID") or not speaker.get("StartTicks") or
            standalone.get("nodeUID") != identity["nodeUID"] or
            standalone.get("installationUID") != installation or standalone.get("routerID") != vtep):
        raise fixture.IsolationError("worker speaker attestation is not established")
    if status.get("Error") or status.get("RecoveryState") != "RUNNING" or not (
            status.get("Snapshot") or {}).get("Authoritative"):
        raise fixture.IsolationError("worker import is not authoritative and running")
    record = journal["Record"]
    state = record["State"]
    domain = record["Domain"]
    if domain.get("InstallationUID") != installation or domain.get("NodeUID") != identity["nodeUID"]:
        raise fixture.IsolationError("remote import journal recovery domain differs")
    receipts = status.get("Receipts") or []
    durable = list((state.get("Receipts") or {}).values())
    if (record.get("Pending") is not None or state.get("Quarantine") or
            len(receipts) != 1 or durable != receipts):
        raise fixture.IsolationError("remote import does not have one matching durable receipt")
    receipt = receipts[0]
    endpoint = receipt["Endpoint"]
    if receipt.get("Network") != {"NetworkID": "tenant-a", "LogicalSwitch": "tenant-a",
                                  "VNI": 1000, "BridgeDevice": "evpnbr1000",
                                  "VXLANDevice": "evpnvx1000"}:
        raise fixture.IsolationError("remote import network scope differs")
    if (endpoint.get("MAC"), endpoint.get("IP"), endpoint.get("RemoteVTEP"),
            endpoint.get("ValidationState")) != (
            "02:00:00:00:00:30", "10.244.240.30", "198.18.30.4", "validated"):
        raise fixture.IsolationError("remote imported endpoint differs")
    if not endpoint.get("Routes") or any(
            route["Route"].get("Classification") != "remote" or
            route["Route"]["Source"].get("NeighborIP") != "198.18.30.4"
            for route in endpoint["Routes"]):
        raise fixture.IsolationError("remote import route provenance differs")
    if any(not receipt[kind].get("ACK") or receipt[kind].get("Status") != "confirmed"
           for kind in ("FDB", "Neighbor")):
        raise fixture.IsolationError("remote import kernel receipts are unconfirmed")
    if not any(row.get("mac", "").lower() == endpoint["MAC"] and
               row.get("dst") == endpoint["RemoteVTEP"] and row.get("state") == "permanent" and
               {"self", "extern_learn"} <= set(row.get("flags", [])) for row in fdb):
        raise fixture.IsolationError("owned remote FDB entry is absent")
    if not any(row.get("dst") == endpoint["IP"] and row.get("lladdr", "").lower() == endpoint["MAC"]
               and "NOARP" in row.get("state", []) for row in neighbors):
        raise fixture.IsolationError("owned remote neighbor entry is absent")


def verify_local_export_evidence(status, journal, frr_rib, endpoint, identity, installation):
    result = status.get("Result") or {}
    speaker = (status.get("SpeakerProcess") or {}).get("standalone") or {}
    if (status.get("Error") or result.get("RecoveryState") != "RUNNING" or
            result.get("OwnershipUnverified") is not False or
            speaker.get("nodeUID") != identity["nodeUID"] or
            speaker.get("installationUID") != installation or speaker.get("routerID") != endpoint["vtep"]):
        raise fixture.IsolationError("exporter is not running with the expected speaker ownership")
    applied = result.get("Applied") or []
    if len(applied) != 1:
        raise fixture.IsolationError("expected exactly one owned endpoint advertisement")
    owned = applied[0]
    route = owned["Endpoint"]
    if (route.get("NetworkID") != "tenant-a" or
            route.get("EndpointID") != "tenant-a|secondary|" + endpoint["podUID"] or
            route.get("MACAddress") != endpoint["mac"] or route.get("IPAddresses") != [endpoint["ip"]] or
            route.get("VNI") != 1000 or route.get("LocalVTEP") != endpoint["vtep"] or
            route.get("RouteDistinguisher") != endpoint["vtep"] + ":1000" or
            route.get("NodeName") != endpoint["node"] or route.get("Chassis") != identity["chassis"] or
            route.get("RouteTargets") != ["64512:1000"] or route.get("Advertise") is not True or
            not route.get("Incarnation") or not owned.get("PathUUID")):
        raise fixture.IsolationError("export route does not match the authorized endpoint")
    record = journal["Record"]
    domain = record["Domain"]
    receipts = record["State"].get("Receipts") or []
    if (domain.get("InstallationUID") != installation or domain.get("NodeUID") != identity["nodeUID"] or
            record.get("Pending") is not None or len(receipts) != 1 or receipts[0].get("Owned") != owned):
        raise fixture.IsolationError("export route has no matching durable owned UUID receipt")
    rd = endpoint["vtep"] + ":1000"
    routes = frr_rib.get(rd)
    if not isinstance(routes, dict):
        raise fixture.IsolationError("FRR has no expected worker route distinguisher")
    matches = []
    for item in routes.values():
        if not isinstance(item, dict):
            continue
        for group in item.get("paths", []):
            for path in group:
                if (path.get("routeType") == 2 and path.get("mac") == endpoint["mac"] and
                        path.get("ip") == endpoint["ip"] and path.get("ethTag") == 0 and
                        path.get("peerId") == endpoint["vtep"] and
                        any(hop.get("ip") == endpoint["vtep"] for hop in path.get("nexthops", [])) and
                        "RT:64512:1000" in (path.get("extendedCommunity") or {}).get("string", "") and
                        "ET:8" in (path.get("extendedCommunity") or {}).get("string", "")):
                    matches.append(path)
    if len(matches) != 1:
        raise fixture.IsolationError("FRR lacks one matching worker Type-2 route")
    return owned["PathUUID"]


def verify_local_rib(paths, endpoint, path_uuid):
    matches = []
    for path in paths or []:
        nlri = path.get("NLRI") or {}
        if (nlri.get("RD"), nlri.get("MAC"), nlri.get("IP")) != (
                endpoint["vtep"] + ":1000", endpoint["mac"], endpoint["ip"]):
            continue
        attrs = path.get("PathAttributes") or {}
        communities = attrs.get("ExtendedCommunities") or []
        if (not path.get("Local") or
                (path.get("PathUUID") and path["PathUUID"] != path_uuid) or
                nlri.get("EthernetTag") != 0 or nlri.get("Labels") != [1000] or
                nlri.get("ESI") != "00000000000000000000" or attrs.get("NextHop") != endpoint["vtep"] or
                attrs.get("Origin") != "igp" or
                not any(c.get("Kind") == "route-target" and c.get("Value") == "64512:1000"
                        for c in communities) or
                not any(c.get("Kind") == "encapsulation" and c.get("Value") == "vxlan"
                        for c in communities)):
            raise fixture.IsolationError("local Type-2 RIB route attributes or ownership differ")
        matches.append(path)
    if len(matches) != 1:
        raise fixture.IsolationError("local GoBGP RIB lacks exactly one expected Type-2 path")
    return matches[0]


def export_recovery_namespace_identity(readlink_value, stat_value):
    namespace = re.fullmatch(r"net:\[(\d+)\]", readlink_value)
    device_inode = re.fullmatch(r"(\d+):(\d+)", stat_value)
    if not namespace or not device_inode or namespace.group(1) != device_inode.group(2):
        raise fixture.IsolationError("network namespace readlink and stat identities differ")
    return f"NS({device_inode.group(1)}:{device_inode.group(2)})"


def inventory_configuration(inputs):
    """Render the existing example from supplied observations; never provision resources."""
    def require(condition, message):
        if not condition:
            raise fixture.IsolationError(message)

    def uid(value, label):
        try:
            parsed = uuid.UUID(value)
            require(parsed.int != 0 and str(parsed) == value, f"invalid {label} UUID")
        except (ValueError, TypeError, AttributeError) as error:
            raise fixture.IsolationError(f"invalid {label} UUID") from error
        return value

    def token(value, label, pattern):
        require(isinstance(value, str) and re.fullmatch(pattern, value) is not None,
                f"invalid {label}")
        return value

    def address(value, label):
        try:
            addr = ipaddress.IPv4Address(value)
        except (ValueError, TypeError) as error:
            raise fixture.IsolationError(f"invalid {label} IPv4 address") from error
        require(str(addr) == value and not (addr.is_multicast or addr.is_loopback or
                addr.is_unspecified or addr.is_link_local or addr.is_reserved),
                f"invalid {label} IPv4 address")
        return value

    cluster = uid(inputs["clusterUID"], "cluster")
    installation = uid(inputs["installationUID"], "installation")
    image = token(inputs["agentImage"], "agent image", r"[A-Za-z0-9][A-Za-z0-9._:/@-]*")
    client_image = token(inputs["clientImage"], "client image", r"[A-Za-z0-9][A-Za-z0-9._:/@-]*")
    nb = token(inputs["ovnNBHost"], "OVN NB host", r"[A-Za-z0-9][A-Za-z0-9.-]*")
    sb = token(inputs["ovnSBHost"], "OVN SB host", r"[A-Za-z0-9][A-Za-z0-9.-]*")
    remote = address(inputs["remoteVTEP"], "remote VTEP")
    workers = inputs["workers"]
    require(isinstance(workers, list) and len(workers) == 2, "exactly two workers are required")
    identities = []
    for w in workers:
        metadata = w["node"]["metadata"]
        name = token(metadata["name"], "Node name", r"[a-z0-9](?:[a-z0-9.-]*[a-z0-9])?")
        node_uid = uid(metadata["uid"], "Node")
        chassis = token(w["chassis"], "chassis", r"[A-Za-z0-9][A-Za-z0-9_.-]*")
        vtep = address(w["vtep"], "worker VTEP")
        uid(w["bootID"], "boot")
        uid(w["serviceUID"], "speaker service")
        token(w["hostNetns"], "host network namespace", r"net:\[[0-9]+\]")
        token(w["speakerCgroup"], "speaker cgroup", r"/[A-Za-z0-9_.@/-]+")
        for field in ("ctrSHA256", "cniSHA256", "multusSHA256", "runtimeSHA256",
                      "wrapperExecutableSHA256", "wrapperScriptSHA256", "gobgpdSHA256"):
            token(w[field], field, r"[0-9a-f]{64}")
        identities.append((name, node_uid, chassis, vtep, w["serviceUID"]))
    for column, label in enumerate(("Node name", "Node UID", "chassis", "VTEP", "speaker service")):
        require(len({x[column] for x in identities}) == 2, f"duplicate {label}")
    require(remote not in {x[3] for x in identities}, "remote VTEP must differ from workers")
    require(cluster != installation, "cluster and installation identities must differ")

    def template(name, values):
        source = replace_addresses((EXAMPLE / name).read_text())
        for key, value in values.items():
            source = source.replace("<" + key + ">", value)
        require(not re.search(r"<[A-Z_]+>", source), f"unresolved placeholder in {name}")
        return source

    def replace_addresses(source):
        substitutions = {"192.0.2.11": identities[0][3], "192.0.2.12": identities[1][3],
                         "192.0.2.30": remote}
        return re.sub(r"192\.0\.2\.(?:11|12|30)\b", lambda m: substitutions[m.group()], source)

    shared = {"NODE_A": identities[0][0], "NODE_B": identities[1][0],
              "NODE_A_UID": identities[0][1], "NODE_B_UID": identities[1][1],
              "CHASSIS_A": identities[0][2], "CHASSIS_B": identities[1][2],
              "CLUSTER_UID": cluster, "NODE_AGENT_IMAGE": image, "CLIENT_IMAGE": client_image,
              "OVN_NB_HOST": nb, "OVN_SB_HOST": sb}
    manifests = {name: template(name, shared) for name in
                 ("evpncore.yaml", "rbac.yaml", "reservations.yaml", "workloads.yaml", "frr.conf")}
    require(len(re.findall(r"^  enabled: true$", manifests["evpncore.yaml"], re.MULTILINE)) == 1,
            "unexpected EvpnCore enabled field")
    manifests["evpncore-disabled.yaml"] = re.sub(
        r"^  enabled: true$", "  enabled: false", manifests["evpncore.yaml"], count=1, flags=re.MULTILINE)
    rendered = {}
    for w, (name, node_uid, chassis, vtep, service_uid) in zip(workers, identities):
        values = {"NODE": name, "NODE_UID": node_uid, "CHASSIS": chassis, "VTEP": vtep,
                  "INSTALLATION_UID": installation, "BOOT_ID": w["bootID"],
                  "HOST_NETNS": w["hostNetns"], "SERVICE_UID": service_uid,
                  "SPEAKER_CGROUP": w["speakerCgroup"], "GOBGPD_SHA256": w["gobgpdSHA256"],
                  "CTR_SHA256": w["ctrSHA256"], "CNI_SHA256": w["cniSHA256"],
                  "MULTUS_SHA256": w["multusSHA256"], "RUNTIME_SHA256": w["runtimeSHA256"]}
        speaker = template("gobgpd.yaml", values)
        values["GOBGP_CONFIG_SHA256"] = hashlib.sha256(speaker.encode()).hexdigest()
        configs = {f: json.loads(template(f, values))
                   for f in ("export.json", "import.json", "cni.json", "provider.json")}
        configs["cni.json"]["Admission"].update({
            "WrapperExecutableSHA256": w["wrapperExecutableSHA256"],
            "WrapperScriptSHA256": w["wrapperScriptSHA256"],
            "WrapperScriptPath": "/opt/cni/bin/evpn-cni"})
        rendered[name] = {"nodeUID": node_uid, "chassis": chassis, "vtep": vtep,
                          "gobgpd.yaml": speaker, **configs}
    commands = {
        "ovn": [
            ["ovn-nbctl", "ls-add", "tenant-a"],
            ["ovn-nbctl", "set", "Logical_Switch", "tenant-a",
             "other_config:dynamic-routing-vni=1000",
             "other_config:dynamic-routing-bridge-ifname=evpnbr1000",
             "other_config:dynamic-routing-vxlan-ifname=evpnvx1000",
             "other_config:dynamic-routing-advertise-ifname=evpnadv1000",
             "other_config:dynamic-routing-redistribute=fdb,ip"]],
        "workers": {}, "externalFRR": [
            ["ip", "link", "add", "br1000", "type", "bridge"],
            ["ip", "addr", "add", "10.244.240.1/24", "dev", "br1000"],
            ["ip", "link", "set", "br1000", "up"],
            ["ip", "link", "add", "vxlan1000", "type", "vxlan", "id", "1000",
             "local", remote, "dstport", "4789", "nolearning"],
            ["ip", "link", "set", "vxlan1000", "master", "br1000"],
            ["ip", "link", "set", "vxlan1000", "up"],
            ["ip", "netns", "add", "remote-workload"],
            ["ip", "link", "add", "frr-host0", "type", "veth", "peer", "name", "frr-work0"],
            ["ip", "link", "set", "frr-host0", "master", "br1000"],
            ["ip", "link", "set", "frr-host0", "up"],
            ["ip", "link", "set", "frr-work0", "netns", "remote-workload"],
            ["ip", "-n", "remote-workload", "link", "set", "lo", "up"],
            ["ip", "-n", "remote-workload", "link", "set", "frr-work0", "address", "02:00:00:00:00:30"],
            ["ip", "-n", "remote-workload", "addr", "add", "10.244.240.30/24", "dev", "frr-work0"],
            ["ip", "-n", "remote-workload", "link", "set", "frr-work0", "up"]]}
    for name, _, _, vtep, _ in identities:
        commands["workers"][name] = [
            ["ip", "link", "add", "evpnbr1000", "type", "bridge"],
            ["ip", "link", "set", "evpnbr1000", "up"],
            ["ip", "link", "add", "evpnvx1000", "type", "vxlan", "id", "1000",
             "local", vtep, "dstport", "60100", "nolearning"],
            ["ip", "link", "set", "evpnvx1000", "master", "evpnbr1000"],
            ["ip", "link", "set", "evpnvx1000", "up"],
            ["ip", "link", "add", "evpnadv1000", "type", "dummy"],
            ["ip", "link", "set", "evpnadv1000", "master", "evpnbr1000"],
            ["ip", "link", "set", "evpnadv1000", "up"],
            ["ovs-vsctl", "set", "Open_vSwitch", ".", "external_ids:ovn-evpn-local-ip=" + vtep,
             "external_ids:ovn-evpn-vxlan-ports=4789"]]
    return {"scenario": INVENTORY_SCENARIO, "status": "CONFIGURATION_ONLY",
            "note": "Commands are unexecuted plans; ownership and all supplied identities require live verification.",
            "manifests": manifests, "workers": rendered, "datapathCommands": commands}


class OperatorSecondaryCNI(fixture.IsolatedSecondaryCNI):
    def __init__(self, ctr=None, ctr_sha256=None):
        super().__init__()
        self.ctr = Path(ctr) if ctr else None
        self.ctr_sha256 = ctr_sha256
        self.authenticated = False

    def initialize(self):
        context = self.docker("context", "inspect").stdout
        document = json.loads(context)
        if (len(document) != 1 or
                document[0].get("Endpoints", {}).get("docker", {}).get("Host") !=
                "unix:///var/run/docker.sock" or
                fixture.os.environ.get("DOCKER_HOST", "unix:///var/run/docker.sock") !=
                "unix:///var/run/docker.sock"):
            raise fixture.IsolationError("Docker endpoint must be the local default Unix socket")
        super().initialize()

    def build_extra(self, build):
        fixture.run(["go", "build", "-o", build / "operator", "./deploy/operator"],
                    timeout=900, env=dict(fixture.os.environ, CGO_ENABLED="0"))
        shutil.copyfile(fixture.ROOT / "deploy/operator/core-resources.yaml",
                        build / "core-resources.yaml")
        return ("COPY operator /usr/local/bin/evpn-core-operator\n"
                "COPY core-resources.yaml /core-resources.yaml\n")

    def agent_socket(self):
        return "/run/evpn-node-agent/agent.sock"

    def ovsdb_host_socket(self):
        return "/run/openvswitch/db.sock"

    def result_status(self):
        return ("OPERATOR_SECONDARY_CNI_AUTHENTICATED_PASS" if self.authenticated
                else "OPERATOR_SECONDARY_CNI_BASELINE_PASS")

    def result_details(self):
        if self.authenticated:
            return {"continuity": "PASS", "overallAcceptance": "BLOCKED",
                    "type2": "NOT_TESTED", "routeRecovery": "NOT_TESTED",
                    "nodeLoss": "NOT_TESTED"}
        return {"continuity": "BLOCKED", "overallAcceptance": "BLOCKED",
                "authorization": "legacy CNI mode; authenticated continuity not enabled"}

    def configure_runtime_daemonset(self, daemonset):
        # These standard paths are used only inside the newly created Kind workers.
        for node in self.workers:
            self.worker(node, "sh", "-ec",
                        "for p in /run/openvswitch /run/ovn /run/evpn-node-agent; do "
                        "if [ -e \"$p\" ]; then test -d \"$p\" && "
                        "test -z \"$(find \"$p\" -mindepth 1 -maxdepth 1 -print -quit)\"; fi; "
                        "done")
        pod = daemonset["spec"]["template"]["spec"]
        pod["containers"] = [c for c in pod["containers"] if c["name"] == "ovn"]
        for volume in pod["volumes"]:
            if volume["name"] == "ovs-run":
                volume["hostPath"]["path"] = "/run/openvswitch"
            elif volume["name"] == "ovn-run":
                volume["hostPath"]["path"] = "/run/ovn"
        return daemonset

    def deploy_secondary_ovn(self):
        super().deploy_secondary_ovn()
        self.external_before = self.external_inventory()
        try:
            self.install_operator()
            self.verify_operator()
            if self.external_inventory() != self.external_before:
                raise fixture.IsolationError("Operator changed external OVN resources on installation")
            self.verify_node_local()
            if self.ctr:
                self.provision_continuity()
        except (fixture.IsolationError, OSError, ValueError, fixture.subprocess.TimeoutExpired):
            try:
                logs = self.k("-n", "evpn-operator-system", "logs",
                              "deployment/evpn-core-operator", "--tail=500", check=False, timeout=30)
                self.record("operator-setup-diagnostics.json", {
                    "returncode": logs.returncode, "stdout": logs.stdout, "stderr": logs.stderr})
            except (fixture.IsolationError, OSError, fixture.subprocess.TimeoutExpired) as error:
                self.record("operator-setup-diagnostics.json", {"error": str(error)})
            raise

    def install_operator(self, *, create_baseline_core=True):
        source = (fixture.ROOT / "deploy/operator/manifests/operator.yaml").read_text()
        original = "evpn4ovn/operator:kind"
        if source.count(original) != 1:
            raise fixture.IsolationError("unexpected Operator image reference in manifest")
        path = self.directory / "operator-install.yaml"
        path.write_text(source.replace(original, self.image))
        self.k("apply", "-f", path)
        self.k("wait", "--for=condition=Established",
               "crd/evpncores.evpn.romanspb80.github.io", "--timeout=60s")
        patch = {"spec": {"template": {"spec": {"containers": [{
            "name": "operator", "command": ["/usr/local/bin/evpn-core-operator"]}]}}}}
        self.k("-n", "evpn-operator-system", "patch", "deployment", "evpn-core-operator",
               "--type=strategic", "-p", json.dumps(patch))
        self.k("-n", "evpn-operator-system", "rollout", "status",
               "deployment/evpn-core-operator", "--timeout=180s")
        if not create_baseline_core:
            return
        core = {
            "apiVersion": "evpn.romanspb80.github.io/v1alpha1",
            "kind": "EvpnCore",
            "metadata": {"name": "secondary", "namespace": "cni-test"},
            "spec": {"enabled": True, "dataplaneMode": "external-ovn",
                     "appNamespace": "cni-test", "ovnNamespace": "cni-test",
                     "nodeAgent": {"enabled": True, "image": self.image,
                                   "nodeSelector": {"evpn.e2e/owner": self.owner},
                                   "ovnNBEndpoint": "tcp:secondary-ovn.cni-test.svc:6641",
                                   "ovnSBEndpoint": "tcp:secondary-ovn.cni-test.svc:6642"}},
        }
        self.apply("operator-core.json", core)

    def get_json(self, *args):
        return json.loads(self.k(*args, "-o", "json").stdout)

    def discover_inventory_workers(self):
        """Bind Kubernetes Node UIDs to the independently observed local OVN chassis."""
        if len(self.workers) != 2 or len(set(self.workers)) != 2:
            raise fixture.IsolationError("inventory scenario requires exactly two distinct workers")
        discovered = {}
        for node in self.workers:
            obj = self.get_json("get", "node", node)
            metadata = obj.get("metadata", {})
            try:
                node_uid = uuid.UUID(metadata.get("uid", ""))
            except (ValueError, TypeError, AttributeError) as error:
                raise fixture.IsolationError(f"{node}: invalid Kubernetes Node UID") from error
            labels = metadata.get("labels", {})
            if (metadata.get("name") != node or metadata.get("deletionTimestamp") or
                    node_uid.int == 0 or str(node_uid) != metadata["uid"] or
                    "node-role.kubernetes.io/control-plane" in labels or
                    labels.get("evpn.e2e/owner") != self.owner or
                    not any(c.get("type") == "Ready" and c.get("status") == "True"
                            for c in obj.get("status", {}).get("conditions", []))):
                raise fixture.IsolationError(f"{node}: Kubernetes worker identity or readiness mismatch")
            chassis = self.runtime(node, "ovn", "ovs-vsctl", "--if-exists", "get",
                                   "Open_vSwitch", ".", "external_ids:system-id").stdout.strip().strip('"')
            if not re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9_.-]*", chassis):
                raise fixture.IsolationError(f"{node}: missing local OVN chassis identity")
            raw = self.k("-n", "cni-test", "exec", "deploy/secondary-ovn-central", "--",
                         "ovn-sbctl", "--bare", "--columns=_uuid", "find", "Chassis",
                         "name=" + chassis).stdout.split()
            try:
                chassis_uid = uuid.UUID(raw[0]) if len(raw) == 1 else None
            except ValueError as error:
                raise fixture.IsolationError(f"{node}: invalid SB chassis UUID") from error
            if chassis_uid is None or chassis_uid.int == 0 or str(chassis_uid) != raw[0]:
                raise fixture.IsolationError(f"{node}: expected exactly one matching SB chassis")
            discovered[node] = {"nodeUID": str(node_uid), "chassis": chassis,
                                "chassisUUID": str(chassis_uid)}
        for key in ("nodeUID", "chassis", "chassisUUID"):
            if len({item[key] for item in discovered.values()}) != len(self.workers):
                raise fixture.IsolationError(f"duplicate worker {key}")
        self.record("inventory-worker-identities.json", discovered)
        return discovered

    def verify_inventory_operator(self, namespace, core_name, expected_selector, *,
                                  expected_identities=None, expected_installation=None,
                                  expected_cluster=None):
        """Collect Operator and host fence evidence without treating Pod readiness as acceptance."""
        core = self.get_json("-n", namespace, "get", "evpncore", core_name)
        if expected_identities is not None:
            inventory = core["spec"]["workerInventory"]
            declared = inventory["workers"]
            observed = {w["nodeName"]: {"nodeUID": w["nodeUID"], "chassis": w["chassis"]}
                        for w in declared}
            expected = {name: {"nodeUID": identity["nodeUID"], "chassis": identity["chassis"]}
                        for name, identity in expected_identities.items()}
            if (len(declared) != len(expected) or observed != expected or
                    core["metadata"]["uid"] != expected_installation or
                    inventory["clusterUID"] != expected_cluster):
                raise fixture.IsolationError("published inventory scope differs from discovered identities")
        lease = self.get_json("-n", "evpn-operator-system", "get", "lease", "evpn-core-installation")
        ds = self.get_json("-n", namespace, "get", "daemonset", "evpn-node-agent")
        pods = self.get_json("-n", namespace, "get", "pods", "-l", "app=evpn-node-agent")["items"]
        nodes = [self.get_json("get", "node", node) for node in self.workers]
        publication = self.get_json("-n", namespace, "get", "configmap", "evpn-worker-inventory")
        if core["spec"]["nodeAgent"].get("type2JournalHostPath") != "/var/lib/evpn-worker":
            raise fixture.IsolationError("unexpected inventory journal host path")
        journals = {node: self.worker(node, "cat", "/var/lib/evpn-worker/inventory/journal.json").stdout
                    for node in self.workers}
        operator_ownership.verify_external_ovn(
            core, lease, ds, pods, nodes, expected_selector,
            inventory_publication=publication, inventory_journals=journals)
        agent = core["spec"]["nodeAgent"]
        template = ds["spec"]["template"]["spec"]
        if (template.get("serviceAccountName") != "evpn-provider-observer" or
                template.get("automountServiceAccountToken") is not True):
            raise fixture.IsolationError("inventory service account identity mismatch")
        args = template["containers"][0]["args"]
        for field, flag in (("exportConfigPath", "--evpn-export-config="),
                            ("remoteConfigPath", "--evpn-remote-config=")):
            path = agent.get(field)
            if not path or args.count(flag + path) != 1:
                raise fixture.IsolationError(f"inventory agent {field} not configured")
        for pod in pods:
            node = pod["spec"]["nodeName"]
            if (pod["spec"].get("serviceAccountName") != "evpn-provider-observer" or
                    pod["spec"].get("automountServiceAccountToken") is not True):
                raise fixture.IsolationError(f"inventory service account mismatch on {node}")
            containers = [c for c in pod["spec"]["containers"] if c.get("name") == "agent"]
            if len(containers) != 1 or containers[0].get("args") != args:
                raise fixture.IsolationError(f"inventory agent arguments differ on {node}")
            actual = self.k("-n", namespace, "exec", pod["metadata"]["name"], "-c",
                            "agent", "--", "printenv", "NODE_NAME").stdout.strip()
            if actual != node:
                raise fixture.IsolationError(f"inventory agent node identity differs on {node}")
        self.record("inventory-operator-ownership.json", {
            "core": core, "lease": lease, "daemonset": ds, "pods": pods, "nodes": nodes,
            "publication": publication, "journals": journals,
            "acceptance": "LOCAL_FENCES_MATCH", "runtimeHealth": "UNCONFIRMED"})
        return core, ds, pods, nodes

    def verify_operator(self):
        last_error = "operator resources not yet available"
        for _ in range(180):
            try:
                core = self.get_json("-n", "cni-test", "get", "evpncore", "secondary")
                lease = self.get_json("-n", "evpn-operator-system", "get", "lease",
                                      "evpn-core-installation")
                ds = self.get_json("-n", "cni-test", "get", "daemonset", "evpn-node-agent")
                pods = self.get_json("-n", "cni-test", "get", "pods", "-l",
                                     "app=evpn-node-agent")["items"]
                nodes = [self.get_json("get", "node", node) for node in self.workers]
                operator_ownership.verify_external_ovn(
                    core, lease, ds, pods, nodes, {"evpn.e2e/owner": self.owner},
                    require_continuity=self.authenticated)
                config = core["spec"]["nodeAgent"]
                if (config.get("exportConfigPath") or config.get("remoteConfigPath") or
                        (not self.authenticated and (config.get("cniContinuityConfigPath") or
                                                     config.get("providerAttestation")))):
                    raise fixture.IsolationError("unexpected continuity or Type-2 configuration in baseline")
                self.record("operator-ownership.json", {
                    "core": core, "lease": lease, "daemonset": ds, "pods": pods, "nodes": nodes,
                    "continuity": "ENABLED" if self.authenticated else "DISABLED", "type2": "NOT_TESTED"})
                return core, ds, pods, nodes
            except (fixture.IsolationError, ValueError, KeyError) as error:
                last_error = str(error)
                time.sleep(1)
        raise fixture.IsolationError(f"Operator ownership did not converge: {last_error}")

    def verify_node_local(self):
        core, ds, pods, nodes = self.verify_operator()
        node_uids = {n["metadata"]["name"]: n["metadata"]["uid"] for n in nodes}
        pod_by_node = {p["spec"]["nodeName"]: p for p in pods}
        expected_args = next(c["args"] for c in ds["spec"]["template"]["spec"]["containers"]
                             if c["name"] == "agent")
        identities = {}
        for node in self.workers:
            pod = pod_by_node[node]
            containers = [c for c in pod["spec"]["containers"] if c.get("name") == "agent"]
            if (len(containers) != 1 or containers[0].get("image") != self.image or
                    containers[0].get("args") != expected_args):
                raise fixture.IsolationError(f"actual agent configuration mismatch on {node}")
            actual_node = self.k("-n", "cni-test", "exec", pod["metadata"]["name"], "-c",
                                 "agent", "--", "printenv", "NODE_NAME").stdout.strip()
            if actual_node != node:
                raise fixture.IsolationError(f"agent node identity mismatch on {node}")
            host_ovs = self.worker(node, "stat", "-Lc", "%d:%i",
                                   "/run/openvswitch/db.sock").stdout.strip()
            pod_ovs = self.k("-n", "cni-test", "exec", pod["metadata"]["name"], "-c",
                             "agent", "--", "stat", "-Lc", "%d:%i",
                             "/var/run/openvswitch/db.sock").stdout.strip()
            host_socket = self.worker(node, "sh", "-ec",
                                      "test -S /run/evpn-node-agent/agent.sock && "
                                      "stat -Lc '%d:%i' /run/evpn-node-agent/agent.sock").stdout.strip()
            pod_socket = self.k("-n", "cni-test", "exec", pod["metadata"]["name"], "-c",
                                "agent", "--", "stat", "-Lc", "%d:%i",
                                "/run/evpn-node-agent/agent.sock").stdout.strip()
            if not host_ovs or host_ovs != pod_ovs or not host_socket or host_socket != pod_socket:
                raise fixture.IsolationError(f"node-local agent or OVS socket mismatch on {node}")
            identities[node] = {"nodeUID": node_uids[node], "agentPodUID": pod["metadata"]["uid"],
                                "ovsSocketDeviceInode": host_ovs,
                                "agentSocketDeviceInode": host_socket}
        self.record("operator-node-local.json", {
            "installationUID": core["metadata"]["uid"], "nodes": identities,
            "cniContinuity": "ENABLED" if self.authenticated else "DISABLED",
            "provider": "NOT_CONFIGURED", "journal": "NODE_LOCAL" if self.authenticated else "NOT_CONFIGURED"})

    def provision_continuity(self):
        core, _, _, nodes = self.verify_operator()
        installation = core["metadata"]["uid"]
        node_uids = {node["metadata"]["name"]: node["metadata"]["uid"] for node in nodes}
        if hashlib.sha256(self.ctr.read_bytes()).hexdigest() != self.ctr_sha256:
            raise fixture.IsolationError("administrator-provided ctr SHA256 mismatch")
        role = {
            "apiVersion": "v1", "kind": "List", "items": [
                {"apiVersion": "v1", "kind": "ServiceAccount",
                 "metadata": {"name": "evpn-provider-observer", "namespace": "cni-test"}},
                {"apiVersion": "rbac.authorization.k8s.io/v1", "kind": "ClusterRole",
                 "metadata": {"name": "evpn-cni-lab-node-observer"},
                 "rules": [{"apiGroups": [""], "resources": ["nodes"],
                            "resourceNames": self.workers, "verbs": ["get"]}]},
                {"apiVersion": "rbac.authorization.k8s.io/v1", "kind": "ClusterRoleBinding",
                 "metadata": {"name": "evpn-cni-lab-node-observer"},
                 "roleRef": {"apiGroup": "rbac.authorization.k8s.io", "kind": "ClusterRole",
                             "name": "evpn-cni-lab-node-observer"},
                 "subjects": [{"kind": "ServiceAccount", "name": "evpn-provider-observer",
                               "namespace": "cni-test"}]},
                {"apiVersion": "rbac.authorization.k8s.io/v1", "kind": "Role",
                 "metadata": {"name": "evpn-cni-lab-pod-observer", "namespace": "cni-test"},
                 "rules": [{"apiGroups": [""], "resources": ["pods"], "verbs": ["get"]}]},
                {"apiVersion": "rbac.authorization.k8s.io/v1", "kind": "RoleBinding",
                 "metadata": {"name": "evpn-cni-lab-pod-observer", "namespace": "cni-test"},
                 "roleRef": {"apiGroup": "rbac.authorization.k8s.io", "kind": "Role",
                             "name": "evpn-cni-lab-pod-observer"},
                 "subjects": [{"kind": "ServiceAccount", "name": "evpn-provider-observer",
                               "namespace": "cni-test"}]},
            ]}
        self.apply("continuity-rbac.json", role)
        configs = {}
        for node in self.workers:
            self.worker(node, "sh", "-ec",
                        "test ! -e /usr/local/libexec/evpn-ctr; "
                        "test ! -e /run/evpn-node-agent/cni-continuity.json; "
                        "test ! -e /run/evpn-node-agent/cni-receipts-v1; "
                        "mkdir -p /usr/local/libexec /etc/evpn/provider-attestation")
            self.docker("cp", self.ctr, f"{node}:/usr/local/libexec/evpn-ctr")
            self.worker(node, "sh", "-ec",
                        "chown root:root /usr/local/libexec/evpn-ctr; "
                        "chmod 755 /usr/local/libexec/evpn-ctr; "
                        "chmod 600 /run/containerd/containerd.sock")
            def value(*args):
                return self.worker(node, *args).stdout.strip()
            def digest(path):
                output = value("sha256sum", path).split()
                if not output or not re.fullmatch(r"[0-9a-f]{64}", output[0]):
                    raise fixture.IsolationError(f"invalid executable digest on {node}: {path}")
                return output[0]
            ctr_hash = digest("/usr/local/libexec/evpn-ctr")
            if ctr_hash != self.ctr_sha256:
                raise fixture.IsolationError(f"ctr identity mismatch on {node}")
            self.worker(node, "/usr/local/libexec/evpn-ctr", "--address",
                        "/run/containerd/containerd.sock", "version")
            runtime = value("sh", "-ec", "command -v containerd")
            config = {
                "JournalDirectory": "/run/evpn-node-agent/cni-receipts-v1", "Create": True,
                "Domain": {"InstallationUID": installation, "NodeUID": node_uids[node],
                           "BootID": value("cat", "/proc/sys/kernel/random/boot_id"),
                           "NetworkNamespace": value("readlink", "/proc/self/ns/net")},
                "NodeName": node, "Kubeconfig": "", "CRIEndpoint": "unix:///run/containerd/containerd.sock",
                "Networks": {"secondary-isolation": "ls-secondary"},
                "CTRPath": "/usr/local/libexec/evpn-ctr", "CTRSHA256": ctr_hash,
                "Admission": {"CNIExecutableSHA256": digest("/opt/cni/bin/evpn-cni.real"),
                              "MultusExecutableSHA256": digest("/opt/cni/bin/multus"),
                              "RuntimeExecutableSHA256": digest(runtime),
                              "WrapperExecutableSHA256": digest("/bin/sh"),
                              "WrapperScriptSHA256": digest("/opt/cni/bin/evpn-cni"),
                              "WrapperScriptPath": "/opt/cni/bin/evpn-cni"}}
            config_path = self.directory / f"continuity-{node}.json"
            config_path.write_text(json.dumps(config) + "\n")
            self.docker("cp", config_path, f"{node}:/run/evpn-node-agent/cni-continuity.json")
            self.worker(node, "sh", "-ec",
                        "chown root:root /run/evpn-node-agent/cni-continuity.json; "
                        "chmod 600 /run/evpn-node-agent/cni-continuity.json")
            configs[node] = config
        self.continuity_configs = configs
        self.authenticated = True
        patch = {"spec": {"nodeAgent": {"providerAttestation": True,
                  "providerCTRHostPath": "/usr/local/libexec/evpn-ctr",
                  "cniContinuityConfigPath": "/run/evpn-node-agent/cni-continuity.json"}}}
        self.k("-n", "cni-test", "patch", "evpncore", "secondary", "--type=merge", "-p", json.dumps(patch))
        self.verify_operator()
        self.verify_node_local()
        startup = {node: self.read_journal(node) for node in self.workers}
        if len({startup[node]["Record"]["Domain"]["ConfigDigest"] for node in self.workers}) != len(self.workers):
            raise fixture.IsolationError("node journal configuration digests are not distinct")
        self.record("continuity-startup.json", startup)

    def external_inventory(self):
        inventory = {}
        for kind, name in (("deployment", "secondary-ovn-central"),
                           ("service", "secondary-ovn"),
                           ("daemonset", "secondary-ovn-worker")):
            obj = self.get_json("-n", "cni-test", "get", kind, name)
            metadata = obj["metadata"]
            if metadata.get("labels", {}).get("evpn.e2e/owner") != self.owner:
                raise fixture.IsolationError(f"external {kind}/{name} ownership changed")
            inventory[name] = {"uid": metadata["uid"], "spec": obj["spec"]}
        return inventory

    def capture_workload_failure(self):
        super().capture_workload_failure()
        evidence = {}
        for name, args in (
                ("operator", ("-n", "evpn-operator-system", "logs",
                              "deployment/evpn-core-operator", "--tail=500")),
                ("agents", ("-n", "cni-test", "get", "pods", "-l",
                            "app=evpn-node-agent", "-o", "yaml")),
                ("core", ("-n", "cni-test", "get", "evpncore", "secondary", "-o", "yaml"))):
            try:
                result = self.k(*args, check=False, timeout=30)
                evidence[name] = {"returncode": result.returncode, "stdout": result.stdout,
                                  "stderr": result.stderr}
            except (OSError, fixture.subprocess.TimeoutExpired) as error:
                evidence[name] = {"error": str(error)}
        self.record("operator-workload-diagnostics.json", evidence)

    def cni_journal_path(self):
        return "/run/evpn-node-agent/cni-receipts-v1/journal.json"

    def read_journal(self, node):
        raw = self.worker(node, "cat", self.cni_journal_path()).stdout
        envelope = json.loads(raw)
        prefix = '{"Record":'
        if not raw.startswith(prefix):
            raise fixture.IsolationError(f"{node}: malformed journal envelope")
        _, end = json.JSONDecoder().raw_decode(raw, len(prefix))
        digest = hashlib.sha256(raw[len(prefix):end].encode()).hexdigest()
        if digest != envelope.get("SHA256"):
            raise fixture.IsolationError(f"{node}: journal checksum mismatch")
        record = envelope["Record"]
        expected = self.continuity_configs[node]["Domain"]
        if (any(record["Domain"][key] != expected[key] for key in
                ("InstallationUID", "NodeUID", "BootID", "NetworkNamespace")) or
                record["Pending"] is not None):
            raise fixture.IsolationError(f"{node}: wrong journal domain or pending intent")
        if not re.fullmatch(r"[0-9a-f]{64}", record["Domain"]["ConfigDigest"]):
            raise fixture.IsolationError(f"{node}: invalid journal configuration digest")
        return envelope

    def journal(self, node, uid, state):
        envelope = self.read_journal(node)
        record = envelope["Record"]
        endpoints = list(record["State"]["Endpoints"].values())
        matches = [item for item in endpoints if item["Identity"]["workload_uid"] == uid]
        if len(matches) != 1 or matches[0]["State"] != state:
            raise fixture.IsolationError(f"{node}: expected {state} receipt for {uid}")
        ack = matches[0]
        if state == "active":
            admission, receipt = ack.get("Admission") or {}, ack["Receipt"]
            if (admission.get("PodUID") != uid or
                    admission.get("ContainerID") != ack["Runtime"]["container_id"] or
                    not admission.get("NetNSDevice") or not admission.get("NetNSInode") or
                    admission["NetNSDevice"] != receipt["NetNSDevice"] or
                    admission["NetNSInode"] != receipt["NetNSInode"] or
                    any(not receipt.get(key) or receipt[key] == "unobserved"
                        for key in ("SwitchUUID", "LSPUUID", "InterfaceUUID", "PortUUID", "BridgeUUID")) or
                    not ack.get("VerifiedAt") or ack["VerifiedAt"] < ack["CreatedAt"]):
                raise fixture.IsolationError(f"{node}: incomplete authenticated endpoint receipt")
        return envelope, ack

    def wait_journal(self, node, uid, state):
        last = ""
        for _ in range(90):
            try:
                return self.journal(node, uid, state)
            except (fixture.IsolationError, KeyError, ValueError) as error:
                last = str(error)
                time.sleep(1)
        raise fixture.IsolationError(f"journal did not reach {state} on {node}: {last}")

    def endpoints_before_delete(self, pods):
        if not self.authenticated:
            return
        _, _, agents, _ = self.verify_operator()
        agents = {pod["spec"]["nodeName"]: pod for pod in agents}
        before = {}
        for suffix, node in zip(("a", "b"), self.workers):
            envelope, ack = self.wait_journal(node, pods[suffix]["metadata"]["uid"], "active")
            lock = self.worker(node, "stat", "-c", "%d:%i:%u:%g:%a:%h",
                               "/run/evpn-node-agent/cni-receipts-v1/owner.lock").stdout.strip()
            if not lock:
                raise fixture.IsolationError(f"{node}: missing journal lock identity")
            before[node] = {"journal": envelope, "ack": ack, "lock": lock}
        self.record("continuity-before-restart.json", before)
        a, b = self.workers
        self.k("-n", "cni-test", "delete", "pod", agents[a]["metadata"]["name"],
               "--wait=true", "--timeout=120s")
        call = self.cni_attachment_ids["b"]["wrapperCallDirectory"]
        self.worker(b, "sh", "-ec",
                    "while IFS= read -r line; do export \"$line\"; done < \"$1/env\"; "
                    "export CNI_COMMAND=CHECK; "
                    "exec /opt/cni/bin/evpn-cni.real < \"$1/config\"", "sh", call)
        _, _, replacement, _ = self.verify_operator()
        replacement = {pod["spec"]["nodeName"]: pod for pod in replacement}
        if (replacement[a]["metadata"]["uid"] == agents[a]["metadata"]["uid"] or
                replacement[b]["metadata"]["uid"] != agents[b]["metadata"]["uid"]):
            raise fixture.IsolationError("agent restart did not replace only worker A")
        after = {}
        for suffix, node in zip(("a", "b"), self.workers):
            envelope, ack = self.wait_journal(node, pods[suffix]["metadata"]["uid"], "active")
            lock = self.worker(node, "stat", "-c", "%d:%i:%u:%g:%a:%h",
                               "/run/evpn-node-agent/cni-receipts-v1/owner.lock").stdout.strip()
            if ack["Receipt"] != before[node]["ack"]["Receipt"] or lock != before[node]["lock"]:
                raise fixture.IsolationError(f"{node}: recovered resource receipt changed")
            after[node] = {"journal": envelope, "ack": ack, "lock": lock}
        self.verify_bindings(pods)
        self.check_cni(pods)
        for suffix, destination in (("a", "b"), ("b", "a")):
            result = self.k("-n", "cni-test", "exec", "pod-" + suffix, "--",
                            "ping", "-I", "net1", "-c", "3", "-W", "2",
                            fixture.secondary_ip(destination), timeout=30)
            if "0% packet loss" not in result.stdout:
                raise fixture.IsolationError(f"post-restart net1 connectivity failed on {suffix}")
        self.verify_foreign_resources()
        self.record("continuity-after-restart.json", after)

    def verify_cleanup(self, pods):
        super().verify_cleanup(pods)
        if self.authenticated:
            journals = {}
            for suffix, node in zip(("a", "b"), self.workers):
                journal, _ = self.wait_journal(node, pods[suffix]["metadata"]["uid"], "deleted")
                journals[node] = journal
            self.record("continuity-after-del.json", journals)

    def delete_pods(self, pods):
        self.k("-n", "cni-test", "delete", "pod", "pod-a", "--wait=true", "--timeout=120s")
        survivor = pods["b"]
        uid = survivor["metadata"]["uid"]
        node = survivor["spec"]["nodeName"]
        lsp = fixture.expected_lsp_name(uid)
        name = self.k("-n", "cni-test", "exec", "deploy/secondary-ovn-central", "--",
                      "ovn-nbctl", "--bare", "--columns=name", "find",
                      "Logical_Switch_Port", f"name={lsp}").stdout.strip()
        ovs = self.runtime(node, "ovn", "ovs-vsctl", "--bare", "--columns=name", "find",
                           "Interface", f"external_ids:evpn.framework/workload-uid={uid}").stdout.strip()
        self.k("-n", "cni-test", "exec", "pod-b", "--", "ip", "address", "show", "dev", "net1")
        if name != lsp or not ovs or "\n" in ovs:
            raise fixture.IsolationError("deleting pod-a affected pod-b's owned endpoint")
        self.record("surviving-endpoint.json", {"podUID": uid, "node": node,
                                                "logicalPort": lsp, "ovsInterface": ovs})
        self.k("-n", "cni-test", "delete", "pod", "pod-b", "--wait=true", "--timeout=120s")
        self.record("pod-delete.json", {"status": "PASS"})

    def test_endpoints(self):
        self.k("-n", "evpn-operator-system", "rollout", "restart",
               "deployment/evpn-core-operator")
        self.k("-n", "evpn-operator-system", "rollout", "status",
               "deployment/evpn-core-operator", "--timeout=180s")
        self.verify_operator()
        if self.external_inventory() != self.external_before:
            raise fixture.IsolationError("Operator changed external OVN resources")
        runtime_before = json.loads((self.directory / "secondary-runtime-ownership.json").read_text())
        self.record("secondary-runtime-before-cni.json", runtime_before)
        super().test_endpoints()
        self.verify_operator()
        self.verify_node_local()
        if self.external_inventory() != self.external_before:
            raise fixture.IsolationError("external OVN resources changed during CNI test")
        self.capture_runtime_identities()
        runtime_after = json.loads((self.directory / "secondary-runtime-ownership.json").read_text())
        if runtime_after != runtime_before:
            raise fixture.IsolationError("external OVN runtime identity changed during CNI test")
        self.record("continuity-stage.json", {
            "status": "PASS" if self.authenticated else "BLOCKED",
            "reason": "CNI journal, re-attestation and endpoint recovery checked" if self.authenticated
                      else "authenticated mode was not requested",
            "agentRestart": "PASS" if self.authenticated else "NOT_RUN",
            "type2": "NOT_TESTED",
        })


class OperatorInventoryType2(OperatorSecondaryCNI):
    """Clean-install inventory slice with an external FRR peer."""

    def __init__(self, ctr, ctr_sha256, gobgpd, gobgpd_sha256):
        super().__init__(ctr, ctr_sha256)
        self.gobgpd = Path(gobgpd)
        self.gobgpd_sha256 = gobgpd_sha256
        self.cluster_uid = str(uuid.uuid4())
        self.frr = self.cluster + "-frr"
        self.forwarding_status = "NOT_REACHED"

    def record(self, name, value):
        if name in ("failure.json", "workload-evidence-error.json", "inventory-acceptance-unknown.json"):
            value = redact_inventory_evidence(value)
        super().record(name, value)

    def capture_workload_failure(self):
        evidence = {}

        def capture(name, invoke, *args):
            try:
                result = invoke(*args, check=False, timeout=30)
                item = {"returncode": result.returncode, "stdout": result.stdout or "",
                        "stderr": result.stderr or ""}
            except (OSError, fixture.subprocess.TimeoutExpired, lab.LabError) as error:
                item = {"error": str(error)}
            evidence[name] = item
            return item

        queries = {
            "core": ("-n", "evpn-system", "get", "evpncore", "evpn-core", "-o", "json"),
            "publication": ("-n", "evpn-system", "get", "configmap", "evpn-worker-inventory", "-o", "json"),
            "reservations": ("-n", "evpn-operator-system", "get", "configmap", "evpn-worker-reservations", "-o", "json"),
            "lease": ("-n", "evpn-operator-system", "get", "lease", "evpn-core-installation", "-o", "json"),
            "daemonset": ("-n", "evpn-system", "get", "daemonset", "evpn-node-agent", "-o", "json"),
            "pods": ("-n", "evpn-system", "get", "pods", "-l", "app=evpn-node-agent", "-o", "json"),
            "events": ("-n", "evpn-system", "get", "events", "-o", "json"),
            "operatorLogs": ("-n", "evpn-operator-system", "logs", "deployment/evpn-core-operator", "--tail=500"),
        }
        for name, args in queries.items():
            capture(name, self.k, *args)
        journals = {node: capture("journal:" + node, self.worker, node, "cat",
                                  "/var/lib/evpn-worker/inventory/journal.json")
                    for node in self.workers}
        for node in self.workers:
            capture("attestationPaths:" + node, self.worker, node, "stat", "-c",
                    "%n %F %u:%g %a", "/etc", "/etc/evpn", "/etc/evpn/provider-attestation",
                    "/etc/evpn/provider-attestation/provider.json", "/etc/evpn/gobgpd.yaml")
        try:
            pods = json.loads(evidence["pods"].get("stdout", ""))["items"]
        except (ValueError, KeyError, TypeError):
            pods = []
        for pod in pods:
            name = pod.get("metadata", {}).get("name")
            if name:
                capture("podLogs:" + name, self.k, "-n", "evpn-system", "logs",
                        name, "-c", "agent", "--tail=500")
                capture("podEvents:" + name, self.k, "-n", "evpn-system", "get", "events",
                        "--field-selector", "involvedObject.name=" + name, "-o", "json")
        self.record("inventory-failure-resources.json", redact_inventory_evidence(evidence))
        try:
            core = json.loads(evidence["core"].get("stdout", ""))
            try:
                publication = json.loads(evidence["publication"].get("stdout", ""))
            except ValueError:
                publication = {}
            evidence["comparison"] = inventory_evidence_comparisons(core, publication, journals)
        except (ValueError, KeyError, TypeError) as error:
            evidence["comparison"] = {"error": "core or publication unavailable: " + str(error)}
        self.record("inventory-failure-diagnostics.json", redact_inventory_evidence(evidence))

    def reserve_lab(self):
        lab.init(self.directory, "cni-inventory-frr", self.cluster,
                 "evpn33b3-" + self.cluster, self.frr)

    def build_extra(self, build):
        extra = super().build_extra(build)
        fixture.run(["go", "build", "-o", build / "type2rib", "./tools/e2e/type2rib"],
                    timeout=900, env=dict(fixture.os.environ, CGO_ENABLED="0"))
        fixture.run(["go", "build", "-o", build / "peerpidfd-probe",
                     "./tools/e2e/peerpidfdprobe"],
                    timeout=900, env=dict(fixture.os.environ, CGO_ENABLED="0"))
        return extra + "COPY peerpidfd-probe /usr/local/bin/peerpidfd-probe\n"

    def deploy_secondary_ovn(self):
        fixture.IsolatedSecondaryCNI.deploy_secondary_ovn(self)

    def cni_journal_path(self):
        return "/var/lib/evpn-worker/cni/journal.json"

    def result_status(self):
        return ("OPERATOR_INVENTORY_TWO_WORKER_PASS" if self.forwarding_status == "PASS"
                else "OPERATOR_INVENTORY_TWO_WORKER_PARTIAL")

    def result_details(self):
        return {"scenario": INVENTORY_SCENARIO, "inventoryAcceptance": "LOCAL_FENCES_MATCH",
                "type2": "REMOTE_IMPORT_OBSERVED", "localExport": "EXPORT_RECEIPT_AND_FRR_OBSERVED",
                "localSpeakerRIB": "OBSERVED",
                "liveEVPNType2": "PASS" if self.forwarding_status == "PASS" else "OBSERVED",
                "vxlanForwarding": self.forwarding_status, "recovery": "NOT_TESTED"}

    def apply_text(self, name, text, *, create=False):
        path = self.directory / name
        path.write_text(text)
        path.chmod(0o600)
        self.k("create" if create else "apply", "-f", path)

    def prepare_disabled_core(self):
        self.k("create", "namespace", "evpn-system")
        core = {"apiVersion": "evpn.romanspb80.github.io/v1alpha1", "kind": "EvpnCore",
                "metadata": {"name": "evpn-core", "namespace": "evpn-system"},
                "spec": {"enabled": False}}
        path = self.directory / "inventory-disabled-core.json"
        path.write_text(json.dumps(core))
        self.k("create", "-f", path)
        core = self.get_json("-n", "evpn-system", "get", "evpncore", "evpn-core")
        installation = core["metadata"]["uid"]
        if str(uuid.UUID(installation)) != installation or core["spec"]["enabled"] is not False:
            raise fixture.IsolationError("disabled EvpnCore has invalid installation identity")
        existing = self.k("-n", "evpn-system", "get", "daemonset", "evpn-node-agent",
                          "--ignore-not-found", "-o", "name").stdout.strip()
        if existing:
            raise fixture.IsolationError("refusing existing Node-Agent workload in clean installation")
        return installation

    def prepare_inventory_workers(self, identities, installation):
        if hashlib.sha256(self.ctr.read_bytes()).hexdigest() != self.ctr_sha256:
            raise fixture.IsolationError("ctr SHA256 mismatch")
        if hashlib.sha256(self.gobgpd.read_bytes()).hexdigest() != self.gobgpd_sha256:
            raise fixture.IsolationError("GoBGP executable SHA256 mismatch")
        observations = []
        export_domains = {}
        for node in self.workers:
            obj = self.get_json("get", "node", node)
            if obj["metadata"]["uid"] != identities[node]["nodeUID"]:
                raise fixture.IsolationError(f"{node}: Node UID changed during provisioning")
            vtep = ("198.18.30.2", "198.18.30.3")[self.workers.index(node)]
            local = json.loads(self.worker(node, "ip", "-j", "-4", "addr").stdout)
            if sum(a.get("local") == vtep for link in local for a in link.get("addr_info", [])) != 1:
                raise fixture.IsolationError(f"{node}: reserved VTEP is not uniquely local")
            self.worker(node, "sh", "-ec",
                        "test ! -e /usr/local/bin/gobgpd; test ! -e /etc/evpn/gobgpd.yaml; "
                        "test ! -e /etc/systemd/system/evpn-e2e-gobgpd.service; "
                        "test ! -e /etc/evpn/provider-attestation; test ! -e /run/gobgp; "
                        "test ! -e /var/lib/evpn-worker; test ! -e /opt/evpn/bin/ctr; "
                        "test ! -e /opt/evpn/bin/type2rib; "
                        "test ! -e /run/evpn-node-agent/export.json; "
                        "test ! -e /run/evpn-node-agent/import.json; "
                        "test ! -e /run/evpn-node-agent/cni.json; "
                        "test ! -L /run/evpn-node-agent; "
                        "if [ -e /run/evpn-node-agent ]; then "
                        "test -d /run/evpn-node-agent && "
                        "test \"$(stat -c '%u:%g:%a' /run/evpn-node-agent)\" = 0:0:700; "
                        "else install -d -o 0 -g 0 -m 700 /run/evpn-node-agent; fi; "
                        "install -d -m 700 /etc/evpn/provider-attestation /run/gobgp "
                        "/var/lib/evpn-worker /var/lib/evpn-worker/import /opt/evpn/bin")
            self.docker("cp", self.ctr, f"{node}:/opt/evpn/bin/ctr")
            self.docker("cp", self.gobgpd, f"{node}:/usr/local/bin/gobgpd")
            self.docker("cp", self.directory / "build/type2rib", f"{node}:/opt/evpn/bin/type2rib")
            self.worker(node, "chown", "root:root", "/opt/evpn/bin/ctr", "/usr/local/bin/gobgpd")
            self.worker(node, "chmod", "755", "/opt/evpn/bin/ctr", "/usr/local/bin/gobgpd")
            self.worker(node, "chmod", "600", "/run/containerd/containerd.sock")
            self.worker(node, "/opt/evpn/bin/ctr", "--address", "/run/containerd/containerd.sock", "version")
            def value(*args):
                return self.worker(node, *args).stdout.strip()
            def digest(path):
                return value("sha256sum", path).split()[0]
            if digest("/opt/evpn/bin/ctr") != self.ctr_sha256 or digest("/usr/local/bin/gobgpd") != self.gobgpd_sha256:
                raise fixture.IsolationError(f"{node}: installed binary digest mismatch")
            speaker = (EXAMPLE / "gobgpd.yaml").read_text().replace("<VTEP>", vtep).replace(
                "192.0.2.30", "198.18.30.4")
            speaker_file = self.directory / f"{node}-gobgpd.yaml"
            speaker_file.write_text(speaker)
            speaker_file.chmod(0o600)
            self.docker("cp", speaker_file, f"{node}:/etc/evpn/gobgpd.yaml")
            self.docker("cp", EXAMPLE / "gobgpd.service", f"{node}:/etc/systemd/system/evpn-e2e-gobgpd.service")
            self.worker(node, "chown", "root:root", "/etc/evpn/gobgpd.yaml")
            self.worker(node, "chmod", "600", "/etc/evpn/gobgpd.yaml")
            self.worker(node, "systemctl", "daemon-reload")
            self.worker(node, "systemctl", "start", "evpn-e2e-gobgpd.service")
            cgroup = value("systemctl", "show", "-p", "ControlGroup", "--value", "evpn-e2e-gobgpd.service")
            pid = value("systemctl", "show", "-p", "MainPID", "--value", "evpn-e2e-gobgpd.service")
            if not pid.isdigit() or int(pid) <= 0 or not cgroup or cgroup == "/":
                raise fixture.IsolationError(f"{node}: speaker process identity unavailable")
            self.worker(node, "sh", "-ec",
                        'test "$(readlink /proc/1/ns/cgroup)" = "$(readlink /proc/$1/ns/cgroup)"; '
                        'test "$(readlink /proc/1/ns/cgroup)" = "$(readlink /proc/self/ns/cgroup)"; '
                        'test "$(awk -F: \'$1 == "0" && $2 == "" {print $3}\' /proc/$1/cgroup)" = "$2"',
                        "sh", pid, cgroup)
            self.worker(node, "sh", "-ec",
                        'i=0; while [ ! -S /run/gobgp/gobgp.sock ]; do '
                        'kill -0 "$1"; i=$((i+1)); test "$i" -lt 30; sleep 1; done',
                        "sh", pid)
            runtime = value("sh", "-ec", "command -v containerd")
            boot_id = value("cat", "/proc/sys/kernel/random/boot_id")
            host_netns = value("readlink", "/proc/self/ns/net")
            export_netns = export_recovery_namespace_identity(
                host_netns, value("stat", "-Lc", "%d:%i", "/proc/self/ns/net"))
            export_domains[node] = {
                "InstallationUID": installation, "NodeUID": identities[node]["nodeUID"],
                "BootID": boot_id, "NetworkNamespace": export_netns}
            observations.append({
                "node": obj, "chassis": identities[node]["chassis"], "vtep": vtep,
                "bootID": boot_id, "hostNetns": host_netns,
                "serviceUID": str(uuid.uuid4()), "speakerCgroup": cgroup,
                "ctrSHA256": self.ctr_sha256, "gobgpdSHA256": self.gobgpd_sha256,
                "cniSHA256": digest("/opt/cni/bin/evpn-cni.real"),
                "multusSHA256": digest("/opt/cni/bin/multus"),
                "runtimeSHA256": digest(runtime),
                "wrapperExecutableSHA256": digest("/bin/sh"),
                "wrapperScriptSHA256": digest("/opt/cni/bin/evpn-cni")})
        inputs = {"clusterUID": self.cluster_uid, "installationUID": installation,
                  "agentImage": self.image, "clientImage": self.image,
                  "ovnNBHost": "secondary-ovn.cni-test.svc", "ovnSBHost": "secondary-ovn.cni-test.svc",
                  "remoteVTEP": "198.18.30.4", "workers": observations}
        rendered = inventory_configuration(inputs)
        self.export_domains = export_domains
        for node in self.workers:
            worker = rendered["workers"][node]
            for name, destination in (("export.json", "/run/evpn-node-agent/export.json"),
                                      ("import.json", "/run/evpn-node-agent/import.json"),
                                      ("cni.json", "/run/evpn-node-agent/cni.json"),
                                      ("provider.json", "/etc/evpn/provider-attestation/provider.json")):
                path = self.directory / f"{node}-{name}"
                path.write_text(json.dumps(worker[name]) + "\n")
                path.chmod(0o600)
                if destination.startswith("/run/evpn-node-agent/"):
                    result = fixture.subprocess.run(
                        ["docker", "exec", "-i", node, "sh", "-ec",
                         'umask 077; set -C; cat > "$1"', "sh", destination],
                        input=path.read_text(), text=True, stdout=fixture.subprocess.PIPE,
                        stderr=fixture.subprocess.PIPE, timeout=60, pass_fds=lab.lock_descriptors())
                    if result.returncode:
                        raise fixture.IsolationError(
                            f"{node}: failed writing {destination}: {result.stderr}")
                else:
                    self.docker("cp", path, f"{node}:{destination}")
                    self.worker(node, "chown", "root:root", destination)
                self.worker(node, "chmod", "600", destination)
            for command in rendered["datapathCommands"]["workers"][node]:
                if command[0] == "ovs-vsctl":
                    self.runtime(node, "ovn", *command)
                else:
                    self.worker(node, *command)
        for command in rendered["datapathCommands"]["ovn"]:
            self.k("-n", "cni-test", "exec", "deploy/secondary-ovn-central", "--", *command)
        return rendered

    def provision_external_frr(self, rendered):
        state = lab.load(self.directory)
        if (state["profile"] != "cni-inventory-frr" or state["frr"] != self.frr or
                state["phase"] != "ready"):
            raise fixture.IsolationError("external FRR is not in the ready owned lab")
        lab.verify_owned(state)
        for command in rendered["datapathCommands"]["externalFRR"]:
            self.docker("exec", self.frr, *command)
        config = rendered["manifests"]["frr.conf"]
        path = self.directory / "external-frr-rendered.conf"
        path.write_text(config)
        path.chmod(0o600)
        commands = ["configure terminal"]
        commands.extend(line.strip() for line in config.splitlines()
                        if line.strip() and not line.startswith(("frr defaults", "hostname",
                                                                 "service integrated", "!")))
        commands.append("end")
        args = ["exec", self.frr, "vtysh"]
        for command in commands:
            args.extend(["-c", command])
        self.docker(*args)
        self.docker("exec", self.frr, "ip", "netns", "exec", "remote-workload",
                    "ping", "-n", "-c", "2", "-W", "2", "10.244.240.1")
        self.record("external-frr-fixture.json", {
            "container": self.frr, "vtep": "198.18.30.4", "vni": 1000,
            "workerVTEPs": [rendered["workers"][node]["vtep"] for node in self.workers],
            "scope": "provisioned, sessions and forwarding not established by this record"})

    def activate_inventory_core(self, rendered):
        manifests = rendered["manifests"]
        self.apply_text("inventory-reservations.yaml", manifests["reservations.yaml"], create=True)
        self.apply_text("inventory-rbac.yaml", manifests["rbac.yaml"])
        if manifests["evpncore.yaml"].count('evpn.example.com/worker: "true"') != 1:
            raise fixture.IsolationError("unexpected inventory Node selector in example")
        core = manifests["evpncore.yaml"].replace('evpn.example.com/worker: "true"',
                                                  f'evpn.e2e/owner: "{self.owner}"')
        self.apply_text("inventory-core.yaml", core)

    def await_inventory_acceptance(self, identities, installation):
        last = ""
        for _ in range(180):
            try:
                return self.verify_inventory_operator("evpn-system", "evpn-core",
                                                      {"evpn.e2e/owner": self.owner},
                                                      expected_identities=identities,
                                                      expected_installation=installation,
                                                      expected_cluster=self.cluster_uid)
            except (fixture.IsolationError, lab.LabError, OSError, ValueError, KeyError) as error:
                last = str(error)
                time.sleep(1)
        self.record("inventory-acceptance-unknown.json", {"acceptance": "UNKNOWN", "error": last})
        raise fixture.IsolationError(f"inventory acceptance unconfirmed: {last}")

    def await_external_import(self, node, identity, installation, vtep):
        last = ""
        for _ in range(180):
            try:
                status = json.loads(self.worker(node, "cat", "/run/evpn-node-agent/import-status.json").stdout)
                export = json.loads(self.worker(node, "cat", "/run/evpn-node-agent/export-status.json").stdout)
                journal = json.loads(self.worker(
                    node, "cat", "/var/lib/evpn-worker/import/receipts-v1/journal.json").stdout)
                fdb = json.loads(self.worker(node, "bridge", "-j", "fdb", "show",
                                             "dev", "evpnvx1000").stdout)
                neighbors = json.loads(self.worker(node, "ip", "-j", "neigh", "show",
                                                   "dev", "evpnbr1000").stdout)
                verify_external_import_evidence(status, export, journal, fdb, neighbors,
                                                identity, installation, vtep)
                self.record(f"external-import-{node}.json", {
                    "status": status, "export": export, "journal": journal,
                    "fdb": fdb, "neighbors": neighbors})
                return
            except (fixture.IsolationError, OSError, ValueError, KeyError, TypeError) as error:
                last = str(error)
                time.sleep(1)
        raise fixture.IsolationError(f"{node}: remote Type-2 import not verified: {last}")

    def create_inventory_endpoints(self, rendered, identities):
        names = []
        for suffix, node, (ip, mac) in zip(("a", "b"), self.workers, INVENTORY_ENDPOINTS):
            name = "tenant-a-worker-" + suffix
            config = {"cniVersion": "1.0.0", "name": name, "type": "evpn-cni",
                      "nodeAgentSocket": self.agent_socket(), "networkID": "tenant-a",
                      "attachmentID": "secondary", "logicalSwitch": "tenant-a",
                      "ipAddress": ip + "/24", "macAddress": mac, "creationIdentity": True}
            objects = {"apiVersion": "v1", "kind": "List", "items": [
                {"apiVersion": "k8s.cni.cncf.io/v1", "kind": "NetworkAttachmentDefinition",
                 "metadata": {"name": name, "namespace": "evpn-system",
                              "labels": {"evpn.e2e/owner": self.owner}},
                 "spec": {"config": json.dumps(config, separators=(",", ":"))}},
                {"apiVersion": "v1", "kind": "Pod",
                 "metadata": {"name": name, "namespace": "evpn-system",
                              "labels": {"evpn.e2e/owner": self.owner},
                              "annotations": {"k8s.v1.cni.cncf.io/networks": name}},
                 "spec": {"nodeName": node, "containers": [{
                     "name": "client", "image": self.image, "imagePullPolicy": "Never",
                     "securityContext": {"capabilities": {"add": ["NET_RAW", "NET_ADMIN"]}},
                     "command": ["/bin/sh", "-c", "sleep infinity"]}]}}]}
            self.apply_text(f"inventory-endpoint-{suffix}.json", json.dumps(objects), create=True)
            names.append((node, name, ip, mac))
        self.k("-n", "evpn-system", "wait", "--for=condition=Ready",
               *["pod/" + name for _, name, _, _ in names], "--timeout=180s")
        endpoints = {}
        for node, name, ip, mac in names:
            pod = self.get_json("-n", "evpn-system", "get", "pod", name)
            metadata = pod["metadata"]
            if (pod["spec"].get("nodeName") != node or metadata.get("deletionTimestamp") or
                    metadata.get("labels", {}).get("evpn.e2e/owner") != self.owner):
                raise fixture.IsolationError(f"{node}: endpoint Pod placement or ownership differs")
            uid = metadata["uid"]
            try:
                if str(uuid.UUID(uid)) != uid:
                    raise ValueError("noncanonical UUID")
            except (ValueError, TypeError, AttributeError) as error:
                raise fixture.IsolationError(f"{node}: invalid endpoint Pod UID") from error
            network_status = json.loads(metadata["annotations"]["k8s.v1.cni.cncf.io/network-status"])
            attachment = [entry for entry in network_status if entry.get("interface") == "net1"]
            if len(attachment) != 1 or attachment[0].get("ips") != [ip]:
                raise fixture.IsolationError(f"{node}: endpoint network status differs")
            port = fixture.expected_lsp_name(uid, "tenant-a", "secondary")
            binding = self.wait_for_binding(port, node, identities[node]["chassis"])
            if binding["chassisUUID"] != identities[node]["chassisUUID"]:
                raise fixture.IsolationError(f"{node}: endpoint chassis UUID differs")
            lsp_uuid = self.k("-n", "cni-test", "exec", "deploy/secondary-ovn-central", "--",
                              "ovn-nbctl", "--bare", "--columns=_uuid", "find",
                              "Logical_Switch_Port", "name=" + port).stdout.strip()
            membership = self.k("-n", "cni-test", "exec", "deploy/secondary-ovn-central", "--",
                                "ovn-nbctl", "get", "Logical_Switch", "tenant-a", "ports").stdout.strip()
            if not lsp_uuid or lsp_uuid not in membership.split():
                # OVSDB may render the UUID set in brackets and comma-separated.
                if not lsp_uuid or lsp_uuid not in re.findall(
                        r"[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}", membership):
                    raise fixture.IsolationError(f"{node}: endpoint LSP is not on tenant-a")
            self.continuity_configs = getattr(self, "continuity_configs", {})
            self.continuity_configs[node] = rendered["workers"][node]["cni.json"]
            endpoints[node] = {"node": node, "pod": name, "podUID": uid, "ip": ip, "mac": mac,
                               "lsp": port, "lspUUID": lsp_uuid, "binding": binding,
                               "vtep": rendered["workers"][node]["vtep"]}
        for node, endpoint in endpoints.items():
            journal, ack = self.wait_journal(node, endpoint["podUID"], "active")
            if ack["Receipt"]["LSPUUID"] != endpoint["lspUUID"]:
                raise fixture.IsolationError(f"{node}: CNI receipt LSP identity differs")
            self.record(f"inventory-endpoint-{node}.json", {"endpoint": endpoint, "journal": journal})
        return endpoints

    def await_local_export(self, node, endpoint, identity, installation):
        last = ""
        for _ in range(180):
            try:
                status = json.loads(self.worker(node, "cat", "/run/evpn-node-agent/export-status.json").stdout)
                raw = self.worker(node, "cat", "/var/lib/evpn-worker/export/journal.json").stdout
                journal = json.loads(raw)
                prefix = '{"Record":'
                if not raw.startswith(prefix):
                    raise fixture.IsolationError("malformed export journal envelope")
                _, end = json.JSONDecoder().raw_decode(raw, len(prefix))
                if hashlib.sha256(raw[len(prefix):end].encode()).hexdigest() != journal.get("SHA256"):
                    raise fixture.IsolationError("export journal checksum mismatch")
                domain = journal["Record"]["Domain"]
                expected_domain = self.export_domains[node]
                mismatch = {
                    key: {"expected": expected_domain[key], "observed": domain.get(key)}
                    for key in ("InstallationUID", "NodeUID", "BootID", "NetworkNamespace")
                    if domain.get(key) != expected_domain[key]
                }
                if mismatch:
                    self.record(f"local-export-domain-failure-{node}.json", {
                        "expectedDomain": expected_domain, "observedDomain": domain,
                        "mismatches": mismatch})
                    last = f"export journal recovery domain mismatch: {mismatch}"
                    break
                rib = json.loads(self.docker("exec", self.frr, "vtysh", "-c",
                                             "show bgp l2vpn evpn route json").stdout)
                path_uuid = verify_local_export_evidence(status, journal, rib, endpoint, identity, installation)
                self.record(f"local-export-{node}.json", {"status": status, "journal": journal,
                                                          "frrRIB": rib, "pathUUID": path_uuid,
                                                          "expectedRecoveryDomain": expected_domain})
                return path_uuid
            except (fixture.IsolationError, OSError, ValueError, KeyError, TypeError) as error:
                last = str(error)
                time.sleep(1)
        raise fixture.IsolationError(f"{node}: local export not verified: {last}")

    def verify_speaker_rib(self, node, endpoint, identity, installation, path_uuid):
        before = json.loads(self.worker(node, "cat", "/run/evpn-node-agent/export-status.json").stdout)
        speaker = before.get("SpeakerProcess") or {}
        standalone = speaker.get("standalone") or {}
        applied = (before.get("Result") or {}).get("Applied") or []
        if (before.get("Error") or not speaker.get("PID") or not speaker.get("StartTicks") or
                standalone.get("nodeUID") != identity["nodeUID"] or
                standalone.get("installationUID") != installation or
                standalone.get("routerID") != endpoint["vtep"] or
                (before.get("Result") or {}).get("OwnershipUnverified") is not False or
                len(applied) != 1 or applied[0].get("PathUUID") != path_uuid):
            raise fixture.IsolationError(f"{node}: speaker attestation is unavailable")
        paths = json.loads(self.worker(node, "/opt/evpn/bin/type2rib", "-socket",
                                       "/run/gobgp/gobgp.sock", "-router-id", endpoint["vtep"],
                                       "-pid", str(speaker["PID"]), "-start-ticks", speaker["StartTicks"]).stdout)
        after = json.loads(self.worker(node, "cat", "/run/evpn-node-agent/export-status.json").stdout)
        applied = (after.get("Result") or {}).get("Applied") or []
        if (after.get("Error") or after.get("SpeakerProcess") != speaker or
                (after.get("Result") or {}).get("OwnershipUnverified") is not False or
                len(applied) != 1 or applied[0].get("PathUUID") != path_uuid):
            raise fixture.IsolationError(f"{node}: speaker identity changed during RIB observation")
        match = verify_local_rib(paths, endpoint, path_uuid)
        self.record(f"local-rib-{node}.json", {"speaker": speaker, "paths": paths,
                                               "matchedPath": match, "receiptUUID": path_uuid})

    def collect_ovs_dataplane(self, node, endpoint, expected_chassis, phase="before"):
        tunnels = self.runtime(node, "ovn", "ovs-vsctl", "--bare", "--columns=name", "find",
                               "Interface", "type=vxlan", "options:dst_port=4789").stdout.split()
        if len(tunnels) != 1:
            raise fixture.IsolationError(f"{node}: expected one OVS VXLAN port on UDP 4789")
        tunnel = tunnels[0]
        bridge = self.runtime(node, "ovn", "ovs-vsctl", "iface-to-br", tunnel).stdout.strip()
        port = self.runtime(node, "ovn", "ovs-vsctl", "get", "Interface", tunnel, "ofport").stdout.strip()
        if bridge != "br-int" or not port.isdecimal() or int(port) <= 0:
            raise fixture.IsolationError(f"{node}: OVS VXLAN is not a usable br-int port")
        options = self.runtime(node, "ovn", "ovs-vsctl", "get", "Interface", tunnel, "options").stdout
        external_ids = self.runtime(node, "ovn", "ovs-vsctl", "get", "Interface", tunnel, "external_ids").stdout
        system_id = self.runtime(node, "ovn", "ovs-vsctl", "--if-exists", "get",
                                 "Open_vSwitch", ".", "external_ids:system-id").stdout.strip().strip('"')
        if system_id != expected_chassis:
            raise fixture.IsolationError(f"{node}: OVS tunnel is not on the discovered OVN chassis")
        flows = self.runtime(node, "ovn", "ovs-ofctl", "dump-flows", "br-int").stdout
        ports = self.runtime(node, "ovn", "ovs-ofctl", "dump-ports", "br-int").stdout
        if REMOTE_MAC not in flows.lower():
            raise fixture.IsolationError(f"{node}: imported endpoint is absent from OVS flows")
        flow_counters = {}
        for mac in (endpoint["mac"], REMOTE_MAC):
            matching = [
                line for line in flows.splitlines()
                if re.search(rf"\bdl_dst={re.escape(mac)}\b", line, re.IGNORECASE)
            ]
            flow_counters[mac] = {
                "packets": sum(
                    int(match.group(1))
                    for line in matching
                    if (match := re.search(r"\bn_packets=(\d+)", line))
                ),
                "flows": matching,
            }
        snapshot = {
            "interface": tunnel,
            "bridge": bridge,
            "ofport": int(port),
            "observedAt": utc_timestamp(),
            "observedMonotonicNs": time.monotonic_ns(),
            "options": options,
            "externalIDs": external_ids,
            "ownerEvidence": {
                "model": "OVN chassis-local tunnel",
                "systemID": system_id,
                "expectedChassis": expected_chassis,
                "matchesDiscoveredChassis": True,
            },
            "flows": flows,
            "flowCounters": flow_counters,
            "portCounters": ports,
            "tunnelCounters": parse_ovs_port_counters(ports, int(port)),
        }
        record_name = (f"ovs-dataplane-{node}.json" if phase == "before"
                       else f"ovs-dataplane-{phase}-{node}.json")
        self.record(record_name, snapshot)
        return snapshot

    def collect_linux_forwarding_counters(self):
        remote = (self.forwarding_seeds or {}).get("remoteEndpoint") or {}
        underlay = remote.get("underlayInterface")
        if not underlay:
            raise fixture.IsolationError("external FRR underlay interface identity is unavailable")
        interfaces = {
            "underlay": (underlay, ["ip", "-j", "-s", "link", "show", "dev", underlay]),
            "vxlan": (EXTERNAL_VXLAN_INTERFACE,
                      ["ip", "-j", "-s", "link", "show", "dev", EXTERNAL_VXLAN_INTERFACE]),
            "remoteWorkload": ("frr-work0", [
                "ip", "netns", "exec", "remote-workload",
                "ip", "-j", "-s", "link", "show", "dev", "frr-work0",
            ]),
        }
        counters = {}
        for name, (interface, command) in interfaces.items():
            output = self.docker("exec", self.frr, *command).stdout
            counters[name] = parse_linux_link_counters(output, interface)
        return {
            "observedAt": utc_timestamp(),
            "observedMonotonicNs": time.monotonic_ns(),
            "interfaces": counters,
            "roles": {
                "underlay": "OBSERVATION_ONLY",
                "vxlan": "EXPECTED_EXTERNAL_FORWARDING",
                "remoteWorkload": "EXPECTED_WORKLOAD_FORWARDING",
            },
        }

    @staticmethod
    def interface_for_address(links, address, description):
        matches = [
            link["ifname"] for link in links
            if any(item.get("local") == address for item in link.get("addr_info", []))
        ]
        if len(matches) != 1:
            raise fixture.IsolationError(
                f"{description}: expected one interface for {address}, found {matches}"
            )
        return matches[0]

    def seed_forwarding_state(self, endpoints):
        seeds = {"remoteEndpoint": {}, "workerEndpoints": {}}
        self.forwarding_seeds = seeds
        remote_links = json.loads(self.docker(
            "exec", self.frr, "ip", "-j", "-4", "addr", "show"
        ).stdout)
        remote_iface = self.interface_for_address(remote_links, REMOTE_VTEP, self.frr)
        remote_workload = self.docker(
            "exec", self.frr, "ip", "netns", "exec", "remote-workload",
            "ip", "-j", "-4", "addr", "show", "dev", "frr-work0",
        )
        addresses = json.loads(remote_workload.stdout)
        if (len(addresses) != 1 or
                sum(a.get("local") == REMOTE_IP for a in addresses[0].get("addr_info", [])) != 1):
            raise fixture.IsolationError("external workload IP identity differs")
        remote_link = json.loads(self.docker(
            "exec", self.frr, "ip", "netns", "exec", "remote-workload",
            "ip", "-j", "link", "show", "dev", "frr-work0",
        ).stdout)
        if len(remote_link) != 1 or remote_link[0].get("address", "").lower() != REMOTE_MAC:
            raise fixture.IsolationError("external workload MAC identity differs")
        seeds["remoteEndpoint"].update({
            "ip": REMOTE_IP, "mac": REMOTE_MAC, "interface": "frr-work0",
            "underlayInterface": remote_iface, "vtep": REMOTE_VTEP,
        })

        external_neighbors = json.loads(self.docker(
            "exec", self.frr, "ip", "netns", "exec", "remote-workload",
            "ip", "-j", "neigh", "show",
        ).stdout)
        external_vxlan_fdb = json.loads(self.docker(
            "exec", self.frr, "bridge", "-j", "fdb", "show", "dev",
            EXTERNAL_VXLAN_INTERFACE,
        ).stdout)
        vxlan_links = json.loads(self.docker(
            "exec", self.frr, "ip", "-d", "-j", "link", "show", "dev",
            EXTERNAL_VXLAN_INTERFACE,
        ).stdout)
        observations = [{
                "node": node,
                "expected": {"ip": endpoint["ip"], "mac": endpoint["mac"],
                             "vtep": endpoint["vtep"]},
                "neighbors": [row for row in external_neighbors
                              if row.get("dst") == endpoint["ip"]],
                "fdbRows": [row for row in external_vxlan_fdb
                            if row.get("mac", "").lower() == endpoint["mac"].lower()],
            } for node, endpoint in endpoints.items()]
        fixture_observations = {
            "remoteWorkload": {"ip": REMOTE_IP, "mac": REMOTE_MAC,
                               "interface": "frr-work0", "vtep": REMOTE_VTEP},
            "underlayInterface": remote_iface,
            "vxlanLinkRows": vxlan_links,
            "workerEndpoints": observations,
        }
        self.record("inventory-forwarding-fixture-observations.json", fixture_observations)
        vxlan_identity = validate_external_vxlan_identity(vxlan_links)
        for observation in observations:
            observation["fdbClassification"] = classify_external_vxlan_fdb(
                external_vxlan_fdb, observation["expected"]["mac"],
                observation["expected"]["vtep"],
            )
        self.record("inventory-forwarding-fixture-observations.json", {
            "remoteWorkload": {"ip": REMOTE_IP, "mac": REMOTE_MAC,
                               "interface": "frr-work0", "vtep": REMOTE_VTEP},
            "underlayInterface": remote_iface,
            "vxlan": vxlan_identity,
            "workerEndpoints": observations,
        })
        for node, endpoint in endpoints.items():
            fdb = classify_external_vxlan_fdb(
                external_vxlan_fdb, endpoint["mac"], endpoint["vtep"]
            )
            matches = [row for row in external_neighbors if row.get("dst") == endpoint["ip"]]
            if matches and (len(matches) != 1 or
                            matches[0].get("dev") != "frr-work0" or
                            matches[0].get("lladdr", "").lower() != endpoint["mac"]):
                raise fixture.IsolationError(
                    f"external workload has a conflicting neighbor for {endpoint['ip']}"
                )
            added = not matches
            if added:
                self.docker(
                    "exec", self.frr, "ip", "netns", "exec", "remote-workload",
                    "ip", "neigh", "add", endpoint["ip"], "lladdr", endpoint["mac"],
                    "nud", "permanent", "dev", "frr-work0",
                )
            seeds["workerEndpoints"].setdefault(endpoint["node"], {})["externalNeighbor"] = {
                "ip": endpoint["ip"], "mac": endpoint["mac"], "addedByTest": added,
            }
            fdb_added = fdb["status"] == "MISSING"
            if fdb_added:
                self.docker(
                    "exec", self.frr, "bridge", "fdb", "add", endpoint["mac"],
                    "dev", EXTERNAL_VXLAN_INTERFACE, "dst", endpoint["vtep"], "self", "permanent",
                )
            seeds["workerEndpoints"][endpoint["node"]]["externalRemoteFDB"] = {
                "mac": endpoint["mac"], "device": EXTERNAL_VXLAN_INTERFACE,
                "vni": EXTERNAL_VNI, "remoteVTEP": endpoint["vtep"],
                "addedByTest": fdb_added, "observedState": fdb["status"],
            }
            self.record("inventory-forwarding-fixture-seeds.json", seeds)

        fdb = json.loads(self.docker(
            "exec", self.frr, "bridge", "-j", "fdb", "show", "br", "br1000",
        ).stdout)
        workload_fdb = [row for row in fdb if row.get("mac", "").lower() == REMOTE_MAC]
        fixture_observations["remoteWorkload"]["bridgeFDB"] = {
            "bridge": EXTERNAL_BRIDGE,
            "expectedPort": "frr-host0",
            "rows": workload_fdb,
        }
        self.record("inventory-forwarding-fixture-observations.json", fixture_observations)
        workload_classification = classify_external_workload_bridge_fdb(workload_fdb, REMOTE_MAC)
        workload_fdb_added = workload_classification["status"] == "MISSING"
        if workload_fdb_added:
            self.docker(
                "exec", self.frr, "bridge", "fdb", "add", REMOTE_MAC,
                "dev", "frr-host0", "master", "static",
            )
            fdb = json.loads(self.docker(
                "exec", self.frr, "bridge", "-j", "fdb", "show", "br", EXTERNAL_BRIDGE,
            ).stdout)
            workload_fdb = [row for row in fdb if row.get("mac", "").lower() == REMOTE_MAC]
            workload_classification = classify_external_workload_bridge_fdb(
                workload_fdb, REMOTE_MAC
            )
            if workload_classification["status"] == "MISSING":
                raise fixture.IsolationError("fixture workload FDB entry was not observed after creation")
            workload_fdb_added = True
        seeds["remoteEndpoint"]["bridgeFDB"] = {
            "mac": REMOTE_MAC, "device": "frr-host0", "bridge": EXTERNAL_BRIDGE,
            "entry": workload_classification["entry"],
            "observedState": workload_classification["status"],
            "addedByTest": workload_fdb_added,
        }
        self.record("inventory-forwarding-fixture-seeds.json", seeds)

        for node, endpoint in endpoints.items():
            links = json.loads(self.worker(node, "ip", "-j", "-4", "addr").stdout)
            endpoint["underlayInterface"] = self.interface_for_address(
                links, endpoint["vtep"], node
            )
            link = json.loads(self.k(
                "-n", "evpn-system", "exec", endpoint["pod"], "-c", "client", "--",
                "ip", "-j", "link", "show", "dev", "net1",
            ).stdout)
            addresses = json.loads(self.k(
                "-n", "evpn-system", "exec", endpoint["pod"], "-c", "client", "--",
                "ip", "-j", "-4", "addr", "show", "dev", "net1",
            ).stdout)
            if (len(link) != 1 or link[0].get("address", "").lower() != endpoint["mac"] or
                    len(addresses) != 1 or
                    sum(a.get("local") == endpoint["ip"]
                        for a in addresses[0].get("addr_info", [])) != 1):
                raise fixture.IsolationError(f"{node}: endpoint net1 identity differs")

            neighbors = json.loads(self.k(
                "-n", "evpn-system", "exec", endpoint["pod"], "-c", "client", "--",
                "ip", "-j", "neigh", "show",
            ).stdout)
            matches = [row for row in neighbors if row.get("dst") == REMOTE_IP]
            if matches and (len(matches) != 1 or matches[0].get("dev") != "net1" or
                            matches[0].get("lladdr", "").lower() != REMOTE_MAC):
                raise fixture.IsolationError(
                    f"{node}: endpoint has a conflicting neighbor for {REMOTE_IP}"
                )
            neighbor_added = not matches
            if neighbor_added:
                self.k(
                    "-n", "evpn-system", "exec", endpoint["pod"], "-c", "client", "--",
                    "ip", "neigh", "add", REMOTE_IP, "lladdr", REMOTE_MAC,
                    "nud", "permanent", "dev", "net1",
                )

            seeds["workerEndpoints"][node].update({
                "pod": endpoint["pod"], "podUID": endpoint["podUID"],
                "ip": endpoint["ip"], "mac": endpoint["mac"],
                "neighborAddedByTest": neighbor_added,
            })
        self.record("forwarding-fixture-seeds.json", seeds)
        return seeds

    def cleanup_inventory_forwarding_fdb(self, seeds):
        cleanup = {}
        for node, endpoint in seeds.get("workerEndpoints", {}).items():
            owned = endpoint.get("externalRemoteFDB", {})
            identity = {key: owned.get(key) for key in
                        ("mac", "device", "vni", "remoteVTEP", "addedByTest")}
            if not owned.get("addedByTest"):
                cleanup[node] = {**identity, "status": "PRESERVED_EXISTING"}
                continue
            if (owned.get("device") != EXTERNAL_VXLAN_INTERFACE or
                    owned.get("vni") != EXTERNAL_VNI or
                    not isinstance(owned.get("mac"), str) or
                    not isinstance(owned.get("remoteVTEP"), str)):
                raise fixture.IsolationError(f"{node}: fixture FDB cleanup identity is incomplete")
            rows = json.loads(self.docker(
                "exec", self.frr, "bridge", "-j", "fdb", "show", "dev",
                EXTERNAL_VXLAN_INTERFACE,
            ).stdout)
            current = classify_external_vxlan_fdb(rows, owned["mac"], owned["remoteVTEP"])
            if current["status"] == "MISSING":
                cleanup[node] = {**identity, "status": "ALREADY_ABSENT"}
                continue
            if current["status"] != "observed-existing":
                raise fixture.IsolationError(
                    f"{node}: fixture-owned FDB identity changed; refusing cleanup"
                )
            self.docker(
                "exec", self.frr, "bridge", "fdb", "del", owned["mac"],
                "dev", EXTERNAL_VXLAN_INTERFACE, "dst", owned["remoteVTEP"],
                "self",
            )
            remaining = json.loads(self.docker(
                "exec", self.frr, "bridge", "-j", "fdb", "show", "dev",
                EXTERNAL_VXLAN_INTERFACE,
            ).stdout)
            if classify_external_vxlan_fdb(remaining, owned["mac"], owned["remoteVTEP"])["status"] != "MISSING":
                raise fixture.IsolationError(
                    f"{node}: fixture-owned FDB entry remains after exact cleanup"
                )
            cleanup[node] = {**identity, "status": "REMOVED_EXACT_ENTRY"}
        bridge_owned = seeds.get("remoteEndpoint", {}).get("bridgeFDB", {})
        bridge_identity = {
            key: bridge_owned.get(key)
            for key in ("mac", "device", "bridge", "entry", "addedByTest")
        }
        if not bridge_owned.get("addedByTest"):
            cleanup["remoteEndpointBridge"] = {
                **bridge_identity, "status": "PRESERVED_EXISTING",
            }
        else:
            if (bridge_owned.get("device") != "frr-host0" or
                    bridge_owned.get("bridge") != EXTERNAL_BRIDGE or
                    not isinstance(bridge_owned.get("mac"), str) or
                    not isinstance(bridge_owned.get("entry"), dict)):
                raise fixture.IsolationError("fixture bridge FDB cleanup identity is incomplete")
            rows = json.loads(self.docker(
                "exec", self.frr, "bridge", "-j", "fdb", "show", "br", EXTERNAL_BRIDGE,
            ).stdout)
            current = classify_external_workload_bridge_fdb(rows, bridge_owned["mac"])
            if current["status"] == "MISSING":
                cleanup["remoteEndpointBridge"] = {
                    **bridge_identity, "status": "ALREADY_ABSENT",
                }
            else:
                if current["entry"] != bridge_owned["entry"]:
                    raise fixture.IsolationError(
                        "fixture-owned bridge FDB identity changed; refusing cleanup"
                    )
                self.docker(
                    "exec", self.frr, "bridge", "fdb", "del", bridge_owned["mac"],
                    "dev", "frr-host0", "master",
                )
                remaining = json.loads(self.docker(
                    "exec", self.frr, "bridge", "-j", "fdb", "show", "br", EXTERNAL_BRIDGE,
                ).stdout)
                if classify_external_workload_bridge_fdb(
                        remaining, bridge_owned["mac"])["status"] != "MISSING":
                    raise fixture.IsolationError(
                        "fixture-owned bridge FDB entry remains after exact cleanup"
                    )
                cleanup["remoteEndpointBridge"] = {
                    **bridge_identity, "status": "REMOVED_EXACT_ENTRY",
                }
        self.record("inventory-forwarding-fixture-cleanup.json", cleanup)
        return cleanup

    def observe_inventory_workload_interfaces(self, endpoints):
        observations = {"workers": {}, "external": {}}
        for node in self.workers:
            endpoint = endpoints[node]
            namespace = self.k(
                "-n", "evpn-system", "exec", endpoint["pod"], "-c", "client", "--",
                "readlink", "/proc/self/ns/net",
            ).stdout.strip()
            link_output = self.k(
                "-n", "evpn-system", "exec", endpoint["pod"], "-c", "client", "--",
                "ip", "-j", "-s", "link", "show", "dev", "net1",
            ).stdout
            address_output = self.k(
                "-n", "evpn-system", "exec", endpoint["pod"], "-c", "client", "--",
                "ip", "-j", "-4", "addr", "show", "dev", "net1",
            ).stdout
            observations["workers"][node] = parse_inventory_workload_snapshot(
                namespace, link_output, address_output, "net1",
                endpoint["mac"], endpoint["ip"],
            )
        namespace = self.docker(
            "exec", self.frr, "ip", "netns", "exec", "remote-workload",
            "readlink", "/proc/self/ns/net",
        ).stdout.strip()
        link_output = self.docker(
            "exec", self.frr, "ip", "netns", "exec", "remote-workload",
            "ip", "-j", "-s", "link", "show", "dev", "frr-work0",
        ).stdout
        address_output = self.docker(
            "exec", self.frr, "ip", "netns", "exec", "remote-workload",
            "ip", "-j", "-4", "addr", "show", "dev", "frr-work0",
        ).stdout
        observations["external"] = parse_inventory_workload_snapshot(
            namespace, link_output, address_output, "frr-work0", REMOTE_MAC, REMOTE_IP
        )
        return observations

    def collect_worker_linux_state(self, endpoints):
        snapshots = {}
        for node in self.workers:
            endpoint = endpoints[node]
            links = json.loads(self.worker(node, "ip", "-d", "-j", "link", "show").stdout)
            bridges = [link for link in links if (link.get("linkinfo") or {}).get("info_kind") == "bridge"]
            vxlan = [link for link in links if (link.get("linkinfo") or {}).get("info_kind") == "vxlan"]
            bridge_matches = [link for link in bridges if link.get("ifname") == "evpnbr1000"]
            vxlan_matches = [link for link in vxlan if link.get("ifname") == "evpnvx1000"]
            if len(bridge_matches) != 1 or len(vxlan_matches) != 1:
                raise InventoryEvidenceError(
                    "ATTRIBUTION_UNCERTAIN",
                    f"{node}: expected exact Linux bridge/VXLAN interfaces for the worker integration",
                )
            bridge, vxlan_link = bridge_matches[0], vxlan_matches[0]
            bridge_name = bridge.get("ifname")
            vxlan_name = vxlan_link.get("ifname")
            vxlan_info = vxlan_link.get("linkinfo") or {}
            vxlan_data = vxlan_info.get("info_data") or {}
            try:
                vxlan_local = str(ipaddress.ip_address(vxlan_data.get("local", "")))
                vxlan_port = int(vxlan_data.get("port", -1))
            except (TypeError, ValueError) as error:
                raise InventoryEvidenceError(
                    "ATTRIBUTION_UNCERTAIN", f"{node}: worker VXLAN options are malformed"
                ) from error
            if (vxlan_info.get("info_kind") != "vxlan" or
                    str(vxlan_data.get("id")) != str(EXTERNAL_VNI) or
                    vxlan_local != endpoint["vtep"] or vxlan_port != 60100 or
                    vxlan_link.get("master") != bridge_name or
                    "UP" not in vxlan_link.get("flags", [])):
                raise InventoryEvidenceError(
                    "ATTRIBUTION_UNCERTAIN",
                    f"{node}: worker Linux bridge/VXLAN identity does not match expected VNI/VTEP",
                )
            underlay = parse_linux_link_counters(
                self.worker(node, "ip", "-j", "-s", "link", "show", "dev", endpoint["underlayInterface"]).stdout,
                endpoint["underlayInterface"],
            )
            bridge_counters = parse_linux_link_counters(
                self.worker(node, "ip", "-j", "-s", "link", "show", "dev", bridge_name).stdout,
                bridge_name,
            )
            vxlan_counters = parse_linux_link_counters(
                self.worker(node, "ip", "-j", "-s", "link", "show", "dev", vxlan_name).stdout,
                vxlan_name,
            )
            fdb = json.loads(self.worker(node, "bridge", "-j", "fdb", "show", "dev", vxlan_name).stdout)
            neigh = json.loads(self.worker(node, "ip", "-j", "neigh", "show", "dev", bridge_name).stdout)
            snapshots[node] = {
                "observedAt": utc_timestamp(),
                "observedMonotonicNs": time.monotonic_ns(),
                "bridge": {"name": bridge_name, "ifindex": bridge_counters["ifindex"],
                           "role": "CONTROL_PLANE_INTEGRATION_OBSERVATION",
                           "counters": bridge_counters},
                "vxlan": {"name": vxlan_name, "ifindex": vxlan_counters["ifindex"],
                          "role": "CONTROL_PLANE_INTEGRATION_OBSERVATION",
                          "vni": EXTERNAL_VNI, "localVTEP": vxlan_local,
                          "destinationPort": vxlan_port, "master": bridge_name,
                          "counters": vxlan_counters, "fdb": fdb},
                "underlay": {"name": endpoint["underlayInterface"], "ifindex": underlay["ifindex"],
                             "role": "OBSERVATION_ONLY", "counters": underlay},
                "neighbors": neigh,
            }
        return snapshots

    def worker_linux_counter_delta(self, before, after):
        for node in self.workers:
            before_state = before[node]
            after_state = after[node]
            for name in ("bridge", "vxlan", "underlay"):
                if before_state[name]["name"] != after_state[name]["name"]:
                    raise fixture.IsolationError(f"{node}: Linux {name} interface identity changed")
                if before_state[name]["counters"]["ifindex"] != after_state[name]["counters"]["ifindex"]:
                    raise fixture.IsolationError(f"{node}: Linux {name} ifindex changed")
        deltas = {}
        for node in self.workers:
            node_delta = {}
            for name in ("bridge", "vxlan", "underlay"):
                delta = {"ifname": before[node][name]["name"], "ifindex": before[node][name]["counters"]["ifindex"]}
                for direction in ("rx", "tx"):
                    delta[direction] = {}
                    for counter in ("packets", "bytes", "errors", "dropped"):
                        value = (after[node][name]["counters"][direction][counter] -
                                 before[node][name]["counters"][direction][counter])
                        if value < 0:
                            raise InventoryEvidenceError(
                                "COUNTER_MISMATCH",
                                f"{node}: Linux {name} {direction} {counter} counter moved backwards"
                            )
                        delta[direction][counter] = value
                node_delta[name] = delta
            deltas[node] = node_delta
        return deltas

    def start_inventory_workload_captures(self, endpoints):
        capture_specs = []
        for index, node in enumerate(self.workers):
            endpoint = endpoints[node]
            suffix = "a" if index == 0 else "b"
            capture_specs.append({
                "name": f"inventory-worker-{suffix}-net1",
                "pcap": f"inventory-worker-{suffix}-net1.pcap",
                "decode": f"inventory-worker-{suffix}-net1-decoded.json",
                "expectedCount": INVENTORY_CAPTURE_LIMIT,
                "filter": (
                    "icmp and ((src host " + endpoint["ip"] + " and dst host " + REMOTE_IP + ") or "
                    "(src host " + REMOTE_IP + " and dst host " + endpoint["ip"] + "))"
                ),
                "command": [
                    "kubectl", "-n", "evpn-system", "exec", endpoint["pod"], "-c", "client", "--",
                    "timeout", "--signal=INT", "--kill-after=5s",
                    f"{INVENTORY_CAPTURE_MAX_SECONDS}s", "tcpdump",
                    "--immediate-mode", "-nn", "-U", "-i", "net1",
                    "-c", str(INVENTORY_CAPTURE_LIMIT), "-w", "-", (
                        "icmp and ((src host " + endpoint["ip"] + " and dst host " + REMOTE_IP + ") or "
                        "(src host " + REMOTE_IP + " and dst host " + endpoint["ip"] + "))"
                    ),
                ],
            })
        endpoint_hosts = " or ".join(f"host {endpoints[node]['ip']}" for node in self.workers)
        capture_specs.append({
            "name": "inventory-frr-workload",
            "pcap": "inventory-frr-workload.pcap",
            "decode": "inventory-frr-workload-decoded.json",
            "expectedCount": INVENTORY_CAPTURE_LIMIT * len(self.workers),
            "filter": f"icmp and host {REMOTE_IP} and ({endpoint_hosts})",
            "command": [
                "docker", "exec", self.frr, "ip", "netns", "exec", "remote-workload",
                "timeout", "--signal=INT", "--kill-after=5s",
                f"{INVENTORY_CAPTURE_MAX_SECONDS}s", "tcpdump",
                "--immediate-mode", "-nn", "-U", "-i", "frr-work0",
                "-c", str(INVENTORY_CAPTURE_LIMIT * len(self.workers)), "-w", "-",
                f"icmp and host {REMOTE_IP} and ({endpoint_hosts})",
            ],
        })
        captures = []
        try:
            for spec in capture_specs:
                pcap_path = self.directory / spec["pcap"]
                if pcap_path.exists() or pcap_path.is_symlink():
                    raise fixture.IsolationError(f"refusing pre-existing capture artifact {spec['pcap']}")
                stream = pcap_path.open("wb")
                process = subprocess.Popen(
                    spec["command"], stdout=stream, stderr=subprocess.PIPE, text=True,
                )
                ready = False
                stderr_lines = []
                ready_deadline = time.monotonic() + 30
                while time.monotonic() < ready_deadline:
                    if process.poll() is not None:
                        break
                    if process.stderr is None:
                        break
                    ready_read, _, _ = select.select([process.stderr], [], [], 0.5)
                    if not ready_read:
                        continue
                    line = process.stderr.readline()
                    if not line:
                        continue
                    stripped = line.rstrip()
                    stderr_lines.append(stripped)
                    if "listening on " in stripped.lower():
                        ready = True
                        break
                if not ready:
                    process.terminate()
                    try:
                        process.wait(timeout=10)
                    except subprocess.TimeoutExpired:
                        process.kill()
                        process.wait(timeout=10)
                    stream.close()
                    raise InventoryEvidenceError(
                        "CAPTURE_SETUP_FAILURE", f"{spec['name']}: capture did not become ready"
                    )
                captures.append({
                    **spec,
                    "readyAt": utc_timestamp(),
                    "readyMonotonicNs": time.monotonic_ns(),
                    "startedAt": utc_timestamp(),
                    "startedMonotonicNs": time.monotonic_ns(),
                    "process": process,
                    "stream": stream,
                    "stderrLines": stderr_lines,
                })
        except Exception as error:
            for capture in captures:
                process = capture.get("process")
                stream = capture.get("stream")
                if process is not None and process.poll() is None:
                    process.terminate()
                    try:
                        process.wait(timeout=10)
                    except subprocess.TimeoutExpired:
                        process.kill()
                        process.wait(timeout=10)
                if stream is not None and not stream.closed:
                    stream.close()
            if isinstance(error, fixture.IsolationError):
                raise
            raise InventoryEvidenceError("CAPTURE_SETUP_FAILURE", str(error)) from error
        return captures

    def stop_inventory_workload_captures(self, captures, endpoints):
        decode = {}
        details = []
        failures = []
        for capture in captures:
            process = capture["process"]
            stream = capture["stream"]
            stderr_lines = list(capture.get("stderrLines", []))
            timed_out = False
            try:
                stdout_data, stderr_data = process.communicate(
                    timeout=INVENTORY_CAPTURE_MAX_SECONDS + 10
                )
            except subprocess.TimeoutExpired:
                timed_out = True
                process.kill()
                stdout_data, stderr_data = process.communicate()
            finally:
                stream.flush()
                stream.close()
            if stdout_data:
                # tcpdump writes only to stderr when -w is used; guard for diagnostics.
                stderr_lines.append(stdout_data.strip())
            if stderr_data:
                stderr_lines.extend(line for line in stderr_data.splitlines() if line)
            stderr_text = "\n".join(stderr_lines)
            detail = {
                "name": capture["name"],
                "pcap": capture["pcap"],
                "command": capture["command"],
                "filter": capture["filter"],
                "readyAt": capture["readyAt"],
                "readyMonotonicNs": capture["readyMonotonicNs"],
                "startedAt": capture["startedAt"],
                "startedMonotonicNs": capture["startedMonotonicNs"],
                "completedAt": utc_timestamp(),
                "completedMonotonicNs": time.monotonic_ns(),
                "timedOut": timed_out,
                "exitStatus": process.returncode,
                "stderr": stderr_text,
                "expectedCount": capture["expectedCount"],
                "status": "INCOMPLETE",
            }
            try:
                stats = parse_capture_statistics(stderr_text)
                detail["statistics"] = stats
                if timed_out or process.returncode != 0:
                    raise InventoryEvidenceError(
                        "INCOMPLETE_CAPTURE",
                        f"{capture['name']}: capture command incomplete "
                        f"(timeout={timed_out}, exit={process.returncode})",
                    )
                validate_capture_statistics(stats, capture["expectedCount"])
                packets = fixture.decode_icmp_pcap(self.directory / capture["pcap"])
                if capture["name"].startswith("inventory-worker-"):
                    index = 0 if capture["name"].startswith("inventory-worker-a") else 1
                    endpoint = {**endpoints[self.workers[index]], "node": self.workers[index]}
                    summary = verify_inventory_workload_packets(packets, endpoint)
                    decode[endpoint["node"]] = {
                        "capture": capture["pcap"],
                        "decodedPath": capture["decode"],
                        "packetSummary": summary,
                    }
                else:
                    endpoint_list = [
                        {**endpoints[node], "node": node} for node in self.workers
                    ]
                    summary = verify_inventory_external_workload_packets(packets, endpoint_list)
                    decode["external"] = {
                        "capture": capture["pcap"],
                        "decodedPath": capture["decode"],
                        "packetSummary": summary,
                    }
                lab.write(self.directory / capture["decode"], packets)
                detail["status"] = "COMPLETE"
            except (fixture.IsolationError, OSError, ValueError, KeyError, TypeError) as error:
                detail["error"] = str(error)
                failures.append(error)
            details.append(detail)
        report = {"captures": details, "decoded": decode}
        self.record("inventory-workload-captures.json", report)
        if failures:
            error = failures[0]
            if isinstance(error, InventoryEvidenceError):
                raise error
            raise InventoryEvidenceError("INCOMPLETE_CAPTURE", str(error)) from error
        return report

    def record_inventory_evidence_manifest(self, endpoints, traffic_window, workload_captures, dataplane_status):
        source = verify_inventory_source_provenance(
            Path.home() / "evpn-for-ovn-source-provenance.json",
            Path.home() / "evpn-for-ovn-source.tar",
        )
        image_id = self.docker("image", "inspect", "--format", "{{.Id}}", self.image).stdout.strip()
        if not image_id.startswith("sha256:"):
            raise fixture.IsolationError("node-agent image ID is unavailable")
        runtime_images = {
            "ovn": {"reference": self.image, "imageID": image_id},
            "externalFRR": {
                "container": self.frr,
                "imageReference": self.docker(
                    "inspect", "--format", "{{.Config.Image}}", self.frr
                ).stdout.strip(),
                "imageID": self.docker(
                    "inspect", "--format", "{{.Image}}", self.frr
                ).stdout.strip(),
            },
            "workers": {},
        }
        for node in self.workers:
            values = self.docker(
                "inspect", "--format", "{{.Config.Image}}|{{.Image}}", node
            ).stdout.strip().split("|", 1)
            if len(values) != 2 or not values[1].startswith("sha256:"):
                raise InventoryEvidenceError("MISSING_OBSERVATION", f"{node}: worker image identity unavailable")
            runtime_images["workers"][node] = {
                "reference": values[0], "imageID": values[1],
            }
        manifest = {
            "scenario": INVENTORY_SCENARIO,
            "status": dataplane_status,
            "source": {**source, "archiveHashCommand": SOURCE_ARCHIVE_HASH_COMMAND,
                       "guestVerified": True},
            "workers": [{
                "node": node,
                "pod": endpoints[node]["pod"],
                "podUID": endpoints[node]["podUID"],
                "vtep": endpoints[node]["vtep"],
            } for node in self.workers],
            "runtimeIdentities": {
                "images": runtime_images,
                "ctrSHA256": self.ctr_sha256,
                "gobgpdSHA256": self.gobgpd_sha256,
            },
            "trafficWindow": traffic_window,
            "workloadCaptureWindow": workload_captures,
            "observationContract": {
                "workloadInterfaces": "EXPECTED_FORWARDING",
                "ovsTunnelAndType2Flow": "EXPECTED_FORWARDING",
                "workerLinuxBridgeVxlan": "CONTROL_PLANE_INTEGRATION_OBSERVATION; zero deltas valid",
                "workerLinuxUnderlay": "OBSERVATION_ONLY",
                "incompleteEvidence": "INCOMPLETE; no PASS manifest is emitted",
            },
        }
        manifest["artifacts"] = evidence_file_manifest(
            self.directory, sorted(INVENTORY_EVIDENCE_ARTIFACTS)
        )
        self.record("inventory-evidence-manifest.json", manifest)
        write_inventory_manifest_checksum(self.directory)
        return manifest

    def start_inventory_capture(self, endpoints):
        state_file = self.directory / "inventory-capture-state.sh"
        if state_file.exists() or state_file.is_symlink():
            raise fixture.IsolationError("refusing a pre-existing inventory capture state file")
        workers = []
        for index, (node, endpoint) in enumerate(endpoints.items()):
            pid = lab.command([
                "docker", "inspect", "--format", "{{.State.Pid}}", node
            ]).stdout.strip()
            if not pid.isdecimal() or int(pid) <= 0:
                raise fixture.IsolationError(f"{node}: worker container PID is unavailable")
            workers.append((index, node, endpoint, int(pid)))
        frr_pid = lab.command([
            "docker", "inspect", "--format", "{{.State.Pid}}", self.frr
        ]).stdout.strip()
        if not frr_pid.isdecimal() or int(frr_pid) <= 0:
            raise fixture.IsolationError("external FRR container PID is unavailable")

        outputs = ["worker-a-underlay", "worker-b-underlay", "frr-underlay"]
        script = f"""set -eu
ROOT={shlex.quote(str(lab.ROOT))}
KIND_CLUSTER={shlex.quote(self.cluster)}
EVIDENCE_DIR={shlex.quote(str(self.directory))}
source "$ROOT/tools/lib/cni-evpn-frr-interop-test.sh"
INTEROP_CAPTURE_PHASE=inventory
INTEROP_CAPTURE_OUTPUT_NAMES=({' '.join(outputs)})
trap 'declare -p INTEROP_CAPTURE_PHASE INTEROP_CAPTURE_STAGING_DIR INTEROP_CAPTURE_UNITS INTEROP_CAPTURE_NAMES INTEROP_CAPTURE_MAIN_PIDS INTEROP_CAPTURE_CGROUPS INTEROP_CAPTURE_OUTPUT_NAMES > {shlex.quote(str(state_file))}' EXIT
interop_capture_create_staging_dir
"""
        for index, node, endpoint, pid in workers:
            interface = endpoint["underlayInterface"]
            suffix = "a" if index == 0 else "b"
            filter_text = f"udp port 4789 and host {endpoint['vtep']} and host {REMOTE_VTEP}"
            script += (
                f"interop_capture_start_one inventory-worker-{suffix}-underlay {pid} "
                f"{shlex.quote(interface)} {shlex.quote(filter_text)} {INVENTORY_CAPTURE_MAX_SECONDS}\n"
            )
        frr_links = json.loads(self.docker("exec", self.frr, "ip", "-j", "-4", "addr").stdout)
        frr_interface = self.interface_for_address(frr_links, REMOTE_VTEP, self.frr)
        frr_filter = (f"udp port 4789 and host {REMOTE_VTEP} and "
                      "(host 198.18.30.2 or host 198.18.30.3)")
        script += (
            f"interop_capture_start_one inventory-frr-underlay {int(frr_pid)} "
            f"{shlex.quote(frr_interface)} {shlex.quote(frr_filter)} {INVENTORY_CAPTURE_MAX_SECONDS}\n"
            "interop_capture_assert_active inventory-ready\n"
        )
        capture_started_at = utc_timestamp()
        capture_started_monotonic_ns = time.monotonic_ns()
        result = lab.command(["bash", "-ec", script], check=False, timeout=90)
        capture_ready_at = utc_timestamp()
        capture_ready_monotonic_ns = time.monotonic_ns()
        if state_file.is_file() and not state_file.is_symlink():
            state_file.chmod(0o600)
        self.record("inventory-capture-start.json", {
            "returncode": result.returncode, "stdout": result.stdout, "stderr": result.stderr,
            "startedAt": capture_started_at,
            "startedMonotonicNs": capture_started_monotonic_ns,
            "readyAt": capture_ready_at if result.returncode == 0 else None,
            "readyMonotonicNs": capture_ready_monotonic_ns if result.returncode == 0 else None,
            "runtimeLimitSeconds": INVENTORY_CAPTURE_MAX_SECONDS,
            "workers": [{"node": node, "pid": pid, "interface": endpoint["underlayInterface"],
                         "vtep": endpoint["vtep"]}
                        for _, node, endpoint, pid in workers],
            "frr": {"container": self.frr, "pid": int(frr_pid),
                    "interface": frr_interface, "vtep": REMOTE_VTEP},
        })
        if result.returncode:
            if state_file.is_file():
                self.stop_inventory_capture(require_packets=False)
            raise fixture.IsolationError("failed to start owned bounded VXLAN captures")
        if not state_file.is_file() or state_file.is_symlink():
            raise fixture.IsolationError("inventory capture process state was not preserved")

    def stop_inventory_capture(self, require_packets=True):
        state_file = self.directory / "inventory-capture-state.sh"
        if not state_file.is_file() or state_file.is_symlink():
            raise InventoryEvidenceError("CAPTURE_SETUP_FAILURE", "inventory capture state is unavailable")
        preamble = f"""set -eu
ROOT={shlex.quote(str(lab.ROOT))}
KIND_CLUSTER={shlex.quote(self.cluster)}
EVIDENCE_DIR={shlex.quote(str(self.directory))}
source "$ROOT/tools/lib/cni-evpn-frr-interop-test.sh"
source {shlex.quote(str(state_file))}
INTEROP_CAPTURE_VERIFY_COMMAND=(python3 "$ROOT/tools/e2e/operator_secondary_cni.py" --verify-inventory-captures "$EVIDENCE_DIR" "$EVIDENCE_DIR/inventory-forwarding-endpoints.json")
rc=0
interop_capture_stop || rc=$?
printf '{{"shutdownFailure":%s,"verificationFailure":%s,"exit":%s}}\\n' "$INTEROP_CAPTURE_SHUTDOWN_FAILURE" "$INTEROP_PACKET_VERIFICATION_FAILURE" "$rc" > "$EVIDENCE_DIR/inventory-capture-stop.json"
exit "$rc"
"""
        result = lab.command(["bash", "-ec", preamble], check=False, timeout=120)
        stop_file = self.directory / "inventory-capture-stop.json"
        if not stop_file.is_file():
            raise fixture.IsolationError(
                f"inventory capture shutdown did not produce status: {result.stderr}"
            )
        stop = json.loads(stop_file.read_text())
        stop["observedAt"] = utc_timestamp()
        stop["observedMonotonicNs"] = time.monotonic_ns()
        self.record("inventory-capture-stop-command.json", {
            "returncode": result.returncode, "stdout": result.stdout, "stderr": result.stderr,
            "status": stop,
        })
        if stop.get("shutdownFailure") != 0:
            raise InventoryEvidenceError("INCOMPLETE_CAPTURE", "inventory VXLAN capture shutdown is unproven")
        if require_packets and (stop.get("verificationFailure") != 0 or result.returncode != 0):
            raise InventoryEvidenceError("INCOMPLETE_CAPTURE", "inventory VXLAN packet verification failed")
        return stop

    def verify_inventory_forwarding(self, endpoints, identities, ovs_before):
        evidence = {"status": "IN_PROGRESS", "scenario": INVENTORY_SCENARIO, "workers": {}}
        self.forwarding_status = "IN_PROGRESS"
        self.forwarding_seeds = None
        traffic_started = False
        capture_started = False
        workload_capture_started = False
        workload_captures = []
        traffic_window = {"startedAt": None, "startedMonotonicNs": None,
                          "completedAt": None, "completedMonotonicNs": None}
        self.record("dataplane-forwarding.json", evidence)
        try:
            self.seed_forwarding_state(endpoints)
            linux_before = self.collect_linux_forwarding_counters()
            worker_linux_before = self.collect_worker_linux_state(endpoints)
            workload_before = self.observe_inventory_workload_interfaces(endpoints)
            evidence["linux"] = {"externalBefore": linux_before, "workersBefore": worker_linux_before}
            evidence["workload"] = {"before": workload_before}
            self.record("dataplane-forwarding.json", evidence)
            capture_endpoints = []
            for index, node in enumerate(self.workers):
                endpoint = dict(endpoints[node])
                endpoint["node"] = node
                endpoint["icmpIdentifiers"] = {
                    "workerToExternal": 20000 + index,
                    "externalToWorker": 30000 + index,
                }
                endpoint["capture"] = (
                    "inventory-worker-a-underlay.pcap" if index == 0
                    else "inventory-worker-b-underlay.pcap"
                )
                capture_endpoints.append(endpoint)
                evidence["workers"][node] = {
                    "endpoint": {
                        key: endpoint[key]
                        for key in ("pod", "podUID", "ip", "mac", "vtep", "icmpIdentifiers")
                    },
                    "forward": None,
                    "return": None,
                    "ovs": None,
                }
            self.record("inventory-forwarding-endpoints.json", capture_endpoints)
            self.start_inventory_capture(endpoints)
            capture_started = True
            workload_capture_endpoints = {
                item["node"]: item for item in capture_endpoints
            }
            workload_captures = self.start_inventory_workload_captures(
                workload_capture_endpoints
            )
            workload_capture_started = True
            traffic_window["startedAt"] = utc_timestamp()
            traffic_window["startedMonotonicNs"] = time.monotonic_ns()
            for index, node in enumerate(self.workers):
                endpoint = endpoints[node]
                worker_identifier = 20000 + index
                external_identifier = 30000 + index
                traffic_window.setdefault("icmpIdentifiers", {})[node] = {
                    "workerToExternal": worker_identifier,
                    "externalToWorker": external_identifier,
                }
                traffic_started = True
                forward = self.k(
                    "-n", "evpn-system", "exec", endpoint["pod"], "-c", "client", "--",
                    "ping", "-n", "-e", str(worker_identifier), "-I", "net1",
                    "-c", "3", "-W", "3", REMOTE_IP,
                    check=False, timeout=30,
                )
                forward_result = parse_inventory_ping_result(forward.stdout, forward.returncode)
                evidence["workers"][node]["forward"] = {
                    **forward_result, "command": [
                        "kubectl", "exec", endpoint["pod"], "ping", "-n", "-e",
                        str(worker_identifier), "-I", "net1", "-c", "3", "-W", "3", REMOTE_IP,
                    ],
                    "stdout": forward.stdout, "stderr": forward.stderr,
                }
                if forward_result["status"] != "PASS":
                    raise InventoryEvidenceError(
                        "FUNCTIONAL_DATAPLANE_FAILURE",
                        f"{node}: workload-to-external IPv4 ping failed",
                    )

                reverse = lab.command([
                    "docker", "exec", self.frr, "ip", "netns", "exec", "remote-workload",
                    "ping", "-n", "-e", str(external_identifier), "-I", "frr-work0",
                    "-c", "3", "-W", "3", endpoint["ip"],
                ], check=False, timeout=30)
                reverse_result = parse_inventory_ping_result(reverse.stdout, reverse.returncode)
                evidence["workers"][node]["return"] = {
                    **reverse_result, "command": ["docker", "exec", self.frr,
                                                   "ip", "netns", "exec", "remote-workload",
                                                   "ping", "-n", "-e", str(external_identifier),
                                                   "-I", "frr-work0", "-c", "3", "-W", "3",
                                                   endpoint["ip"]],
                    "stdout": reverse.stdout, "stderr": reverse.stderr,
                }
                if reverse_result["status"] != "PASS":
                    raise InventoryEvidenceError(
                        "FUNCTIONAL_DATAPLANE_FAILURE",
                        f"{node}: external-to-workload IPv4 ping failed",
                    )
            traffic_window["completedAt"] = utc_timestamp()
            traffic_window["completedMonotonicNs"] = time.monotonic_ns()
            try:
                self.stop_inventory_capture(require_packets=True)
            finally:
                capture_started = False
            workload_capture_report = self.stop_inventory_workload_captures(workload_captures, endpoints)
            workload_capture_started = False
            self.record("inventory-workload-captures.json", workload_capture_report)
            packet_evidence = json.loads(
                (self.directory / "inventory-packet-evidence.json").read_text()
            )
            linux_after = self.collect_linux_forwarding_counters()
            worker_linux_after = self.collect_worker_linux_state(endpoints)
            workload_after = self.observe_inventory_workload_interfaces(endpoints)
            linux_deltas = {}
            for name, before in linux_before["interfaces"].items():
                after = linux_after["interfaces"][name]
                if before["ifindex"] != after["ifindex"]:
                    raise InventoryEvidenceError(
                        "ATTRIBUTION_UNCERTAIN",
                        f"Linux {name} interface identity changed during forwarding",
                    )
                delta = {"ifname": before["ifname"], "ifindex": before["ifindex"]}
                for direction in ("rx", "tx"):
                    delta[direction] = {}
                    for counter in ("packets", "bytes", "errors", "dropped"):
                        value = after[direction][counter] - before[direction][counter]
                        if value < 0:
                            raise InventoryEvidenceError(
                                "COUNTER_MISMATCH",
                                f"Linux {name} {direction} {counter} counter moved backwards"
                            )
                        delta[direction][counter] = value
                linux_deltas[name] = delta
            for name in ("vxlan", "remoteWorkload"):
                if (linux_deltas[name]["rx"]["packets"] <= 0 or
                        linux_deltas[name]["tx"]["packets"] <= 0):
                    raise InventoryEvidenceError(
                        "COUNTER_MISMATCH",
                        f"Linux {name} RX/TX packet counters did not advance during forwarding"
                    )
            evidence["workload"] = {
                "before": workload_before,
                "after": workload_after,
                "workers": {
                    node: {
                        "counterDelta": inventory_workload_counter_delta(
                            workload_before["workers"][node], workload_after["workers"][node]
                        ),
                        "capture": workload_capture_report["decoded"][node],
                    }
                    for node in self.workers
                },
                "external": {
                    "counterDelta": inventory_workload_counter_delta(
                        workload_before["external"], workload_after["external"]
                    ),
                    "capture": workload_capture_report["decoded"]["external"],
                },
            }
            evidence["trafficWindow"] = traffic_window
            evidence["linux"] = {
                "externalBefore": linux_before,
                "externalAfter": linux_after,
                "externalDelta": linux_deltas,
                "workersBefore": worker_linux_before,
                "workersAfter": worker_linux_after,
                "workersDelta": self.worker_linux_counter_delta(worker_linux_before, worker_linux_after),
            }
            for node in self.workers:
                endpoint = endpoints[node]
                before = ovs_before[node]
                after = self.collect_ovs_dataplane(
                    node, endpoint, identities[node]["chassis"], phase="after"
                )
                if (after["interface"] != before["interface"] or
                        after["ofport"] != before["ofport"] or
                        after["ownerEvidence"] != before["ownerEvidence"]):
                    raise fixture.IsolationError(f"{node}: OVN VXLAN port identity changed during forwarding")
                counter_delta = inventory_ovs_counter_delta(
                    before["tunnelCounters"], after["tunnelCounters"],
                    before["flowCounters"][REMOTE_MAC]["packets"],
                    after["flowCounters"][REMOTE_MAC]["packets"],
                )
                rx_delta = counter_delta["rxPackets"]
                tx_delta = counter_delta["txPackets"]
                flow_delta = counter_delta["remoteType2FlowPackets"]
                evidence["workers"][node]["ovs"] = {
                    "tunnel": after["interface"], "bridge": after["bridge"],
                    "ofport": after["ofport"], "options": after["options"],
                    "ownerEvidence": after["ownerEvidence"],
                    "externalIDs": after["externalIDs"],
                    "before": before["tunnelCounters"], "after": after["tunnelCounters"],
                    "rxDelta": rx_delta, "txDelta": tx_delta,
                    "remoteType2FlowPacketsBefore": before["flowCounters"][REMOTE_MAC]["packets"],
                    "remoteType2FlowPacketsAfter": after["flowCounters"][REMOTE_MAC]["packets"],
                    "remoteType2FlowPacketsDelta": flow_delta,
                }
            evidence["packetEvidence"] = packet_evidence
            evidence["status"] = "PASS"
            self.forwarding_status = "PASS"
            return evidence
        except (fixture.IsolationError, lab.LabError, OSError, ValueError, KeyError, TypeError,
                fixture.subprocess.TimeoutExpired) as error:
            evidence["status"] = "FAIL" if traffic_started else "BLOCKED"
            self.forwarding_status = evidence["status"]
            evidence["failureClass"] = getattr(error, "failure_class", "MISSING_OBSERVATION")
            evidence["failure"] = str(error)
            self.record("dataplane-forwarding.json", evidence)
            raise
        finally:
            if capture_started:
                self.stop_inventory_capture(require_packets=False)
            if workload_capture_started:
                for capture in workload_captures:
                    process = capture.get("process")
                    stream = capture.get("stream")
                    if process is not None and process.poll() is None:
                        process.terminate()
                        try:
                            process.wait(timeout=10)
                        except subprocess.TimeoutExpired:
                            process.kill()
                            process.wait(timeout=10)
                    if stream is not None and not stream.closed:
                        stream.close()
            if self.forwarding_seeds is not None:
                try:
                    evidence["fixtureCleanup"] = self.cleanup_inventory_forwarding_fdb(
                        self.forwarding_seeds
                    )
                    self.record("dataplane-forwarding.json", evidence)
                except (fixture.IsolationError, lab.LabError, OSError, ValueError,
                        KeyError, TypeError, fixture.subprocess.TimeoutExpired) as error:
                    evidence["status"] = "FAIL"
                    evidence["fixtureCleanup"] = {"status": "FAIL", "error": str(error)}
                    self.forwarding_status = "FAIL"
                    self.record("dataplane-forwarding.json", evidence)
                    raise
            if evidence["status"] == "PASS":
                evidence["manifestPath"] = "inventory-evidence-manifest.json"
                self.record("dataplane-forwarding.json", evidence)
                self.record_inventory_evidence_manifest(
                    endpoints, traffic_window, workload_capture_report, "PASS"
                )

    def verify_agent_peerpidfd(self):
        pods = self.get_json("-n", "evpn-system", "get", "pods",
                             "-l", "app=evpn-node-agent")["items"]
        for node in self.workers:
            candidates = [pod for pod in pods if pod.get("spec", {}).get("nodeName") == node
                          and pod.get("status", {}).get("phase") == "Running"
                          and any(container.get("name") == "agent" and
                                  container.get("ready") is True
                                  for container in pod.get("status", {}).get("containerStatuses", []))]
            if len(candidates) != 1:
                raise fixture.IsolationError(
                    f"{node}: expected one ready Node-Agent Pod for the SO_PEERPIDFD probe")
            pod_name = candidates[0]["metadata"]["name"]
            output = self.k("-n", "evpn-system", "exec", pod_name, "-c", "agent", "--",
                            "/usr/local/bin/peerpidfd-probe").stdout
            observed = json.loads(output)
            if (observed.get("operation") != "getsockopt(SOL_SOCKET, SO_PEERPIDFD)" or
                    observed.get("socket") != "AF_UNIX/SOCK_STREAM" or
                    observed.get("descriptor") != "anon_inode:[pidfd]" or
                    not isinstance(observed.get("peerPidFD"), int) or observed["peerPidFD"] < 0):
                raise fixture.IsolationError(f"{node}: Node-Agent container peer pidfd probe failed")
            self.record(f"peerpidfd-container-{node}.json", {
                "pod": pod_name, "node": node, "probe": observed,
            })

    def test_endpoints(self):
        self.install_operator(create_baseline_core=False)
        identities = self.discover_inventory_workers()
        installation = self.prepare_disabled_core()
        rendered = self.prepare_inventory_workers(identities, installation)
        self.provision_external_frr(rendered)
        self.activate_inventory_core(rendered)
        self.await_inventory_acceptance(identities, installation)
        self.verify_agent_peerpidfd()
        for node in self.workers:
            self.await_external_import(node, identities[node], installation, rendered["workers"][node]["vtep"])
        endpoints = self.create_inventory_endpoints(rendered, identities)
        for node in self.workers:
            path_uuid = self.await_local_export(node, endpoints[node], identities[node], installation)
            self.verify_speaker_rib(node, endpoints[node], identities[node], installation, path_uuid)
        ovs_before = {
            node: self.collect_ovs_dataplane(
                node, endpoints[node], identities[node]["chassis"], phase="before"
            )
            for node in self.workers
        }
        self.verify_inventory_forwarding(endpoints, identities, ovs_before)


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--scenario", choices=("secondary-cni", INVENTORY_SCENARIO), default="secondary-cni")
    parser.add_argument("--verify-inventory-captures", nargs=2, metavar=("EVIDENCE_DIR", "ENDPOINTS_JSON"),
                        help="verify the bounded inventory scenario VXLAN captures")
    parser.add_argument("--check-config", metavar="INPUT_JSON",
                        help="render inventory scenario configuration offline from supplied observations")
    parser.add_argument("--confirm-disposable-host", action="store_true",
                        help="confirm this is an isolated disposable Docker host")
    parser.add_argument("--continuity-ctr", help="administrator-provided static ctr matching worker containerd")
    parser.add_argument("--continuity-ctr-sha256", help="administrator-pinned SHA256 of ctr")
    parser.add_argument("--inventory-gobgpd", help="administrator-provided standalone GoBGP executable")
    parser.add_argument("--inventory-gobgpd-sha256", help="administrator-pinned SHA256 of GoBGP executable")
    args = parser.parse_args(argv)
    if args.verify_inventory_captures:
        if (args.check_config or args.confirm_disposable_host or args.continuity_ctr or
                args.continuity_ctr_sha256 or args.inventory_gobgpd or args.inventory_gobgpd_sha256):
            parser.error("--verify-inventory-captures cannot be combined with live-run options")
        directory = Path(args.verify_inventory_captures[0])
        config_path = Path(args.verify_inventory_captures[1])
        if (directory.is_symlink() or not directory.is_dir() or config_path.is_symlink() or
                not config_path.is_file() or config_path.parent != directory):
            parser.error("capture verification requires an evidence directory and local endpoint manifest")
        evidence = verify_inventory_forwarding_captures(
            directory, json.loads(config_path.read_text())
        )
        lab.write(directory / "inventory-packet-evidence.json", evidence)
        return
    if args.scenario == INVENTORY_SCENARIO:
        if args.check_config:
            if (args.confirm_disposable_host or args.continuity_ctr or args.continuity_ctr_sha256
                    or args.inventory_gobgpd or args.inventory_gobgpd_sha256):
                parser.error("--check-config is offline and cannot be combined with live-run options")
            print(json.dumps(inventory_configuration(json.loads(Path(args.check_config).read_text())), indent=2))
            return
        if not args.confirm_disposable_host:
            parser.error("a verified disposable Docker host and explicit confirmation are required")
        for path, digest in ((args.continuity_ctr, args.continuity_ctr_sha256),
                             (args.inventory_gobgpd, args.inventory_gobgpd_sha256)):
            if (not path or not digest or not Path(path).is_file() or
                    not re.fullmatch(r"[0-9a-f]{64}", digest) or
                    hashlib.sha256(Path(path).read_bytes()).hexdigest() != digest):
                parser.error("inventory execution requires existing ctr and gobgpd binaries with matching SHA256 pins")
        with lab.exclusive():
            OperatorInventoryType2(args.continuity_ctr, args.continuity_ctr_sha256,
                                   args.inventory_gobgpd, args.inventory_gobgpd_sha256).run()
        return
    if args.inventory_gobgpd or args.inventory_gobgpd_sha256:
        parser.error("--inventory-gobgpd options require the inventory scenario")
    if args.check_config:
        parser.error("--check-config requires the inventory Type-2 scenario")
    if not args.confirm_disposable_host:
        parser.error("a verified disposable Docker host and explicit confirmation are required")
    if bool(args.continuity_ctr) != bool(args.continuity_ctr_sha256):
        parser.error("--continuity-ctr and --continuity-ctr-sha256 must be supplied together")
    if args.continuity_ctr and (not Path(args.continuity_ctr).is_file() or
                                not re.fullmatch(r"[0-9a-f]{64}", args.continuity_ctr_sha256)):
        parser.error("ctr must be an existing file and its SHA256 must be lowercase hex")
    if args.continuity_ctr and hashlib.sha256(Path(args.continuity_ctr).read_bytes()).hexdigest() != args.continuity_ctr_sha256:
        parser.error("ctr SHA256 does not match the administrator pin")
    with lab.exclusive():
        OperatorSecondaryCNI(args.continuity_ctr, args.continuity_ctr_sha256).run()


if __name__ == "__main__":
    try:
        main()
    except (fixture.IsolationError, KeyError, OSError, ValueError, fixture.subprocess.TimeoutExpired) as error:
        print(f"operator secondary CNI: {error}", file=sys.stderr)
        sys.exit(1)
