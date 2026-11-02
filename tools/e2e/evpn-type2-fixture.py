#!/usr/bin/env python3
"""Own a disposable GoBGP/FRR EVPN Type-2 route fixture."""

from __future__ import annotations

import argparse
import hashlib
import ipaddress
import json
import os
import re
import shlex
import shutil
import subprocess
import sys
import time
import uuid
from datetime import datetime, timezone
from pathlib import Path
from typing import Any, Callable

sys.path.insert(0, str(Path(__file__).resolve().parent))
import containerlab as lab

ROOT = Path(__file__).resolve().parents[2]
IMAGE_RE = re.compile(r"^[A-Za-z0-9./:_-]+@sha256:[0-9a-f]{64}$")
RUN_ID_RE = re.compile(r"^[a-f0-9-]{36}$")
DEFAULT_STATE_ROOT = Path(
    os.environ.get(
        "EVPN_E2E_STATE_ROOT",
        str(Path.home() / ".local/state/evpn-for-ovn/e2e-runs"),
    )
)
COMMAND_TIMEOUT = 15
WAIT_TIMEOUT = 90
ARP_CAPTURE_TIMEOUT = 20


class FixtureError(Exception):
    pass


class JsonCommandOutput:
    __slots__ = ("document", "stdout", "stderr")
    document: Any
    stdout: str
    stderr: str

    def __init__(self, document: Any, stdout: str, stderr: str) -> None:
        self.document = document
        self.stdout = stdout
        self.stderr = stderr


def utc_now() -> str:
    return datetime.now(timezone.utc).isoformat()


def write_json(path: Path, value: Any) -> None:
    temporary = path.with_suffix(path.suffix + ".tmp")
    temporary.write_text(json.dumps(value, indent=2, sort_keys=True) + "\n")
    temporary.replace(path)


def run_command(
    command: list[str],
    timeout: int = COMMAND_TIMEOUT,
    check: bool = True,
    capture_stderr: bool = False,
) -> subprocess.CompletedProcess[str]:
    try:
        result = subprocess.run(
            command,
            text=True,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE if capture_stderr else subprocess.STDOUT,
            timeout=timeout,
            check=False,
            pass_fds=lab.lock_descriptors(),
        )
    except subprocess.TimeoutExpired as exc:
        output = exc.stdout or ""
        error = exc.stderr or ""
        if isinstance(output, bytes):
            output = output.decode(errors="replace")
        if isinstance(error, bytes):
            error = error.decode(errors="replace")
        raise FixtureError(
            f"command timed out after {timeout}s: {command[0]} {command[1:]}\n"
            f"stdout:\n{output}"
            + (f"\nstderr:\n{error}" if capture_stderr and error else "")
        ) from exc
    if check and result.returncode:
        raise FixtureError(
            f"command failed ({result.returncode}): {command[0]} {command[1:]}\n"
            f"stdout:\n{result.stdout}"
            + (
                f"\nstderr:\n{result.stderr}"
                if capture_stderr and result.stderr
                else ""
            )
        )
    return result


def _valid_gobgp_rib_schema(document: Any) -> bool:
    return isinstance(document, dict) and all(
        isinstance(route_paths, list)
        and all(isinstance(path, dict) for path in route_paths)
        for route_paths in document.values()
    )


def _valid_frr_evpn_schema(document: Any) -> bool:
    if not isinstance(document, dict):
        return False
    if type(document.get("numPrefix")) is not int or type(
        document.get("numPaths")
    ) is not int:
        return False
    for rd, rd_record in document.items():
        if rd in {"numPrefix", "numPaths"}:
            continue
        if not isinstance(rd, str) or not isinstance(rd_record, dict):
            return False
        if not isinstance(rd_record.get("rd"), str):
            return False
        for route_key, route in rd_record.items():
            if route_key == "rd":
                continue
            if route_key == "numRoutes":
                if type(route) is not int or route < 0:
                    return False
                continue
            if (
                not isinstance(route_key, str)
                or not isinstance(route, dict)
                or not isinstance(route.get("prefix"), str)
                or not isinstance(route.get("paths"), list)
            ):
                return False
            if any(
                not isinstance(path_set, list)
                or any(not isinstance(path, dict) for path in path_set)
                for path_set in route["paths"]
            ):
                return False
    return True


def _valid_frr_exact_route_schema(document: Any) -> bool:
    if (
        not isinstance(document, dict)
        or not isinstance(document.get("rd"), str)
        or type(document.get("routeType")) is not int
        or not isinstance(document.get("mac"), str)
        or not isinstance(document.get("ip"), str)
        or type(document.get("numPaths")) is not int
    ):
        return False
    route_keys = [
        key
        for key in document
        if isinstance(key, str) and key.startswith("[2]:")
    ]
    if "paths" in document:
        if (route_keys or not isinstance(document.get("prefix"), str)
                or not document["prefix"].startswith("[2]:")
                or type(document.get("pathCount")) is not int
                or document["pathCount"] != document["numPaths"]):
            return False
        route_keys = ["paths"]
    return bool(route_keys) and all(
        isinstance(document[key], list)
        and all(
            isinstance(path_set, list)
            and all(isinstance(path, dict) for path in path_set)
            for path_set in document[key]
        )
        for key in route_keys
    )


def parse_json_command_output(
    stdout: str,
    stderr: str,
    exit_code: int,
    speaker: str,
    schema_validator: Callable[[Any], bool],
) -> JsonCommandOutput:
    diagnostics = f"\nstderr:\n{stderr}" if stderr else ""
    if exit_code:
        raise FixtureError(
            f"{speaker} JSON command failed with exit code {exit_code}; "
            f"stdout:\n{stdout}{diagnostics}"
        )
    if not stdout.strip():
        raise FixtureError(
            f"{speaker} JSON command returned empty stdout{diagnostics}"
        )
    try:
        document = json.loads(stdout)
    except json.JSONDecodeError as exc:
        raise FixtureError(
            f"{speaker} JSON command returned malformed stdout ({exc}); "
            f"stdout:\n{stdout}{diagnostics}"
        ) from exc
    if not schema_validator(document):
        raise FixtureError(
            f"{speaker} JSON command returned an unexpected schema; "
            f"stdout:\n{stdout}{diagnostics}"
        )
    return JsonCommandOutput(document, stdout, stderr)


def run_json_command(
    command: list[str],
    speaker: str,
    schema_validator: Callable[[Any], bool],
) -> JsonCommandOutput:
    result = run_command(command, check=False, capture_stderr=True)
    return parse_json_command_output(
        result.stdout, result.stderr or "", result.returncode, speaker, schema_validator
    )


def docker_json(*args: str) -> Any:
    result = run_command(["docker", *args])
    try:
        return json.loads(result.stdout)
    except json.JSONDecodeError as exc:
        raise FixtureError(f"invalid Docker JSON response: {exc}") from exc


def digest_image(value: str, name: str) -> str:
    if not IMAGE_RE.fullmatch(value):
        raise FixtureError(
            f"{name} must be an immutable image reference ending in @sha256:<64 lowercase hex>"
        )
    return value


def valid_asn(value: int, name: str) -> int:
    if not 1 <= value <= 4294967295:
        raise FixtureError(f"{name} must be in 1..4294967295")
    return value


def valid_rd_rt(value: str, name: str) -> str:
    parts = value.split(":")
    if len(parts) != 2 or not parts[1].isdigit():
        raise FixtureError(f"{name} must use ASN:number or IPv4:number notation")
    if int(parts[1]) > 4294967295:
        raise FixtureError(f"{name} local administrator must fit in 32 bits")
    if "." in parts[0]:
        parse_ipv4(parts[0], f"{name} administrator")
    elif not parts[0].isdigit() or int(parts[0]) > 4294967295:
        raise FixtureError(f"{name} administrator must be a 32-bit ASN or IPv4 address")
    return value


def parse_ipv4(value: str, name: str) -> ipaddress.IPv4Address:
    try:
        parsed = ipaddress.ip_address(value)
    except ValueError as exc:
        raise FixtureError(f"{name} must be an IPv4 address") from exc
    if not isinstance(parsed, ipaddress.IPv4Address) or parsed.is_unspecified:
        raise FixtureError(f"{name} must be a non-unspecified IPv4 address")
    return parsed


def generated_subnet(run_id: str) -> ipaddress.IPv4Network:
    space = ipaddress.ip_network("198.18.0.0/15")
    networks = list(space.subnets(new_prefix=24))
    slot = int(hashlib.sha256(run_id.encode()).hexdigest()[:8], 16) % len(networks)
    return networks[slot]


def generated_endpoint_subnet(
    run_id: str, fixture_subnet: ipaddress.IPv4Network
) -> ipaddress.IPv4Network:
    networks = list(ipaddress.ip_network("198.18.0.0/15").subnets(new_prefix=24))
    start = int(hashlib.sha256(f"{run_id}:endpoint".encode()).hexdigest()[:8], 16)
    for offset in range(len(networks)):
        candidate = networks[(start + offset) % len(networks)]
        if candidate == fixture_subnet or network_overlaps_existing(candidate):
            continue
        return candidate
    raise FixtureError("no isolated endpoint subnet is available")


def endpoint_namespace_name(run_id: str) -> str:
    return f"evpn-ep-{run_id.replace('-', '')[:12]}"


def network_overlaps_existing(candidate: ipaddress.IPv4Network) -> list[str]:
    ids = run_command(["docker", "network", "ls", "-q"]).stdout.split()
    if not ids:
        return []
    networks = docker_json("network", "inspect", *ids)
    conflicts = []
    for network in networks:
        for config in network.get("IPAM", {}).get("Config", []) or []:
            subnet = config.get("Subnet")
            if not subnet:
                continue
            try:
                existing = ipaddress.ip_network(subnet, strict=False)
            except ValueError:
                continue
            if candidate.overlaps(existing):
                conflicts.append(f"{network.get('Name')} ({subnet})")
    return conflicts


def run_dir(state_root: Path, run_id: str) -> Path:
    if not RUN_ID_RE.fullmatch(run_id):
        raise FixtureError("run ID must be a UUID returned by start")
    try:
        canonical_id = str(uuid.UUID(run_id))
    except ValueError as exc:
        raise FixtureError("run ID must be a canonical UUID returned by start") from exc
    if canonical_id != run_id:
        raise FixtureError("run ID must be a canonical UUID returned by start")
    path = state_root / run_id
    if not path.is_dir():
        raise FixtureError(f"run not found: {run_id}")
    return path


