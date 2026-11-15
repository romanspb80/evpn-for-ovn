#!/usr/bin/env python3
"""Disposable Kindnet + Multus + project-owned OVN/OVS secondary CNI proof."""

import errno
import hashlib
import ipaddress
import json
import os
from pathlib import Path
import re
import secrets
import shutil
import struct
import subprocess
import sys
import time
import traceback
import uuid

import containerlab as lab


ROOT = lab.ROOT
OVN_IMAGE = "evpn4ovn/ovn-ovs:kind-26.09"
OVN_IMAGE_ID = "sha256:c593278d6b9fffcd51a7b355bd80efca291eda4bc7848c3dd838078ef5109505"
MULTUS_URL = ("https://github.com/k8snetworkplumbingwg/multus-cni/releases/"
              "download/v4.3.1/multus-cni_4.3.1_linux_amd64.tar.gz")
MULTUS_SHA256 = "88346aaec1049ec669e11157771b498e056a44073cbf93975361d52e2e338f9e"
LINK_KEYS = ("ifindex", "ifname", "address", "ifalias", "mtu", "master", "operstate")
HOST_DATAPLANE_PROCESSES = {"ovsdb-server", "ovs-vswitchd", "ovn-controller", "ovn-northd"}
HOST_DATAPLANE_SOCKETS = (
    Path("/run/openvswitch/db.sock"), Path("/var/run/openvswitch/db.sock"),
    Path("/run/ovn/ovnnb_db.sock"), Path("/run/ovn/ovnsb_db.sock"),
    Path("/var/run/ovn/ovnnb_db.sock"), Path("/var/run/ovn/ovnsb_db.sock"),
)


class IsolationError(lab.LabError):
    pass


class SecondaryEndpointNotReady(IsolationError):
    pass


def datapath_tunnel_key(raw, logical_switch_uuid):
    try:
        switch_uuid = str(uuid.UUID(logical_switch_uuid))
        document = json.loads(raw)
        headings = document["headings"]
        rows = document["data"]
        if (not isinstance(headings, list) or len(headings) != 3 or
                set(headings) != {"_uuid", "external_ids", "tunnel_key"} or
                not isinstance(rows, list) or
                any(not isinstance(row, list) or len(row) != 3 for row in rows)):
            raise ValueError("unexpected headings or rows")
        matches = []
        for row in rows:
            item = dict(zip(headings, row))
            ids = item["external_ids"]
            if (not isinstance(ids, list) or len(ids) != 2 or ids[0] != "map" or
                    not isinstance(ids[1], list) or any(
                        not isinstance(pair, list) or len(pair) != 2 or
                        not all(isinstance(value, str) for value in pair) for pair in ids[1])):
                raise ValueError("invalid external_ids map")
            mapping = dict(ids[1])
            if len(mapping) != len(ids[1]):
                raise ValueError("duplicate external_ids key")
            if mapping.get("logical-switch") == switch_uuid:
                matches.append(item)
        if len(matches) != 1:
            raise IsolationError(f"expected one Datapath_Binding for logical switch {switch_uuid}, "
                                 f"found {len(matches)}")
        match = matches[0]
        identity = match["_uuid"]
        if not isinstance(identity, list) or len(identity) != 2 or identity[0] != "uuid":
            raise ValueError("invalid Datapath_Binding UUID")
        datapath_uuid = str(uuid.UUID(identity[1]))
        key = match["tunnel_key"]
        if type(key) is not int or not 1 <= key <= 0xFFFFFF:
            raise IsolationError(f"invalid OVN datapath tunnel_key for {switch_uuid}: {key!r}")
        return {"logicalSwitchUUID": switch_uuid, "datapathUUID": datapath_uuid,
                "tunnelKey": key, "externalIDs": dict(match["external_ids"][1])}
    except (ValueError, KeyError, TypeError, json.JSONDecodeError) as error:
        raise IsolationError(f"unexpected Datapath_Binding output: {error}") from error


def secondary_sbdb_remote(service, endpoints, central_pod, owner):
    if service.get("metadata", {}).get("labels", {}).get("evpn.e2e/owner") != owner:
        raise IsolationError("secondary OVN Service ownership mismatch")
    spec = service.get("spec", {})
    try:
        address = ipaddress.ip_address(spec.get("clusterIP", ""))
    except ValueError as error:
        raise IsolationError("secondary OVN Service has no usable ClusterIP") from error
    if (address.is_unspecified or address.is_loopback or address.is_multicast or
            address.is_link_local):
        raise IsolationError("secondary OVN Service has no usable ClusterIP")
    ports = [p for p in spec.get("ports", []) if p.get("name") == "southbound"]
    if (len(ports) != 1 or ports[0].get("protocol", "TCP") != "TCP" or
            ports[0].get("port") != 6642 or ports[0].get("targetPort") != 6642):
        raise IsolationError("secondary OVN Service Southbound port mismatch")
    pod_metadata = central_pod.get("metadata", {})
    pod_ip = central_pod.get("status", {}).get("podIP")
    if not pod_metadata.get("uid") or not pod_ip:
        raise IsolationError("secondary OVN central Pod identity is incomplete")
    ready = []
    for subset in endpoints.get("subsets", []):
        if not any(p.get("name") == "southbound" and p.get("port") == 6642 and
                   p.get("protocol", "TCP") == "TCP" for p in subset.get("ports", [])):
            continue
        ready.extend(subset.get("addresses", []))
    if not any(a.get("ip") == pod_ip and
               a.get("targetRef", {}).get("uid") == pod_metadata["uid"] and
               a.get("targetRef", {}).get("kind") == "Pod" for a in ready):
        raise SecondaryEndpointNotReady("secondary OVN Service has no ready central SBDB endpoint")
    host = f"[{address}]" if address.version == 6 else str(address)
    return f"tcp:{host}:6642"


def primary_link_inventory(document):
    links = json.loads(document) if isinstance(document, str) else document
    result = {}
    for link in links:
        result[link["ifname"]] = {key: link.get(key) for key in LINK_KEYS}
        info = link.get("linkinfo") or {}
        result[link["ifname"]]["kind"] = info.get("info_kind")
    return result


def expected_lsp_name(pod_uid, network_id="secondary-isolation", attachment_id="s"):
    endpoint_id = f"{network_id}|{attachment_id}|{pod_uid}"
    sanitized_endpoint = re.sub(r"[^a-z0-9-]", "", endpoint_id.lower())
    return "-".join((
        "net1", network_id, attachment_id, sanitized_endpoint,
    ))[:63]


def secondary_ip(suffix):
    return {"a": "192.0.2.10", "b": "192.0.2.11"}[suffix]


def decode_geneve_pcap(path):
    return _decode_pcap(path, _decode_geneve_frame)


def decode_icmp_pcap(path):
    return _decode_pcap(path, _decode_icmp_frame)


def _decode_pcap(path, decoder):
    data = Path(path).read_bytes()
    formats = {
        b"\xd4\xc3\xb2\xa1": "<",
        b"\xa1\xb2\xc3\xd4": ">",
        b"\x4d\x3c\xb2\xa1": "<",
        b"\xa1\xb2\x3c\x4d": ">",
    }
    if len(data) < 24 or data[:4] not in formats:
        raise IsolationError("unsupported or truncated Geneve pcap header")
    endian = formats[data[:4]]
    link_type = struct.unpack_from(endian + "I", data, 20)[0]
    if link_type not in (1, 113, 276):
        raise IsolationError(f"unsupported Geneve pcap link type: {link_type}")
    packets = []
    offset = 24
    while offset < len(data):
        if offset + 16 > len(data):
            raise IsolationError("truncated Geneve pcap packet header")
        _, _, captured, original = struct.unpack_from(endian + "IIII", data, offset)
        offset += 16
        if captured > original or offset + captured > len(data):
            raise IsolationError("truncated Geneve pcap frame")
        frame = data[offset:offset + captured]
        offset += captured
        frame = _normalize_link_layer(frame, link_type)
        if frame is None:
            continue
        packet = decoder(frame)
        if packet is not None:
            packets.append(packet)
    if offset != len(data):
        raise IsolationError("invalid Geneve pcap framing")
    return packets


def _normalize_link_layer(frame, link_type):
    if link_type == 1:
        return frame
    if link_type == 113:
        if len(frame) < 16:
            return None
        protocol, payload = frame[14:16], frame[16:]
    else:
        if len(frame) < 20:
            return None
        protocol, payload = frame[:2], frame[20:]
    return b"\x00" * 12 + protocol + payload


def _decode_geneve_frame(frame):
    if len(frame) < 14 or frame[12:14] != b"\x08\x00":
        return None
    outer = _decode_ipv4(frame, 14, len(frame))
    if outer is None or outer["protocol"] != 17:
        return None
    udp = outer["offset"]
    if udp + 16 > len(frame):
        return None
    source_port, destination_port = struct.unpack_from("!HH", frame, udp)
    if destination_port != 6081:
        return None
    udp_length = struct.unpack_from("!H", frame, udp + 4)[0]
    udp_end = udp + udp_length
    if udp_length < 16 or udp_end > outer["end"]:
        return None
    geneve = udp + 8
    option_length = (frame[geneve] & 0x3f) * 4
    if (frame[geneve] >> 6 != 0 or frame[geneve + 2:geneve + 4] != b"\x65\x58" or
            geneve + 8 + option_length + 14 > udp_end):
        return None
    vni = int.from_bytes(frame[geneve + 4:geneve + 7], "big")
    inner_ethernet = geneve + 8 + option_length
    if frame[inner_ethernet + 12:inner_ethernet + 14] != b"\x08\x00":
        return None
    inner = _decode_ipv4(frame, inner_ethernet + 14, udp_end)
    if inner is None or inner["protocol"] != 1:
        return None
    icmp = inner["offset"]
    if icmp + 8 > inner["end"]:
        return None
    kind, code = frame[icmp], frame[icmp + 1]
    if kind not in (0, 8) or code != 0:
        return None
    identifier, sequence = struct.unpack_from("!HH", frame, icmp + 4)
    return {
        "outerSource": outer["source"],
        "outerDestination": outer["destination"],
        "outerSourcePort": source_port,
        "vni": vni,
        "innerSource": inner["source"],
        "innerDestination": inner["destination"],
        "icmpType": kind,
        "icmpIdentifier": identifier,
        "icmpSequence": sequence,
        "payload": frame[icmp + 8:inner["end"]].hex(),
    }


def _decode_icmp_frame(frame):
    if len(frame) < 14 or frame[12:14] != b"\x08\x00":
        return None
    inner = _decode_ipv4(frame, 14, len(frame))
    if inner is None or inner["protocol"] != 1:
        return None
    icmp = inner["offset"]
    if icmp + 8 > inner["end"] or frame[icmp + 1] != 0 or frame[icmp] not in (0, 8):
        return None
    identifier, sequence = struct.unpack_from("!HH", frame, icmp + 4)
    return {
        "source": inner["source"],
        "destination": inner["destination"],
        "icmpType": frame[icmp],
        "icmpIdentifier": identifier,
        "icmpSequence": sequence,
        "payload": frame[icmp + 8:inner["end"]].hex(),
    }


