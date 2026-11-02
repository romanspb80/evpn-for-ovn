#!/usr/bin/env python3
"""Run authorized Netlink EVPN acceptance against an existing Type-2 fixture."""

from __future__ import annotations

import argparse
import hashlib
import importlib.util
import ipaddress
import json
import re
import select
import shutil
import subprocess
import struct
import sys
import time
import uuid
from datetime import datetime, timezone
from pathlib import Path
from typing import Any, Callable


ROOT = Path(__file__).resolve().parents[2]
FIXTURE_SCRIPT = ROOT / "tools/e2e/evpn-type2-fixture.py"
DOCKERFILE = ROOT / "tools/e2e/netlink/Dockerfile"
AUTHORIZATION = "I_OWN_THIS_DISPOSABLE_ENVIRONMENT"
IMAGE_RE = re.compile(r"^[A-Za-z0-9./:_-]+@sha256:[0-9a-f]{64}$")
COMMAND_TIMEOUT = 30
WAIT_TIMEOUT = 90
LOCAL_ENDPOINT_INTERFACE = "ep-local"
LOCAL_ENDPOINT_BRIDGE_PORT = "ep-port"
SPEC = importlib.util.spec_from_file_location("evpn_type2_fixture", FIXTURE_SCRIPT)
if SPEC is None or SPEC.loader is None:
    raise RuntimeError(f"cannot load shared fixture helpers from {FIXTURE_SCRIPT}")
FIXTURE = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(FIXTURE)
PCAP_DECODER_SPEC = importlib.util.spec_from_file_location(
    "cni_vxlan_pcap_verify", ROOT / "tools/lib/cni-vxlan-pcap-verify.py"
)
if PCAP_DECODER_SPEC is None or PCAP_DECODER_SPEC.loader is None:
    raise RuntimeError("cannot load shared Ethernet/IPv4/ARP/ICMP PCAP decoder")
PCAP_DECODER = importlib.util.module_from_spec(PCAP_DECODER_SPEC)
PCAP_DECODER_SPEC.loader.exec_module(PCAP_DECODER)


class AcceptanceError(Exception):
    pass


def now() -> str:
    return datetime.now(timezone.utc).isoformat()


def run(
    command: list[str], timeout: int = COMMAND_TIMEOUT, check: bool = True
) -> subprocess.CompletedProcess[str]:
    try:
        result = subprocess.run(
            command,
            text=True,
            stdout=subprocess.PIPE,
            stderr=subprocess.STDOUT,
            timeout=timeout,
            check=False,
            pass_fds=FIXTURE.lab.lock_descriptors(),
        )
    except subprocess.TimeoutExpired as exc:
        output = exc.stdout or ""
        if isinstance(output, bytes):
            output = output.decode(errors="replace")
        raise AcceptanceError(
            f"command timed out after {timeout}s: {command[0]} {command[1:]}\n{output}"
        ) from exc
    if check and result.returncode:
        raise AcceptanceError(
            f"command failed ({result.returncode}): {command[0]} {command[1:]}\n"
            f"{result.stdout}"
        )
    return result


def docker_json(*args: str) -> Any:
    try:
        return json.loads(run(["docker", *args]).stdout)
    except json.JSONDecodeError as exc:
        raise AcceptanceError(f"invalid Docker JSON response: {exc}") from exc


def require_digest(value: str, name: str) -> str:
    if not IMAGE_RE.fullmatch(value):
        raise AcceptanceError(f"{name} must be pinned by @sha256:<64 lowercase hex>")
    return value


def write_json(path: Path, value: Any) -> None:
    temporary = path.with_suffix(path.suffix + ".tmp")
    temporary.write_text(json.dumps(value, indent=2, sort_keys=True) + "\n")
    temporary.replace(path)


def prepare_build_context(directory: Path) -> Path:
    context = directory / "netlink-build-context"
    if context.exists():
        raise AcceptanceError(f"build context already exists: {context}")
    context.mkdir()
    for relative in (
        "go.mod",
        "go.sum",
        "pkg",
        "gen/evpn_mgmt",
        "gen/ovsdb_mgmt",
        "services/ovsdb_mgmt/ovnnb",
        "services/bgp_ad_handler",
        "tools/e2e/netlink/route",
    ):
        source = ROOT / relative
        destination = context / relative
        if source.is_dir():
            shutil.copytree(source, destination)
        else:
            destination.parent.mkdir(parents=True, exist_ok=True)
            shutil.copy2(source, destination)
    return context


def fixture_directory(state_root: Path, run_id: str) -> Path:
    try:
        parsed = uuid.UUID(run_id)
    except ValueError as exc:
        raise AcceptanceError("run ID must be the UUID of an existing shared fixture") from exc
    directory = state_root / str(parsed)
    if not directory.is_dir():
        raise AcceptanceError(f"shared fixture run directory does not exist: {directory}")
    return directory


def allocate_fixture_addresses(
    subnet_value: str, reserved_values: list[str], count: int
) -> list[str]:
    if count < 1:
        raise AcceptanceError("at least one fixture network address must be allocated")
    subnet = ipaddress.ip_network(subnet_value, strict=True)
    reserved = {ipaddress.ip_address(value) for value in reserved_values}
    reserved.update({subnet.network_address, subnet.broadcast_address})
    addresses = []
    for address in subnet.hosts():
        if address not in reserved:
            addresses.append(str(address))
            reserved.add(address)
            if len(addresses) == count:
                return addresses
    raise AcceptanceError(
        f"fixture subnet {subnet} has fewer than {count} unallocated host addresses"
    )


def local_endpoint_addresses(run_id: str) -> tuple[str, str]:
    digest = hashlib.sha256(run_id.encode()).digest()
    subnet = ipaddress.ip_network(
        f"172.{16 + digest[0] % 16}.{digest[1]}.0/24"
    )
    return str(subnet.network_address + 1), str(subnet.network_address + 2)


def local_endpoint_setup_commands(
    bridge: str,
    bridge_mac: str,
    endpoint: dict[str, Any],
    gateway: str,
    remote_ips: list[str],
) -> list[list[str]]:
    commands = [
        ["ip", "link", "set", "dev", bridge, "address", bridge_mac],
        ["ip", "address", "add", f"{gateway}/24", "dev", bridge],
        [
            "ip",
            "link",
            "add",
            LOCAL_ENDPOINT_INTERFACE,
            "type",
            "veth",
            "peer",
            "name",
            LOCAL_ENDPOINT_BRIDGE_PORT,
        ],
        [
            "ip",
            "link",
            "set",
            "dev",
            LOCAL_ENDPOINT_INTERFACE,
            "address",
            endpoint["mac"],
        ],
        [
            "ip",
            "link",
            "set",
            "dev",
            LOCAL_ENDPOINT_BRIDGE_PORT,
            "master",
            bridge,
        ],
        ["ip", "link", "set", "dev", LOCAL_ENDPOINT_BRIDGE_PORT, "up"],
        [
            "ip",
            "address",
            "add",
            f"{endpoint['ip']}/24",
            "dev",
            LOCAL_ENDPOINT_INTERFACE,
        ],
        ["ip", "link", "set", "dev", LOCAL_ENDPOINT_INTERFACE, "up"],
    ]
    commands.extend(
        [
            "ip",
            "route",
            "replace",
            f"{remote_ip}/32",
            "via",
            gateway,
            "dev",
            LOCAL_ENDPOINT_INTERFACE,
            "src",
            endpoint["ip"],
        ]
        for remote_ip in remote_ips
    )
    return commands


def local_endpoint_interface_matches(
    endpoint: dict[str, Any], document: Any
) -> tuple[bool, str]:
    if not isinstance(document, list):
        return False, "local endpoint interface JSON must be a list"
    interfaces = [
        interface
        for interface in document
        if isinstance(interface, dict)
        and interface.get("ifname") == LOCAL_ENDPOINT_INTERFACE
    ]
    if len(interfaces) != 1:
        return False, (
            f"expected one {LOCAL_ENDPOINT_INTERFACE} interface, found "
            f"{len(interfaces)}"
        )
    interface = interfaces[0]
    actual_mac = str(interface.get("address", "")).lower()
    addresses = {
        str(address.get("local"))
        for address in interface.get("addr_info", [])
        if isinstance(address, dict)
    }
    valid = actual_mac == endpoint["mac"].lower() and endpoint["ip"] in addresses
    return valid, (
        f"interface={LOCAL_ENDPOINT_INTERFACE} MAC={actual_mac} "
        f"IP={sorted(addresses)} expected MAC={endpoint['mac']} IP={endpoint['ip']}"
    )


def validate_fixture_endpoint_addresses(params: dict[str, Any]) -> None:
    allowed_subnets = [ipaddress.ip_network(params["subnet"], strict=True)]
    endpoint_subnet_value = params.get("endpoint_subnet")
    if endpoint_subnet_value:
        allowed_subnets.append(
            ipaddress.ip_network(endpoint_subnet_value, strict=True)
        )
    for endpoint in params["endpoints"]:
        address = ipaddress.ip_address(endpoint["ip"])
        if not any(address in subnet for subnet in allowed_subnets):
            raise AcceptanceError(
                f"fixture endpoint {endpoint['name']} is outside the owned endpoint subnets"
            )


def load_fixture(directory: Path, authorization: str) -> dict[str, Any]:
    manifest = FIXTURE.load_manifest(directory)
    if authorization != AUTHORIZATION:
        raise AcceptanceError("explicit disposable-environment authorization is required")
    FIXTURE.capture_resources(directory, manifest, allow_empty=False, compare_recorded=True)
    if manifest.get("phase") != "ready":
        raise AcceptanceError(f"shared fixture is not ready: {manifest.get('phase')}")
    if manifest.get("fixture") != "evpn-type2":
        raise AcceptanceError("run manifest is not an EVPN Type-2 fixture")
    if manifest.get("source_commit") != git_head():
        raise AcceptanceError("fixture source commit does not match the current checkout")
    resources = manifest.get("resources", {})
    if len(resources.get("networks", [])) != 1 or len(resources.get("containers", [])) != 2:
        raise AcceptanceError("shared fixture resource inventory is incomplete")
    if not manifest.get("parameters", {}).get("endpoints"):
        raise AcceptanceError("shared fixture has no endpoint identities")
    if any(
        endpoint.get("state") != "not-advertised"
        for endpoint in manifest["parameters"]["endpoints"]
    ):
        raise AcceptanceError(
            "shared fixture must be pristine with all remote routes not advertised"
        )
    for role in ("gobgp", "frr"):
        require_digest(manifest.get("images", {}).get(role, ""), f"{role} image")
    return manifest