def load_manifest(directory: Path) -> dict[str, Any]:
    path = directory / "manifest.json"
    try:
        manifest = json.loads(path.read_text())
    except (OSError, json.JSONDecodeError) as exc:
        raise FixtureError(f"cannot read ownership manifest: {exc}") from exc
    if (
        manifest.get("run_id") != directory.name
        or manifest.get("schema_version") != 1
        or manifest.get("lab_project")
        != f"evpn-type2-{directory.name.replace('-', '')[:20]}"
    ):
        raise FixtureError("ownership manifest identity mismatch")
    return manifest


def save_manifest(directory: Path, manifest: dict[str, Any]) -> None:
    manifest["updated_at"] = utc_now()
    write_json(directory / "manifest.json", manifest)


def record(
    directory: Path, manifest: dict[str, Any], action: str, status: str, detail: str
) -> None:
    evidence = directory / "evidence"
    evidence.mkdir(exist_ok=True)
    filename = f"{int(time.time() * 1000)}-{action}-{len(manifest['results']) + 1}.txt"
    (evidence / filename).write_text(detail)
    result = {
        "action": action,
        "status": status,
        "at": utc_now(),
        "evidence": f"evidence/{filename}",
    }
    manifest["results"].append(result)
    report_path = directory / "report.json"
    if report_path.exists():
        report = json.loads(report_path.read_text())
        report["results"].append(result)
        write_json(report_path, report)
    save_manifest(directory, manifest)


def fixture_exec(manifest: dict[str, Any], directory: Path, *args: str) -> list[str]:
    if len(args) < 4 or args[:2] != ("exec", "-T") or args[2] not in ("gobgp", "frr"):
        raise FixtureError("fixture command must exec a known Containerlab node")
    state = lab.load(directory)
    return ["docker", "exec", f"{state['name']}-{args[2]}", *args[3:]]


def docker_labels(resources: dict[str, Any]) -> dict[str, str]:
    return resources.get("Config", {}).get("Labels", {}) or resources.get(
        "Labels", {}
    ) or {}


def capture_resources(
    directory: Path,
    manifest: dict[str, Any],
    allow_empty: bool = True,
    compare_recorded: bool = False,
) -> None:
    state = lab.load(directory)
    if state["owner"] != manifest["run_id"] or state["profile"] != "type2":
        raise FixtureError("Containerlab fixture ownership mismatch")
    resources = lab.verify_owned(state)
    container_ids = [identity for name, identity in resources["containers"].items()
                     if name not in state.get("auxiliary", [])]
    network_ids = [resources["network"]] if resources["network"] else []
    if compare_recorded:
        recorded = manifest["created_resources"]
        if not set(container_ids).issubset(recorded["containers"]) or not set(
            network_ids
        ).issubset(recorded["networks"]):
            raise FixtureError(
                "found resources not recorded by this run; refusing to manage them"
            )
    if not allow_empty and (len(container_ids) != 2 or len(network_ids) != 1):
        raise FixtureError(
            f"expected two containers and one network; found {len(container_ids)} "
            f"containers and {len(network_ids)} networks"
        )
    manifest["resources"] = {
        "containers": container_ids,
        "networks": network_ids,
    }
    if not compare_recorded:
        manifest["created_resources"] = {
            "containers": sorted(
                set(manifest["created_resources"]["containers"]) | set(container_ids)
            ),
            "networks": sorted(
                set(manifest["created_resources"]["networks"]) | set(network_ids)
            ),
        }
    save_manifest(directory, manifest)


MAC_ROLE_INDICES = {
    "primary": 1,
    "secondary": 2,
    "bridge": 3,
    "netlink-primary": 4,
    "netlink-local-endpoint": 5,
    "netlink-bridge": 6,
}


def default_mac(run_id: str, index: int) -> str:
    raw = hashlib.sha256(f"{run_id}:{index}".encode()).digest()[:6]
    return ":".join(f"{byte:02x}" for byte in bytes([(raw[0] | 2) & 0xFE]) + raw[1:])


def mac_for_role(run_id: str, role: str) -> str:
    try:
        index = MAC_ROLE_INDICES[role]
    except KeyError as exc:
        raise FixtureError(f"unknown fixture MAC role: {role}") from exc
    return default_mac(run_id, index)


def validate_mac_identities(
    bridge_mac: str,
    endpoints: list[dict[str, Any]],
    local_endpoint: dict[str, Any],
    netlink_bridge_mac: str,
) -> None:
    identities = [
        ("fixture bridge", bridge_mac),
        *((f"fixture endpoint {endpoint['name']}", endpoint["mac"]) for endpoint in endpoints),
        ("local endpoint", local_endpoint["mac"]),
        ("Netlink bridge", netlink_bridge_mac),
    ]
    seen: dict[str, str] = {}
    for identity, mac_value in identities:
        mac = str(mac_value).lower()
        if not re.fullmatch(r"(?:[0-9a-f]{2}:){5}[0-9a-f]{2}", mac):
            raise FixtureError(f"{identity} has an invalid MAC address: {mac_value}")
        first_octet = int(mac[:2], 16)
        if first_octet & 3 != 2:
            raise FixtureError(
                f"{identity} MAC must be unicast and locally administered: {mac}"
            )
        if mac in seen:
            raise FixtureError(
                f"duplicate MAC identity {mac}: {seen[mac]} and {identity}"
            )
        seen[mac] = identity


def endpoint_namespace_paths(parameters: dict[str, Any]) -> tuple[str, str]:
    namespace = parameters["endpoint_namespace"]
    return f"/tmp/{namespace}.pid", f"/tmp/{namespace}.parent"


def endpoint_namespace_pid(
    directory: Path, manifest: dict[str, Any]
) -> int:
    pidfile, _ = endpoint_namespace_paths(manifest["parameters"])
    output = container_exec(
        directory,
        manifest,
        "frr",
        "sh",
        "-ec",
        f"test -s {shlex.quote(pidfile)} && cat {shlex.quote(pidfile)}",
    ).strip()
    if not output.isdigit() or int(output) <= 1:
        raise FixtureError("endpoint namespace process has an invalid PID")
    return int(output)


def endpoint_namespace_start_command(parameters: dict[str, Any]) -> list[str]:
    pidfile, parentfile = endpoint_namespace_paths(parameters)
    namespace = parameters["endpoint_namespace"]
    command = (
        "unshare --net --fork sh -c "
        + shlex.quote(
            f'printf "%s\\n" "$$" > {shlex.quote(pidfile)}; '
            "exec sleep 2147483"
        )
        + f" </dev/null >/tmp/{namespace}.log 2>&1 & "
        + f'printf "%s\\n" "$!" > {shlex.quote(parentfile)}'
    )
    return ["sh", "-c", command]


def frr_topology_commands(
    parameters: dict[str, Any], endpoint_pid: int
) -> list[list[str]]:
    secondary = next(
        endpoint
        for endpoint in parameters["endpoints"]
        if endpoint["name"] == "secondary"
    )
    subnet = ipaddress.ip_network(parameters["endpoint_subnet"])
    endpoint_address = ipaddress.ip_address(secondary["ip"])
    gateway = ipaddress.ip_address(parameters["endpoint_gateway"])
    if endpoint_address not in subnet or gateway not in subnet:
        raise FixtureError("secondary endpoint and SVI gateway must share the endpoint subnet")
    endpoint_cidr = f"{endpoint_address}/{subnet.prefixlen}"
    commands = [
        ["ip", "address", "add", f"{parameters['remote_vtep']}/32", "dev", "eth0"],
        [
            "ip",
            "link",
            "add",
            "br-e2e",
            "type",
            "bridge",
            "vlan_filtering",
            "0",
        ],
        ["ip", "link", "set", "dev", "br-e2e", "address", parameters["bridge_mac"]],
        ["ip", "address", "add", f"{gateway}/{subnet.prefixlen}", "dev", "br-e2e"],
        [
            "ip",
            "link",
            "add",
            "vx-e2e",
            "type",
            "vxlan",
            "id",
            str(parameters["vni"]),
            "local",
            str(parameters["remote_vtep"]),
            "dstport",
            str(parameters["udp_port"]),
            "nolearning",
        ],
        ["ip", "link", "add", "ep2", "type", "veth", "peer", "name", "sw2"],
        ["ip", "link", "set", "dev", "ep2", "netns", str(endpoint_pid)],
        ["ip", "link", "set", "dev", "sw2", "master", "br-e2e"],
        ["ip", "link", "set", "dev", "sw2", "up"],
        ["ip", "link", "set", "dev", "vx-e2e", "master", "br-e2e"],
        [
            "ip",
            "link",
            "set",
            "dev",
            "vx-e2e",
            "type",
            "bridge_slave",
            "neigh_suppress",
            "on",
            "learning",
            "off",
        ],
        ["ip", "link", "set", "dev", "vx-e2e", "up"],
        ["ip", "link", "set", "dev", "br-e2e", "up"],
        [
            "nsenter",
            "--target",
            str(endpoint_pid),
            "--net",
            "ip",
            "link",
            "set",
            "dev",
            "lo",
            "up",
        ],
        [
            "nsenter",
            "--target",
            str(endpoint_pid),
            "--net",
            "ip",
            "link",
            "set",
            "dev",
            "ep2",
            "address",
            secondary["mac"],
        ],
        [
            "nsenter",
            "--target",
            str(endpoint_pid),
            "--net",
            "ip",
            "address",
            "add",
            endpoint_cidr,
            "dev",
            "ep2",
        ],
        [
            "nsenter",
            "--target",
            str(endpoint_pid),
            "--net",
            "ip",
            "link",
            "set",
            "dev",
            "ep2",
            "up",
        ],
    ]
    for endpoint in parameters["endpoints"]:
        if endpoint["originator"] != "frr" or endpoint["name"] == "secondary":
            continue
        endpoint_interface, bridge_interface = frr_endpoint_interfaces(endpoint)
        endpoint_address = ipaddress.ip_address(endpoint["ip"])
        if endpoint_address not in subnet:
            raise FixtureError(
                f"FRR endpoint {endpoint['name']} is outside the endpoint subnet"
            )
        namespace_command = ["nsenter", "--target", str(endpoint_pid), "--net"]
        commands.extend(
            [
                [
                    "ip", "link", "add", endpoint_interface, "type", "veth",
                    "peer", "name", bridge_interface,
                ],
                [
                    "ip", "link", "set", "dev", endpoint_interface,
                    "netns", str(endpoint_pid),
                ],
                [
                    "ip", "link", "set", "dev", bridge_interface,
                    "master", "br-e2e",
                ],
                ["ip", "link", "set", "dev", bridge_interface, "up"],
                [
                    *namespace_command, "ip", "link", "set", "dev",
                    endpoint_interface, "address", endpoint["mac"],
                ],
                [
                    *namespace_command, "ip", "address", "add",
                    f"{endpoint_address}/{subnet.prefixlen}",
                    "dev", endpoint_interface,
                ],
                [
                    *namespace_command, "ip", "link", "set", "dev",
                    endpoint_interface, "up",
                ],
            ]
        )
    return commands