def _decode_ipv4(frame, offset, limit):
    if offset + 20 > limit or frame[offset] >> 4 != 4:
        return None
    header_length = (frame[offset] & 0x0f) * 4
    total_length = struct.unpack_from("!H", frame, offset + 2)[0]
    fragment = struct.unpack_from("!H", frame, offset + 6)[0]
    if (header_length < 20 or offset + total_length > limit or
            total_length < header_length or fragment & 0x1fff):
        return None
    return {
        "source": ".".join(str(part) for part in frame[offset + 12:offset + 16]),
        "destination": ".".join(str(part) for part in frame[offset + 16:offset + 20]),
        "protocol": frame[offset + 9],
        "offset": offset + header_length,
        "end": offset + total_length,
    }


def assert_geneve_probe_evidence(captures, probes, vteps, vni, receiver_captures):
    if set(captures) != set(vteps) or len(vteps) != 2 or vni is None:
        raise IsolationError("Geneve evidence has ambiguous worker/VTEP/datapath identity")
    if set(receiver_captures) != {probe["source"] for probe in probes}:
        raise IsolationError("net1 captures are missing or ambiguous")

    def select(packets, probe, kind, source, destination, source_key, destination_key):
        matching = [p for p in packets if p["icmpIdentifier"] == probe["identifier"]
                    and p["icmpType"] == kind]
        if len(matching) != 3 or {p["icmpSequence"] for p in matching} != {1, 2, 3}:
            raise IsolationError(f"missing or ambiguous ICMP packets for {probe['name']}")
        result = {}
        marker = probe["marker"]
        if not marker:
            raise IsolationError("empty ICMP payload marker")
        for packet in matching:
            if (packet[source_key], packet[destination_key]) != (source, destination):
                raise IsolationError(f"endpoint identity mismatch for {probe['name']}")
            try:
                payload = bytes.fromhex(packet["payload"])
            except ValueError as error:
                raise IsolationError("invalid ICMP payload") from error
            expected = (marker * ((56 + len(marker) - 1) // len(marker)))[:56]
            # iputils ping overwrites the first 16 payload bytes with struct timeval.
            if len(payload) != 56 or payload[16:] != expected[16:]:
                raise IsolationError(f"ICMP payload marker mismatch for {probe['name']}")
            result[packet["icmpSequence"]] = payload
        return result

    for probe in probes:
        reference = None
        for kind, source, destination, source_node, destination_node in (
                (8, probe["sourceIP"], probe["destinationIP"],
                 probe["sourceNode"], probe["destinationNode"]),
                (0, probe["destinationIP"], probe["sourceIP"],
                 probe["destinationNode"], probe["sourceNode"])):
            for packets in receiver_captures.values():
                observed = select(packets, probe, kind, source, destination,
                                  "source", "destination")
                if reference is None:
                    reference = observed
                elif observed != reference:
                    raise IsolationError(f"net1 payload mismatch for {probe['name']}")
            for node, packets in captures.items():
                matching = [p for p in packets if p["icmpIdentifier"] == probe["identifier"]
                            and p["icmpType"] == kind]
                if any(p["vni"] != vni or
                       (p["outerSource"], p["outerDestination"]) !=
                       (vteps[source_node], vteps[destination_node]) for p in matching):
                    raise IsolationError(f"Geneve datapath or VTEP mismatch for {probe['name']} on {node}")
                observed = select(packets, probe, kind, source, destination,
                                  "innerSource", "innerDestination")
                if observed != reference:
                    raise IsolationError(f"Geneve payload mismatch for {probe['name']} on {node}")


def foreign_resource_report(preexisting, *, comparison_complete=False):
    if preexisting:
        return {
            "status": "PRESERVED" if comparison_complete else "FOREIGN_WITNESS_CAPTURED",
            "resources": preexisting,
        }
    return {
        "status": "FOREIGN_WITNESS_NOT_AVAILABLE",
        "resources": [],
        "reason": "the isolated Kindnet/OVN test domain has no suitable pre-existing foreign OVS/OVN resource",
    }


def assert_foreign_resources_preserved(preexisting, after):
    for resource in preexisting:
        scope = resource["scope"]
        current = after.get(scope)
        if current is None:
            raise IsolationError(f"pre-existing foreign resource scope disappeared: {scope}")
        if "inventory" in resource:
            if current.get("inventory") != resource["inventory"]:
                raise IsolationError(f"pre-existing foreign inventory changed in {scope}")
        elif "links" in resource:
            for name, identity in resource["links"].items():
                if current.get("links", {}).get(name) != identity:
                    raise IsolationError(f"pre-existing foreign link changed in {scope}: {name}")


def validate_image_receipt(receipt, image, reference, owner):
    if (not receipt or receipt.get("reference") != reference or
            receipt.get("owner") != owner or not receipt.get("imageID")):
        raise IsolationError("missing or mismatched run-owned Docker image receipt")
    if image is None:
        return
    labels = image.get("Config", {}).get("Labels") or {}
    tags = image.get("RepoTags") or []
    if (image.get("Id") != receipt.get("imageID") or reference not in tags or
            labels.get("evpn.e2e.owner") != owner or
            labels.get("evpn.e2e.reference") != reference):
        raise IsolationError("Docker image identity or ownership receipt changed; preserving it")


def cleanup_image_reference(receipt, image, reference, owner, remove):
    validate_image_receipt(receipt, image, reference, owner)
    if image is None:
        return "already-absent"
    other_tags = set(image.get("RepoTags") or []) - {reference}
    remove(reference)
    return {"removedReference": reference, "preservedTags": sorted(other_tags)}


def assert_primary_preserved(before, after):
    if before["kindnet"] != after["kindnet"]:
        raise IsolationError("primary kindnet Pod identity changed")
    if before["cni_config_sha256"] != after["cni_config_sha256"]:
        raise IsolationError("primary CNI conflist changed")
    current = after["links"]
    missing = sorted(set(before["links"]) - set(current))
    changed = sorted(name for name, value in before["links"].items()
                     if name in current and current[name] != value)
    if missing or changed:
        raise IsolationError(f"primary worker link inventory changed: missing={missing}, changed={changed}")
    baseline_addresses = before["addresses"]
    current_addresses = after["addresses"]
    for name, addresses in baseline_addresses.items():
        if name not in current_addresses:
            raise IsolationError(f"primary worker address interface missing: {name}")
        current = {json.dumps(item, sort_keys=True) for item in current_addresses[name]}
        missing = [item for item in addresses if json.dumps(item, sort_keys=True) not in current]
        if missing:
            raise IsolationError(f"primary worker addresses changed on {name}: missing={missing}")
    current_routes = {json.dumps(route, sort_keys=True) for route in after["routes"]}
    missing_routes = [route for route in before["routes"]
                      if json.dumps(route, sort_keys=True) not in current_routes]
    if missing_routes:
        raise IsolationError(f"primary worker routes changed: {missing_routes}")


def assert_primary_restored(before, after):
    assert_primary_preserved(before, after)


def process_netns(pid):
    path = f"/proc/{pid}/ns/net"
    try:
        return os.stat(path).st_ino, "observed"
    except FileNotFoundError:
        return None, "terminated" if not Path(f"/proc/{pid}").exists() else "namespace-unavailable"
    except PermissionError:
        try:
            result = run(["sudo", "-n", "stat", "-Lc", "%i", path], check=False,
                         env=dict(os.environ, LC_ALL="C"))
        except (OSError, subprocess.TimeoutExpired):
            return None, "permission-denied"
        if result.returncode == 0 and result.stdout.strip().isdigit():
            return int(result.stdout.strip()), "observed"
        if not Path(f"/proc/{pid}").exists():
            return None, "terminated"
        if "No such file or directory" in result.stderr:
            return None, "namespace-unavailable"
        return None, "permission-denied"
    except OSError as error:
        if error.errno == errno.ENOENT and not Path(f"/proc/{pid}").exists():
            return None, "terminated"
        return None, "namespace-unavailable"


def host_dataplane_inventory():
    processes = []
    host_netns = os.stat("/proc/self/ns/net").st_ino
    for line in run(["ps", "-eo", "pid=,comm="]).stdout.splitlines():
        fields = line.split()
        if len(fields) >= 2 and fields[1] in HOST_DATAPLANE_PROCESSES:
            pid = fields[0]
            if not pid.isdecimal():
                raise IsolationError(f"invalid process ID in host inventory: {pid}")
            netns, status = process_netns(pid)
            if status == "terminated":
                continue
            processes.append({"pid": pid, "name": fields[1], "netns": netns,
                              "namespaceStatus": status,
                              "ownership": "host-or-unknown" if netns in (host_netns, None)
                              else "non-host-unverified"})
    sockets = [str(path) for path in HOST_DATAPLANE_SOCKETS if path.exists()]
    links = json.loads(run(["ip", "-j", "-d", "link", "show"]).stdout)
    ovs_links = {
        link["ifname"]: {
            "ifindex": link.get("ifindex"),
            "kind": (link.get("linkinfo") or {}).get("info_kind"),
            "ifalias": link.get("ifalias"),
        }
        for link in links
        if ((link.get("linkinfo") or {}).get("info_kind") == "openvswitch" or
            link["ifname"] in ("br-int", "ovs-system"))
    }
    return {
        "processes": processes,
        "hostProcesses": [process for process in processes
                          if process["netns"] in (host_netns, None)],
        "sockets": sockets,
        "ovsLinks": ovs_links,
    }


def assert_host_dataplane_isolated(inventory):
    if inventory["processes"] or inventory["sockets"] or inventory["ovsLinks"]:
        raise IsolationError(
            "isolated CNI lab refuses a host with existing OVS/OVN processes, sockets, "
            "or datapath links")


def run(args, *, check=True, timeout=300, env=None, capture=True):
    result = subprocess.run(
        list(map(str, args)), text=True, stdout=subprocess.PIPE if capture else None,
        stderr=subprocess.PIPE if capture else None, timeout=timeout, env=env,
        pass_fds=lab.lock_descriptors())
    if check and result.returncode:
        raise IsolationError(
            f"command failed ({result.returncode}): {' '.join(map(str, args))}\n"
            f"{result.stdout or ''}\n{result.stderr or ''}")
    return result


class IsolatedSecondaryCNI:
    def __init__(self):
        self.cluster = "cni-sec-" + secrets.token_hex(4)
        self.owner = "secondary-" + secrets.token_hex(6)
        self.directory = Path("/tmp") / f"{self.cluster}-evidence"
        self.kubeconfig = self.directory / "kubeconfig"
        self.context = f"kind-{self.cluster}"
        self.workers = [self.cluster + "-worker", self.cluster + "-worker2"]
        self.runtime_pods = {}
        self.primary_before = {}
        self.host_dataplane_before = None
        self.cni_attachment_ids = {}
        self.created_image = False
        self.image = f"evpn4ovn/isolated-cni-agent:{self.cluster}"
        self.image_id = None
        self.image_receipt = self.directory / "image-ownership.json"
        self.preexisting_foreign = []
        self.foreign_status = "FOREIGN_WITNESS_NOT_AVAILABLE"

    def k(self, *args, timeout=300, check=True):
        return run(["kubectl", "--kubeconfig", self.kubeconfig, "--context", self.context,
                    *args], check=check, timeout=timeout)

    def docker(self, *args, timeout=300, check=True):
        return run(["docker", *args], check=check, timeout=timeout)

    def inspect_run_image(self):
        result = self.docker("image", "inspect", self.image, check=False)
        if result.returncode == 0:
            return json.loads(result.stdout)[0]
        if "no such image" in result.stderr.lower() or "not found" in result.stderr.lower():
            return None
        raise IsolationError(f"could not establish Docker image tag ownership: {result.stderr}")

    def cleanup_run_image(self):
        if not self.image_receipt.is_file():
            raise IsolationError("run-owned Docker image receipt is missing; preserving image")
        receipt = json.loads(self.image_receipt.read_text())
        image = self.inspect_run_image()
        outcome = cleanup_image_reference(receipt, image, self.image, self.owner,
                                          lambda reference: self.docker("image", "rm", reference))
        if outcome != "already-absent":
            remaining = self.inspect_run_image()
            if remaining is not None:
                raise IsolationError("run-owned Docker image tag remains after cleanup")
            for tag in outcome["preservedTags"]:
                result = self.docker("image", "inspect", tag, check=False)
                if result.returncode != 0:
                    raise IsolationError(f"shared Docker tag was not preserved: {tag}")
                preserved = json.loads(result.stdout)[0]
                if preserved.get("Id") != receipt["imageID"]:
                    raise IsolationError(f"shared Docker tag identity changed: {tag}")

    def worker(self, node, *args, timeout=120, check=True):
        return self.docker("exec", node, *args, timeout=timeout, check=check)

    def reserve_lab(self):
        lab.init(self.directory, "cni-isolated", self.cluster, "evpn33b3-" + self.cluster)

    def initialize(self):
        if self.directory.exists():
            raise IsolationError(f"refusing pre-existing evidence directory: {self.directory}")
        if (lab.STATE_ROOT / "active.json").exists():
            raise IsolationError("a retained Containerlab lease must be explicitly recovered first")
        for binary in ("kind", "docker", "kubectl", "containerlab", "go", "curl", "ps", "ip"):
            if shutil.which(binary) is None:
                raise IsolationError(f"required executable is unavailable: {binary}")
        if not Path("/sys/module/openvswitch").is_dir():
            raise IsolationError("Open vSwitch kernel module must be loaded before the lab starts")
        if not (ROOT / "go.mod").is_file():
            raise IsolationError("repository root is unavailable")
        running_containers = self.docker("ps", "-q").stdout.split()
        if running_containers:
            raise IsolationError(
                f"isolated CNI lab requires a disposable Docker host without unrelated "
                f"running containers (found {len(running_containers)})")
        self.host_dataplane_before = host_dataplane_inventory()
        assert_host_dataplane_isolated(self.host_dataplane_before)
        self.directory.mkdir(mode=0o700, parents=True)
        self.reserve_lab()
        self.record("run.json", {
            "cluster": self.cluster, "owner": self.owner, "primaryCNI": "Kindnet",
            "topology": "one control-plane, two workers; one independent secondary OVN domain",
            "evpnType2": "not tested",
        })
        self.record("host-primary-before.json", self.host_dataplane_before)

    def verify_host_dataplane(self, output):
        current = host_dataplane_inventory()
        if (current["hostProcesses"] != self.host_dataplane_before["hostProcesses"] or
                current["sockets"] != self.host_dataplane_before["sockets"] or
                current["ovsLinks"] != self.host_dataplane_before["ovsLinks"]):
            raise IsolationError(f"host OVS/OVN inventory changed: {current}")
        self.record(output, current)

    def record(self, name, value):
        lab.write(self.directory / name, value)

    def build(self):
        build = self.directory / "build"
        build.mkdir(mode=0o700)
        run(["go", "build", "-o", build / "evpn-node-agent", "./cmd/evpn-node-agent"],
            timeout=900, env=dict(os.environ, CGO_ENABLED="0"))
        run(["go", "build", "-o", build / "evpn-cni", "./cmd/evpn-cni"],
            timeout=900, env=dict(os.environ, CGO_ENABLED="0"))
        extra_image_lines = self.build_extra(build)
        if self.docker("image", "inspect", OVN_IMAGE, check=False).returncode:
            raise IsolationError(f"pinned OVN/OVS image is unavailable: {OVN_IMAGE}")
        actual = json.loads(self.docker("image", "inspect", OVN_IMAGE).stdout)[0]["Id"]
        if actual != OVN_IMAGE_ID:
            raise IsolationError(f"OVN/OVS image identity mismatch: {actual}")
        if self.docker("image", "inspect", self.image, check=False).returncode == 0:
            raise IsolationError(f"refusing pre-existing agent image tag: {self.image}")
        dockerfile = build / "Dockerfile"
        dockerfile.write_text(
            f"FROM {OVN_IMAGE}\n"
            f"LABEL evpn.e2e.owner=\"{self.owner}\" "
            f"evpn.e2e.reference=\"{self.image}\"\n"
            "COPY evpn-node-agent /usr/local/bin/evpn-node-agent\n"
            "COPY evpn-cni /usr/local/bin/evpn-cni.real\n"
            + extra_image_lines
        )
        self.docker("build", "--quiet", "-t", self.image, "-f", dockerfile, build,
                    timeout=900)
        self.created_image = True
        image = json.loads(self.docker("image", "inspect", self.image).stdout)[0]
        self.image_id = image["Id"]
        labels = image.get("Config", {}).get("Labels") or {}
        if (labels.get("evpn.e2e.owner") != self.owner or
                labels.get("evpn.e2e.reference") != self.image or
                self.image not in (image.get("RepoTags") or [])):
            raise IsolationError("built agent image has no exact run-owned tag identity")
        self.record("image-ownership.json", {
            "owner": self.owner, "reference": self.image, "imageID": self.image_id,
        })

    def build_extra(self, build):
        return ""

    def agent_socket(self):
        return f"/run/evpn-secondary/{self.cluster}/agent/agent.sock"

    def configure_runtime_daemonset(self, daemonset):
        return daemonset

    def ovsdb_host_socket(self):
        return f"/run/evpn-secondary/{self.cluster}/openvswitch/db.sock"

    def result_status(self):
        return "ISOLATED_SECONDARY_CNI_NATIVE_OVN_PASS"

    def result_details(self):
        return {}

    def endpoints_before_delete(self, pods):
        pass

    def provision(self):
        lab.up(self.directory)
        self.verify_host_dataplane("host-primary-after-kind.json")
        self.k("wait", "--for=condition=Ready", "nodes", "--all", "--timeout=180s")
        nodes = json.loads(self.k("get", "nodes", "-o", "json").stdout)["items"]
        expected = {self.cluster + suffix for suffix in
                    ("-control-plane", "-worker", "-worker2")}
        if {item["metadata"]["name"] for item in nodes} != expected:
            raise IsolationError("unexpected Kubernetes node membership")
        run(["kind", "load", "docker-image", OVN_IMAGE, self.image, "--name", self.cluster],
            timeout=600)
        for node in self.workers:
            self.assert_primary_cni(node)
            snapshot = self.primary_snapshot(node)
            self.primary_before[node] = snapshot
        self.preexisting_foreign = self.capture_preexisting_foreign_resources()
        self.record("primary-before.json", self.primary_before)
        self.record("foreign-resource-inventory.json",
                    foreign_resource_report(self.preexisting_foreign))
        self.install_multus()
        self.deploy_secondary_ovn()

    def capture_preexisting_foreign_resources(self):
        resources = []
        if self.host_dataplane_before["processes"] or self.host_dataplane_before["sockets"] or \
                self.host_dataplane_before["ovsLinks"]:
            resources.append({"scope": "host", "inventory": self.host_dataplane_before})
        for node, snapshot in self.primary_before.items():
            links = {name: link for name, link in snapshot["links"].items()
                     if link["kind"] == "openvswitch" or name in ("br-int", "ovs-system")}
            if links:
                resources.append({"scope": node, "links": links})
        return resources

    def primary_snapshot(self, node, *, allow_secondary=False):
        br_int_check = (
            f"if [ -e /sys/class/net/br-int ] && "
            f"[ \"$(cat /sys/class/net/br-int/ifalias)\" != '{self.owner}' ]; then "
            "echo 'foreign br-int present' >&2; exit 74; fi\n"
            if allow_secondary else "test ! -e /sys/class/net/br-int\n")
        process_check = "" if allow_secondary else (
            "if ps -eo comm= | grep -Ex 'ovsdb-server|ovs-vswitchd|ovn-controller|ovn-northd'; then\n"
            "  echo 'pre-existing host OVS/OVN process' >&2\n"
            "  exit 73\n"
            "fi\n")
        script = r"""
set -eu
test -s /etc/cni/net.d/10-kindnet.conflist
test ! -S /run/openvswitch/db.sock
test ! -S /run/ovn/ovnnb_db.sock
test ! -S /run/ovn/ovnsb_db.sock
PROCESS_CHECK
ip -j -d link show
""".replace("PROCESS_CHECK\n", process_check)
        script = script.replace("test -s /etc/cni/net.d/10-kindnet.conflist\n",
                                "test -s /etc/cni/net.d/10-kindnet.conflist\n" + br_int_check)
        if not allow_secondary:
            script = script.replace("test ! -S /run/openvswitch/db.sock\n",
                                    f"test ! -e /run/evpn-secondary/{self.cluster}\n"
                                    "test ! -S /run/openvswitch/db.sock\n")
        result = self.worker(node, "sh", "-ec", script)
        links = primary_link_inventory(result.stdout)
        if not allow_secondary and any(link["kind"] == "openvswitch" for link in links.values()):
            raise IsolationError(f"OVS datapath interface already exists on {node}")
        addresses = json.loads(self.worker(node, "ip", "-j", "address", "show").stdout)
        addresses = {
            entry["ifname"]: [
                {key: item.get(key) for key in ("family", "local", "prefixlen", "scope", "label")}
                for item in sorted(entry.get("addr_info", []),
                                   key=lambda item: json.dumps(item, sort_keys=True))
            ]
            for entry in addresses
        }
        routes = json.loads(self.worker(node, "ip", "-j", "route", "show", "table", "main").stdout)
        routes.sort(key=lambda route: json.dumps(route, sort_keys=True))
        cni_hash = self.worker(node, "sha256sum", "/etc/cni/net.d/10-kindnet.conflist").stdout.split()[0]
        pods = json.loads(self.k("get", "pods", "-n", "kube-system", "-o", "json").stdout)["items"]
        candidates = [pod for pod in pods if pod["spec"].get("nodeName") == node and
                      any("kindnet" in (c["name"] + c["image"]).lower()
                          for c in pod["spec"]["containers"])]
        if len(candidates) != 1:
            raise IsolationError(f"cannot establish exact primary Kindnet Pod on {node}")
        pod = candidates[0]
        if pod.get("status", {}).get("phase") != "Running" or not any(
                condition.get("type") == "Ready" and condition.get("status") == "True"
                for condition in pod.get("status", {}).get("conditions", [])):
            raise IsolationError(f"primary Kindnet Pod is not Ready on {node}")
        return {
            "node": node, "kindnet": {
                "uid": pod["metadata"]["uid"],
                "containerIDs": sorted(s.get("containerID", "")
                                       for s in pod.get("status", {}).get("containerStatuses", [])),
            },
            "cni_config_sha256": cni_hash, "links": links,
            "addresses": addresses, "routes": routes,
        }

    def install_multus(self):
        archive = self.directory / "multus.tar.gz"
        run(["curl", "-fsSL", "--retry", "2", MULTUS_URL, "-o", archive], timeout=300)
        digest = hashlib.sha256(archive.read_bytes()).hexdigest()
        if digest != MULTUS_SHA256:
            raise IsolationError(f"Multus checksum mismatch: {digest}")
        run(["tar", "-xzf", archive, "-C", self.directory,
             "multus-cni_4.3.1_linux_amd64/multus"])
        self.k("apply", "-f", ROOT / "deploy/kind/cni-multus-test/rbac.yaml")
        self.k("wait", "--for=condition=Established",
               "crd/network-attachment-definitions.k8s.cni.cncf.io", "--timeout=60s")
        cni = self.directory / "build/evpn-cni"
        cni_size = cni.stat().st_size
        cni_sha = hashlib.sha256(cni.read_bytes()).hexdigest()
        wrapper = self.directory / "cni-wrapper"
        wrapper.write_text(
            "#!/bin/sh\n"
            "set +e\n"
            "dir=$(mktemp -d /run/evpn-cni-evidence/call.XXXXXXXX) || exit 1\n"
            "env | grep '^CNI_' > \"$dir/env\"\n"
            "cat > \"$dir/config\"\n"
            "/opt/cni/bin/evpn-cni.real < \"$dir/config\" > \"$dir/stdout\" 2> \"$dir/stderr\"\n"
            "rc=$?\n"
            "echo \"$rc\" > \"$dir/rc\"\n"
            "cat \"$dir/stdout\"\n"
            "cat \"$dir/stderr\" >&2\n"
            "exit \"$rc\"\n")
        multus = self.directory / "multus-cni_4.3.1_linux_amd64/multus"
        for node in self.workers:
            self.worker(node, "sh", "-ec",
                        "test ! -e /opt/cni/bin/multus; "
                        "test ! -e /opt/cni/bin/evpn-cni; "
                        "test ! -e /opt/cni/bin/evpn-cni.real; "
                        "test ! -e /etc/cni/net.d/00-multus.conf; "
                        "mkdir -p /etc/cni/net.d/multus.d /run/evpn-cni-evidence")
            for source, destination in (
                    (multus, "/opt/cni/bin/multus"),
                    (cni, "/opt/cni/bin/evpn-cni.real"),
                    (wrapper, "/opt/cni/bin/evpn-cni")):
                self.stage_worker_executable(node, source, destination)
            self.verify_worker_executable(node, "/opt/cni/bin/evpn-cni.real",
                                          cni_size, cni_sha)
            self.worker(node, "/opt/cni/bin/multus", "--version")
        token = self.k("-n", "kube-system", "create", "token", "multus-test",
                       "--duration=2h").stdout.strip()
        ca = self.k("config", "view", "--raw", "--minify",
                    "-o", "jsonpath={.clusters[0].cluster.certificate-authority-data}").stdout.strip()
        for node in self.workers:
            apiserver = self.worker(node, "awk", "/server:/{print $2; exit}",
                                    "/etc/kubernetes/kubelet.conf").stdout.strip()
            kubeconfig = (
                "apiVersion: v1\nkind: Config\nclusters:\n- name: test\n  cluster:\n"
                f"    server: {apiserver}\n    certificate-authority-data: {ca}\n"
                "users:\n- name: multus\n  user:\n"
                f"    token: {token}\ncontexts:\n- name: test\n"
                "  context: {cluster: test, user: multus}\ncurrent-context: test\n")
            result = subprocess.run(
                ["docker", "exec", "-i", node, "sh", "-c",
                 "umask 077; cat > /etc/cni/net.d/multus.d/kubeconfig"],
                input=kubeconfig, text=True, stdout=subprocess.PIPE, stderr=subprocess.PIPE,
                timeout=60, pass_fds=lab.lock_descriptors())
            if result.returncode:
                raise IsolationError(f"failed writing Multus kubeconfig on {node}: {result.stderr}")
            self.worker(node, "cp", "/etc/cni/net.d/10-kindnet.conflist",
                        "/run/kindnet-before.json")
            primary = json.loads(self.worker(node, "cat",
                                             "/etc/cni/net.d/10-kindnet.conflist").stdout)
            config = {
                "cniVersion": "1.0.0", "name": "multus-test", "type": "multus",
                "kubeconfig": "/etc/cni/net.d/multus.d/kubeconfig",
                "logLevel": "debug", "logFile": "/var/log/multus.log",
                "logToStderr": True, "retryDeleteOnError": True, "delegates": [primary],
            }
            result = subprocess.run(
                ["docker", "exec", "-i", node, "sh", "-c",
                 "cat > /etc/cni/net.d/00-multus.conf"],
                input=json.dumps(config) + "\n", text=True, stdout=subprocess.PIPE,
                stderr=subprocess.PIPE, timeout=60, pass_fds=lab.lock_descriptors())
            if result.returncode:
                raise IsolationError(f"failed writing Multus config on {node}: {result.stderr}")
            self.worker(node, "cmp", "/run/kindnet-before.json",
                        "/etc/cni/net.d/10-kindnet.conflist")
        self.record("multus.json", {
            "release": "v4.3.1", "sha256": digest, "workers": self.workers,
            "primaryConflistPreserved": True,
        })

    def stage_worker_executable(self, node, source, destination):
        if destination not in ("/opt/cni/bin/multus", "/opt/cni/bin/evpn-cni.real",
                               "/opt/cni/bin/evpn-cni"):
            raise IsolationError(f"refusing unexpected worker executable destination: {destination}")
        size = source.stat().st_size
        digest = hashlib.sha256(source.read_bytes()).hexdigest()
        if destination in ("/opt/cni/bin/evpn-cni.real", "/opt/cni/bin/evpn-cni"):
            kind = "real" if destination.endswith(".real") else "wrapper"
            helper = ROOT / "tools/lib/cni-agent-image-test.sh"
            lab.command([
                "bash", "-ec",
                'source "$1"; stage_worker_cni_file "$2" "$3" "$4" "$5" "$6" "$7"',
                "stage-worker-cni-file", str(helper), node, str(source), destination,
                str(size), digest, kind,
            ], timeout=300)
        else:
            self.docker("cp", source, f"{node}:{destination}")
            self.worker(node, "chown", "root:root", destination)
            self.worker(node, "chmod", "755", destination)
        self.verify_worker_executable(
            node, destination, size, digest)

    def verify_worker_executable(self, node, destination, size, digest):
        if destination not in ("/opt/cni/bin/multus", "/opt/cni/bin/evpn-cni.real",
                               "/opt/cni/bin/evpn-cni"):
            raise IsolationError(f"refusing unexpected worker executable destination: {destination}")
        self.worker(node, "sh", "-ec",
                    f"test -f {destination} && test ! -L {destination} && "
                    f"test \"$(stat -c '%u:%g:%a:%s' {destination})\" = '0:0:755:{size}' && "
                    f"echo '{digest}  {destination}' | sha256sum -c -")

    def deploy_secondary_ovn(self):
        self.k("create", "namespace", "cni-test")
        for node in self.workers:
            self.k("label", "node", node, f"evpn.e2e/owner={self.owner}")
        central_script = (ROOT / "deploy/containerlab/secondary-ovn-central.sh").read_text()
        worker_script = (ROOT / "deploy/containerlab/secondary-ovn-worker.sh").read_text()
        worker_preflight_script = (
            ROOT / "deploy/containerlab/secondary-ovn-worker-preflight.sh").read_text()
        config_map = {
            "apiVersion": "v1", "kind": "ConfigMap",
            "metadata": {"name": "secondary-ovn-scripts", "namespace": "cni-test",
                         "labels": {"evpn.e2e/owner": self.owner}},
            "data": {
                "central.sh": central_script,
                "worker.sh": worker_script,
                "worker-preflight.sh": worker_preflight_script,
            },
        }
        self.apply("secondary-scripts.json", config_map)
        central = {
            "apiVersion": "apps/v1", "kind": "Deployment",
            "metadata": {"name": "secondary-ovn-central", "namespace": "cni-test",
                         "labels": {"evpn.e2e/owner": self.owner}},
            "spec": {"replicas": 1, "strategy": {"type": "Recreate"},
                     "selector": {"matchLabels": {"app": "secondary-ovn-central"}},
                     "template": {"metadata": {"labels": {"app": "secondary-ovn-central",
                                                           "evpn.e2e/owner": self.owner}},
                                  "spec": {
                                      "nodeSelector": {
                                          "kubernetes.io/hostname": self.cluster + "-control-plane"},
                                      "tolerations": [{
                                          "key": "node-role.kubernetes.io/control-plane",
                                          "operator": "Exists",
                                          "effect": "NoSchedule",
                                      }],
                                      "containers": [{
                                          "name": "central", "image": OVN_IMAGE,
                                          "imagePullPolicy": "Never",
                                          "command": ["/bin/sh", "/lab/central.sh"],
                                          "securityContext": {"privileged": True},
                                          "env": [
                                              {"name": "SECONDARY_OWNER", "value": self.owner},
                                          ],
                                          "ports": [{"name": "northbound", "containerPort": 6641},
                                                    {"name": "southbound", "containerPort": 6642}],
                                          "volumeMounts": [
                                              {"name": "scripts", "mountPath": "/lab", "readOnly": True},
                                              {"name": "ovn-run", "mountPath": "/var/run/ovn"},
                                              {"name": "ovn-data", "mountPath": "/var/lib/ovn"}],
                                          "readinessProbe": {"exec": {"command": [
                                              "/bin/sh", "-ec",
                                              "ovn-nbctl show >/dev/null && ovn-sbctl show >/dev/null"]},
                                              "initialDelaySeconds": 2, "periodSeconds": 2},
                                      }],
                                      "volumes": [
                                          {"name": "scripts", "configMap": {"name": "secondary-ovn-scripts"}},
                                          {"name": "ovn-run", "emptyDir": {}},
                                          {"name": "ovn-data", "emptyDir": {}},
                                      ],
                                  }}},
        }
        service = {
            "apiVersion": "v1", "kind": "Service",
            "metadata": {"name": "secondary-ovn", "namespace": "cni-test",
                         "labels": {"evpn.e2e/owner": self.owner}},
            "spec": {"selector": {"app": "secondary-ovn-central"},
                     "ports": [{"name": "northbound", "port": 6641, "targetPort": 6641},
                               {"name": "southbound", "port": 6642, "targetPort": 6642}]},
        }
        self.apply("secondary-central.json", central)
        self.apply("secondary-service.json", service)
        self.k("-n", "cni-test", "rollout", "status", "deployment/secondary-ovn-central",
               "--timeout=180s")
        service_info = json.loads(self.k("-n", "cni-test", "get", "service",
                                         "secondary-ovn", "-o", "json").stdout)
        pods = json.loads(self.k("-n", "cni-test", "get", "pods", "-l",
                                 "app=secondary-ovn-central", "-o", "json").stdout)["items"]
        if (len(pods) != 1 or pods[0]["spec"].get("nodeName") !=
                self.cluster + "-control-plane" or
                pods[0]["metadata"].get("labels", {}).get("evpn.e2e/owner") != self.owner):
            raise IsolationError("secondary OVN central Pod ownership or placement mismatch")
        for attempt in range(60):
            endpoints = json.loads(self.k("-n", "cni-test", "get", "endpoints",
                                          "secondary-ovn", "-o", "json").stdout)
            try:
                remote = secondary_sbdb_remote(service_info, endpoints, pods[0], self.owner)
                break
            except SecondaryEndpointNotReady:
                if attempt == 59:
                    raise
                time.sleep(1)
        self.record("secondary-sbdb-service.json", {
            "service": service_info, "endpoints": endpoints,
            "centralPodUID": pods[0]["metadata"]["uid"], "ovnRemote": remote})
        self.k("-n", "cni-test", "exec", "deploy/secondary-ovn-central", "--",
               "ovn-nbctl", "ls-add", "ls-secondary")
        self.deploy_runtime_daemonset(remote)
        self.capture_runtime_identities()

    def apply(self, name, value):
        path = self.directory / name
        path.write_text(json.dumps(value, indent=2) + "\n")
        self.k("apply", "-f", path)

    def deploy_runtime_daemonset(self, sbdb_remote):
        propagation = {}
        for node in self.workers:
            result = self.worker(
                node, "sh", "-ec",
                "mkdir -p /run/netns; findmnt -n -o PROPAGATION -T /run/netns")
            value = result.stdout.strip()
            if "shared" not in value.split(","):
                raise IsolationError(f"{node}: /run/netns source mount is not shared: {value!r}")
            propagation[node] = value
        self.record("netns-source-propagation.json", propagation)
        socket_root = f"/run/evpn-secondary/{self.cluster}"
        volumes = [
            ("ovs-run", f"{socket_root}/openvswitch", "/var/run/openvswitch"),
            ("ovs-db", f"{socket_root}/etc-openvswitch", "/etc/openvswitch"),
            ("ovn-run", f"{socket_root}/ovn", "/var/run/ovn"),
            ("identity", f"{socket_root}/identity", "/run/evpn-secondary/identity"),
            ("agent-run", f"{socket_root}/agent", f"{socket_root}/agent"),
            ("netns", "/run/netns", "/run/netns"),
            ("scripts", None, "/lab"),
        ]
        volume_defs = [
            {"name": name, "hostPath": {"path": host, "type": "DirectoryOrCreate"}}
            if host else {"name": name, "configMap": {"name": "secondary-ovn-scripts"}}
            for name, host, _ in volumes]
        runtime_mounts = [{"name": name, "mountPath": path, "readOnly": name == "scripts"}
                          for name, _, path in volumes]
        agent_mounts = [{"name": name, "mountPath": path, "readOnly": name == "scripts"}
                        for name, _, path in volumes]
        next(mount for mount in agent_mounts if mount["name"] == "netns")[
            "mountPropagation"] = "HostToContainer"
        agent_root = f"{socket_root}/agent/agent.sock"
        ds = {
            "apiVersion": "apps/v1", "kind": "DaemonSet",
            "metadata": {"name": "secondary-ovn-worker", "namespace": "cni-test",
                         "labels": {"evpn.e2e/owner": self.owner}},
            "spec": {"selector": {"matchLabels": {"app": "secondary-ovn-worker"}},
                     "template": {
                         "metadata": {"labels": {"app": "secondary-ovn-worker",
                                                 "evpn.e2e/owner": self.owner}},
                         "spec": {
                             "hostNetwork": True, "dnsPolicy": "ClusterFirstWithHostNet",
                             "hostPID": True,
                             "nodeSelector": {"evpn.e2e/owner": self.owner},
                             "automountServiceAccountToken": False,
                             "containers": [
                                 {"name": "ovn", "image": OVN_IMAGE, "imagePullPolicy": "Never",
                                  "command": ["/bin/sh", "/lab/worker.sh"],
                                  "securityContext": {"privileged": True},
                                  "env": [
                                      {"name": "SECONDARY_OWNER", "value": self.owner},
                                      {"name": "OVN_REMOTE",
                                       "value": sbdb_remote},
                                      {"name": "OVN_ENCAP_IP",
                                       "valueFrom": {"fieldRef": {"fieldPath": "status.hostIP"}}},
                                  ],
                                  "volumeMounts": runtime_mounts,
                                  "readinessProbe": {"exec": {"command": [
                                      "/bin/sh", "-ec",
                                      "ovs-vsctl br-exists br-int && "
                                      "test -s /var/run/ovn/ovn-controller.pid && "
                                      "ovn-appctl -t ovn-controller connection-status | "
                                      "grep -qx connected"]},
                                      "initialDelaySeconds": 2, "periodSeconds": 3,
                                      "failureThreshold": 20}},
                                 {"name": "node-agent", "image": self.image,
                                  "imagePullPolicy": "Never",
                                  "command": ["/bin/sh", "-ec",
                                              "for i in $(seq 1 120); do "
                                              "test -S /var/run/openvswitch/db.sock && "
                                              "nc -z secondary-ovn.cni-test.svc 6641 && break; "
                                              "sleep 1; done; "
                                              "test -S /var/run/openvswitch/db.sock && "
                                              "nc -z secondary-ovn.cni-test.svc 6641; "
                                              f"exec /usr/local/bin/evpn-node-agent "
                                              f"--socket={agent_root} --mode=real-ovn-ovs "
                                              "--network-backend=linux-veth "
                                              "--ovn-nb-endpoint=tcp:secondary-ovn.cni-test.svc:6641 "
                                              "--ovsdb-endpoint=unix:/var/run/openvswitch/db.sock"],
                                  "securityContext": {"privileged": True, "runAsUser": 0},
                                  "volumeMounts": agent_mounts,
                                  "readinessProbe": {"exec": {"command": [
                                      "/bin/sh", "-ec", f"test -S {agent_root}"]},
                                      "initialDelaySeconds": 2, "periodSeconds": 2,
                                      "failureThreshold": 30}},
                             ],
                             "volumes": volume_defs,
                         },
                     }},
        }
        ds = self.configure_runtime_daemonset(ds)
        self.apply("secondary-runtime.json", ds)
        self.k("-n", "cni-test", "rollout", "status", "daemonset/secondary-ovn-worker",
               "--timeout=240s")
        pods = json.loads(self.k("-n", "cni-test", "get", "pods", "-l",
                                 "app=secondary-ovn-worker", "-o", "json").stdout)["items"]
        if len(pods) != 2 or {p["spec"]["nodeName"] for p in pods} != set(self.workers):
            raise IsolationError("secondary OVS runtime is not exactly one pod per worker")
        self.runtime_pods = {p["spec"]["nodeName"]: p["metadata"]["name"] for p in pods}
        self.record("runtime-pod-identities.json", {
            node: {"pod": pod, "uid": next(p["metadata"]["uid"] for p in pods
                                           if p["metadata"]["name"] == pod),
                   "containers": next(p["status"]["containerStatuses"] for p in pods
                                      if p["metadata"]["name"] == pod)}
            for node, pod in self.runtime_pods.items()})

    def runtime(self, node, container, *args, timeout=120, check=True):
        pod = self.runtime_pods[node]
        return self.k("-n", "cni-test", "exec", pod, "-c", container, "--", *args,
                      timeout=timeout, check=check)

    def capture_runtime_identities(self):
        identities = {}
        for node in self.workers:
            system_id = self.runtime(node, "ovn", "ovs-vsctl", "get", "Open_vSwitch", ".",
                                     "external_ids:system-id").stdout.strip().strip('"')
            ovs_uuid = self.runtime(node, "ovn", "ovs-vsctl", "--bare", "--columns=_uuid",
                                    "list", "Open_vSwitch").stdout.strip()
            bridge_alias = self.runtime(node, "ovn", "cat",
                                        "/sys/class/net/br-int/ifalias").stdout.strip()
            controller_pid = self.runtime(node, "ovn", "cat",
                                          "/var/run/ovn/ovn-controller.pid").stdout.strip()
            controller_cmd = self.runtime(
                node, "ovn", "sh", "-ec",
                f"tr '\\000' ' ' < /proc/{int(controller_pid)}/cmdline").stdout.strip()
            if not system_id or not ovs_uuid or bridge_alias != self.owner:
                raise IsolationError(f"worker OVS ownership identity is incomplete on {node}")
            if "ovn-controller" not in controller_cmd:
                raise IsolationError(f"controller PID identity mismatch on {node}")
            identities[node] = {
                "systemId": system_id, "ovsDatabaseUUID": ovs_uuid,
                "bridge": "br-int", "bridgeAlias": bridge_alias,
                "ovsdbSocket": self.ovsdb_host_socket(),
                "ovsdbSocketInode": self.runtime(node, "ovn", "stat", "-Lc", "%i",
                                                "/var/run/openvswitch/db.sock").stdout.strip(),
                "controllerPID": controller_pid, "controllerCommand": controller_cmd,
                "pod": self.runtime_pods[node],
            }
        if len({item["systemId"] for item in identities.values()}) != 2:
            raise IsolationError("secondary OVS chassis IDs are not unique")
        if len({item["ovsDatabaseUUID"] for item in identities.values()}) != 2:
            raise IsolationError("secondary OVSDB identities are not independent")
        expected = {item["systemId"] for item in identities.values()}
        chassis = []
        for _ in range(60):
            chassis = [name.strip('"') for name in self.k(
                "-n", "cni-test", "exec", "deploy/secondary-ovn-central", "--",
                "ovn-sbctl", "--bare", "--columns=name", "list", "Chassis").stdout.split()]
            if len(chassis) == len(expected) and set(chassis) == expected:
                break
            time.sleep(1)
        else:
            raise IsolationError(f"secondary SBDB chassis inventory mismatch: {chassis}")
        nb_uuid = self.k("-n", "cni-test", "exec", "deploy/secondary-ovn-central", "--",
                         "ovn-nbctl", "--bare", "--columns=_uuid", "list", "NB_Global").stdout.strip()
        sb_uuid = self.k("-n", "cni-test", "exec", "deploy/secondary-ovn-central", "--",
                         "ovn-sbctl", "--bare", "--columns=_uuid", "list", "SB_Global").stdout.strip()
        central_pod = json.loads(self.k("-n", "cni-test", "get", "pod", "-l",
                                        "app=secondary-ovn-central", "-o", "json").stdout)["items"][0]
        self.record("secondary-runtime-ownership.json", {
            "primaryCNI": "Kindnet", "primaryOVNOrOVS": "none (preflight confirmed)",
            "centralPodUID": central_pod["metadata"]["uid"],
            "centralContainerIDs": central_pod["status"]["containerStatuses"],
            "northboundGlobalUUID": nb_uuid, "southboundGlobalUUID": sb_uuid,
            "workers": identities,
        })

    def cni_calls(self, node):
        result = self.worker(node, "sh", "-ec", "find /run/evpn-cni-evidence "
                             "-maxdepth 2 -type f -name rc -print")
        return result.stdout.splitlines()

    def capture_workload_failure(self):
        evidence = {}

        def capture(name, args):
            try:
                result = run(args, check=False, timeout=20)
                evidence[name] = {"returncode": result.returncode, "stdout": result.stdout,
                                  "stderr": result.stderr}
            except (OSError, subprocess.TimeoutExpired) as error:
                evidence[name] = {"error": str(error)}

        kubectl = ["kubectl", "--kubeconfig", self.kubeconfig, "--context", self.context,
                   "-n", "cni-test"]
        for suffix in ("a", "b"):
            pod = "pod-" + suffix
            capture(pod, kubectl + ["get", "pod", pod, "-o", "yaml"])
            capture(pod + "-events", kubectl + [
                "get", "events", "--field-selector", "involvedObject.name=" + pod, "-o", "yaml"])
            capture("nad-" + suffix, kubectl + [
                "get", "network-attachment-definition", "secondary-" + suffix, "-o", "yaml"])
        for node in self.workers:
            prefix = ["docker", "exec", node]
            capture(node + "-cni-files", prefix + [
                "sh", "-c",
                "for p in /opt/cni/bin/multus /opt/cni/bin/evpn-cni "
                "/opt/cni/bin/evpn-cni.real /etc/cni/net.d/00-multus.conf "
                "/etc/cni/net.d/10-kindnet.conflist; do "
                "stat -c '%n %a %U:%G %s' \"$p\" || true; "
                "case \"$p\" in *.conf|*.conflist) cat \"$p\";; esac; done"])
            capture(node + "-cni-invocations", prefix + [
                "sh", "-c",
                "if [ -d /run/evpn-cni-evidence ]; then "
                "for d in /run/evpn-cni-evidence/call.*; do [ -d \"$d\" ] || continue; "
                "echo \"=== $d ===\"; for f in env config stdout stderr rc; do "
                "if [ -f \"$d/$f\" ]; then echo \"--- $f ---\"; "
                "head -c 16384 \"$d/$f\"; printf '\\n'; fi; done; done; "
                "else echo 'CNI invocation directory absent'; fi"])
            for unit in ("kubelet", "containerd"):
                capture(node + "-" + unit, prefix + [
                    "journalctl", "-u", unit, "-n", "1000", "--no-pager", "-o", "short-iso"])
            pod = self.runtime_pods.get(node)
            if pod:
                for container in ("ovn", "node-agent"):
                    capture(node + "-" + container + "-logs", kubectl + [
                        "logs", pod, "-c", container, "--tail=500"])
                capture(node + "-ovn-files", kubectl + [
                    "exec", pod, "-c", "ovn", "--", "sh", "-c",
                    "for f in /var/log/ovn/ovn-controller.log "
                    "/var/log/openvswitch/ovsdb-server.log /var/log/openvswitch/ovs-vswitchd.log; "
                    "do echo \"=== $f ===\"; tail -100 \"$f\" 2>&1; done; "
                    "echo '=== agent socket ==='; "
                    f"stat /run/evpn-secondary/{self.cluster}/agent/agent.sock 2>&1"])
                for label, command in (
                        ("ovs-interfaces", ["ovs-vsctl", "--format=json", "list", "Interface"]),
                        ("ovs-bridges", ["ovs-vsctl", "--format=json", "list", "Bridge"]),
                        ("ovs-flows", ["ovs-ofctl", "dump-flows", "br-int"])):
                    capture(node + "-" + label, kubectl + [
                        "exec", pod, "-c", "ovn", "--", *command])
        capture("central-logs", kubectl + [
            "logs", "deployment/secondary-ovn-central", "-c", "central", "--tail=500"])
        for label, command in (
                ("nb-switches", ["ovn-nbctl", "--format=json", "list", "Logical_Switch"]),
                ("nb-ports", ["ovn-nbctl", "--format=json", "list", "Logical_Switch_Port"]),
                ("sb-datapaths", ["ovn-sbctl", "--format=json", "list", "Datapath_Binding"]),
                ("sb-bindings", ["ovn-sbctl", "--format=json", "list", "Port_Binding"]),
                ("sb-chassis", ["ovn-sbctl", "--format=json", "list", "Chassis"])):
            capture(label, kubectl + ["exec", "deploy/secondary-ovn-central", "--", *command])
        self.record("workload-failure-evidence.json", evidence)

    def test_endpoints(self):
        for node, suffix in zip(self.workers, ("a", "b")):
            ip = secondary_ip(suffix)
            mac = "0a:58:f0:00:00:0a" if suffix == "a" else "0a:58:f0:00:00:0b"
            nad = {
                "apiVersion": "k8s.cni.cncf.io/v1", "kind": "NetworkAttachmentDefinition",
                "metadata": {"name": "secondary-" + suffix, "namespace": "cni-test",
                             "labels": {"evpn.e2e/owner": self.owner}},
                "spec": {"config": json.dumps({
                    "cniVersion": "1.0.0", "name": "secondary-" + suffix,
                    "type": "evpn-cni",
                    "nodeAgentSocket": self.agent_socket(),
                    "networkID": "secondary-isolation", "attachmentID": "s",
                    "logicalSwitch": "ls-secondary", "ipAddress": ip + "/24",
                    "macAddress": mac,
                }, separators=(",", ":"))},
            }
            self.apply("nad-" + suffix + ".json", nad)
            pod = {
                "apiVersion": "v1", "kind": "Pod",
                "metadata": {"name": "pod-" + suffix, "namespace": "cni-test",
                             "labels": {"evpn.e2e/owner": self.owner},
                             "annotations": {"k8s.v1.cni.cncf.io/networks": "secondary-" + suffix}},
                "spec": {"nodeName": node, "containers": [{
                    "name": "client", "image": self.image, "imagePullPolicy": "Never",
                    "securityContext": {"capabilities": {"add": ["NET_RAW"]}},
                    "command": ["/bin/sh", "-c", "sleep infinity"]}]},
            }
            self.apply("pod-" + suffix + ".json", pod)
        self.k("-n", "cni-test", "wait", "--for=condition=Ready", "pod/pod-a", "pod/pod-b",
               "--timeout=180s")
        pods = {suffix: json.loads(self.k("-n", "cni-test", "get", "pod", "pod-" + suffix,
                                           "-o", "json").stdout)
                for suffix in ("a", "b")}
        for suffix, pod in pods.items():
            status = json.loads(pod["metadata"]["annotations"][
                "k8s.v1.cni.cncf.io/network-status"])
            secondary = [item for item in status if item.get("interface") == "net1"]
            if len(secondary) != 1:
                raise IsolationError(f"missing unique net1 attachment for pod-{suffix}")
            expected_ip = secondary_ip(suffix)
            if secondary[0].get("ips") != [expected_ip]:
                raise IsolationError(f"unexpected net1 address for pod-{suffix}")
        self.record("pod-identities.json", {
            suffix: {"uid": pod["metadata"]["uid"], "podIP": pod["status"]["podIP"],
                     "interface": "net1"}
            for suffix, pod in pods.items()})
        self.verify_primary("primary-after-add.json")
        self.verify_bindings(pods)
        self.check_cni(pods)
        self.verify_connectivity(pods)
        self.verify_foreign_resources()
        self.endpoints_before_delete(pods)
        self.delete_pods(pods)
        self.verify_cleanup(pods)
        self.verify_primary("primary-after-del.json")
        self.verify_primary_connectivity_after_del()

    def verify_primary_connectivity_after_del(self):
        names = ["primary-control-a", "primary-control-b"]
        for node, name in zip(self.workers, names):
            self.apply(name + ".json", {
                "apiVersion": "v1", "kind": "Pod",
                "metadata": {"name": name, "namespace": "cni-test",
                             "labels": {"evpn.e2e/owner": self.owner}},
                "spec": {"nodeName": node, "containers": [{
                    "name": "client", "image": self.image, "imagePullPolicy": "Never",
                    "securityContext": {"capabilities": {"add": ["NET_RAW"]}},
                    "command": ["/bin/sh", "-c", "sleep infinity"]}]},
            })
        self.k("-n", "cni-test", "wait", "--for=condition=Ready",
               *["pod/" + name for name in names], "--timeout=180s")
        addresses = [
            json.loads(self.k("-n", "cni-test", "get", "pod", name, "-o", "json").stdout)
            ["status"]["podIP"] for name in names
        ]
        output = {}
        for index, name in enumerate(names):
            result = self.k("-n", "cni-test", "exec", name, "--",
                            "ping", "-I", "eth0", "-c", "2", "-W", "2",
                            addresses[1 - index], timeout=30)
            if "0% packet loss" not in result.stdout:
                raise IsolationError(f"primary eth0 control ping after DEL failed for {name}")
            output[name] = result.stdout
        self.record("primary-connectivity-after-del.json", output)
        self.k("-n", "cni-test", "delete", "pod", *names, "--wait=true", "--timeout=120s")

    def verify_bindings(self, pods):
        endpoints = {}
        for suffix in ("a", "b"):
            uid = pods[suffix]["metadata"]["uid"]
            lsp = expected_lsp_name(uid)
            actual_lsp = self.k(
                "-n", "cni-test", "exec", "deploy/secondary-ovn-central", "--",
                "ovn-nbctl", "--bare", "--columns=name", "find", "Logical_Switch_Port",
                f"name={lsp}").stdout.strip()
            if actual_lsp != lsp:
                raise IsolationError(f"OVN logical switch port missing or ambiguous: {lsp}")
            workload_uid = self.k(
                "-n", "cni-test", "exec", "deploy/secondary-ovn-central", "--",
                "ovn-nbctl", "get", "Logical_Switch_Port", lsp,
                "external_ids:evpn.framework/workload-uid").stdout.strip().strip('"')
            if workload_uid != uid:
                raise IsolationError(f"OVN LSP ownership metadata mismatch for {uid}")
            lsp_uuid = self.k(
                "-n", "cni-test", "exec", "deploy/secondary-ovn-central", "--",
                "ovn-nbctl", "--bare", "--columns=_uuid", "find", "Logical_Switch_Port",
                f"name={lsp}").stdout.strip()
            membership = self.k(
                "-n", "cni-test", "exec", "deploy/secondary-ovn-central", "--",
                "ovn-nbctl", "get", "Logical_Switch", "ls-secondary", "ports").stdout.strip()
            if lsp_uuid not in membership:
                raise IsolationError(f"OVN LSP is not attached to ls-secondary: {lsp}")
            node = pods[suffix]["spec"]["nodeName"]
            if node not in self.runtime_pods:
                raise IsolationError(f"workload {uid} is on unexpected worker {node}")
            system_id = self.runtime(node, "ovn", "ovs-vsctl", "get", "Open_vSwitch", ".",
                                     "external_ids:system-id").stdout.strip().strip('"')
            binding = self.wait_for_binding(lsp, node, system_id)
            ovs_interface = self.runtime(
                node, "ovn", "ovs-vsctl", "--bare", "--columns=name", "find",
                "Interface", f"external_ids:evpn.framework/workload-uid={uid}").stdout.strip()
            if not ovs_interface or "\n" in ovs_interface:
                raise IsolationError(f"OVS interface ownership is missing or ambiguous for {uid}")
            bridge = self.runtime(node, "ovn", "ovs-vsctl", "port-to-br",
                                  ovs_interface).stdout.strip()
            if bridge != "br-int":
                raise IsolationError(f"endpoint interface {ovs_interface} is attached to {bridge}")
            host_link = json.loads(self.runtime(
                node, "ovn", "ip", "-j", "-d", "link", "show", "dev",
                ovs_interface).stdout)
            expected_alias = (
                f"evpn-node-agent|secondary-isolation|s|"
                f"secondary-isolation|s|{uid}")
            if (len(host_link) != 1 or host_link[0].get("ifalias") != expected_alias or
                    (host_link[0].get("linkinfo") or {}).get("info_kind") != "veth"):
                raise IsolationError(f"OVS endpoint is not the owned host veth for {uid}")
            pod_link = json.loads(self.k(
                "-n", "cni-test", "exec", "pod-" + suffix, "--",
                "ip", "-j", "-d", "link", "show", "dev", "net1").stdout)
            expected_mac = "0a:58:f0:00:00:0a" if suffix == "a" else "0a:58:f0:00:00:0b"
            pod_addresses = json.loads(self.k(
                "-n", "cni-test", "exec", "pod-" + suffix, "--",
                "ip", "-j", "address", "show", "dev", "net1").stdout)
            expected_ip = secondary_ip(suffix)
            actual_ips = [address["local"] for address in pod_addresses[0].get("addr_info", [])
                          if address.get("family") == "inet"]
            if (len(pod_link) != 1 or pod_link[0].get("address") != expected_mac or
                    (pod_link[0].get("linkinfo") or {}).get("info_kind") != "veth" or
                    expected_ip not in actual_ips):
                raise IsolationError(f"pod-{suffix} net1 link identity/address mismatch")
            endpoints[uid] = {
                "pod": "pod-" + suffix, "node": node, "podUID": uid,
                "logicalSwitchPort": lsp, "logicalSwitchPortUUID": lsp_uuid,
                "logicalSwitch": "ls-secondary", "portBindingChassis": binding["chassisUUID"],
                "portBindingChassisName": system_id, "portBindingUp": True,
                "hostVeth": ovs_interface,
                "hostVethAlias": host_link[0]["ifalias"], "hostVethIndex": host_link[0]["ifindex"],
                "ovsBridge": bridge,
                "ovsOfport": self.runtime(node, "ovn", "ovs-vsctl", "get", "Interface",
                                          ovs_interface, "ofport").stdout.strip(),
                "podInterface": "net1", "podMAC": pod_link[0]["address"],
                "podIPs": actual_ips,
            }
        self.record("endpoint-runtime-identities.json", endpoints)
        self.record("type2-scope.json", {
            "status": "NOT_TESTED", "nativeForwarding": "OVN Geneve only",
        })

    def wait_for_binding(self, logical_port, node, system_id, timeout=60):
        if not system_id:
            raise IsolationError(f"missing OVS system-id for worker {node}")
        deadline = time.monotonic() + timeout
        last = None
        while True:
            tables = {}
            for table, columns in (("Port_Binding", "_uuid,logical_port,chassis,up"),
                                   ("Chassis", "_uuid,name,hostname")):
                raw = self.k("-n", "cni-test", "exec", "deploy/secondary-ovn-central", "--",
                             "ovn-sbctl", "--timeout=5", "--format=json",
                             f"--columns={columns}", "list", table, timeout=8).stdout
                document = json.loads(raw)
                headings = document["headings"]
                if len(headings) != len(set(headings)) or set(headings) != set(columns.split(",")):
                    raise IsolationError(f"unexpected {table} columns: {headings}")
                rows = document["data"]
                if any(len(row) != len(headings) for row in rows):
                    raise IsolationError(f"malformed {table} rows")
                tables[table] = [dict(zip(headings, row)) for row in rows]
            bindings = [row for row in tables["Port_Binding"]
                        if row["logical_port"] == logical_port]
            chassis = [row for row in tables["Chassis"] if row["name"] == system_id]
            if len(bindings) > 1 or len(chassis) > 1:
                raise IsolationError(f"ambiguous SBDB binding/chassis: port={logical_port} "
                                     f"bindings={bindings} chassis={chassis}")
            last = {"bindings": bindings, "chassis": chassis}
            if (len(bindings) == len(chassis) == 1 and
                    chassis[0]["hostname"] == node and
                    isinstance(chassis[0]["_uuid"], list) and
                    len(chassis[0]["_uuid"]) == 2 and chassis[0]["_uuid"][0] == "uuid" and
                    bindings[0]["chassis"] == chassis[0]["_uuid"] and
                    bindings[0]["up"] is True):
                return {"chassisUUID": chassis[0]["_uuid"][1]}
            if time.monotonic() >= deadline:
                raise IsolationError(f"timed out waiting for binding {logical_port} on "
                                     f"worker {node} chassis {system_id}: {last}")
            time.sleep(min(1, max(0, deadline - time.monotonic())))

    def check_cni(self, pods):
        attachments = {}
        for suffix, node in zip(("a", "b"), self.workers):
            uid = pods[suffix]["metadata"]["uid"]
            call = self.worker(node, "sh", "-ec",
                               "for d in /run/evpn-cni-evidence/call.*; do "
                               "grep -q '^CNI_COMMAND=ADD$' \"$d/env\" && "
                               f"grep -q 'K8S_POD_UID={uid}' \"$d/env\" && "
                               "test \"$(cat \"$d/rc\")\" = 0 && { echo \"$d\"; exit 0; }; "
                               "done; exit 1").stdout.strip()
            env = {}
            for entry in self.worker(node, "cat", f"{call}/env").stdout.splitlines():
                key, separator, value = entry.partition("=")
                if separator:
                    env[key] = value
            if (env.get("CNI_COMMAND") != "ADD" or env.get("CNI_IFNAME") != "net1" or
                    not env.get("CNI_CONTAINERID") or not env.get("CNI_NETNS") or
                    f"K8S_POD_UID={uid}" not in env.get("CNI_ARGS", "")):
                raise IsolationError(f"incomplete Multus CNI ADD identity for pod-{suffix}")
            attachments[suffix] = {
                "podUID": uid, "sandboxID": env["CNI_CONTAINERID"],
                "netnsPath": env["CNI_NETNS"], "interface": env["CNI_IFNAME"],
                "cniArgs": env["CNI_ARGS"],
                "configSHA256": hashlib.sha256(
                    self.worker(node, "cat", f"{call}/config").stdout.encode()).hexdigest(),
                "node": node, "wrapperCallDirectory": call,
            }
            self.worker(node, "sh", "-ec",
                        "while IFS= read -r line; do export \"$line\"; done < \"$1/env\"; "
                        "export CNI_COMMAND=CHECK; "
                        "exec /opt/cni/bin/evpn-cni.real < \"$1/config\"", "sh", call)
        self.cni_attachment_ids = attachments
        self.record("cni-attachment-identities.json", attachments)
        self.record("cni-check.json", {"status": "PASS", "method": "replay of Multus-captured CNI args"})

    def verify_connectivity(self, pods):
        node_ips = {}
        for node in self.workers:
            node_ips[node] = json.loads(self.k("get", "node", node, "-o", "json").stdout)[
                "status"]["addresses"]
            node_ips[node] = next(a["address"] for a in node_ips[node] if a["type"] == "InternalIP")
        if len(set(node_ips.values())) != 2:
            raise IsolationError(f"worker VTEP addresses are ambiguous: {node_ips}")
        switch_uuid = self.k(
            "-n", "cni-test", "exec", "deploy/secondary-ovn-central", "--",
            "ovn-nbctl", "--bare", "--columns=_uuid", "find", "Logical_Switch",
            "name=ls-secondary").stdout.strip()
        datapaths = self.k(
            "-n", "cni-test", "exec", "deploy/secondary-ovn-central", "--",
            "ovn-sbctl", "--format=json", "--columns=_uuid,external_ids,tunnel_key",
            "list", "Datapath_Binding").stdout
        datapath = datapath_tunnel_key(datapaths, switch_uuid)
        datapath_key = datapath["tunnelKey"]
        self.record("ovn-datapath-identity.json", datapath)
        probes = []
        for suffix, source, destination in (
                ("a-to-b", "a", "b"), ("b-to-a", "b", "a")):
            marker = b"EVPN" + bytes.fromhex(self.owner.split("-")[1][:8]) + suffix.encode()[:1]
            identifier = int(hashlib.sha256(
                f"{self.owner}:{suffix}".encode()).hexdigest()[:4], 16) or 1
            probes.append({
                "name": suffix, "identifier": identifier, "marker": marker,
                "source": source, "destination": destination,
                "sourceNode": pods[source]["spec"]["nodeName"],
                "destinationNode": pods[destination]["spec"]["nodeName"],
                "sourceIP": secondary_ip(source), "destinationIP": secondary_ip(destination),
            })
        if len({probe["identifier"] for probe in probes}) != len(probes):
            raise IsolationError("probe ICMP identifiers collide")
        captures = {}
        receiver_captures = {}
        procs = []
        receiver_procs = []
        try:
            for probe in probes:
                node = pods[probe["destination"]]["spec"]["nodeName"]
                pod_name = "pod-" + probe["destination"]
                pcap = f"/tmp/{self.owner}-net1-{probe['destination']}.pcap"
                if self.k("-n", "cni-test", "exec", pod_name, "-c", "client", "--",
                          "test", "-e", pcap, check=False).returncode == 0:
                    raise IsolationError(
                        f"refusing pre-existing net1 packet capture path on {pod_name}")
                command = [
                    "kubectl", "--kubeconfig", self.kubeconfig, "--context", self.context,
                    "-n", "cni-test", "exec", pod_name, "-c", "client", "--",
                    "sh", "-ec",
                    f"timeout 35 tcpdump -nn -U -i net1 -c 12 -w {pcap} "
                    f"'icmp and host {probe['sourceIP']} and "
                    f"host {probe['destinationIP']}'",
                ]
                out = open(self.directory / f"capture-net1-{probe['destination']}.out", "w")
                err = open(self.directory / f"capture-net1-{probe['destination']}.err", "w")
                try:
                    proc = subprocess.Popen(
                        command, stdout=out, stderr=err, text=True,
                        pass_fds=lab.lock_descriptors())
                except OSError:
                    out.close()
                    err.close()
                    raise
                receiver_procs.append(
                    (probe["destination"], pod_name, pcap, proc, out, err))
            for node in self.workers:
                target = next(n for n in self.workers if n != node)
                pcap = f"/tmp/{self.owner}-{node}.pcap"
                exists = self.worker(node, "test", "-e", pcap, check=False)
                if exists.returncode == 0:
                    raise IsolationError(f"refusing pre-existing packet capture path on {node}")
                if exists.returncode != 1:
                    raise IsolationError(f"cannot check packet capture path on {node}: "
                                         f"exit={exists.returncode}: {exists.stderr}")
                command = ["kubectl", "--kubeconfig", self.kubeconfig, "--context", self.context,
                           "-n", "cni-test", "exec", self.runtime_pods[node], "-c", "ovn", "--",
                           "sh", "-ec",
                           f"timeout -s INT 25 tcpdump -nn -U -i eth0 -c 256 -w {pcap} "
                           f"'udp dst port 6081 and host {node_ips[node]} and host {node_ips[target]}'"]
                out = open(self.directory / f"capture-{node}.out", "w")
                err = open(self.directory / f"capture-{node}.err", "w")
                try:
                    proc = subprocess.Popen(
                        command, stdout=out, stderr=err, text=True,
                        pass_fds=lab.lock_descriptors())
                except OSError:
                    out.close()
                    err.close()
                    raise
                procs.append((node, proc, out, err))
            deadline = time.monotonic() + 15
            capture_logs = [
                (proc, self.directory / f"capture-net1-{suffix}.err")
                for suffix, _, _, proc, _, _ in receiver_procs
            ] + [(proc, self.directory / f"capture-{node}.err")
                 for node, proc, _, _ in procs]
            while True:
                if any(proc.poll() is not None for proc, _ in capture_logs):
                    raise IsolationError("packet capture exited before probes started")
                if all("listening on" in path.read_text() for _, path in capture_logs):
                    break
                if time.monotonic() >= deadline:
                    raise IsolationError("packet captures did not become ready")
                time.sleep(0.1)
            probe_outputs = {}
            for probe in probes:
                route = json.loads(self.k(
                    "-n", "cni-test", "exec", "pod-" + probe["source"], "--",
                    "ip", "-j", "route", "get", probe["destinationIP"],
                    "from", probe["sourceIP"], "oif", "net1").stdout)
                if len(route) != 1 or route[0].get("dev") != "net1":
                    raise IsolationError(f"probe {probe['name']} has no exact net1 route: {route}")
                result = self.k(
                    "-n", "cni-test", "exec", "pod-" + probe["source"], "--",
                    "ping", "-I", "net1", "-e", str(probe["identifier"]),
                    "-p", probe["marker"].hex(), "-c", "3", "-W", "2",
                    probe["destinationIP"], timeout=30)
                if "0% packet loss" not in result.stdout:
                    raise IsolationError(f"probe {probe['name']} reported packet loss")
                probe_outputs[probe["name"]] = result.stdout
            for suffix in ("a", "b"):
                destination = pods["b" if suffix == "a" else "a"]["status"]["podIP"]
                result = self.k("-n", "cni-test", "exec", "pod-" + suffix, "--",
                                "ping", "-I", "eth0", "-c", "2", "-W", "2",
                                destination, timeout=30)
                if "0% packet loss" not in result.stdout:
                    raise IsolationError(f"primary eth0 control ping failed for pod-{suffix}")
            for node, proc, out, err in procs:
                rc = proc.wait(timeout=35)
                out.close()
                err.close()
                if rc not in (0, 124):
                    raise IsolationError(f"incomplete Geneve capture on {node}: exit={rc}")
                pcap = f"/tmp/{self.owner}-{node}.pcap"
                local_pcap = self.directory / f"geneve-{node}.pcap"
                self.k("-n", "cni-test", "cp", "-c", "ovn",
                       f"{self.runtime_pods[node]}:{pcap}", str(local_pcap))
                packets = decode_geneve_pcap(local_pcap)
                captures[node] = {
                    "packets": packets,
                    "pcapSHA256": hashlib.sha256(local_pcap.read_bytes()).hexdigest(),
                    "pcapBytes": local_pcap.stat().st_size,
                }
        finally:
            for _, proc, out, err in procs:
                if proc.poll() is None:
                    proc.terminate()
                    try:
                        proc.wait(timeout=5)
                    except subprocess.TimeoutExpired:
                        proc.kill()
                        proc.wait()
                if not out.closed:
                    out.close()
                if not err.closed:
                    err.close()
            for _, _, _, proc, out, err in receiver_procs:
                if proc.poll() is None:
                    proc.terminate()
                    try:
                        proc.wait(timeout=5)
                    except subprocess.TimeoutExpired:
                        proc.kill()
                        proc.wait()
                if not out.closed:
                    out.close()
                if not err.closed:
                    err.close()
        for suffix, pod_name, pcap, proc, out, err in receiver_procs:
            rc = proc.wait(timeout=5)
            if rc:
                raise IsolationError(
                    f"incomplete net1 receiver capture on {pod_name}: exit={rc}")
            local_pcap = self.directory / f"net1-receiver-{suffix}.pcap"
            self.k("-n", "cni-test", "cp", "-c", "client",
                   f"{pod_name}:{pcap}", str(local_pcap))
            packets = decode_icmp_pcap(local_pcap)
            receiver_captures[suffix] = {
                "packets": packets,
                "pcapSHA256": hashlib.sha256(local_pcap.read_bytes()).hexdigest(),
                "pcapBytes": local_pcap.stat().st_size,
            }
        assert_geneve_probe_evidence(
            {node: capture["packets"] for node, capture in captures.items()},
            probes, node_ips, datapath_key,
            {suffix: capture["packets"] for suffix, capture in receiver_captures.items()})
        self.record("geneve-packet-evidence.json", captures)
        self.record("net1-receiver-packet-evidence.json", receiver_captures)
        self.record("geneve-probe-identities.json", {
            "ovnDatapathTunnelKey": datapath_key, "vteps": node_ips,
            "probes": [{
                **{key: value for key, value in probe.items() if key != "marker"},
                "markerHex": probe["marker"].hex(),
            } for probe in probes],
            "pingOutput": probe_outputs,
        })
        self.record("native-forwarding.json", {
            "forward": "PASS via Pod A net1, correlated inner ICMP and secondary Geneve",
            "reverse": "PASS via Pod B net1, correlated inner ICMP and secondary Geneve",
            "primaryPodNetwork": "PASS via eth0", "tunnel": "Geneve/UDP 6081",
            "ovnDatapathTunnelKey": datapath_key,
            "evpnType2": "NOT_TESTED",
        })

    def verify_foreign_resources(self):
        observed = {
            "host": {"inventory": host_dataplane_inventory()},
        }
        for node, before in self.primary_before.items():
            after = self.primary_snapshot(node, allow_secondary=True)
            assert_primary_preserved(before, after)
            observed[node] = {
                "links": {
                    name: identity for name, identity in after["links"].items()
                    if (identity["kind"] == "openvswitch" or name in ("br-int", "ovs-system"))
                    and identity["ifalias"] != self.owner
                },
            }
        assert_foreign_resources_preserved(self.preexisting_foreign, observed)
        report = foreign_resource_report(
            self.preexisting_foreign, comparison_complete=True)
        self.foreign_status = report["status"]
        self.record("foreign-resource-inventory-after.json", report)

    def delete_pods(self, pods):
        self.k("-n", "cni-test", "delete", "pod", "pod-a", "pod-b", "--wait=true",
               "--timeout=120s")
        self.record("pod-delete.json", {"status": "PASS"})

    def verify_cleanup(self, pods):
        del_identities = {}
        for node, suffix in zip(self.workers, ("a", "b")):
            uid = pods[suffix]["metadata"]["uid"]
            for _ in range(60):
                ovs = self.runtime(node, "ovn", "ovs-vsctl", "--bare", "--columns=name",
                                   "find", "Interface",
                                   f"external_ids:evpn.framework/workload-uid={uid}").stdout.strip()
                lsp = self.k("-n", "cni-test", "exec", "deploy/secondary-ovn-central", "--",
                             "ovn-nbctl", "--bare", "--columns=name", "find",
                             "Logical_Switch_Port",
                             f"external_ids:evpn.framework/workload-uid={uid}").stdout.strip()
                if not ovs and not lsp:
                    break
                time.sleep(1)
            else:
                raise IsolationError(f"CNI DEL left owned resources for UID {uid}")
            alias = self.runtime(node, "ovn", "sh", "-ec",
                                 "for p in /sys/class/net/*/ifalias; do "
                                 f"test \"$(cat \"$p\")\" = 'evpn-node-agent|secondary-isolation|s|secondary-isolation|s|{uid}' "
                                 "&& echo \"$p\"; done", check=False).stdout.strip()
            if alias:
                raise IsolationError(f"CNI DEL left owned host veth for UID {uid}: {alias}")
            del_call = self.worker(
                node, "sh", "-ec",
                "for d in /run/evpn-cni-evidence/call.*; do "
                "grep -q '^CNI_COMMAND=DEL$' \"$d/env\" && "
                f"grep -q 'K8S_POD_UID={uid}' \"$d/env\" && "
                "test \"$(cat \"$d/rc\")\" = 0 && { echo \"$d\"; exit 0; }; "
                "done; exit 1").stdout.strip()
            if not del_call:
                raise IsolationError(f"successful Multus DEL was not recorded for UID {uid}")
            del_env = {}
            for entry in self.worker(node, "cat", f"{del_call}/env").stdout.splitlines():
                key, separator, value = entry.partition("=")
                if separator:
                    del_env[key] = value
            add_identity = self.cni_attachment_ids[suffix]
            if (del_env.get("CNI_COMMAND") != "DEL" or
                    del_env.get("CNI_CONTAINERID") != add_identity["sandboxID"] or
                    del_env.get("CNI_IFNAME") != add_identity["interface"] or
                    f"K8S_POD_UID={uid}" not in del_env.get("CNI_ARGS", "")):
                raise IsolationError(f"CNI DEL identity did not match ADD for UID {uid}")
            del_identities[suffix] = {
                "podUID": uid, "sandboxID": del_env["CNI_CONTAINERID"],
                "netnsPath": del_env.get("CNI_NETNS", ""),
                "interface": del_env["CNI_IFNAME"], "node": node,
                "wrapperCallDirectory": del_call,
            }
        self.verify_foreign_resources()
        self.record("cni-del-identities.json", del_identities)
        self.record("cni-cleanup.json", {"status": "PASS", "ownedEndpoints": "removed",
                                         "foreignWitness": self.foreign_status,
                                         "primaryCNI": "preserved"})

    def verify_primary(self, output):
        current = {}
        for node in self.workers:
            current[node] = self.primary_snapshot(node, allow_secondary=True)
        self.record(output, current)
        for node, snapshot in current.items():
            if output == "primary-after-del.json":
                assert_primary_restored(self.primary_before[node], snapshot)
            else:
                assert_primary_preserved(self.primary_before[node], snapshot)

    def assert_primary_cni(self, node):
        pods = json.loads(self.k("get", "pods", "-A", "-o", "json").stdout)["items"]
        unexpected = []
        for pod in pods:
            if pod["spec"].get("nodeName") != node:
                continue
            for container in pod["spec"].get("containers", []):
                identity = (container.get("name", "") + " " +
                            container.get("image", "")).lower()
                if re.search(r"ovn|openvswitch|ovs-", identity):
                    unexpected.append((pod["metadata"]["namespace"],
                                       pod["metadata"]["name"], identity))
        if unexpected:
            raise IsolationError(f"cannot establish OVS/OVN ownership before test on {node}: "
                                 f"{unexpected}")

    def run(self):
        self.initialize()
        failure = None
        workload_started = False
        try:
            self.build()
            self.provision()
            workload_started = True
            self.test_endpoints()
        except Exception as error:
            failure = error
            self.record("failure.json", {"error": str(error),
                                         "traceback": traceback.format_exc()})
            if workload_started:
                try:
                    self.capture_workload_failure()
                except Exception as evidence_error:
                    self.record("workload-evidence-error.json", {
                        "error": str(evidence_error), "traceback": traceback.format_exc()})
        finally:
            cleanup_errors = []
            lab_clean = True
            if (self.directory / "lab.json").exists():
                try:
                    state = lab.load(self.directory)
                    if state["phase"] not in ("reserved", "down"):
                        try:
                            self.verify_host_dataplane("host-primary-after-experiment.json")
                        except (IsolationError, OSError, ValueError, subprocess.TimeoutExpired) as error:
                            cleanup_errors.append(f"host OVS/OVN preservation: {error}")
                        try:
                            lab.collect(self.directory)
                        except (IsolationError, lab.LabError, OSError, ValueError,
                                subprocess.TimeoutExpired) as error:
                            cleanup_errors.append(f"evidence collection: {error}")
                        try:
                            lab.down(self.directory)
                        except (IsolationError, lab.LabError, OSError, ValueError,
                                subprocess.TimeoutExpired) as error:
                            lab_clean = False
                            cleanup_errors.append(f"lab teardown: {error}")
                except (IsolationError, lab.LabError, OSError, ValueError,
                        subprocess.TimeoutExpired) as error:
                    lab_clean = False
                    cleanup_errors.append(f"lab state inspection: {error}")
            if self.created_image and lab_clean:
                try:
                    self.cleanup_run_image()
                except (IsolationError, OSError, ValueError, subprocess.TimeoutExpired) as error:
                    cleanup_errors.append(f"agent image cleanup: {error}")
            elif self.created_image and not lab_clean:
                cleanup_errors.append("agent image preserved because lab teardown was not confirmed")
            if cleanup_errors:
                self.record("cleanup-error.json", {"errors": cleanup_errors})
                if failure is None:
                    failure = IsolationError("; ".join(cleanup_errors))
        if failure:
            raise failure
        self.record("result.json", {
            "status": self.result_status(),
            "primaryCNI": "Kindnet",
            "liveEVPNType2": "NOT_TESTED",
            "cleanup": "PASS",
            **self.result_details(),
        })
        print(f"{self.result_status()} evidence={self.directory}")


def main():
    if len(sys.argv) != 1:
        raise IsolationError("runner takes no arguments")
    with lab.exclusive():
        IsolatedSecondaryCNI().run()


if __name__ == "__main__":
    try:
        main()
    except (IsolationError, OSError, ValueError, subprocess.TimeoutExpired) as error:
        print(f"isolated secondary CNI: {error}", file=sys.stderr)
        sys.exit(1)