def validate_fixture_resources(
    fixture: dict[str, Any], run_id: str
) -> tuple[dict[str, Any], str, str]:
    if fixture.get("run_id") != run_id:
        raise AcceptanceError("fixture manifest run ID does not match the requested run")
    network_id = fixture["resources"]["networks"][0]
    network = docker_json("network", "inspect", network_id)[0]
    state = FIXTURE.lab.load(RUN_DIR)
    owned = FIXTURE.lab.verify_owned(state)
    if state["owner"] != run_id or owned["network"] != network_id:
        raise AcceptanceError("fixture Containerlab network receipt does not match the run")
    params = fixture["parameters"]
    subnet = ipaddress.ip_network(params["subnet"], strict=True)
    if not any(
        ipaddress.ip_network(item["Subnet"], strict=True) == subnet
        for item in network.get("IPAM", {}).get("Config", [])
        if item.get("Subnet")
    ):
        raise AcceptanceError("fixture Docker network subnet does not match its manifest")
    containers = {
        role: fixture_container(fixture, run_id, role)
        for role in ("gobgp", "frr")
    }
    inspected_ids = set(network.get("Containers", {}))
    if inspected_ids != set(fixture["resources"]["containers"]):
        raise AcceptanceError(
            "fixture network has missing or unowned attached containers: "
            f"{sorted(inspected_ids)}"
        )
    expected_ips = {
        "gobgp": params["gobgp_address"],
        "frr": params["frr_address"],
    }
    for role, container_id in containers.items():
        inspect = own_container(container_id, run_id, "type2")
        if inspect.get("State", {}).get("Status") != "running":
            raise AcceptanceError(f"owned {role} fixture container is not running")
        if inspect.get("Config", {}).get("Image") != fixture["images"][role]:
            raise AcceptanceError(f"owned {role} container image differs from the manifest")
        endpoint = inspect.get("NetworkSettings", {}).get("Networks", {}).get(
            network["Name"], {}
        )
        if endpoint.get("IPAddress") != expected_ips[role]:
            raise AcceptanceError(f"owned {role} container IP differs from the manifest")
    frr_addresses = json.loads(
        run(["docker", "exec", containers["frr"], "ip", "-j", "address", "show", "dev", "eth0"]).stdout
    )
    frr_ipv4_addresses = {
        item["local"].split("/", 1)[0]
        for interface in frr_addresses
        for item in interface.get("addr_info", [])
        if item.get("family") == "inet" and item.get("local")
    }
    if params["remote_vtep"] not in frr_ipv4_addresses:
        raise AcceptanceError(
            "fixture remote VTEP is not configured on the owned FRR container"
        )
    validate_fixture_endpoint_addresses(params)
    if params["endpoints"][0]["mac"] == params["endpoints"][1]["mac"]:
        raise AcceptanceError("fixture endpoint MAC identities are not distinct")
    return network, containers["gobgp"], containers["frr"]


def git_head() -> str:
    return run(["git", "-C", str(ROOT), "rev-parse", "HEAD"]).stdout.strip()


def own_container(container_id: str, run_id: str, fixture: str) -> dict[str, Any]:
    inspect = docker_json("inspect", container_id)[0]
    labels = inspect.get("Config", {}).get("Labels") or {}
    if labels.get("evpn.e2e.owner") != run_id or labels.get("evpn.e2e.fixture") != fixture:
        raise AcceptanceError(f"container {container_id} is not owned by this acceptance run")
    return inspect


def fixture_container(fixture: dict[str, Any], run_id: str, service: str) -> str:
    for container_id in fixture["resources"]["containers"]:
        inspect = own_container(container_id, run_id, "type2")
        labels = inspect.get("Config", {}).get("Labels") or {}
        if labels.get("evpn.e2e.service") == service:
            return container_id
    raise AcceptanceError(f"shared fixture has no owned {service} container")


def container_exec(container_id: str, *args: str, check: bool = True) -> str:
    own_container(container_id, RUN_ID, "netlink")
    return run(["docker", "exec", container_id, *args], check=check).stdout


def save_evidence(name: str, text: str) -> str:
    path = EVIDENCE / name
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(text)
    return str(path.relative_to(RUN_DIR))


def save_binary_evidence(name: str, data: bytes) -> str:
    path = EVIDENCE / name
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_bytes(data)
    return str(path.relative_to(RUN_DIR))


def add_gate(name: str, status: str, detail: str, evidence: str = "") -> None:
    if status not in {"PASS", "FAIL", "SKIP", "NOT RUN"}:
        raise AcceptanceError(f"invalid gate status: {status}")
    item = {
        "name": name,
        "status": status,
        "checked_at": now(),
        "detail": detail,
        "evidence": evidence or None,
    }
    REPORT["gates"].append(item)
    write_json(REPORT_PATH, REPORT)


def gate(name: str, action: Callable[[], tuple[str, str]]) -> bool:
    try:
        status, detail = action()
        evidence = save_evidence(
            f"gates/{name.replace(' ', '-').lower()}.txt", detail
        )
    except Exception as exc:
        status = "FAIL"
        detail = str(exc)
        evidence = save_evidence(
            f"gates/{name.replace(' ', '-').lower()}.txt", detail
        )
    add_gate(name, status, detail, evidence)
    return status == "PASS"


def check(condition: bool, detail: str) -> tuple[str, str]:
    return ("PASS" if condition else "FAIL", detail)


def fixture_cli(directory: Path, authorization: str, *args: str) -> str:
    result = run(
        [
            sys.executable,
            str(FIXTURE_SCRIPT),
            *args,
            "--state-root",
            str(directory.parent),
        ],
        timeout=WAIT_TIMEOUT,
        check=False,
    )
    if result.returncode:
        raise AcceptanceError(
            f"shared fixture action failed ({result.returncode}):\n{result.stdout}"
        )
    return result.stdout


def record_routes(
    directory: Path,
    manifest: dict[str, Any],
    endpoints: list[dict[str, Any]],
    present: bool,
) -> tuple[bool, str]:
    outputs = FIXTURE.route_outputs(directory, manifest)
    lines = []
    okay = True
    for endpoint in endpoints:
        found, valid, details, exact_route_evidence = FIXTURE.observe_route_pair(
            directory, manifest, endpoint, outputs[0], outputs[1]
        )
        route_ok = (
            found is True and valid
            if present
            else found is False and not valid
        )
        okay = okay and route_ok
        lines.extend(details)
        if exact_route_evidence:
            lines.append(f"FRR exact route JSON:\n{exact_route_evidence}")
    lines.extend(
        [
            "GoBGP RIB JSON:",
            outputs[0].stdout,
            "FRR route table JSON:",
            outputs[1].stdout,
        ]
    )
    return okay, "\n".join(lines)


def gobgp_route_records_present(output: str, endpoint: dict[str, Any]) -> bool:
    try:
        document = json.loads(output)
    except json.JSONDecodeError:
        return False
    return bool(FIXTURE._route_records(document, endpoint))


def classify_local_route_export(
    local_rib_present: bool,
    adj_out: dict[str, Any],
    adj_out_present: bool,
    frr_received: dict[str, Any],
    frr_received_present: bool,
    frr_accepted_present: bool,
) -> str:
    if not local_rib_present:
        return "Route absent from GoBGP Local RIB"
    if adj_out.get("exitCode") != 0:
        return "Insufficient evidence to determine the export/import boundary"
    if not adj_out_present:
        return "Route absent from GoBGP export"
    if frr_accepted_present:
        return "Route accepted into the FRR EVPN route table"
    if frr_received.get("exitCode") == 0 and frr_received_present:
        return "Route exported and received by FRR but absent from accepted routes"
    return "Route exported but absent from FRR accepted routes; receive/rejection boundary unavailable"


def _run_diagnostic_command(command: list[str]) -> dict[str, Any]:
    try:
        result = FIXTURE.run_command(
            command, timeout=WAIT_TIMEOUT, check=False, capture_stderr=True
        )
        return {
            "command": command,
            "exitCode": result.returncode,
            "stdout": result.stdout,
            "stderr": result.stderr,
        }
    except Exception as exc:
        return {
            "command": command,
            "exitCode": None,
            "stdout": "",
            "stderr": "",
            "error": f"{type(exc).__name__}: {exc}",
        }


def capture_local_route_diagnostics(
    directory: Path,
    manifest: dict[str, Any],
    endpoint: dict[str, Any],
    local_vtep: str,
) -> str:
    params = manifest["parameters"]
    gobgp_adj_out = _run_diagnostic_command(
        FIXTURE.fixture_exec(
            manifest,
            directory,
            "exec",
            "-T",
            "gobgp",
            "gobgp",
            "neighbor",
            params["frr_address"],
            "adj-out",
            "-a",
            "evpn",
            "-j",
        )
    )
    frr_received = _run_diagnostic_command(
        FIXTURE.fixture_exec(
            manifest,
            directory,
            "exec",
            "-T",
            "frr",
            "vtysh",
            "-c",
            "show bgp l2vpn evpn neighbors "
            f"{params['gobgp_address']} received-routes json",
        )
    )
    frr_accepted = _run_diagnostic_command(
        FIXTURE.fixture_exec(
            manifest,
            directory,
            "exec",
            "-T",
            "frr",
            "vtysh",
            "-c",
            f"show bgp l2vpn evpn neighbors {params['gobgp_address']} routes json",
        )
    )
    logs = [
        _run_diagnostic_command(
            ["docker", "logs", "--tail", "500",
             fixture_container(manifest, manifest["run_id"], service)]
        )
        for service in ("gobgp", "frr")
    ]

    local_rib_present = False
    frr_table_present = False
    local_rib = None
    frr_table = None
    frr_exact = None
    observation = {"found": None, "valid": False, "detail": ""}
    try:
        local_rib, frr_table = FIXTURE.route_outputs(directory, manifest)
        local_rib_present = gobgp_route_records_present(
            local_rib.stdout, endpoint
        )
        frr_table_present, _, _, _ = FIXTURE.observe_route_pair(
            directory, manifest, endpoint, local_rib, frr_table
        )
        frr_exact = FIXTURE.frr_exact_route_output(directory, manifest, endpoint)
        found, valid, detail = FIXTURE.route_observation(
            frr_exact.stdout,
            endpoint,
            int(params["vni"]),
            params["rd"],
            params["rt"],
            local_vtep,
            "FRR exact-route JSON",
            "gobgp",
            params["gobgp_address"],
            params["gobgp_router_id"],
        )
        observation = {"found": found, "valid": valid, "detail": detail}
        frr_table_present = found is True
    except Exception as exc:
        observation["detail"] = f"{type(exc).__name__}: {exc}"

    adj_out_present = gobgp_route_records_present(
        gobgp_adj_out.get("stdout", ""), endpoint
    )
    frr_received_present = gobgp_route_records_present(
        frr_received.get("stdout", ""), endpoint
    )
    classification = classify_local_route_export(
        local_rib_present,
        gobgp_adj_out,
        adj_out_present,
        frr_received,
        frr_received_present,
        frr_table_present,
    )
    diagnostics = {
        "endpoint": {"mac": endpoint["mac"], "ip": endpoint["ip"]},
        "classification": classification,
        "gobgpLocalRib": {
            "present": local_rib_present,
            "stdout": local_rib.stdout if local_rib else "",
            "stderr": local_rib.stderr if local_rib else "",
        },
        "gobgpAdjRibOut": {
            **gobgp_adj_out,
            "exactRoutePresent": adj_out_present,
        },
        "frrReceivedRoutes": {
            **frr_received,
            "exactRoutePresent": frr_received_present,
        },
        "frrAcceptedRoutes": {
            **frr_accepted,
            "exactRoutePresent": frr_table_present,
            "exactRouteObservation": observation,
            "routeTableStdout": frr_table.stdout if frr_table else "",
            "routeTableStderr": frr_table.stderr if frr_table else "",
            "exactRouteStdout": frr_exact.stdout if frr_exact else "",
            "exactRouteStderr": frr_exact.stderr if frr_exact else "",
        },
        "speakerLogs": logs,
    }
    evidence_path = save_evidence(
        "route-diagnostics/local-export-import.json",
        json.dumps(diagnostics, indent=2),
    )
    REPORT["topology"]["local_route_export_classification"] = classification
    write_json(REPORT_PATH, REPORT)
    return f"classification={classification}; evidence={evidence_path}"


def wait_handler_log(
    container_id: str, patterns: list[str], timeout: int = WAIT_TIMEOUT
) -> str:
    deadline = time.monotonic() + timeout
    last = ""
    while time.monotonic() < deadline:
        own_container(container_id, RUN_ID, "netlink")
        last = run(["docker", "logs", "--tail", "2000", container_id], check=False).stdout
        if all(pattern in last for pattern in patterns):
            return last
        state = docker_json("inspect", container_id)[0].get("State", {})
        if state.get("Status") != "running":
            raise AcceptanceError(f"Netlink handler exited: {last}")
        time.sleep(2)
    raise AcceptanceError(f"handler did not produce expected evidence {patterns}:\n{last}")