def frr_endpoint_interfaces(endpoint: dict[str, Any]) -> tuple[str, str]:
    if endpoint["name"] == "secondary":
        return "ep2", "sw2"
    if endpoint["name"] == "netlink-primary":
        return "ep3", "sw3"
    raise FixtureError(f"unsupported FRR endpoint {endpoint['name']}")


def secondary_advertisement_commands(
    parameters: dict[str, Any], endpoint_pid: int, reactivate: bool = False
) -> list[list[str]]:
    secondary = next(
        endpoint
        for endpoint in parameters["endpoints"]
        if endpoint["name"] == "secondary"
    )
    return frr_endpoint_advertisement_commands(
        parameters, secondary, endpoint_pid, reactivate
    )


def frr_endpoint_advertisement_commands(
    parameters: dict[str, Any],
    endpoint: dict[str, Any],
    endpoint_pid: int,
    reactivate: bool = False,
) -> list[list[str]]:
    subnet = ipaddress.ip_network(parameters["endpoint_subnet"])
    address = ipaddress.ip_address(endpoint["ip"])
    endpoint_interface, bridge_interface = frr_endpoint_interfaces(endpoint)
    namespace_command = ["nsenter", "--target", str(endpoint_pid), "--net"]
    commands = []
    if reactivate:
        commands.extend(
            [
                [
                    *namespace_command,
                    "ip",
                    "address",
                    "add",
                    f"{address}/{subnet.prefixlen}",
                    "dev", endpoint_interface,
                ],
                [
                    "ip", "link", "set", "dev", bridge_interface,
                    "master", "br-e2e",
                ],
                ["ip", "link", "set", "dev", bridge_interface, "up"],
            ]
        )
    commands.append(
        [
            *namespace_command,
            "ping",
            "-n",
            "-I",
            endpoint_interface,
            "-c",
            "1",
            "-W",
            "2",
            parameters["endpoint_gateway"],
        ]
    )
    return commands


def secondary_withdrawal_commands(
    parameters: dict[str, Any], endpoint_pid: int
) -> list[list[str]]:
    secondary = next(
        endpoint
        for endpoint in parameters["endpoints"]
        if endpoint["name"] == "secondary"
    )
    return frr_endpoint_withdrawal_commands(parameters, secondary, endpoint_pid)


def frr_endpoint_withdrawal_commands(
    parameters: dict[str, Any],
    endpoint: dict[str, Any],
    endpoint_pid: int,
) -> list[list[str]]:
    subnet = ipaddress.ip_network(parameters["endpoint_subnet"])
    address = ipaddress.ip_address(endpoint["ip"])
    endpoint_interface, bridge_interface = frr_endpoint_interfaces(endpoint)
    return [
        [
            "nsenter",
            "--target",
            str(endpoint_pid),
            "--net",
            "ip",
            "address",
            "del",
            f"{address}/{subnet.prefixlen}",
            "dev",
            endpoint_interface,
        ],
        ["ip", "neigh", "del", str(address), "dev", "br-e2e"],
        ["ip", "link", "set", "dev", bridge_interface, "nomaster"],
    ]


def arp_capture_commands(
    directory: Path,
    manifest: dict[str, Any],
    endpoint_pid: int,
    endpoint: dict[str, Any] | None = None,
) -> dict[str, list[str]]:
    endpoint = endpoint or {"name": "secondary"}
    endpoint_interface, bridge_interface = frr_endpoint_interfaces(endpoint)
    prefix = "" if endpoint["name"] == "secondary" else f"{endpoint['name']}-"
    commands = {
        f"{prefix}endpoint-{endpoint_interface}": [
            "nsenter",
            "--target",
            str(endpoint_pid),
            "--net",
        ],
        f"{prefix}bridge-{bridge_interface}": [],
        f"{prefix}bridge-br-e2e": [],
    }
    return {
        name: fixture_exec(
            manifest,
            directory,
            "exec",
            "-T",
            "frr",
            *prefix,
            "timeout",
            str(ARP_CAPTURE_TIMEOUT),
            "tcpdump",
            "-n",
            "-U",
            "-i",
            interface,
            "-w",
            "-",
            "arp",
        )
        for (name, prefix), interface in zip(
            commands.items(),
            (endpoint_interface, bridge_interface, "br-e2e"),
            strict=True,
        )
    }


def start_arp_captures(
    directory: Path,
    manifest: dict[str, Any],
    endpoint_pid: int,
    endpoint: dict[str, Any] | None = None,
) -> list[dict[str, Any]]:
    capture_dir = directory / "evidence" / "packet-capture"
    capture_dir.mkdir(parents=True, exist_ok=True)
    sessions = []
    for name, command in arp_capture_commands(
        directory, manifest, endpoint_pid, endpoint
    ).items():
        pcap_path = capture_dir / f"{name}.pcap"
        stderr_path = capture_dir / f"{name}.stderr"
        with pcap_path.open("wb") as pcap_stream:
            process = subprocess.Popen(
                command,
                stdout=pcap_stream,
                stderr=subprocess.PIPE,
            )
        sessions.append(
            {
                "name": name,
                "command": command,
                "process": process,
                "pcap_path": pcap_path,
                "stderr_path": stderr_path,
                "started_at": utc_now(),
            }
        )
    time.sleep(0.25)
    return sessions


def finish_arp_captures(sessions: list[dict[str, Any]]) -> str:
    evidence = []
    for session in sessions:
        process = session["process"]
        try:
            _, stderr = process.communicate(timeout=ARP_CAPTURE_TIMEOUT + 10)
        except subprocess.TimeoutExpired:
            process.terminate()
            _, stderr = process.communicate(timeout=5)
        stderr_text = (stderr or b"").decode(errors="replace")
        session["stderr_path"].write_text(stderr_text)
        pcap_path = session["pcap_path"]
        pcap_size = pcap_path.stat().st_size
        metadata = {
            "interface": session["name"],
            "command": session["command"],
            "started_at": session["started_at"],
            "finished_at": utc_now(),
            "exit_code": process.returncode,
            "pcap": str(pcap_path),
            "pcap_bytes": pcap_size,
            "stderr": str(session["stderr_path"]),
        }
        write_json(pcap_path.with_suffix(".json"), metadata)
        evidence.append(json.dumps(metadata, sort_keys=True))
    return "\nPacket capture results:\n" + "\n".join(evidence)


def cleanup_frr_topology(
    directory: Path, manifest: dict[str, Any]
) -> list[str]:
    parameters = manifest["parameters"]
    namespace = parameters.get("endpoint_namespace")
    output = []
    if namespace:
        pidfile, parentfile = endpoint_namespace_paths(parameters)
        process_cleanup = (
            f"if test -s {shlex.quote(pidfile)}; then "
            f"pid=$(cat {shlex.quote(pidfile)}); "
            'case "$pid" in ""|*[!0-9]*) exit 2;; esac; '
            'if test -r "/proc/$pid/cmdline"; then '
            'case "$(tr "\\000" " " < "/proc/$pid/cmdline")" in '
            '*"sleep 2147483"*) kill "$pid";; '
            '*) echo "endpoint namespace PID does not match" >&2; exit 3;; '
            "esac; fi; fi; "
            f"rm -f {shlex.quote(pidfile)} {shlex.quote(parentfile)} "
            f"/tmp/{namespace}.log"
        )
        output.append(
            container_exec(
                directory, manifest, "frr", "sh", "-ec", process_cleanup
            )
        )
    links = container_exec(directory, manifest, "frr", "ip", "-o", "link", "show")
    for interface in ("sw2", "sw3", "br-e2e"):
        if re.search(rf"(?m)^\d+:\s+{re.escape(interface)}(?:@[^:]+)?:", links):
            output.append(
                container_exec(
                    directory, manifest, "frr", "ip", "link", "del", "dev", interface
                )
            )
    return output


def setup_frr_topology(
    directory: Path, manifest: dict[str, Any]
) -> list[str]:
    output = []
    try:
        parameters = manifest["parameters"]
        container_exec(
            directory,
            manifest,
            "frr",
            *endpoint_namespace_start_command(parameters),
        )
        pidfile, _ = endpoint_namespace_paths(parameters)
        deadline = time.monotonic() + 10
        endpoint_pid = None
        while time.monotonic() < deadline:
            result = run_command(
                fixture_exec(
                    manifest,
                    directory,
                    "exec",
                    "-T",
                    "frr",
                    "sh",
                    "-c",
                    f"test -s {shlex.quote(pidfile)} && cat {shlex.quote(pidfile)}",
                ),
                check=False,
            )
            if result.returncode == 0 and result.stdout.strip().isdigit():
                endpoint_pid = int(result.stdout.strip())
                break
            time.sleep(0.1)
        if endpoint_pid is None:
            log_path = f"/tmp/{parameters['endpoint_namespace']}.log"
            log_output = container_exec(
                directory, manifest, "frr", "sh", "-c", f"cat {shlex.quote(log_path)}"
            )
            raise FixtureError(
                "process-owned endpoint network namespace did not start:\n"
                + log_output
            )
        namespace_ids = container_exec(
            directory,
            manifest,
            "frr",
            "sh",
            "-ec",
            f"readlink /proc/{endpoint_pid}/ns/net; readlink /proc/1/ns/net",
        ).splitlines()
        if len(namespace_ids) != 2 or namespace_ids[0] == namespace_ids[1]:
            raise FixtureError("endpoint process did not enter a distinct network namespace")
        for command in frr_topology_commands(parameters, endpoint_pid):
            output.append(container_exec(directory, manifest, "frr", *command))
    except Exception as setup_error:
        try:
            cleanup_frr_topology(directory, manifest)
        except Exception as rollback_error:
            raise FixtureError(
                f"FRR topology setup failed: {setup_error}; "
                f"topology rollback failed: {rollback_error}"
            ) from setup_error
        raise
    return output