def wait_handler_log_occurrences(
    container_id: str, pattern: str, minimum: int, timeout: int = WAIT_TIMEOUT
) -> str:
    deadline = time.monotonic() + timeout
    last = ""
    while time.monotonic() < deadline:
        own_container(container_id, RUN_ID, "netlink")
        last = run(
            ["docker", "logs", "--tail", "2000", container_id], check=False
        ).stdout
        if last.count(pattern) >= minimum:
            return last
        state = docker_json("inspect", container_id)[0].get("State", {})
        if state.get("Status") != "running":
            raise AcceptanceError(f"Netlink handler exited: {last}")
        time.sleep(2)
    raise AcceptanceError(
        f"handler did not log {pattern!r} at least {minimum} times:\n{last}"
    )


def exact_fdb(output: str, mac: str, vtep: str, device: str) -> bool:
    target = mac.lower()
    expected_vtep = vtep.lower()
    expected_device = device.lower()
    if not expected_device:
        return False

    stripped = output.lstrip()
    if stripped.startswith(("[", "{")):
        try:
            entries = json.loads(output)
        except json.JSONDecodeError:
            return False
        if isinstance(entries, dict):
            entries = [entries]
        if not isinstance(entries, list):
            return False
        for entry in entries:
            if not isinstance(entry, dict):
                continue
            entry_device = entry.get("dev", entry.get("ifname"))
            flags = entry.get("flags", [])
            if isinstance(flags, str):
                flags = flags.split()
            if not isinstance(flags, list):
                continue
            if (
                str(entry.get("mac", "")).lower() == target
                and str(entry.get("dst", entry.get("destination", ""))).lower()
                == expected_vtep
                and (entry_device is None or str(entry_device).lower() == expected_device)
                and (entry.get("self") is True or "self" in flags)
                and (
                    str(entry.get("state", "")).lower() == "permanent"
                    or entry.get("permanent") is True
                    or "permanent" in flags
                )
            ):
                return True
        return False

    for line in output.lower().splitlines():
        fields = line.split()
        if not fields or fields[0] != target:
            continue
        if "dev" in fields:
            dev_indices = [index for index, value in enumerate(fields) if value == "dev"]
            if (
                len(dev_indices) != 1
                or dev_indices[0] + 1 >= len(fields)
                or fields[dev_indices[0] + 1] != expected_device
            ):
                continue
        dst_indices = [index for index, value in enumerate(fields) if value == "dst"]
        if (
            len(dst_indices) == 1
            and dst_indices[0] + 1 < len(fields)
            and fields[dst_indices[0] + 1] == expected_vtep
            and "self" in fields
            and "permanent" in fields
        ):
            return True
    return False


def exact_neighbor(output: str, ip: str, mac: str) -> bool:
    for line in output.lower().splitlines():
        fields = line.split()
        if (
            len(fields) >= 4
            and fields[0] == ip.lower()
            and "lladdr" in fields
            and fields.index("lladdr") + 1 < len(fields)
            and fields[fields.index("lladdr") + 1] == mac.lower()
            and "permanent" in fields
        ):
            return True
    return False


def overall_status(
    gates: list[dict[str, Any]], required: list[str], cleanup_status: str
) -> str:
    by_name = {item["name"]: item["status"] for item in gates}
    if cleanup_status != "PASS":
        return "FAIL"
    if any(by_name.get(name) != "PASS" for name in required):
        return "FAIL"
    return "PASS"


def vtep_neighbor(output: str, vtep: str) -> bool:
    return exact_neighbor(output, vtep, "00:00:00:00:00:00")


def flood_fdb(output: str, vtep: str, device: str) -> bool:
    return exact_fdb(output, "00:00:00:00:00:00", vtep, device)


def kernel_snapshot(handler_id: str) -> dict[str, str]:
    return {
        "link": container_exec(handler_id, "ip", "-d", "-j", "link", "show", "dev", VXLAN),
        "bridge_link": container_exec(handler_id, "ip", "-d", "-j", "link", "show", "dev", BRIDGE),
        "underlay": container_exec(handler_id, "ip", "-j", "address", "show", "dev", "eth0"),
        "bridge": container_exec(handler_id, "bridge", "-j", "link", "show"),
        "fdb": container_exec(handler_id, "bridge", "fdb", "show", "dev", VXLAN),
        "fdb_device": VXLAN,
        "neighbors": container_exec(handler_id, "ip", "neigh", "show", "dev", BRIDGE),
        "vtep_neighbors": container_exec(handler_id, "ip", "neigh", "show", "dev", VXLAN),
    }


def link_configuration(snapshot: dict[str, str], vni: int, local_vtep: str) -> bool:
    links = json.loads(snapshot["link"])
    if len(links) != 1:
        return False
    link = links[0]
    link_info = link.get("linkinfo", {})
    data = link_info.get("info_data", {})
    if (
        link.get("ifname") != VXLAN
        or link_info.get("info_kind") != "vxlan"
        or link.get("master") != BRIDGE
        or "UP" not in link.get("flags", [])
    ):
        return False
    ports = [data[key] for key in ("dstport", "port") if key in data]
    try:
        port_valid = bool(ports) and all(int(port) == 4789 for port in ports)
        vni_valid = int(data.get("id", -1)) == vni
    except (TypeError, ValueError):
        return False
    underlay = json.loads(snapshot["underlay"])
    underlay_addresses = {
        address["local"].split("/", 1)[0]
        for interface in underlay
        if interface.get("ifname") == "eth0"
        for address in interface.get("addr_info", [])
        if address.get("family") == "inet" and address.get("local")
    }
    return (
        vni_valid
        and port_valid
        and local_vtep in underlay_addresses
    )


def bridge_configuration(snapshot: dict[str, str]) -> bool:
    bridge = json.loads(snapshot["bridge_link"])
    vxlan = json.loads(snapshot["link"])
    return (
        len(bridge) == 1
        and bridge[0].get("ifname") == BRIDGE
        and bridge[0].get("linkinfo", {}).get("info_kind") == "bridge"
        and "UP" in bridge[0].get("flags", [])
        and len(vxlan) == 1
        and vxlan[0].get("ifname") == VXLAN
        and vxlan[0].get("linkinfo", {}).get("info_kind") == "vxlan"
        and vxlan[0].get("master") == BRIDGE
        and "UP" in vxlan[0].get("flags", [])
    )


def overlay_capture_matches(output: str, local_vtep: str, remote_vtep: str) -> bool:
    addresses = set(re.findall(r"\b(?:\d{1,3}\.){3}\d{1,3}\b", output))
    return (
        "UDP" in output.upper()
        and "4789" in output
        and local_vtep in addresses
        and remote_vtep in addresses
    )


def verify_frr_endpoint(
    fixture: dict[str, Any], run_id: str, endpoint: dict[str, Any]
) -> str:
    container_id = fixture_container(fixture, run_id, "frr")
    endpoint_pid = FIXTURE.endpoint_namespace_pid(RUN_DIR, fixture)
    endpoint_interface, bridge_interface = FIXTURE.frr_endpoint_interfaces(endpoint)
    endpoint_link_output = run(
        [
            "docker", "exec", container_id, "nsenter", "--target",
            str(endpoint_pid), "--net", "ip", "-d", "-j", "link", "show",
            "dev", endpoint_interface,
        ]
    ).stdout
    endpoint_address_output = run(
        [
            "docker", "exec", container_id, "nsenter", "--target",
            str(endpoint_pid), "--net", "ip", "-j", "address", "show",
            "dev", endpoint_interface,
        ]
    ).stdout
    bridge_link_output = run(
        [
            "docker", "exec", container_id, "ip", "-d", "-j", "link", "show",
            "dev", bridge_interface,
        ]
    ).stdout
    endpoint_links = json.loads(endpoint_link_output)
    endpoint_addresses_json = json.loads(endpoint_address_output)
    bridge_links = json.loads(bridge_link_output)
    endpoint_link = endpoint_links[0] if endpoint_links else None
    bridge_link = bridge_links[0] if bridge_links else None
    endpoint_addresses = [
        address
        for interface in endpoint_addresses_json
        for address in interface.get("addr_info", [])
    ]
    endpoint_ready = (
        endpoint_link is not None
        and bridge_link is not None
        and endpoint_link.get("address", "").lower() == endpoint["mac"].lower()
        and any(
            address.get("local") == endpoint["ip"]
            for address in endpoint_addresses
        )
        and bridge_link.get("master") == "br-e2e"
    )
    if not endpoint_ready:
        raise AcceptanceError(
            f"shared fixture's FRR endpoint {endpoint['name']} does not match the manifest:\n"
            f"endpoint namespace PID={endpoint_pid}\n"
            f"{endpoint_interface} link: {endpoint_link_output}\n"
            f"{endpoint_interface} addresses: {endpoint_address_output}\n"
            f"{bridge_interface} bridge link: {bridge_link_output}"
        )
    REPORT["topology"].setdefault("frr_endpoint_veths", {})[endpoint["name"]] = {
        "container_id": container_id,
        "endpoint_namespace_pid": endpoint_pid,
        "endpoint_interface": endpoint_interface,
        "bridge_interface": bridge_interface,
        "created_by": "shared-type2-fixture",
    }
    write_json(REPORT_PATH, REPORT)
    return (
        f"endpoint namespace PID={endpoint_pid}\n"
        f"ip -d -j link show dev {endpoint_interface}:\n{endpoint_link_output}\n"
        f"ip -j address show dev {endpoint_interface}:\n{endpoint_address_output}\n"
        f"ip -d -j link show dev {bridge_interface}:\n{bridge_link_output}"
    )


def create_handler(
    fixture: dict[str, Any],
    fixture_dir: Path,
    handler_image: str,
    local_vtep: str,
    vni: int,
) -> tuple[str, str]:
    network = docker_json("network", "inspect", fixture["resources"]["networks"][0])[0]
    labels = network.get("Labels") or {}
    if labels.get("evpn.e2e.owner") != RUN_ID or labels.get("containerlab") != "":
        raise AcceptanceError("refusing to attach handler to a network not owned by this run")
    state = FIXTURE.lab.load(fixture_dir)
    if FIXTURE.lab.verify_owned(state)["network"] != network["Id"]:
        raise AcceptanceError("handler network differs from the Containerlab ownership receipt")
    command = [
        "docker", "create",
        "--name", HANDLER_NAME,
        "--label", f"evpn.e2e.owner={RUN_ID}",
        "--label", "evpn.e2e.fixture=netlink",
        "--label", f"containerlab={state['name']}",
        "--label", f"org.opencontainers.image.revision={git_head()}",
        "--network", network["Name"],
        "--ip", local_vtep,
        "--cap-drop", "ALL",
        "--cap-add", "NET_ADMIN",
        "--cap-add", "NET_RAW",
        "--security-opt", "no-new-privileges",
        "--tmpfs", "/tmp:rw,noexec,nosuid,size=64m",
        "-e", "BGP_AD_HANDLER_MODE=netlink",
        "-e", f"GOBGP_ENDPOINT={fixture['parameters']['gobgp_address']}:50051",
        "-e", f"DYNAMIC_ROUTING_VNIS={vni}",
        "-e", f"NETLINK_BRIDGE_DEV={BRIDGE}",
        "-e", f"NETLINK_VXLAN_DEV={VXLAN}",
        "-e", f"NETLINK_ADVERTISE_DEV={BRIDGE}",
        "-e", "NETLINK_MANAGE_VXLAN=true",
        "-e", "NETLINK_VXLAN_PORT=4789",
        handler_image,
    ]
    created = run(command)
    container_id = created.stdout.strip()
    REPORT["topology"]["handler_container_id"] = container_id
    add_owned_resource(
        "docker-container", container_id, "netlink", HANDLER_NAME, handler_image
    )
    write_json(REPORT_PATH, REPORT)
    own_container(container_id, RUN_ID, "netlink")
    image = docker_json("image", "inspect", handler_image)[0]
    REPORT["images"]["netlink_handler"] = {
        "reference": handler_image,
        "image_id": image["Id"],
        "repo_digests": image.get("RepoDigests", []),
        "source_revision": git_head(),
    }
    REPORT["topology"]["handler_network_id"] = network["Id"]
    write_json(REPORT_PATH, REPORT)
    run(["docker", "start", container_id])
    return container_id, image["Id"]