def write_configs(
    directory: Path,
    values: dict[str, Any],
) -> None:
    gobgp = f"""[global.config]
as = {values['gobgp_asn']}
router-id = "{values['gobgp_router_id']}"
port = 179

[[neighbors]]
  [neighbors.config]
  neighbor-address = "{values['frr_address']}"
  peer-as = {values['frr_asn']}
  [[neighbors.afi-safis]]
    [neighbors.afi-safis.config]
    afi-safi-name = "l2vpn-evpn"
"""
    frr = f"""frr defaults traditional
hostname evpn-type2-frr
log stdout
!
router bgp {values['frr_asn']}
 bgp router-id {values['frr_router_id']}
 no bgp default ipv4-unicast
 neighbor {values['gobgp_address']} remote-as {values['gobgp_asn']}
 !
 address-family l2vpn evpn
  neighbor {values['gobgp_address']} activate
  advertise-all-vni
  advertise-svi-ip
  vni {values['vni']}
   rd {values['rd']}
   route-target import {values['rt']}
   route-target export {values['rt']}
  exit-vni
 exit-address-family
!
line vty
"""
    daemons = """zebra=yes
bgpd=yes
ospfd=no
ospf6d=no
ripd=no
ripngd=no
isisd=no
pimd=no
pim6d=no
ldpd=no
nhrpd=no
eigrpd=no
babeld=no
sharpd=no
pbrd=no
bfdd=no
fabricd=no
vrrpd=no
pathd=no
vtysh_enable=yes
zebra_options="  -A 127.0.0.1 -s 90000000"
bgpd_options="   -A 127.0.0.1"
"""
    (directory / "gobgp.toml").write_text(gobgp)
    (directory / "frr.conf").write_text(frr)
    (directory / "daemons").write_text(daemons)
    (directory / "vtysh.conf").write_text(
        "service integrated-vtysh-config\n"
        "hostname evpn-type2-frr\n"
    )


def wait_for_gobgp_api(directory: Path, manifest: dict[str, Any]) -> str:
    deadline = time.monotonic() + WAIT_TIMEOUT
    last_output = ""
    while time.monotonic() < deadline:
        result = run_command(
            fixture_exec(
                manifest,
                directory,
                "exec",
                "-T",
                "gobgp",
                "gobgp",
                "global",
                "rib",
                "-a",
                "evpn",
                "-j",
            ),
            check=False,
        )
        last_output = result.stdout
        if result.returncode == 0:
            try:
                json.loads(last_output)
                return last_output
            except json.JSONDecodeError:
                pass
        time.sleep(2)
    raise FixtureError(
        f"GoBGP TCP gRPC API did not become ready within {WAIT_TIMEOUT}s:\n"
        f"{last_output}"
    )


def wait_for_frr_daemon(directory: Path, manifest: dict[str, Any]) -> str:
    deadline = time.monotonic() + WAIT_TIMEOUT
    last_output = ""
    command = (
        "test -s /var/run/frr/bgpd.pid && "
        'kill -0 "$(cat /var/run/frr/bgpd.pid)"'
    )
    while time.monotonic() < deadline:
        result = run_command(
            fixture_exec(manifest, directory, "exec", "-T", "frr", "sh", "-ec", command),
            check=False,
        )
        last_output = result.stdout
        if result.returncode == 0:
            summary = run_command(
                fixture_exec(
                    manifest,
                    directory,
                    "exec",
                    "-T",
                    "frr",
                    "vtysh",
                    "-c",
                    "show bgp l2vpn evpn summary",
                ),
                check=False,
            )
            last_output = summary.stdout
            if summary.returncode == 0:
                return (
                    "FRR bgpd process is running; BGP daemon CLI is responsive "
                    "before peer-state validation.\n"
                    + summary.stdout
                )
        time.sleep(2)
    raise FixtureError(
        f"FRR bgpd did not become ready within {WAIT_TIMEOUT}s; "
        f"daemon readiness is distinct from BGP session state:\n{last_output}"
    )


ESTABLISHED_STATES = {"establ", "established", "session_state_established"}


def _peer_identity(output: str, expected_peer: str, speaker: str) -> str | None:
    headers = re.findall(
        r"(?m)^\s*BGP neighbor is\s+([0-9a-fA-F:.]+)(?:,|\s*$)", output
    )
    expected = ipaddress.ip_address(expected_peer)
    if headers:
        if len(headers) != 1:
            raise FixtureError(
                f"{speaker}: unavailable: expected one BGP neighbor identity, "
                f"found {len(headers)}"
            )
        try:
            observed_peer = ipaddress.ip_address(headers[0])
        except ValueError as exc:
            raise FixtureError(f"{speaker}: malformed BGP peer identity") from exc
        if observed_peer != expected:
            raise FixtureError(
                f"{speaker}: wrong BGP peer {observed_peer}; expected {expected}"
            )
        return None

    table_headers = []
    for line in output.splitlines():
        columns = line.split()
        if columns and columns[0] == "Peer" and "State" in columns:
            table_headers.append(columns.index("State"))
    if len(table_headers) != 1:
        raise FixtureError(
            f"{speaker}: unavailable: expected one detailed peer identity or "
            "peer table header"
        )
    state_column = table_headers[0]
    matches = []
    for line in output.splitlines():
        columns = line.split()
        if len(columns) <= state_column:
            continue
        try:
            address = ipaddress.ip_address(columns[0])
        except ValueError:
            continue
        if address == expected:
            matches.append(columns[state_column])
    if len(matches) != 1:
        raise FixtureError(
            f"{speaker}: expected one peer-table row for {expected}, found {len(matches)}"
        )
    return matches[0]


def peer_observation(
    output: str, expected_peer: str, speaker: str, exit_code: int = 0
) -> tuple[bool, str]:
    if exit_code:
        raise FixtureError(
            f"{speaker}: neighbor command failed with exit code {exit_code}: {output}"
        )
    table_state = _peer_identity(output, expected_peer, speaker)
    states = re.findall(r"(?m)^\s*BGP state\s*=\s*([A-Za-z0-9_]+)", output)
    if len(states) > 1 or (not states and table_state is None):
        raise FixtureError(
            f"{speaker}: unavailable: expected one BGP state field, found {len(states)}"
        )
    observed_state = states[0] if states else table_state
    state = observed_state.lower()
    if state not in ESTABLISHED_STATES:
        return False, f"{speaker}: peer {expected_peer} state is {observed_state}"
    return True, f"{speaker}: peer {expected_peer} state is {observed_state}"


def evpn_family_observation(
    output: str, expected_peer: str, speaker: str, exit_code: int = 0
) -> tuple[bool, str]:
    if exit_code:
        raise FixtureError(
            f"{speaker}: neighbor capability command failed with exit code "
            f"{exit_code}: {output}"
        )
    _peer_identity(output, expected_peer, speaker)
    lines = [
        line.strip()
        for line in output.splitlines()
        if re.search(r"(?i)(?:l2vpn[- ]evpn|address family l2vpn evpn)", line)
    ]
    negotiated = [
        line
        for line in lines
        if re.search(r"(?i)advertised\s+and\s+received", line)
    ]
    if len(negotiated) == 1:
        return True, f"{speaker}: L2VPN EVPN negotiated: {negotiated[0]}"
    if not lines:
        return False, f"{speaker}: L2VPN EVPN negotiation evidence unavailable"
    return False, (
        f"{speaker}: L2VPN EVPN is configured or mentioned but not confirmed "
        "as advertised and received: " + "; ".join(lines)
    )


def wait_for_peer(
    directory: Path, manifest: dict[str, Any], gobgp_peer: str, frr_peer: str
) -> str:
    deadline = time.monotonic() + WAIT_TIMEOUT
    last_output = ""
    gobgp_command = fixture_exec(
        manifest,
        directory,
        "exec",
        "-T",
        "gobgp",
        "gobgp",
        "neighbor",
        frr_peer,
    )
    frr_command = fixture_exec(
        manifest,
        directory,
        "exec",
        "-T",
        "frr",
        "vtysh",
        "-c",
        f"show bgp neighbors {gobgp_peer}",
    )
    while time.monotonic() < deadline:
        gobgp_result = run_command(gobgp_command, check=False)
        gobgp_established, gobgp_detail = peer_observation(
            gobgp_result.stdout, frr_peer, "GoBGP", gobgp_result.returncode
        )
        frr_result = run_command(frr_command, check=False)
        frr_established, frr_detail = peer_observation(
            frr_result.stdout, gobgp_peer, "FRR", frr_result.returncode
        )
        last_output = (
            f"{gobgp_detail}\n{frr_detail}\n"
            f"GoBGP detail:\n{gobgp_result.stdout}\n"
            f"FRR detail:\n{frr_result.stdout}"
        )
        if gobgp_established and frr_established:
            return last_output
        time.sleep(2)
    raise FixtureError(
        f"BGP peer did not establish on both speakers within {WAIT_TIMEOUT}s:\n"
        f"{last_output}"
    )