def run_route_helper(
    image: str,
    network_name: str,
    helper_ip: str,
    endpoint: dict[str, str],
    fixture: dict[str, Any],
    local_vtep: str,
    action: str,
    path_uuid: str = "",
) -> dict[str, str]:
    params = fixture["parameters"]
    args = [
        "/usr/local/bin/evpn-netlink-route",
        "--action", action,
        "--endpoint-id", f"netlink-{RUN_ID}",
        "--incarnation", RUN_ID,
        "--network-id", f"netlink-e2e-{RUN_ID}",
        "--mac", endpoint["mac"],
        "--ip", endpoint["ip"],
        "--vni", str(params["vni"]),
        "--rd", params["rd"],
        "--rt", params["rt"],
        "--local-vtep", local_vtep,
    ]
    if path_uuid:
        args.extend(["--path-uuid", path_uuid])
    name = f"evpn-netlink-route-{RUN_ID[:12]}-{action}"
    create = run(
        [
            "docker", "create",
            "--name", name,
            "--label", f"evpn.e2e.owner={RUN_ID}",
            "--label", "evpn.e2e.fixture=netlink-route-helper",
            "--label", f"containerlab={FIXTURE.lab.load(RUN_DIR)['name']}",
            "--network", network_name,
            "--ip", helper_ip,
            "-e", f"GOBGP_ENDPOINT={params['gobgp_address']}:50051",
            "--entrypoint", "/usr/local/bin/evpn-netlink-route",
            image,
            *args[1:],
        ]
    )
    helper_id = create.stdout.strip()
    REPORT["topology"].setdefault("route_helper_containers", []).append(helper_id)
    add_owned_resource(
        "docker-container",
        helper_id,
        "netlink-route-helper",
        name,
        image,
    )
    write_json(REPORT_PATH, REPORT)
    inspect = docker_json("inspect", helper_id)[0]
    labels = inspect.get("Config", {}).get("Labels") or {}
    if (
        labels.get("evpn.e2e.owner") != RUN_ID
        or labels.get("evpn.e2e.fixture") != "netlink-route-helper"
    ):
        raise AcceptanceError("route helper container ownership could not be verified")
    run(["docker", "start", helper_id])
    wait = run(["docker", "wait", helper_id], timeout=20, check=False)
    logs = run(["docker", "logs", helper_id], check=False).stdout
    save_evidence(f"route-helper/{action}-{helper_id[:12]}.txt", logs)
    if wait.returncode or wait.stdout.strip() != "0":
        raise AcceptanceError(f"route helper {action} failed:\n{logs}")
    try:
        result = json.loads(logs.strip().splitlines()[-1])
    except (IndexError, json.JSONDecodeError) as exc:
        raise AcceptanceError(f"route helper returned no JSON receipt:\n{logs}") from exc
    if action == "advertise" and not result.get("path_uuid"):
        raise AcceptanceError("GoBGP local route advertisement returned no exact path UUID")
    rm = run(["docker", "rm", helper_id], check=False)
    if rm.returncode:
        raise AcceptanceError(f"could not remove owned route helper {helper_id}: {rm.stdout}")
    mark_resource_removed(helper_id)
    return result


def assert_remote_state(
    snapshot: dict[str, str],
    endpoints: list[dict[str, Any]],
    remote_vtep: str,
    present: bool,
) -> tuple[bool, str]:
    okay = True
    details = []
    for endpoint in endpoints:
        fdb = exact_fdb(
            snapshot["fdb"], endpoint["mac"], remote_vtep, snapshot.get("fdb_device", "")
        )
        neighbor = exact_neighbor(snapshot["neighbors"], endpoint["ip"], endpoint["mac"])
        details.append(
            f"{endpoint['name']} mac={endpoint['mac']} ip={endpoint['ip']} "
            f"fdb={fdb} neighbor={neighbor}"
        )
        okay = okay and (fdb == present) and (neighbor == present)
    shared = vtep_neighbor(snapshot["vtep_neighbors"], remote_vtep)
    flood = flood_fdb(
        snapshot["fdb"], remote_vtep, snapshot.get("fdb_device", "")
    )
    details.append(f"shared_vtep_neighbor={shared} flood_fdb={flood}")
    return okay, "\n".join(details)


def expected_ping_packets(
    endpoint: dict[str, Any], local_ip: str, local_mac: str, reverse: bool
) -> list[tuple[str, str, int, str, str]]:
    source_ip, destination_ip = (
        (endpoint["ip"], local_ip) if reverse else (local_ip, endpoint["ip"])
    )
    source_mac, destination_mac = (
        (endpoint["mac"].lower(), local_mac.lower())
        if reverse
        else (local_mac.lower(), endpoint["mac"].lower())
    )
    return [
        (source_ip, destination_ip, 8, source_mac, destination_mac),
        (destination_ip, source_ip, 0, destination_mac, source_mac),
    ]


def inner_frame_error(frame: bytes, offset: int) -> str | None:
    ethernet = PCAP_DECODER.ethernet_packet(frame, offset)
    if ethernet is None:
        return "truncated inner Ethernet frame"
    ether_type, payload_offset = ethernet[2], ethernet[3]
    if ether_type == 0x0806 and len(frame) < payload_offset + 28:
        return "truncated inner ARP packet"
    if ether_type == 0x0800 and PCAP_DECODER.ipv4_packet(frame, payload_offset) is None:
        return "malformed or incomplete inner IPv4 packet"
    if ether_type == 0x86DD:
        if len(frame) < payload_offset + 40 or frame[payload_offset] >> 4 != 6:
            return "malformed or incomplete inner IPv6 packet"
        payload_length = struct.unpack_from("!H", frame, payload_offset + 4)[0]
        if len(frame) < payload_offset + 40 + payload_length:
            return "truncated inner IPv6 payload"
    return None


def decode_vxlan_records(
    records: list[dict[str, Any]],
) -> tuple[list[dict[str, Any]], list[dict[str, Any]], list[str]]:
    packets: list[dict[str, Any]] = []
    arp_packets: list[dict[str, Any]] = []
    errors = []
    for index, record in enumerate(records, start=1):
        frame = record.get("frame")
        if not isinstance(frame, bytes):
            errors.append(f"packet {index}: missing or invalid frame bytes")
            continue
        ethernet = PCAP_DECODER.ethernet_packet(frame)
        if ethernet is None:
            errors.append(f"packet {index}: truncated Ethernet frame")
            continue
        if ethernet[2] != 0x0800:
            errors.append(f"packet {index}: unsupported outer EtherType {ethernet[2]}")
            continue
        outer = PCAP_DECODER.ipv4_packet(frame, ethernet[3])
        if outer is None:
            errors.append(f"packet {index}: malformed or incomplete outer IPv4 packet")
            continue
        if outer["protocol"] != 17:
            errors.append(f"packet {index}: outer IPv4 packet is not UDP")
            continue
        udp_offset = outer["payloadOffset"]
        if outer["end"] < udp_offset + 8:
            errors.append(f"packet {index}: truncated UDP header")
            continue
        source_port, destination_port, udp_length = struct.unpack_from(
            "!HHH", frame, udp_offset
        )
        if destination_port != 4789:
            errors.append(f"packet {index}: UDP destination port is not 4789")
            continue
        if udp_length < 16 or udp_offset + udp_length > outer["end"]:
            errors.append(f"packet {index}: invalid or truncated VXLAN UDP payload")
            continue
        vxlan_offset = udp_offset + 8
        flags = frame[vxlan_offset]
        vni = int.from_bytes(frame[vxlan_offset + 4 : vxlan_offset + 7], "big")
        if not flags & 0x08:
            errors.append(f"packet {index}: VXLAN I flag is absent")
            continue
        inner_offset = vxlan_offset + 8
        vxlan_payload = frame[: udp_offset + udp_length]
        inner_error = inner_frame_error(vxlan_payload, inner_offset)
        if inner_error:
            errors.append(f"packet {index}: {inner_error}")
            continue
        inner = PCAP_DECODER.inner_packet(vxlan_payload, inner_offset)
        if inner is None:
            errors.append(f"packet {index}: malformed or incomplete inner Ethernet packet")
            continue
        summary = {
            "timestamp": record["timestamp"],
            "outerSource": outer["source"],
            "outerDestination": outer["destination"],
            "udpSourcePort": source_port,
            "udpDestinationPort": destination_port,
            "vni": vni,
            "inner": {
                key: value
                for key, value in inner.items()
                if key != "icmp"
            },
        }
        if inner.get("arp"):
            arp_packets.append(summary)
        if inner.get("icmp"):
            summary["inner"]["icmpType"] = inner["icmp"]["type"]
            summary["inner"]["icmpSequence"] = inner["icmp"]["sequence"]
            packets.append(summary)
    return packets, arp_packets, errors


def summarize_pcap(
    pcap_path: Path,
    capture_kind: str,
    expected_packets: list[tuple[str, str, int, str, str]],
    local_vtep: str,
    remote_vtep: str,
    vni: int,
    local_ip: str,
    remote_ip: str,
) -> dict[str, Any]:
    _, records = PCAP_DECODER.read_pcap(pcap_path)
    summary: dict[str, Any] = {
        "packetCount": len(records),
        "arpPacketCount": 0,
        "expectedPackets": [],
        "status": "FAIL",
    }
    if capture_kind == "vxlan":
        packets, arp_packets, decode_errors = decode_vxlan_records(records)
        matched: set[tuple[str, str, int, str, str]] = set()
        evidence = []
        for packet in packets:
            inner = packet["inner"]
            signature = (
                inner.get("sourceIp", ""),
                inner.get("destinationIp", ""),
                inner.get("icmpType", -1),
                inner.get("sourceMac", "").lower(),
                inner.get("destinationMac", "").lower(),
            )
            expected_outer = (
                (local_vtep, remote_vtep)
                if signature[0] == local_ip
                else (remote_vtep, local_vtep)
                if signature[0] == remote_ip
                else ("", "")
            )
            if (
                signature in expected_packets
                and packet["vni"] == vni
                and (packet["outerSource"], packet["outerDestination"])
                == expected_outer
            ):
                matched.add(signature)
                evidence.append(packet)
        summary["arpPacketCount"] = len(arp_packets)
        summary["vxlanPackets"] = packets
        summary["arpPackets"] = arp_packets
        summary["decodeErrors"] = decode_errors
        summary["expectedPackets"] = [
            {
                "sourceIp": item[0],
                "destinationIp": item[1],
                "icmpType": item[2],
                "sourceMac": item[3],
                "destinationMac": item[4],
            }
            for item in expected_packets
        ]
        summary["matchedExpectedPackets"] = evidence
        summary["status"] = (
            "PASS"
            if len(matched) == len(expected_packets) and not decode_errors
            else "FAIL"
        )
        return summary

    if capture_kind != "endpoint":
        raise ValueError(f"unsupported capture kind: {capture_kind}")
    matched = set()
    packets = []
    arp_packets = []
    decode_errors = []
    for index, record in enumerate(records, start=1):
        inner = PCAP_DECODER.inner_packet(record["frame"])
        if inner is None:
            decode_errors.append(
                f"packet {index}: malformed or incomplete Ethernet/IP packet"
            )
            continue
        if inner["etherType"] == 0x0806 and not inner.get("arp"):
            decode_errors.append(f"packet {index}: malformed or incomplete ARP packet")
            continue
        if inner["etherType"] == 0x0800 and not inner.get("icmp"):
            decode_errors.append(
                f"packet {index}: captured IPv4 packet is not a complete ICMP packet"
            )
            continue
        if not inner.get("arp") and not inner.get("icmp"):
            decode_errors.append(f"packet {index}: unsupported captured packet type")
            continue
        if inner.get("arp"):
            arp_packets.append(
                {"timestamp": record["timestamp"], "arp": inner["arp"]}
            )
        if not inner.get("icmp"):
            continue
        signature = (
            inner.get("sourceIp", ""),
            inner.get("destinationIp", ""),
            inner["icmp"]["type"],
            inner.get("sourceMac", "").lower(),
            inner.get("destinationMac", "").lower(),
        )
        packets.append(
            {
                "timestamp": record["timestamp"],
                "sourceIp": signature[0],
                "destinationIp": signature[1],
                "icmpType": signature[2],
                "sourceMac": signature[3],
                "destinationMac": signature[4],
                "icmpSequence": inner["icmp"]["sequence"],
            }
        )
        if signature in expected_packets:
            matched.add(signature)
    summary["arpPacketCount"] = len(arp_packets)
    summary["arpPackets"] = arp_packets
    summary["endpointPackets"] = packets
    summary["decodeErrors"] = decode_errors
    summary["expectedPackets"] = [
        {
            "sourceIp": item[0],
            "destinationIp": item[1],
            "icmpType": item[2],
            "sourceMac": item[3],
            "destinationMac": item[4],
        }
        for item in expected_packets
    ]
    summary["matchedExpectedPackets"] = [
        packet for packet in packets
        if (
            packet["sourceIp"],
            packet["destinationIp"],
            packet["icmpType"],
            packet["sourceMac"],
            packet["destinationMac"],
        ) in matched
    ]
    summary["status"] = (
        "PASS" if len(matched) == len(expected_packets) and not decode_errors else "FAIL"
    )
    return summary