def wait_for_evpn_family(
    directory: Path, manifest: dict[str, Any], gobgp_peer: str, frr_peer: str
) -> str:
    deadline = time.monotonic() + WAIT_TIMEOUT
    last_output = ""
    gobgp_command = fixture_exec(
        manifest,
        directory,
        "exec",
        "-T",
        "gobgp",
        "gobgp",
        "neighbor",
        frr_peer,
    )
    frr_command = fixture_exec(
        manifest,
        directory,
        "exec",
        "-T",
        "frr",
        "vtysh",
        "-c",
        f"show bgp neighbors {gobgp_peer}",
    )
    while time.monotonic() < deadline:
        gobgp_result = run_command(gobgp_command, check=False)
        gobgp_ok, gobgp_detail = evpn_family_observation(
            gobgp_result.stdout, frr_peer, "GoBGP", gobgp_result.returncode
        )
        frr_result = run_command(frr_command, check=False)
        frr_ok, frr_detail = evpn_family_observation(
            frr_result.stdout, gobgp_peer, "FRR", frr_result.returncode
        )
        last_output = (
            f"{gobgp_detail}\n{frr_detail}\n"
            f"GoBGP detail:\n{gobgp_result.stdout}\n"
            f"FRR detail:\n{frr_result.stdout}"
        )
        if gobgp_ok and frr_ok:
            return last_output
        time.sleep(2)
    raise FixtureError(
        f"L2VPN EVPN was not confirmed negotiated on both peers within "
        f"{WAIT_TIMEOUT}s:\n{last_output}"
    )


def container_exec(
    directory: Path, manifest: dict[str, Any], service: str, *args: str
) -> str:
    return run_command(
        fixture_exec(manifest, directory, "exec", "-T", service, *args)
    ).stdout


def route_outputs(
    directory: Path, manifest: dict[str, Any]
) -> tuple[JsonCommandOutput, JsonCommandOutput]:
    gobgp = run_json_command(
        fixture_exec(
            manifest,
            directory,
            "exec",
            "-T",
            "gobgp",
            "gobgp",
            "global",
            "rib",
            "-a",
            "evpn",
            "-j",
        ),
        "GoBGP",
        _valid_gobgp_rib_schema,
    )
    frr = run_json_command(
        fixture_exec(
            manifest,
            directory,
            "exec",
            "-T",
            "frr",
            "vtysh",
            "-c",
            "show bgp l2vpn evpn route type macip json",
        ),
        "FRR",
        _valid_frr_evpn_schema,
    )
    return gobgp, frr


def frr_exact_route_output(
    directory: Path,
    manifest: dict[str, Any],
    endpoint: dict[str, Any],
) -> JsonCommandOutput:
    mac = endpoint["mac"]
    if not re.fullmatch(r"(?:[0-9a-fA-F]{2}:){5}[0-9a-fA-F]{2}", mac):
        raise FixtureError("route MAC must contain six colon-separated octets")
    address = parse_ipv4(endpoint["ip"], "route IP")
    rd = valid_rd_rt(manifest["parameters"]["rd"], "RD")
    return run_json_command(
        fixture_exec(
            manifest,
            directory,
            "exec",
            "-T",
            "frr",
            "vtysh",
            "-c",
            "show bgp l2vpn evpn route "
            f"rd {rd} mac {mac} ip {address} json",
        ),
        "FRR exact EVPN route",
        _valid_frr_exact_route_schema,
    )


def json_output_evidence(output: JsonCommandOutput) -> str:
    stderr = output.stderr if output.stderr else "<empty>"
    return f"stdout:\n{output.stdout}\nstderr:\n{stderr}"


def _normalized_key(value: str) -> str:
    return re.sub(r"[^a-z0-9]", "", value.lower())


def _json_values(value: Any) -> list[str]:
    if isinstance(value, dict):
        return [item for child in value.values() for item in _json_values(child)]
    if isinstance(value, list):
        return [item for child in value for item in _json_values(child)]
    if value is None:
        return []
    return [str(value)]


def _keyed_values(value: Any, keys: set[str]) -> list[str]:
    found: list[str] = []
    if isinstance(value, dict):
        for key, child in value.items():
            normalized = _normalized_key(str(key))
            if normalized in keys:
                found.extend(_json_values(child))
            found.extend(_keyed_values(child, keys))
    elif isinstance(value, list):
        for child in value:
            found.extend(_keyed_values(child, keys))
    return found


def _go_bgp_extended_community_values(value: Any) -> list[str]:
    found: list[str] = []
    if isinstance(value, dict):
        attr_type = value.get("type")
        attr_name = str(value.get("typeName", value.get("name", ""))).lower()
        if attr_type == 16 or "extendedcommunity" in attr_name:
            found.extend(_json_values(value.get("value", value)))
        for child in value.values():
            found.extend(_go_bgp_extended_community_values(child))
    elif isinstance(value, list):
        for child in value:
            found.extend(_go_bgp_extended_community_values(child))
    return found


def _go_bgp_next_hop_values(value: Any) -> list[str]:
    found: list[str] = []
    if isinstance(value, dict):
        attr_type = value.get("type")
        attr_name = str(value.get("typeName", value.get("name", ""))).lower()
        if attr_type == 3 or "nexthop" in attr_name:
            found.extend(_json_values(value.get("value", value)))
        for child in value.values():
            found.extend(_go_bgp_next_hop_values(child))
    elif isinstance(value, list):
        for child in value:
            found.extend(_go_bgp_next_hop_values(child))
    return found


def _contains_exact_ip(value: str, expected: str) -> bool:
    return re.search(
        rf"(?<![0-9.]){re.escape(expected)}(?![0-9.])", value
    ) is not None


def _route_records(
    document: Any, endpoint: dict[str, str]
) -> list[tuple[dict[str, Any], str]]:
    expected_mac = endpoint["mac"].lower()
    expected_ip = endpoint["ip"]
    records: list[tuple[dict[str, Any], str]] = []

    def visit(value: Any, context: tuple[str, ...]) -> None:
        if isinstance(value, dict):
            serialized = json.dumps(value, sort_keys=True)
            identity = " ".join((*context, serialized)).lower()
            has_mac = re.search(
                rf"(?<![0-9a-f]){re.escape(expected_mac)}(?![0-9a-f])",
                identity,
            )
            has_ip = _contains_exact_ip(identity, expected_ip)
            keys = {_normalized_key(str(key)) for key in value}
            has_path_attributes = bool(
                keys & {"attr", "attrs", "attributes", "pathattributes"}
            )
            has_route_identity = bool(
                keys & {"nlri", "prefix", "route"}
            ) or {"mac", "ip"}.issubset(keys) or {
                "macaddress",
                "ipaddress",
            }.issubset(keys)
            if has_mac and has_ip and has_path_attributes and has_route_identity:
                records.append((value, " ".join(context)))
            for key, child in value.items():
                visit(child, (*context, str(key)))
        elif isinstance(value, list):
            for child in value:
                visit(child, context)

    visit(document, ())
    attributed = [
        item
        for item in records
        if _keyed_values(
            item[0], {"attr", "attrs", "attributes", "pathattributes"}
        )
    ]
    return attributed


def _contains_exact_mac(value: str, expected: str) -> bool:
    return re.search(
        rf"(?<![0-9a-f]){re.escape(expected.lower())}(?![0-9a-f])",
        value.lower(),
    ) is not None


def _frr_table_route_presence(
    document: dict[str, Any],
    endpoint: dict[str, str],
) -> tuple[bool | None, str]:
    endpoint_identity_seen = False
    for rd_key, rd_group in document.items():
        if rd_key in {"numPrefix", "numPaths"}:
            continue
        if not isinstance(rd_group, dict):
            continue
        for route_key, route in rd_group.items():
            if route_key == "rd" or not isinstance(route, dict):
                continue
            prefix = route.get("prefix", route_key)
            if not isinstance(prefix, str):
                continue
            identity = f"{rd_key} {rd_group.get('rd', '')} {route_key} {prefix}"
            has_mac = _contains_exact_mac(identity, endpoint["mac"])
            has_ip = _contains_exact_ip(identity, endpoint["ip"])
            if has_mac and has_ip:
                paths = route.get("paths")
                if not isinstance(paths, list):
                    return None, "FRR: unavailable: malformed route paths"
                if any(
                    isinstance(path, dict)
                    for path_set in paths
                    if isinstance(path_set, list)
                    for path in path_set
                ):
                    return True, "FRR route table contains the exact MAC/IP identity"
                endpoint_identity_seen = True
            elif has_mac or has_ip:
                endpoint_identity_seen = True
    if endpoint_identity_seen:
        return None, "FRR: endpoint identity exists but has no valid Type-2 path"
    return False, (
        f"FRR: exact MAC/IP route is absent "
        f"(MAC={endpoint['mac']} IP={endpoint['ip']})"
    )


def _frr_exact_route_observation(
    document: dict[str, Any],
    endpoint: dict[str, str],
    vni: int,
    rd: str,
    rt: str,
    next_hop: str,
    originator: str,
    expected_peer: str,
    expected_router_id: str,
) -> tuple[bool | None, bool, str]:
    route_rd = document.get("rd")
    route_mac = document.get("mac")
    route_ip = document.get("ip")
    if (
        document.get("routeType") != 2
        or route_rd != rd
        or route_mac != endpoint["mac"]
        or route_ip != endpoint["ip"]
    ):
        return None, False, "FRR: exact-route JSON identity does not match the query"
    route_keys = [
        key
        for key in document
        if isinstance(key, str)
        and key.startswith("[2]:")
        and _contains_exact_mac(key, endpoint["mac"])
        and _contains_exact_ip(key, endpoint["ip"])
    ]
    path_sets = [document[key] for key in route_keys]
    if "paths" in document:
        prefix = document["prefix"]
        if not (_contains_exact_mac(prefix, endpoint["mac"])
                and _contains_exact_ip(prefix, endpoint["ip"])):
            return None, False, "FRR: exact-route prefix does not match the query"
        path_sets = [document["paths"]]
    paths = [
        path
        for route_paths in path_sets
        for path_set in route_paths
        for path in path_set
    ]
    if not paths or document["numPaths"] != len(paths):
        return None, False, (
            "FRR: exact-route JSON has inconsistent path data "
            f"(numPaths={document['numPaths']} parsedPaths={len(paths)})"
        )

    rt_match = re.compile(
        rf"(?:RT\s*[:=]\s*)?{re.escape(rt)}(?![0-9])", re.IGNORECASE
    )
    failures: list[str] = []
    for path in paths:
        route_failures: list[str] = []
        extended = path.get("extendedCommunity")
        community = extended.get("string", "") if isinstance(extended, dict) else ""
        if not rt_match.search(community):
            route_failures.append("RT unavailable or incorrect")

        observed_vnis = [path[key] for key in ("VNI", "vni") if key in path]
        if not observed_vnis or any(
            not isinstance(value, str) or value != str(vni)
            for value in observed_vnis
        ):
            route_failures.append("route-level VNI unavailable or incorrect")

        nexthops = path.get("nexthops")
        observed_next_hops = [
            item.get("ip", "")
            for item in nexthops
            if isinstance(item, dict)
        ] if isinstance(nexthops, list) else []
        if next_hop not in observed_next_hops:
            route_failures.append("BGP next hop unavailable or incorrect")

        peer = path.get("peer")
        peer_id = peer.get("peerId") if isinstance(peer, dict) else None
        router_id = peer.get("routerId") if isinstance(peer, dict) else None
        if originator == "frr":
            if (
                path.get("local") is not True
                or peer_id != "0.0.0.0"
                or router_id != expected_router_id
            ):
                route_failures.append("FRR-local route provenance is unavailable or incorrect")
        else:
            if (
                path.get("local") is True
                or peer_id == "0.0.0.0"
                or peer_id != expected_peer
                or router_id != expected_router_id
            ):
                route_failures.append("remote GoBGP peer provenance is unavailable or incorrect")

        if not route_failures:
            return True, True, (
                f"FRR: PASS originator={originator} peer={peer_id} "
                f"MAC={endpoint['mac']} IP={endpoint['ip']} "
                f"VNI={vni} RD={rd} RT={rt} next-hop={next_hop}"
            )
        failures.extend(route_failures)
    return True, False, "FRR: exact route found, but " + "; ".join(
        dict.fromkeys(failures)
    )