def start_pcap_capture(
    handler_id: str, label: str, interface: str, bpf_filter: str
) -> dict[str, Any]:
    safe_label = re.sub(r"[^A-Za-z0-9._-]", "-", label)
    pcap_path = EVIDENCE / "captures" / "pcap" / f"{safe_label}.pcap"
    if pcap_path.exists():
        raise AcceptanceError(f"refusing to overwrite packet capture {pcap_path}")
    pcap_path.parent.mkdir(parents=True, exist_ok=True)
    namespace = container_exec(handler_id, "readlink", "/proc/self/ns/net").strip()
    pcap_file = pcap_path.open("wb")
    command = [
        "docker", "exec", handler_id, "sh", "-c",
        'tcpdump -nn -U -s 0 -c 256 -i "$1" -w - "$2" & '
        'capture_pid=$!; printf "CAPTURE_PID=%s\\n" "$capture_pid" >&2; '
        'wait "$capture_pid"',
        "sh", interface, bpf_filter,
    ]
    process = subprocess.Popen(
        command, stdout=pcap_file, stderr=subprocess.PIPE, text=True
    )
    capture: dict[str, Any] = {
        "name": label,
        "interface": interface,
        "namespace": {
            "containerId": handler_id,
            "networkNamespace": namespace,
        },
        "filter": bpf_filter,
        "startedAt": now(),
        "pcapPath": pcap_path,
        "process": process,
        "pcapFile": pcap_file,
        "stderrLines": [],
        "remotePid": None,
        "startError": None,
    }
    deadline = time.monotonic() + 8
    listening = False
    try:
        while time.monotonic() < deadline:
            if process.poll() is not None:
                raise AcceptanceError("tcpdump exited before capture became ready")
            ready, _, _ = select.select([process.stderr], [], [], 0.25)
            if not ready:
                continue
            line = process.stderr.readline()
            if not line:
                continue
            capture["stderrLines"].append(line.rstrip())
            pid_match = re.search(r"CAPTURE_PID=(\d+)", line)
            if pid_match:
                capture["remotePid"] = pid_match.group(1)
            if "listening on" in line.lower():
                listening = True
            if capture["remotePid"] and listening:
                return capture
        raise AcceptanceError("timed out waiting for tcpdump readiness evidence")
    except Exception as exc:
        capture["startError"] = str(exc)
        signal_exit_code = None
        if process.poll() is None and capture.get("remotePid"):
            signal_result = run(
                [
                    "docker", "exec", handler_id, "sh", "-c",
                    'kill -INT "$1"', "sh", str(capture["remotePid"]),
                ],
                check=False,
            )
            signal_exit_code = signal_result.returncode
            if signal_result.returncode:
                run(
                    [
                        "docker", "exec", handler_id, "sh", "-c",
                        'kill -KILL "$1"', "sh", str(capture["remotePid"]),
                    ],
                    check=False,
                )
        if process.poll() is None and not capture.get("remotePid"):
            process.kill()
        try:
            _, remaining_stderr = process.communicate(timeout=10)
        except subprocess.TimeoutExpired:
            process.kill()
            _, remaining_stderr = process.communicate()
        capture["completedAt"] = now()
        capture["exitCode"] = process.returncode
        capture["signalExitCode"] = signal_exit_code
        capture["stderr"] = "\n".join(
            [
                *capture["stderrLines"],
                (remaining_stderr or "").strip(),
            ]
        ).strip()
        capture["stopError"] = None
        capture["pcapBytes"] = pcap_path.stat().st_size if pcap_path.exists() else 0
        capture["pcapSha256"] = (
            hashlib.sha256(pcap_path.read_bytes()).hexdigest()
            if pcap_path.exists()
            else None
        )
        capture["status"] = "FAIL"
        capture["pcapFile"].flush()
        capture["pcapFile"].close()
        return capture


def finish_pcap_capture(handler_id: str, capture: dict[str, Any]) -> dict[str, Any]:
    process: subprocess.Popen[str] = capture["process"]
    signal_exit_code = None
    stop_error = None
    if process.poll() is None and capture.get("remotePid"):
        signal_result = run(
            [
                "docker", "exec", handler_id, "sh", "-c",
                'kill -INT "$1"', "sh", str(capture["remotePid"]),
            ],
            check=False,
        )
        signal_exit_code = signal_result.returncode
        if signal_result.returncode:
            stop_error = signal_result.stdout
            run(
                [
                    "docker", "exec", handler_id, "sh", "-c",
                    'kill -KILL "$1"', "sh", str(capture["remotePid"]),
                ],
                check=False,
            )
    elif process.poll() is not None:
        signal_exit_code = 0
    try:
        _, remaining_stderr = process.communicate(timeout=10)
    except subprocess.TimeoutExpired:
        process.kill()
        _, remaining_stderr = process.communicate()
        stop_error = (stop_error or "") + "\nlocal tcpdump exec did not stop after SIGINT"
    capture["pcapFile"].flush()
    capture["pcapFile"].close()
    capture["completedAt"] = now()
    capture["exitCode"] = process.returncode
    capture["signalExitCode"] = signal_exit_code
    capture["stderr"] = "\n".join(
        [*capture["stderrLines"], (remaining_stderr or "").strip()]
    ).strip()
    capture["stopError"] = stop_error
    path: Path = capture["pcapPath"]
    capture["pcapBytes"] = path.stat().st_size if path.exists() else 0
    capture["pcapSha256"] = (
        hashlib.sha256(path.read_bytes()).hexdigest() if path.exists() else None
    )
    return capture


def pcap_capture_passed(capture: dict[str, Any]) -> bool:
    return (
        capture.get("exitCode") == 0
        and capture.get("signalExitCode") == 0
        and capture.get("stopError") is None
        and capture.get("decode", {}).get("status") == "PASS"
        and capture.get("startError") is None
    )


def capture_and_ping(
    handler_id: str,
    frr_id: str,
    endpoint: dict[str, Any],
    local_ip: str,
    local_mac: str,
    local_vtep: str,
    remote_vtep: str,
    vni: int,
    label: str,
    reverse: bool = False,
) -> tuple[bool, bool, str]:
    direction = "reverse" if reverse else "forward"
    capture_specs = [
        ("vxlan", "eth0", f"host {remote_vtep} and udp port 4789"),
        (
            "endpoint",
            BRIDGE,
            f"arp or (icmp and host {endpoint['ip']} and host {local_ip})",
        ),
    ]
    expected = expected_ping_packets(endpoint, local_ip, local_mac, reverse)
    captures: list[dict[str, Any]] = []
    capture_start_errors: list[dict[str, Any]] = []
    for kind, interface, bpf_filter in capture_specs:
        try:
            capture = start_pcap_capture(
                handler_id, f"{label}-{kind}", interface, bpf_filter
            )
            capture["kind"] = kind
            captures.append(capture)
        except Exception as exc:
            capture_start_errors.append(
                {
                    "name": f"{label}-{kind}",
                    "interface": interface,
                    "filter": bpf_filter,
                    "startedAt": now(),
                    "status": "FAIL",
                    "error": f"{type(exc).__name__}: {exc}",
                }
            )

    ping = None
    ping_error = None
    try:
        if reverse:
            ping = run(
                [
                    "docker", "exec", frr_id, "ping", "-n", "-c", "3", "-W",
                    "2", "-I", "br-e2e", local_ip,
                ],
                timeout=15,
                check=False,
            )
        else:
            ping = run(
                [
                    "docker", "exec", handler_id, "ping", "-n", "-c", "3",
                    "-W", "2", "-I", local_ip, endpoint["ip"],
                ],
                timeout=15,
                check=False,
            )
    except Exception as exc:
        ping_error = f"{type(exc).__name__}: {exc}"

    capture_results = []
    for capture in captures:
        if capture.get("startError") is None:
            try:
                capture = finish_pcap_capture(handler_id, capture)
                expected_vni = vni if capture["kind"] == "vxlan" else 0
                summary = summarize_pcap(
                    capture["pcapPath"],
                    capture["kind"],
                    expected,
                    local_vtep,
                    remote_vtep,
                    expected_vni,
                    local_ip,
                    endpoint["ip"],
                )
                capture["decode"] = summary
            except Exception as exc:
                capture["decode"] = {
                    "status": "FAIL",
                    "error": f"{type(exc).__name__}: {exc}",
                }
        else:
            capture["decode"] = {
                "status": "FAIL",
                "error": capture["startError"],
            }
        capture["status"] = "PASS" if pcap_capture_passed(capture) else "FAIL"
        capture_results.append(
            {
                key: value
                for key, value in capture.items()
                if key not in {"process", "pcapFile", "pcapPath"}
            }
            | {
                "pcap": str(capture["pcapPath"].relative_to(RUN_DIR)),
            }
        )

    ping_ok = (
        ping is not None
        and ping.returncode == 0
        and "0% packet loss" in ping.stdout
    )
    capture_ok = (
        not capture_start_errors
        and len(capture_results) == len(capture_specs)
        and all(capture["status"] == "PASS" for capture in capture_results)
    )
    detail_obj = {
        "direction": direction,
        "endpoint": endpoint,
        "localIp": local_ip,
        "localMac": local_mac,
        "localVtep": local_vtep,
        "remoteVtep": remote_vtep,
        "vni": vni,
        "ping": {
            "exitCode": ping.returncode if ping else None,
            "output": ping.stdout if ping else "",
            "error": ping_error,
            "status": "PASS" if ping_ok else "FAIL",
        },
        "captureStartErrors": capture_start_errors,
        "captures": capture_results,
        "status": "PASS" if ping_ok and capture_ok else "FAIL",
    }
    evidence_path = save_evidence(
        f"captures/{label}.json", json.dumps(detail_obj, indent=2)
    )
    detail_obj["evidence"] = evidence_path
    return ping_ok, capture_ok, json.dumps(detail_obj, indent=2)