def route_observation(
    output: str,
    endpoint: dict[str, str],
    vni: int,
    rd: str,
    rt: str,
    next_hop: str,
    speaker: str,
    originator: str | None = None,
    expected_peer: str | None = None,
    expected_router_id: str | None = None,
) -> tuple[bool | None, bool, str]:
    try:
        document = json.loads(output)
    except json.JSONDecodeError as exc:
        return None, False, f"{speaker}: unavailable: malformed JSON ({exc})"
    if speaker.startswith("FRR"):
        if _valid_frr_exact_route_schema(document):
            if originator is None or expected_peer is None:
                return True, False, (
                    "FRR: exact-route JSON requires route provenance and exact "
                    "peer verification"
                )
            return _frr_exact_route_observation(
                document,
                endpoint,
                vni,
                rd,
                rt,
                next_hop,
                originator,
                expected_peer,
                expected_router_id or expected_peer,
            )
        if not _valid_frr_evpn_schema(document):
            return None, False, "FRR: unavailable: unexpected EVPN JSON schema"
        found, detail = _frr_table_route_presence(document, endpoint)
        if found is True:
            return True, False, (
                "FRR: route-table identity is present; exact-route JSON is "
                "required for attribute verification"
            )
        return found, False, detail
    if not isinstance(document, (dict, list)):
        return None, False, f"{speaker}: unavailable: expected JSON object or array"

    records = _route_records(document, endpoint)
    if not records:
        serialized = json.dumps(document, sort_keys=True).lower()
        has_mac = re.search(
            rf"(?<![0-9a-f]){re.escape(endpoint['mac'].lower())}(?![0-9a-f])",
            serialized,
        )
        has_ip = _contains_exact_ip(serialized, endpoint["ip"])
        if has_mac and has_ip:
            return None, False, (
                f"{speaker}: unavailable: JSON contains the endpoint identity "
                "but no recognized route/path object with attributes"
            )
        return False, False, (
            f"{speaker}: exact MAC/IP route is absent "
            f"(MAC={endpoint['mac']} IP={endpoint['ip']})"
        )

    expected_rd = re.compile(rf"{re.escape(rd)}(?![0-9])")
    expected_rt = re.compile(
        rf"(?:RT\s*[:=]\s*)?{re.escape(rt)}(?![0-9])", re.IGNORECASE
    )
    expected_vni = re.compile(rf"(?<![0-9]){vni}(?![0-9])")
    for record, context in records:
        failures: list[str] = []
        peer_addresses = _keyed_values(record, {"peeraddress", "peerip"})
        if originator == "gobgp" and speaker.startswith("GoBGP"):
            if peer_addresses:
                failures.append("GoBGP-originated route unexpectedly has a peer path")
        elif originator == "frr" and speaker.startswith("GoBGP"):
            if expected_peer not in peer_addresses:
                failures.append("route is not received from the expected FRR peer")
        serialized = json.dumps(record, sort_keys=True)
        route_text = f"{context} {serialized}"
        rd_values = _keyed_values(
            record, {"rd", "routedistinguisher", "routedistinguishervalue"}
        )
        if not any(expected_rd.search(value) for value in [route_text, *rd_values]):
            failures.append("RD unavailable or incorrect")

        labels = _keyed_values(record, {"label", "labels", "labelstack", "vni", "vnis"})
        if not any(expected_vni.search(value) for value in labels):
            failures.append("VNI/label unavailable or incorrect")

        route_target_values = _keyed_values(
            record,
            {
                "rt",
                "rts",
                "routetarget",
                "routetargets",
                "extendedcommunity",
                "extendedcommunities",
                "extcommunity",
                "extcommunities",
            },
        )
        route_target_values.extend(_go_bgp_extended_community_values(record))
        if not any(expected_rt.search(value) for value in route_target_values):
            failures.append("RT unavailable or incorrect")

        next_hops = _keyed_values(
            record, {"nexthop", "nexthops", "nexthopaddress", "nexthopip"}
        )
        next_hops.extend(_go_bgp_next_hop_values(record))
        if not any(_contains_exact_ip(value, next_hop) for value in next_hops):
            failures.append("BGP next hop/remote VTEP unavailable or incorrect")
        if not failures:
            return True, True, (
                f"{speaker}: PASS MAC={endpoint['mac']} IP={endpoint['ip']} "
                f"VNI={vni} RD={rd} RT={rt} next-hop={next_hop}"
            )
        failures = list(dict.fromkeys(failures))
        last_failures = failures
    return True, False, f"{speaker}: exact route found, but " + "; ".join(last_failures)


def route_expectations(
    manifest: dict[str, Any],
    endpoint: dict[str, Any],
    speaker: str,
) -> tuple[str, str, str | None, str]:
    parameters = manifest["parameters"]
    originator = endpoint.get("originator")
    if originator not in {"gobgp", "frr"}:
        raise FixtureError(f"route {endpoint.get('name')} has no valid originator")
    expected_receiver = "frr" if originator == "gobgp" else "gobgp"
    if endpoint.get("receiver") != expected_receiver:
        raise FixtureError(f"route {endpoint.get('name')} has an invalid receiver")
    if speaker == "GoBGP":
        if originator == "gobgp":
            return (
                originator,
                endpoint.get("local_vtep", "0.0.0.0"),
                None,
                parameters["gobgp_router_id"],
            )
        return (
            originator,
            parameters["remote_vtep"],
            parameters["frr_address"],
            parameters["frr_router_id"],
        )
    if speaker == "FRR":
        if originator == "gobgp":
            return (
                originator,
                endpoint.get("local_vtep", parameters["gobgp_address"]),
                parameters["gobgp_address"],
                parameters["gobgp_router_id"],
            )
        return originator, parameters["remote_vtep"], None, parameters["frr_router_id"]
    raise FixtureError(f"unsupported BGP speaker {speaker}")


def observe_route_pair(
    directory: Path,
    manifest: dict[str, Any],
    endpoint: dict[str, Any],
    gobgp_output: JsonCommandOutput,
    frr_table_output: JsonCommandOutput,
) -> tuple[bool | None, bool, list[str], str]:
    parameters = manifest["parameters"]
    originator, gobgp_next_hop, gobgp_peer, _ = route_expectations(
        manifest, endpoint, "GoBGP"
    )
    gobgp_observation = route_observation(
        gobgp_output.stdout,
        endpoint,
        parameters["vni"],
        parameters["rd"],
        parameters["rt"],
        gobgp_next_hop,
        "GoBGP",
        originator,
        gobgp_peer,
    )
    table_document = json.loads(frr_table_output.stdout)
    frr_found, frr_presence_detail = _frr_table_route_presence(
        table_document, endpoint
    )
    details = [gobgp_observation[2], frr_presence_detail]
    if frr_found is not True:
        found = (
            False
            if gobgp_observation[0] is False and frr_found is False
            else None
        )
        return found, False, details, ""

    try:
        exact_output = frr_exact_route_output(directory, manifest, endpoint)
    except FixtureError as exc:
        details.append(str(exc))
        return True, False, details, str(exc)
    frr_originator, frr_next_hop, frr_peer, frr_router_id = route_expectations(
        manifest, endpoint, "FRR"
    )
    frr_observation = _frr_exact_route_observation(
        exact_output.document,
        endpoint,
        parameters["vni"],
        parameters["rd"],
        parameters["rt"],
        frr_next_hop,
        frr_originator,
        frr_peer or "0.0.0.0",
        frr_router_id,
    )
    details.append(frr_observation[2])
    if gobgp_observation[0] is True and frr_observation[0] is True:
        found = True
    elif gobgp_observation[0] is False and frr_observation[0] is False:
        found = False
    else:
        found = None
    return (
        found,
        gobgp_observation[1] and frr_observation[1],
        details,
        json_output_evidence(exact_output),
    )


def route_state_confirmed(
    target: tuple[bool | None, bool, list[str], str],
    present: bool,
    sibling: tuple[bool | None, bool, list[str], str] | None = None,
) -> bool:
    target_ok = (
        target[0] is True and target[1]
        if present
        else target[0] is False
    )
    sibling_ok = sibling is None or (
        sibling[0] is True and sibling[1]
    )
    return target_ok and sibling_ok


def wait_route_state(
    directory: Path,
    manifest: dict[str, Any],
    endpoint: dict[str, Any],
    present: bool,
    sibling: dict[str, Any] | None = None,
) -> tuple[JsonCommandOutput, str]:
    deadline = time.monotonic() + WAIT_TIMEOUT
    last_outputs: tuple[JsonCommandOutput, JsonCommandOutput] | None = None
    last_details: list[str] = []
    last_route_evidence: list[str] = []
    sibling_details: list[str] = []
    while time.monotonic() < deadline:
        last_outputs = route_outputs(directory, manifest)
        observation = observe_route_pair(
            directory, manifest, endpoint, last_outputs[0], last_outputs[1]
        )
        last_details = observation[2]
        last_route_evidence = [observation[3]] if observation[3] else []
        sibling_observation = None
        sibling_details = []
        if sibling:
            sibling_observation = observe_route_pair(
                directory, manifest, sibling, last_outputs[0], last_outputs[1]
            )
            sibling_details = sibling_observation[2]
            if sibling_observation[3]:
                last_route_evidence.append(sibling_observation[3])
        if route_state_confirmed(observation, present, sibling_observation):
            return last_outputs[0], (
                "FRR route table:\n"
                + json_output_evidence(last_outputs[1])
                + "\nFRR exact-route JSON:\n"
                + "\n".join(last_route_evidence)
            )
        time.sleep(2)
    state = "present" if present else "absent"
    if last_outputs is None:
        raise FixtureError("route state polling did not execute")
    raise FixtureError(
        f"route state was not confirmed as {state} on both speakers within "
        f"{WAIT_TIMEOUT}s; observations: "
        + "; ".join(last_details)
        + ("; sibling: " + "; ".join(sibling_details) if sibling_details else "")
        + "\nGoBGP JSON:\n"
        + json_output_evidence(last_outputs[0])
        + "\nFRR JSON:\n"
        + json_output_evidence(last_outputs[1])
    )


def add_report_result(
    directory: Path,
    manifest: dict[str, Any],
    action: str,
    status: str,
    detail: str,
) -> None:
    record(directory, manifest, action, status, detail)


def start_fixture(args: argparse.Namespace) -> None:
    gobgp_image = digest_image(args.gobgp_image, "GoBGP image")
    frr_image = digest_image(args.frr_image, "FRR image")
    valid_asn(args.gobgp_asn, "GoBGP ASN")
    valid_asn(args.frr_asn, "FRR ASN")
    if not 1 <= args.vni <= 16777215:
        raise FixtureError("VNI must be in 1..16777215")

    run_id = str(uuid.uuid4())
    subnet = generated_subnet(run_id)
    conflicts = network_overlaps_existing(subnet)
    if conflicts:
        raise FixtureError(
            f"generated subnet {subnet} overlaps existing Docker networks: "
            + ", ".join(conflicts)
        )
    endpoint_subnet = generated_endpoint_subnet(run_id, subnet)
    gobgp_address = parse_ipv4(args.gobgp_address or str(subnet.network_address + 3), "GoBGP address")
    frr_address = parse_ipv4(args.frr_address or str(subnet.network_address + 2), "FRR BGP peer address")
    remote_vtep = parse_ipv4(
        args.remote_vtep or str(subnet.network_address + 4), "remote VTEP"
    )
    if any(address not in subnet for address in (gobgp_address, frr_address, remote_vtep)):
        raise FixtureError("GoBGP, FRR peer, and remote VTEP addresses must belong to the generated subnet")
    endpoint_addresses = {
        subnet.network_address + 101,
        subnet.network_address + 102,
        subnet.network_address + 103,
    }
    reserved_addresses = {
        subnet.network_address,
        subnet.broadcast_address,
        subnet.network_address + 1,
        *endpoint_addresses,
    }
    if (
        gobgp_address == frr_address
        or gobgp_address in reserved_addresses
        or frr_address in reserved_addresses
        or remote_vtep in reserved_addresses
        or len({gobgp_address, frr_address, remote_vtep}) != 3
    ):
        raise FixtureError("GoBGP, FRR peer, and remote VTEP addresses must be distinct and non-reserved")
    gobgp_router_id = parse_ipv4(args.gobgp_router_id or str(gobgp_address), "GoBGP router ID")
    frr_router_id = parse_ipv4(args.frr_router_id or str(frr_address), "FRR router ID")
    if gobgp_router_id == frr_router_id:
        raise FixtureError("GoBGP and FRR router IDs must be distinct")
    default_rd = f"{gobgp_address}:{args.vni}"
    rd = valid_rd_rt(args.rd or default_rd, "RD")
    rt = valid_rd_rt(args.rt or f"{args.frr_asn}:{args.vni}", "RT")

    directory = args.state_root / run_id
    if "\n" in str(directory) or "\r" in str(directory):
        raise FixtureError("state directory path cannot contain line breaks")
    directory.mkdir(parents=True, exist_ok=False)
    network_name = f"evpn-type2-{run_id[:12]}"
    params = {
        "subnet": str(subnet),
        "gobgp_address": str(gobgp_address),
        "frr_address": str(frr_address),
        "local_vtep": str(gobgp_address),
        "remote_vtep": str(remote_vtep),
        "gobgp_router_id": str(gobgp_router_id),
        "frr_router_id": str(frr_router_id),
        "rd": rd,
        "rt": rt,
        "vni": args.vni,
        "udp_port": 4789,
        "bridge_mac": mac_for_role(run_id, "bridge"),
        "endpoint_subnet": str(endpoint_subnet),
        "endpoint_gateway": str(endpoint_subnet.network_address + 1),
        "endpoint_namespace": endpoint_namespace_name(run_id),
        "endpoints": [
            {
                "name": "primary",
                "mac": mac_for_role(run_id, "primary"),
                "ip": str(subnet.network_address + 101),
                "originator": "gobgp",
                "receiver": "frr",
                "state": "not-advertised",
            },
            {
                "name": "secondary",
                "mac": mac_for_role(run_id, "secondary"),
                "ip": str(endpoint_subnet.network_address + 2),
                "originator": "frr",
                "receiver": "gobgp",
                "state": "not-advertised",
            },
            {
                "name": "netlink-primary",
                "mac": mac_for_role(run_id, "netlink-primary"),
                "ip": str(endpoint_subnet.network_address + 3),
                "originator": "frr",
                "receiver": "gobgp",
                "state": "not-advertised",
            },
        ],
    }
    values = {
        **params,
        "run_id": run_id,
        "gobgp_asn": args.gobgp_asn,
        "frr_asn": args.frr_asn,
        "gobgp_image": gobgp_image,
        "frr_image": frr_image,
        "network_name": network_name,
    }
    project = f"evpn-type2-{run_id.replace('-', '')[:20]}"
    manifest: dict[str, Any] = {
        "schema_version": 1,
        "fixture": "evpn-type2",
        "run_id": run_id,
        "created_at": utc_now(),
        "updated_at": utc_now(),
        "source_commit": run_command(
            ["git", "-C", str(ROOT), "rev-parse", "HEAD"]
        ).stdout.strip(),
        "lab_project": project,
        "images": {"gobgp": gobgp_image, "frr": frr_image},
        "parameters": params,
        "resources": {"containers": [], "networks": []},
        "created_resources": {"containers": [], "networks": []},
        "phase": "preparing",
        "results": [],
    }
    write_configs(directory, values)
    save_manifest(directory, manifest)
    lab.init(directory, "type2", network_name, network_name, fixture={
        **params, "run_id": run_id, "project": project, "images": manifest["images"],
    })
    report = {
        "schema_version": 1,
        "run_id": run_id,
        "source_commit": manifest["source_commit"],
        "images": manifest["images"],
        "topology": params,
        "results": [],
    }
    write_json(directory / "report.json", report)
    try:
        result = run_command(
            [sys.executable, str(ROOT / "tools/e2e/containerlab.py"),
             "up", "--directory", str(directory), "--profile", "type2",
             "--cluster", network_name, "--network", network_name],
            timeout=600,
        )
        capture_resources(directory, manifest, allow_empty=False)
        setup_output = setup_frr_topology(directory, manifest)
        gobgp_api_output = wait_for_gobgp_api(directory, manifest)
        frr_daemon_output = wait_for_frr_daemon(directory, manifest)
        runtime = {
            "speaker_mode": "standalone (not managed k8gobgp coverage)",
            "gobgp": container_exec(directory, manifest, "gobgp", "gobgpd", "--version"),
            "frr": container_exec(directory, manifest, "frr", "vtysh", "-c", "show version"),
            "images": docker_json("image", "inspect", gobgp_image, frr_image),
        }
        write_json(directory / "runtime-versions.json", runtime)
        gobgp_peer = manifest["parameters"]["gobgp_address"]
        frr_peer = manifest["parameters"]["frr_address"]
        try:
            peer_output = wait_for_peer(
                directory, manifest, gobgp_peer, frr_peer
            )
        except FixtureError as exc:
            add_report_result(
                directory, manifest, "bgp-peer-readiness", "FAIL", str(exc)
            )
            raise
        add_report_result(
            directory, manifest, "bgp-peer-readiness", "PASS", peer_output
        )
        try:
            evpn_family_output = wait_for_evpn_family(
                directory, manifest, gobgp_peer, frr_peer
            )
        except FixtureError as exc:
            add_report_result(
                directory, manifest, "evpn-family-readiness", "FAIL", str(exc)
            )
            raise
        add_report_result(
            directory,
            manifest,
            "evpn-family-readiness",
            "PASS",
            evpn_family_output,
        )
        manifest["phase"] = "ready"
        add_report_result(
            directory,
            manifest,
            "start",
            "PASS",
            result.stdout
            + "\n"
            + "\n".join(setup_output)
            + "\nGoBGP API readiness:\n"
            + gobgp_api_output
            + "\nFRR daemon readiness:\n"
            + frr_daemon_output
            + "\nBGP session readiness:\n"
            + peer_output
            + "\nEVPN family readiness:\n"
            + evpn_family_output,
        )
    except Exception as exc:
        manifest["phase"] = "startup-failed"
        try:
            capture_resources(directory, manifest)
        except Exception as capture_exc:
            exc = FixtureError(f"{exc}\nResource capture also failed: {capture_exc}")
        add_report_result(directory, manifest, "start", "FAIL", str(exc))
        raise
    print(f"run_id={run_id}")
    print(f"manifest={directory / 'manifest.json'}")


def endpoint_by_name(manifest: dict[str, Any], name: str) -> dict[str, str]:
    for endpoint in manifest["parameters"]["endpoints"]:
        if endpoint["name"] == name:
            return endpoint
    raise FixtureError(f"unknown endpoint {name!r}")


def update_endpoint_state(manifest: dict[str, Any], endpoint: dict[str, str], state: str) -> None:
    endpoint["state"] = state