RUN_ID = ""
RUN_DIR = Path(".")
EVIDENCE = Path(".")
REPORT_PATH = Path(".")
OWNERSHIP_MANIFEST_PATH = Path(".")
REPORT: dict[str, Any] = {}
OWNERSHIP_MANIFEST: dict[str, Any] = {}
BRIDGE = "br-e2e"
VXLAN = "vx-e2e"
HANDLER_NAME = ""


def add_owned_resource(
    resource_type: str,
    resource_id: str,
    fixture: str,
    name: str,
    image: str = "",
    container_id: str = "",
) -> None:
    resources = OWNERSHIP_MANIFEST["acceptance_resources"]
    if not any(resource["id"] == resource_id for resource in resources):
        resources.append(
            {
                "type": resource_type,
                "id": resource_id,
                "name": name,
                "owner_run_id": RUN_ID,
                "fixture": fixture,
                "image": image or None,
                "container_id": container_id or None,
                "state": "created",
                "created_at": now(),
            }
        )
    write_json(OWNERSHIP_MANIFEST_PATH, OWNERSHIP_MANIFEST)
    if resource_type == "docker-container":
        FIXTURE.lab.register_auxiliary(RUN_DIR, resource_id)


def mark_resource_removed(resource_id: str) -> None:
    for resource in OWNERSHIP_MANIFEST.get("acceptance_resources", []):
        if resource["id"] == resource_id:
            resource["state"] = "removed"
            resource["removed_at"] = now()
    write_json(OWNERSHIP_MANIFEST_PATH, OWNERSHIP_MANIFEST)


def mark_container_resources_removed(container_id: str) -> None:
    for resource in OWNERSHIP_MANIFEST.get("acceptance_resources", []):
        if resource.get("container_id") == container_id:
            resource["state"] = "removed"
            resource["removed_at"] = now()
    write_json(OWNERSHIP_MANIFEST_PATH, OWNERSHIP_MANIFEST)


def acceptance(args: argparse.Namespace) -> int:
    global RUN_ID, RUN_DIR, EVIDENCE, REPORT_PATH, OWNERSHIP_MANIFEST_PATH
    global REPORT, OWNERSHIP_MANIFEST, BRIDGE, VXLAN, HANDLER_NAME
    if args.authorize_disposable != AUTHORIZATION:
        raise AcceptanceError("explicit disposable-environment authorization is required")
    go_build_image = require_digest(args.go_build_image, "Go build image")
    runtime_image = require_digest(args.runtime_image, "Netlink runtime image")
    if shutil.which("docker") is None:
        raise AcceptanceError("Docker CLI is required")
    run(["docker", "info", "--format", "{{.ServerVersion}}"])
    RUN_ID = str(uuid.UUID(args.run_id))
    image_tag = f"evpn-netlink-e2e:{RUN_ID[:12]}"
    if run(["docker", "image", "inspect", image_tag], check=False).returncode == 0:
        raise AcceptanceError(
            f"refusing to overwrite pre-existing run-specific image {image_tag}"
        )
    state_root = args.state_root.expanduser().resolve()
    RUN_DIR = fixture_directory(state_root, RUN_ID)
    fixture = load_fixture(RUN_DIR, args.authorize_disposable)
    network, gobgp_id, frr_id = validate_fixture_resources(fixture, RUN_ID)
    BRIDGE = f"br-e2e-{RUN_ID[:8]}"
    VXLAN = f"vx-e2e-{RUN_ID[:8]}"
    HANDLER_NAME = f"evpn-netlink-handler-{RUN_ID[:12]}"
    EVIDENCE = RUN_DIR / "evidence" / "netlink-acceptance"
    REPORT_PATH = RUN_DIR / "netlink-report.json"
    OWNERSHIP_MANIFEST_PATH = RUN_DIR / "netlink-ownership.json"
    if REPORT_PATH.exists() or OWNERSHIP_MANIFEST_PATH.exists() or EVIDENCE.exists():
        raise AcceptanceError(
            "acceptance evidence already exists for this fixture run; refusing to overwrite it"
        )
    if (RUN_DIR / "netlink-build-context").exists():
        raise AcceptanceError("an earlier build context exists; refusing to overwrite it")
    EVIDENCE.mkdir(parents=True, exist_ok=False)
    params = fixture["parameters"]
    endpoints = params["endpoints"]
    control_primary = next(ep for ep in endpoints if ep["name"] == "primary")
    primary = next(ep for ep in endpoints if ep["name"] == "netlink-primary")
    secondary = next(ep for ep in endpoints if ep["name"] == "secondary")
    remote_endpoints = (primary, secondary)
    subnet = ipaddress.ip_network(params["subnet"])
    local_vtep, helper_ip = allocate_fixture_addresses(
        str(subnet),
        [
            str(subnet.network_address + 1),
            params["gobgp_address"],
            params["frr_address"],
            params["remote_vtep"],
            *(endpoint["ip"] for endpoint in endpoints),
        ],
        2,
    )
    local_mac = FIXTURE.mac_for_role(RUN_ID, "netlink-local-endpoint")
    local_bridge_mac = FIXTURE.mac_for_role(RUN_ID, "netlink-bridge")
    local_gateway, local_ip = local_endpoint_addresses(RUN_ID)
    local_endpoint = {
        "name": "local",
        "mac": local_mac,
        "ip": local_ip,
        "interface": LOCAL_ENDPOINT_INTERFACE,
        "gateway": local_gateway,
        "local_vtep": local_vtep,
        "originator": "gobgp",
        "receiver": "frr",
    }
    REPORT = {
        "schema_version": 1,
        "run_id": RUN_ID,
        "source_commit": git_head(),
        "created_at": now(),
        "authorization": AUTHORIZATION,
        "images": {
            "gobgp": fixture["images"]["gobgp"],
            "frr": fixture["images"]["frr"],
            "go_build_image": args.go_build_image,
            "runtime_base": args.runtime_image,
        },
        "topology": {
            "fixture_network_id": network["Id"],
            "fixture_network_name": network["Name"],
            "subnet": params["subnet"],
            "vni": params["vni"],
            "remote_vtep": params["remote_vtep"],
            "local_vtep": local_vtep,
            "endpoints": endpoints,
            "local_endpoint": local_endpoint,
            "netlink_bridge_mac": local_bridge_mac,
            "netlink_managed_vxlan": True,
            "host_network": False,
        },
        "gates": [],
        "cleanup": {"status": "NOT RUN", "details": []},
        "overall": "NOT RUN",
    }
    OWNERSHIP_MANIFEST = {
        "schema_version": 1,
        "run_id": RUN_ID,
        "authorization": AUTHORIZATION,
        "source_commit": REPORT["source_commit"],
        "fixture_manifest": str(RUN_DIR / "manifest.json"),
        "fixture_resources": {
            "network_ids": fixture["resources"]["networks"],
            "container_ids": fixture["resources"]["containers"],
        },
        "acceptance_resources": [],
        "build_context": str(RUN_DIR / "netlink-build-context"),
    }
    write_json(REPORT_PATH, REPORT)
    write_json(OWNERSHIP_MANIFEST_PATH, OWNERSHIP_MANIFEST)
    handler_id = ""
    local_route_uuid = ""
    built_image_id = ""
    build_attempted = False
    required_gates = [
        "GoBGP FRR session and three Type-2 routes",
        "Netlink handler startup and Type-2 consumption",
        "local endpoint route advertised to both peers",
        "managed bridge and VXLAN device ownership",
        "VXLAN VNI local VTEP and UDP port configuration",
        "exact remote MAC FDB entries",
        "exact remote IP MAC neighbors",
        "shared VTEP neighbor and flood FDB",
        "forward endpoint connectivity",
        "reverse endpoint connectivity",
        "VXLAN capture and endpoint packet attribution",
        "first remote MAC withdrawal",
        "first route-specific state removal",
        "second MAC state and forwarding preservation",
        "re-advertise first remote MAC",
        "both endpoints recover after re-advertisement",
        "final remote MAC withdrawal",
        "final route-specific state removal",
    ]
    try:
        def verify_remote_routes() -> tuple[str, str]:
            try:
                FIXTURE.validate_mac_identities(
                    params["bridge_mac"],
                    endpoints,
                    local_endpoint,
                    local_bridge_mac,
                )
            except FIXTURE.FixtureError as exc:
                return check(False, f"MAC identity preflight failed: {exc}")
            if (
                len({ep["mac"] for ep in (control_primary, *remote_endpoints)}) != 3
                or len({ep["ip"] for ep in (control_primary, *remote_endpoints)}) != 3
            ):
                return check(
                    False,
                    "fixture route identities are not distinct",
                )
            for endpoint in remote_endpoints:
                endpoint_setup_output = verify_frr_endpoint(
                    fixture, RUN_ID, endpoint
                )
                save_evidence(
                    f"topology/frr-{endpoint['name']}-endpoint.txt",
                    endpoint_setup_output,
                )
            for endpoint in (control_primary, *remote_endpoints):
                current = json.loads((RUN_DIR / "manifest.json").read_text())
                current_ep = next(
                    ep for ep in current["parameters"]["endpoints"]
                    if ep["name"] == endpoint["name"]
                )
                if current_ep["state"] != "advertised":
                    fixture_cli(
                        RUN_DIR, args.authorize_disposable, "advertise", RUN_ID, endpoint["name"]
                    )
            fixture_cli(RUN_DIR, args.authorize_disposable, "observe", RUN_ID)
            peer_detail = FIXTURE.wait_for_peer(
                RUN_DIR, fixture, params["gobgp_address"], params["frr_address"]
            )
            family_detail = FIXTURE.wait_for_evpn_family(
                RUN_DIR, fixture, params["gobgp_address"], params["frr_address"]
            )
            routes_ok, route_detail = record_routes(
                RUN_DIR, fixture, [control_primary, *remote_endpoints], True
            )
            detail = (
                f"three distinct routes; two remote MACs share VTEP "
                f"{params['remote_vtep']}\n"
                f"bilateral peer and family checks: PASS\n"
                f"BGP peer evidence:\n{peer_detail}\n"
                f"EVPN AFI/SAFI evidence:\n{family_detail}\n"
                f"routes_ok={routes_ok}\n"
                f"{route_detail}"
            )
            return check(routes_ok, detail)

        routes_ok = gate("GoBGP FRR session and three Type-2 routes", verify_remote_routes)
        if not routes_ok:
            raise AcceptanceError("shared fixture route identity gate failed")

        build_context = prepare_build_context(RUN_DIR)
        build_attempted = True
        build = run(
            [
                "docker", "build",
                "--build-arg", f"GO_BUILD_IMAGE={go_build_image}",
                "--build-arg", f"NETLINK_RUNTIME_IMAGE={runtime_image}",
                "--label", f"org.opencontainers.image.revision={git_head()}",
                "--label", f"evpn.e2e.owner={RUN_ID}",
                "--label", "evpn.e2e.fixture=netlink-image",
                "--tag", image_tag,
                "--file", str(DOCKERFILE),
                str(build_context),
            ],
            timeout=900,
        )
        save_evidence("build/netlink-handler-image.txt", build.stdout)
        built_image = docker_json("image", "inspect", image_tag)[0]
        built_image_id = built_image["Id"]
        add_owned_resource(
            "docker-image",
            built_image_id,
            "netlink",
            image_tag,
            image_tag,
        )
        handler_id, image_id = create_handler(
            fixture, RUN_DIR, image_tag, local_vtep, params["vni"]
        )
        REPORT["images"]["netlink_handler"]["image_id"] = image_id
        REPORT["images"]["netlink_handler"]["build_image"] = go_build_image
        REPORT["images"]["netlink_handler"]["runtime_base"] = runtime_image
        write_json(REPORT_PATH, REPORT)

        def verify_handler_startup() -> tuple[str, str]:
            startup_logs = wait_handler_log(
                handler_id,
                ["BGP AD handler mode: netlink", "Waiting for EVPN MAC/IP updates..."],
            )
            return check(
                all(
                    f"netlink: installed MAC={ep['mac']} IP={ep['ip']} "
                    f"VNI={params['vni']} remoteVTEP={params['remote_vtep']}"
                    in startup_logs
                    for ep in remote_endpoints
                ),
                startup_logs,
            )

        startup_ok = gate(
            "Netlink handler startup and Type-2 consumption", verify_handler_startup
        )
        if not startup_ok:
            raise AcceptanceError("Netlink handler startup/consumption gate failed")

        endpoint_commands = local_endpoint_setup_commands(
            BRIDGE,
            local_bridge_mac,
            local_endpoint,
            local_gateway,
            [endpoint["ip"] for endpoint in remote_endpoints],
        )
        for command in endpoint_commands:
            run(["docker", "exec", handler_id, *command])

        def advertise_local_endpoint() -> tuple[str, str]:
            nonlocal local_route_uuid
            interface_result = run(
                [
                    "docker",
                    "exec",
                    handler_id,
                    "ip",
                    "-j",
                    "address",
                    "show",
                    "dev",
                    LOCAL_ENDPOINT_INTERFACE,
                ]
            )
            interface_document = json.loads(interface_result.stdout)
            interface_matches, interface_detail = local_endpoint_interface_matches(
                local_endpoint, interface_document
            )
            save_evidence(
                "topology/local-endpoint-interface.json",
                json.dumps(
                    {
                        "interface": LOCAL_ENDPOINT_INTERFACE,
                        "bridgePort": LOCAL_ENDPOINT_BRIDGE_PORT,
                        "bridge": BRIDGE,
                        "routeEndpoint": {
                            "mac": local_endpoint["mac"],
                            "ip": local_endpoint["ip"],
                        },
                        "observation": interface_document,
                        "matches": interface_matches,
                    },
                    indent=2,
                ),
            )
            if not interface_matches:
                return check(False, f"local endpoint interface identity failed: {interface_detail}")

            local_receipt = run_route_helper(
                image_tag,
                network["Name"],
                helper_ip,
                local_endpoint,
                fixture,
                local_vtep,
                "advertise",
            )
            local_route_uuid = local_receipt["path_uuid"]
            REPORT["topology"]["local_type2_path_uuid"] = local_route_uuid
            write_json(REPORT_PATH, REPORT)
            deadline = time.monotonic() + WAIT_TIMEOUT
            local_route_detail = ""
            local_route_ok = False
            while time.monotonic() < deadline:
                local_route_ok, local_route_detail = record_routes(
                    RUN_DIR,
                    fixture,
                    [local_endpoint],
                    True,
                )
                if local_route_ok:
                    break
                time.sleep(2)
            diagnostic_detail = capture_local_route_diagnostics(
                RUN_DIR, fixture, local_endpoint, local_vtep
            )
            return check(
                local_route_ok,
                f"path_uuid={local_route_uuid}\n{local_route_detail}\n"
                f"export/import diagnostics:\n{diagnostic_detail}",
            )

        local_route_ok = gate(
            "local endpoint route advertised to both peers",
            advertise_local_endpoint,
        )
        if not local_route_ok:
            raise AcceptanceError("local Type-2 advertisement gate failed")

        before = kernel_snapshot(handler_id)
        save_evidence("kernel/before-withdrawal.json", json.dumps(before, indent=2))
        gate("managed bridge and VXLAN device ownership", lambda: check(
            bridge_configuration(before),
            f"bridge:\n{before['bridge_link']}\nVXLAN:\n{before['link']}",
        ))
        for interface_name in (BRIDGE, VXLAN):
            add_owned_resource(
                "container-interface",
                f"{handler_id}:{interface_name}",
                "netlink",
                interface_name,
                container_id=handler_id,
            )
        gate("VXLAN VNI local VTEP and UDP port configuration", lambda: check(
            link_configuration(before, params["vni"], local_vtep),
            f"link:\n{before['link']}\nunderlay:\n{before['underlay']}",
        ))
        gate("exact remote MAC FDB entries", lambda: check(
            all(
                exact_fdb(
                    before["fdb"],
                    ep["mac"],
                    params["remote_vtep"],
                    before["fdb_device"],
                )
                for ep in remote_endpoints
            ),
            before["fdb"],
        ))
        gate("exact remote IP MAC neighbors", lambda: check(
            all(exact_neighbor(before["neighbors"], ep["ip"], ep["mac"]) for ep in remote_endpoints),
            before["neighbors"],
        ))
        gate("shared VTEP neighbor and flood FDB", lambda: check(
            vtep_neighbor(before["vtep_neighbors"], params["remote_vtep"])
            and flood_fdb(
                before["fdb"], params["remote_vtep"], before["fdb_device"]
            ),
            f"VTEP neighbors:\n{before['vtep_neighbors']}\nFDB:\n{before['fdb']}",
        ))

        forward_ok, forward_capture_ok, forward_detail = capture_and_ping(
            handler_id,
            frr_id,
            primary,
            local_ip,
            local_mac,
            local_vtep,
            params["remote_vtep"],
            params["vni"],
            "initial-primary-forward",
        )
        gate("forward endpoint connectivity", lambda: check(forward_ok, forward_detail))
        reverse_ok, reverse_capture_ok, reverse_detail = capture_and_ping(
            handler_id,
            frr_id,
            primary,
            local_ip,
            local_mac,
            local_vtep,
            params["remote_vtep"],
            params["vni"],
            "initial-primary-reverse",
            reverse=True,
        )
        gate("reverse endpoint connectivity", lambda: check(reverse_ok, reverse_detail))
        capture_ok = forward_capture_ok and reverse_capture_ok
        gate("VXLAN capture and endpoint packet attribution", lambda: check(
            capture_ok,
            f"forward:\n{forward_detail}\nreverse:\n{reverse_detail}",
        ))

        # Individual withdrawals are required even when the first exposes the known
        # VTEP-wide cleanup regression; continue to gather final-state evidence.
        def withdraw_primary() -> tuple[str, str]:
            try:
                result = fixture_cli(
                    RUN_DIR,
                    args.authorize_disposable,
                    "withdraw",
                    RUN_ID,
                    primary["name"],
                )
                return check(True, result)
            except Exception as exc:
                return check(False, str(exc))

        gate("first remote MAC withdrawal", withdraw_primary)
        try:
            first_logs = wait_handler_log(
                handler_id,
                [f"netlink: removed MAC={primary['mac']} IP={primary['ip']}"],
                timeout=30,
            )
        except Exception as exc:
            first_logs = f"withdrawal log not observed: {exc}"
        first = kernel_snapshot(handler_id)
        save_evidence("kernel/after-first-withdrawal.json", json.dumps(first, indent=2))
        fixture_cli(RUN_DIR, args.authorize_disposable, "observe", RUN_ID)
        primary_absent, primary_absence_detail = record_routes(
            RUN_DIR, fixture, [primary], False
        )
        secondary_present, secondary_presence_detail = record_routes(
            RUN_DIR, fixture, [secondary], True
        )
        control_primary_present, control_primary_detail = record_routes(
            RUN_DIR, fixture, [control_primary], True
        )
        gate("first route-specific state removal", lambda: check(
            primary_absent
            and secondary_present
            and control_primary_present
            and not exact_fdb(
                first["fdb"],
                primary["mac"],
                params["remote_vtep"],
                first["fdb_device"],
            )
            and not exact_neighbor(first["neighbors"], primary["ip"], primary["mac"]),
            f"{first_logs}\n{primary_absence_detail}\n{secondary_presence_detail}\n"
            f"{control_primary_detail}\n"
            f"{json.dumps(first, indent=2)}",
        ))
        sibling_state = (
            exact_fdb(
                first["fdb"],
                secondary["mac"],
                params["remote_vtep"],
                first["fdb_device"],
            )
            and exact_neighbor(first["neighbors"], secondary["ip"], secondary["mac"])
        )
        shared_state = (
            vtep_neighbor(first["vtep_neighbors"], params["remote_vtep"])
            and flood_fdb(
                first["fdb"], params["remote_vtep"], first["fdb_device"]
            )
        )
        sibling_forward, sibling_capture_ok, sibling_detail = capture_and_ping(
            handler_id,
            frr_id,
            secondary,
            local_ip,
            local_mac,
            local_vtep,
            params["remote_vtep"],
            params["vni"],
            "after-primary-withdrawal-secondary-forward",
        )
        gate("second MAC state and forwarding preservation", lambda: check(
            sibling_state and shared_state and sibling_forward and sibling_capture_ok,
            f"sibling route state={sibling_state}; shared VTEP state={shared_state}\n{sibling_detail}",
        ))

        re_advertisement = {"prior_install_count": 0}
        install_log_pattern = (
            f"netlink: installed MAC={primary['mac']} IP={primary['ip']} "
            f"VNI={params['vni']} remoteVTEP={params['remote_vtep']}"
        )

        def re_advertise_primary() -> tuple[str, str]:
            prior_logs = run(
                ["docker", "logs", "--tail", "2000", handler_id], check=False
            ).stdout
            re_advertisement["prior_install_count"] = prior_logs.count(
                install_log_pattern
            )
            try:
                output = fixture_cli(
                    RUN_DIR,
                    args.authorize_disposable,
                    "advertise",
                    RUN_ID,
                    primary["name"],
                )
                return check(True, output)
            except Exception as exc:
                return check(False, str(exc))

        re_advertise_ok = gate(
            "re-advertise first remote MAC", re_advertise_primary
        )
        recovery_ok = False
        recovery_detail = "primary re-advertisement failed"
        if re_advertise_ok:
            try:
                fixture_cli(
                    RUN_DIR,
                    args.authorize_disposable,
                    "observe",
                    RUN_ID,
                )
                routes_recovered, route_recovery_detail = record_routes(
                    RUN_DIR, fixture, [control_primary, primary, secondary], True
                )
                fixture_logs = wait_handler_log_occurrences(
                    handler_id,
                    install_log_pattern,
                    re_advertisement["prior_install_count"] + 1,
                )
                recovered_state = kernel_snapshot(handler_id)
                save_evidence(
                    "kernel/after-readvertisement.json",
                    json.dumps(recovered_state, indent=2),
                )
                forward_recovered = []
                reverse_recovered = []
                capture_details = []
                for endpoint in remote_endpoints:
                    forward, forward_capture, forward_detail = capture_and_ping(
                        handler_id,
                        frr_id,
                        endpoint,
                        local_ip,
                        local_mac,
                        local_vtep,
                        params["remote_vtep"],
                        params["vni"],
                        f"readvertisement-{endpoint['name']}-forward",
                    )
                    reverse, reverse_capture, reverse_detail = capture_and_ping(
                        handler_id,
                        frr_id,
                        endpoint,
                        local_ip,
                        local_mac,
                        local_vtep,
                        params["remote_vtep"],
                        params["vni"],
                        f"readvertisement-{endpoint['name']}-reverse",
                        reverse=True,
                    )
                    forward_recovered.append(forward and forward_capture)
                    reverse_recovered.append(reverse and reverse_capture)
                    capture_details.extend([forward_detail, reverse_detail])
                    save_evidence(
                        f"captures/readvertisement-{endpoint['name']}-forward.txt",
                        forward_detail,
                    )
                    save_evidence(
                        f"captures/readvertisement-{endpoint['name']}-reverse.txt",
                        reverse_detail,
                    )
                state_recovered = (
                    all(
                        exact_fdb(
                            recovered_state["fdb"],
                            ep["mac"],
                            params["remote_vtep"],
                            recovered_state["fdb_device"],
                        )
                        and exact_neighbor(
                            recovered_state["neighbors"], ep["ip"], ep["mac"]
                        )
                        for ep in remote_endpoints
                    )
                    and vtep_neighbor(
                        recovered_state["vtep_neighbors"], params["remote_vtep"]
                    )
                    and flood_fdb(
                        recovered_state["fdb"],
                        params["remote_vtep"],
                        recovered_state["fdb_device"],
                    )
                )
                recovery_ok = (
                    routes_recovered
                    and state_recovered
                    and all(forward_recovered)
                    and all(reverse_recovered)
                )
                recovery_detail = (
                    f"{route_recovery_detail}\nstate_recovered={state_recovered}\n"
                    f"forward={forward_recovered} reverse={reverse_recovered}\n"
                    f"handler log:\n{fixture_logs}\n"
                    + "\n".join(capture_details)
                )
            except Exception as exc:
                recovery_detail = f"re-advertisement recovery evidence failed: {exc}"
        gate(
            "both endpoints recover after re-advertisement",
            lambda: check(recovery_ok, recovery_detail),
        )

        def withdraw_secondary() -> tuple[str, str]:
            try:
                result = fixture_cli(
                    RUN_DIR,
                    args.authorize_disposable,
                    "withdraw",
                    RUN_ID,
                    secondary["name"],
                )
                return check(True, result)
            except Exception as exc:
                return check(False, str(exc))

        gate("final remote MAC withdrawal", withdraw_secondary)
        try:
            final_logs = wait_handler_log(
                handler_id,
                [f"netlink: removed MAC={secondary['mac']} IP={secondary['ip']}"],
                timeout=30,
            )
        except Exception as exc:
            final_logs = f"withdrawal log not observed: {exc}"
        final = kernel_snapshot(handler_id)
        save_evidence("kernel/after-final-withdrawal.json", json.dumps(final, indent=2))
        routes_absent, route_absence_detail = record_routes(
            RUN_DIR, fixture, [secondary], False
        )
        primary_remains, primary_remains_detail = record_routes(
            RUN_DIR, fixture, [primary, control_primary], True
        )
        gate("final route-specific state removal", lambda: check(
            routes_absent
            and primary_remains
            and not exact_fdb(
                final["fdb"],
                secondary["mac"],
                params["remote_vtep"],
                final["fdb_device"],
            )
            and not exact_neighbor(final["neighbors"], secondary["ip"], secondary["mac"])
            and exact_fdb(
                final["fdb"],
                primary["mac"],
                params["remote_vtep"],
                final["fdb_device"],
            )
            and exact_neighbor(final["neighbors"], primary["ip"], primary["mac"])
            and vtep_neighbor(final["vtep_neighbors"], params["remote_vtep"])
            and flood_fdb(
                final["fdb"], params["remote_vtep"], final["fdb_device"]
            ),
            f"{final_logs}\n{route_absence_detail}\n{primary_remains_detail}\n"
            f"{json.dumps(final, indent=2)}",
        ))

    except Exception as exc:
        REPORT["error"] = str(exc)
        write_json(REPORT_PATH, REPORT)
    finally:
        # Exact route receipt and ownership IDs are the only cleanup handles used.
        cleanup_failed = False

        def cleanup_failure(detail: str) -> None:
            nonlocal cleanup_failed
            cleanup_failed = True
            REPORT["cleanup"]["status"] = "FAIL"
            REPORT["cleanup"]["details"].append(detail)

        if local_route_uuid:
            try:
                run_route_helper(
                    image_tag,
                    network["Name"],
                    helper_ip,
                    local_endpoint,
                    fixture,
                    local_vtep,
                    "withdraw",
                    local_route_uuid,
                )
                REPORT["cleanup"]["details"].append("local GoBGP route withdrawn by exact UUID")
            except Exception as exc:
                cleanup_failure(f"local route cleanup failed: {exc}")
        container_id = handler_id or REPORT["topology"].get("handler_container_id", "")
        if container_id:
            handler_id = container_id
        if handler_id:
            try:
                own_container(handler_id, RUN_ID, "netlink")
                logs = run(["docker", "logs", "--tail", "2000", handler_id], check=False).stdout
                save_evidence("cleanup/netlink-handler.log", logs)
                run(["docker", "stop", "--time", "10", handler_id], timeout=20)
                run(["docker", "rm", handler_id], timeout=20)
                mark_resource_removed(handler_id)
                mark_container_resources_removed(handler_id)
                REPORT["cleanup"]["details"].append("owned handler container removed")
            except Exception as exc:
                cleanup_failure(f"handler cleanup failed: {exc}")

        for helper_id in reversed(REPORT["topology"].get("route_helper_containers", [])):
            try:
                inspected = run(["docker", "inspect", helper_id], check=False)
                if inspected.returncode:
                    continue
                helper = json.loads(inspected.stdout)[0]
                labels = helper.get("Config", {}).get("Labels") or {}
                if (
                    labels.get("evpn.e2e.owner") != RUN_ID
                    or labels.get("evpn.e2e.fixture") != "netlink-route-helper"
                ):
                    raise AcceptanceError(
                        f"refusing to clean unowned route helper {helper_id}"
                    )
                helper_logs = run(["docker", "logs", helper_id], check=False).stdout
                save_evidence(
                    f"cleanup/route-helper-{helper_id[:12]}.log", helper_logs
                )
                if helper.get("State", {}).get("Running"):
                    run(["docker", "stop", "--time", "5", helper_id], timeout=15)
                run(["docker", "rm", helper_id], timeout=15)
                mark_resource_removed(helper_id)
            except Exception as exc:
                cleanup_failure(
                    f"route-helper cleanup failed for {helper_id}: {exc}"
                )

        for endpoint in (control_primary, *remote_endpoints):
            try:
                current_fixture = json.loads((RUN_DIR / "manifest.json").read_text())
                current_endpoint = next(
                    ep for ep in current_fixture["parameters"]["endpoints"]
                    if ep["name"] == endpoint["name"]
                )
                if current_endpoint["state"] == "advertised":
                    fixture_cli(
                        RUN_DIR, args.authorize_disposable, "withdraw", RUN_ID, endpoint["name"]
                    )
            except Exception as exc:
                cleanup_failure(
                    f"route cleanup for {endpoint['name']} failed: {exc}"
                )
        try:
            output = fixture_cli(RUN_DIR, args.authorize_disposable, "stop", RUN_ID)
            if "fixture error:" in output:
                raise AcceptanceError(output.strip())
            REPORT["cleanup"]["details"].append(output.strip())
            for resource_id in fixture["resources"]["containers"]:
                mark_resource_removed(resource_id)
            for resource_id in fixture["resources"]["networks"]:
                mark_resource_removed(resource_id)
            for resource in OWNERSHIP_MANIFEST.get("acceptance_resources", []):
                if resource.get("container_id") in fixture["resources"]["containers"]:
                    resource["state"] = "removed"
                    resource["removed_at"] = now()
            write_json(OWNERSHIP_MANIFEST_PATH, OWNERSHIP_MANIFEST)
        except Exception as exc:
            cleanup_failure(f"shared fixture cleanup failed: {exc}")
        try:
            if build_attempted:
                image_inspect = run(
                    ["docker", "image", "inspect", image_tag], check=False
                )
                if image_inspect.returncode == 0:
                    image = json.loads(image_inspect.stdout)[0]
                    image_id = image["Id"]
                    labels = image.get("Config", {}).get("Labels") or {}
                    if (
                        labels.get("evpn.e2e.owner") != RUN_ID
                        or labels.get("evpn.e2e.fixture") != "netlink-image"
                        or (built_image_id and image_id != built_image_id)
                    ):
                        raise AcceptanceError(
                            f"refusing to remove unowned or replaced test image {image_tag}"
                        )
                    if not built_image_id:
                        built_image_id = image_id
                        add_owned_resource(
                            "docker-image",
                            built_image_id,
                            "netlink",
                            image_tag,
                            image_tag,
                        )
                    remove_image = run(
                        ["docker", "image", "rm", image_tag], check=False
                    )
                    if remove_image.returncode:
                        raise AcceptanceError(
                            f"could not remove owned test image {image_tag}: {remove_image.stdout}"
                        )
                    remaining_image = run(
                        ["docker", "image", "inspect", built_image_id], check=False
                    )
                    if remaining_image.returncode == 0:
                        raise AcceptanceError(
                            f"owned test image remains after removing tag {image_tag}"
                        )
                    mark_resource_removed(built_image_id)
            container_inventory = run(
                ["docker", "ps", "-aq", "--filter", f"label=evpn.e2e.owner={RUN_ID}"],
                check=False,
            )
            network_inventory = run(
                ["docker", "network", "ls", "-q", "--filter", f"label=evpn.e2e.owner={RUN_ID}"],
                check=False,
            )
            image_inventory = run(
                ["docker", "image", "ls", "-q", "--filter", f"label=evpn.e2e.owner={RUN_ID}"],
                check=False,
            )
            if (
                container_inventory.returncode
                or network_inventory.returncode
                or image_inventory.returncode
            ):
                raise AcceptanceError("could not verify final Docker owner-label inventory")
            owner_containers = container_inventory.stdout.split()
            owner_networks = network_inventory.stdout.split()
            owner_images = image_inventory.stdout.split()
            if owner_containers or owner_networks or owner_images:
                raise AcceptanceError(
                    "owned resources remain: "
                    f"containers={owner_containers}, networks={owner_networks}, "
                    f"images={owner_images}"
                )
            tracked_resources = [
                f"{item['type']}:{item['id']}"
                for item in OWNERSHIP_MANIFEST["acceptance_resources"]
                if item["state"] != "removed"
            ]
            if tracked_resources:
                raise AcceptanceError(
                    f"owned resources are not recorded as removed: {tracked_resources}"
                )
            REPORT["cleanup"]["details"].append("final owner-label inventory is empty")
            if not cleanup_failed:
                REPORT["cleanup"]["status"] = "PASS"
        except Exception as exc:
            cleanup_failure(str(exc))
        for missing in required_gates[len(REPORT["gates"]):]:
            add_gate(missing, "NOT RUN", "A preceding required setup step failed.")
        if REPORT["cleanup"]["status"] == "NOT RUN":
            REPORT["cleanup"]["status"] = "FAIL"
        REPORT["overall"] = overall_status(
            REPORT["gates"], required_gates, REPORT["cleanup"]["status"]
        )
        REPORT["completed_at"] = now()
        OWNERSHIP_MANIFEST["final_owner_inventory"] = {
            "container_ids": owner_containers if "owner_containers" in locals() else None,
            "network_ids": owner_networks if "owner_networks" in locals() else None,
            "image_ids": owner_images if "owner_images" in locals() else None,
        }
        OWNERSHIP_MANIFEST["cleanup_status"] = REPORT["cleanup"]["status"]
        OWNERSHIP_MANIFEST["completed_at"] = REPORT["completed_at"]
        write_json(OWNERSHIP_MANIFEST_PATH, OWNERSHIP_MANIFEST)
        write_json(REPORT_PATH, REPORT)
    return 0 if REPORT["overall"] == "PASS" else 1


def parser() -> argparse.ArgumentParser:
    result = argparse.ArgumentParser(description=__doc__)
    result.add_argument("--authorize-disposable", required=True)
    result.add_argument("--run-id", required=True, help="existing shared fixture UUID")
    result.add_argument("--state-root", type=Path, default=FIXTURE.DEFAULT_STATE_ROOT)
    result.add_argument("--go-build-image", required=True)
    result.add_argument("--runtime-image", required=True)
    result.set_defaults(handler=acceptance)
    return result


def main() -> int:
    try:
        args = parser().parse_args()
        with FIXTURE.lab.exclusive():
            return args.handler(args)
    except (AcceptanceError, FIXTURE.lab.LabError, OSError, KeyError, ValueError, StopIteration) as exc:
        print(f"netlink acceptance error: {exc}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