def gobgp_route_command(
    directory: Path,
    manifest: dict[str, Any],
    endpoint: dict[str, Any],
    operation: str,
) -> str:
    if operation not in {"add", "del"}:
        raise FixtureError("GoBGP route operation must be add or del")
    parameters = manifest["parameters"]
    mac = endpoint["mac"]
    if not re.fullmatch(r"(?:[0-9a-fA-F]{2}:){5}[0-9a-fA-F]{2}", mac):
        raise FixtureError("route MAC must contain six colon-separated octets")
    address = parse_ipv4(endpoint["ip"], "route IP")
    vni = parameters["vni"]
    if type(vni) is not int or not 1 <= vni <= 16777215:
        raise FixtureError("VNI must be in 1..16777215")
    rd = valid_rd_rt(parameters["rd"], "RD")
    rt = valid_rd_rt(parameters["rt"], "RT")
    return container_exec(
        directory,
        manifest,
        "gobgp",
        "gobgp",
        "global",
        "rib",
        operation,
        "-a",
        "evpn",
        "macadv",
        mac,
        str(address),
        "etag",
        "0",
        "label",
        str(vni),
        "rd",
        rd,
        "rt",
        rt,
        "encap",
        "vxlan",
    )


def action_fixture(args: argparse.Namespace) -> None:
    directory = run_dir(args.state_root, args.run_id)
    manifest = load_manifest(directory)
    if manifest["phase"] != "ready":
        raise FixtureError(f"fixture is not ready (phase={manifest['phase']})")
    action = args.action
    if action == "observe":
        endpoints = [
            endpoint
            for endpoint in manifest["parameters"]["endpoints"]
            if endpoint["state"] == "advertised"
        ]
        if not endpoints:
            raise FixtureError("no routes are marked advertised; missing evidence is not PASS")
        outputs = route_outputs(directory, manifest)
        missing = []
        details = []
        exact_evidence = []
        for endpoint in endpoints:
            found, valid, route_details, frr_evidence = observe_route_pair(
                directory, manifest, endpoint, outputs[0], outputs[1]
            )
            details.extend(route_details)
            if frr_evidence:
                exact_evidence.append(frr_evidence)
            if found is not True or not valid:
                missing.append(endpoint["name"])
        status = "FAIL" if missing else "PASS"
        detail = (
            "\n".join(details)
            + "\nGoBGP JSON:\n"
            + json_output_evidence(outputs[0])
            + "\nFRR route table JSON:\n"
            + json_output_evidence(outputs[1])
            + "\nFRR exact-route JSON:\n"
            + "\n".join(exact_evidence)
        )
        add_report_result(directory, manifest, action, status, detail)
        if missing:
            raise FixtureError("route evidence missing from GoBGP/FRR: " + ", ".join(missing))
        return

    endpoint = endpoint_by_name(manifest, args.endpoint)
    endpoint_pid = (
        endpoint_namespace_pid(directory, manifest)
        if endpoint["originator"] == "frr"
        else None
    )
    if action == "advertise":
        if endpoint["state"] == "advertised":
            raise FixtureError("route is already marked advertised")
        capture_sessions = []
        if endpoint["originator"] == "frr":
            if endpoint_pid is None:
                raise FixtureError("FRR endpoint namespace process is unavailable")
            commands = frr_endpoint_advertisement_commands(
                manifest["parameters"], endpoint, endpoint_pid,
                reactivate=endpoint["state"] == "withdrawn",
            )
            capture_sessions = start_arp_captures(
                directory, manifest, endpoint_pid, endpoint
            )
        try:
            if endpoint["originator"] == "gobgp":
                command_output = gobgp_route_command(
                    directory, manifest, endpoint, "add"
                )
            else:
                command_output = "\n".join(
                    container_exec(directory, manifest, "frr", *command)
                    for command in commands
                )
            observed = wait_route_state(directory, manifest, endpoint, True)
        except Exception as exc:
            capture_evidence = (
                finish_arp_captures(capture_sessions) if capture_sessions else ""
            )
            add_report_result(
                directory,
                manifest,
                f"advertise-{endpoint['name']}",
                "FAIL",
                str(exc) + capture_evidence,
            )
            raise
        capture_evidence = (
            finish_arp_captures(capture_sessions) if capture_sessions else ""
        )
        try:
            update_endpoint_state(manifest, endpoint, "advertised")
            add_report_result(
                directory,
                manifest,
                f"advertise-{endpoint['name']}",
                "PASS",
                command_output
                + "\nGoBGP JSON:\n"
                + json_output_evidence(observed[0])
                + "\n"
                + observed[1]
                + capture_evidence,
            )
        except Exception as exc:
            add_report_result(
                directory,
                manifest,
                f"advertise-{endpoint['name']}",
                "FAIL",
                str(exc) + capture_evidence,
            )
            raise
        return

    if endpoint["state"] != "advertised":
        raise FixtureError("route has not been successfully advertised")
    if endpoint["originator"] == "frr":
        if endpoint_pid is None:
            raise FixtureError("FRR endpoint namespace process is unavailable")
        commands = frr_endpoint_withdrawal_commands(
            manifest["parameters"], endpoint, endpoint_pid
        )
    sibling = next(
        (
            other
            for other in manifest["parameters"]["endpoints"]
            if other["name"] != endpoint["name"] and other["state"] == "advertised"
        ),
        None,
    )
    try:
        if endpoint["originator"] == "gobgp":
            command_output = gobgp_route_command(
                directory, manifest, endpoint, "del"
            )
        else:
            command_output = "\n".join(
                container_exec(directory, manifest, "frr", *command)
                for command in commands
            )
        observed = wait_route_state(directory, manifest, endpoint, False, sibling)
        update_endpoint_state(manifest, endpoint, "withdrawn")
        add_report_result(
            directory,
            manifest,
            f"withdraw-{endpoint['name']}",
            "PASS",
            command_output
            + "\nGoBGP JSON:\n"
            + json_output_evidence(observed[0])
            + "\n"
            + observed[1],
        )
    except Exception as exc:
        add_report_result(
            directory, manifest, f"withdraw-{endpoint['name']}", "FAIL", str(exc)
        )
        raise


def resource_logs(directory: Path, container_id: str) -> None:
    result = run_command(["docker", "logs", "--tail", "2000", container_id], check=False)
    logs = directory / "evidence" / "container-logs"
    logs.mkdir(parents=True, exist_ok=True)
    (logs / f"{container_id[:12]}.log").write_text(result.stdout)


def stop_fixture(args: argparse.Namespace) -> None:
    directory = run_dir(args.state_root, args.run_id)
    manifest = load_manifest(directory)
    if manifest["phase"] == "stopped":
        print(f"already stopped: {args.run_id}")
        return
    capture_resources(directory, manifest, compare_recorded=True)
    resources = manifest["resources"]
    log_paths = []
    for container_id in resources["containers"]:
        resource_logs(directory, container_id)
        log_paths.append(f"evidence/container-logs/{container_id[:12]}.log")
    try:
        topology_output = ""
        topology_error = ""
        if resources["containers"]:
            try:
                topology_output = "\n".join(
                    cleanup_frr_topology(directory, manifest)
                )
            except Exception as exc:
                topology_error = str(exc)
        run_command(
            [sys.executable, str(ROOT / "tools/e2e/containerlab.py"),
             "down", "--directory", str(directory)],
            timeout=600,
        )
        manifest["resources"] = {"containers": [], "networks": []}
        manifest["phase"] = "stopped"
        add_report_result(
            directory,
            manifest,
            "stop",
            "PASS",
            "Removed only manifest-owned resources. Captured logs: "
            + (", ".join(log_paths) if log_paths else "none")
            + "\nEndpoint topology cleanup:\n"
            + (
                topology_output or "no endpoint interfaces remained"
                if not topology_error
                else "in-container cleanup failed; owned container removal "
                "also removed its namespace and interfaces: " + topology_error
            ),
        )
    except Exception as exc:
        manifest["phase"] = "cleanup-failed"
        add_report_result(directory, manifest, "stop", "FAIL", str(exc))
        raise
    print(f"stopped={args.run_id}")
    print(f"evidence={directory}")


def add_state_root(parser: argparse.ArgumentParser) -> None:
    parser.add_argument("--state-root", type=Path, default=DEFAULT_STATE_ROOT)


def parser() -> argparse.ArgumentParser:
    result = argparse.ArgumentParser(description=__doc__)
    commands = result.add_subparsers(dest="command", required=True)
    start = commands.add_parser("start", help="create an isolated GoBGP/FRR fixture")
    add_state_root(start)
    start.add_argument("--gobgp-image", required=True)
    start.add_argument("--frr-image", required=True)
    start.add_argument("--gobgp-asn", type=int, default=64512)
    start.add_argument("--frr-asn", type=int, default=64512)
    start.add_argument("--gobgp-address")
    start.add_argument("--frr-address")
    start.add_argument("--remote-vtep")
    start.add_argument("--gobgp-router-id")
    start.add_argument("--frr-router-id")
    start.add_argument("--rd")
    start.add_argument("--rt")
    start.add_argument("--vni", type=int, default=1000)
    start.set_defaults(handler=start_fixture)

    for name in ("advertise", "withdraw"):
        action = commands.add_parser(name, help=f"{name} one owned Type-2 route")
        add_state_root(action)
        action.add_argument("run_id")
        action.add_argument(
            "endpoint", choices=("primary", "secondary", "netlink-primary")
        )
        action.set_defaults(handler=action_fixture, action=name)

    observe = commands.add_parser("observe", help="verify routes independently on both speakers")
    add_state_root(observe)
    observe.add_argument("run_id")
    observe.set_defaults(handler=action_fixture, action="observe")

    stop = commands.add_parser("stop", help="stop and remove only this run's resources")
    add_state_root(stop)
    stop.add_argument("run_id")
    stop.set_defaults(handler=stop_fixture)
    return result


def main() -> int:
    args = parser().parse_args()
    if shutil.which("docker") is None:
        print("docker CLI is required", file=sys.stderr)
        return 2
    try:
        args.state_root = args.state_root.expanduser().resolve()
        args.state_root.mkdir(parents=True, exist_ok=True)
        with lab.exclusive():
            args.handler(args)
    except (FixtureError, lab.LabError, OSError, KeyError) as exc:
        print(f"fixture error: {exc}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
