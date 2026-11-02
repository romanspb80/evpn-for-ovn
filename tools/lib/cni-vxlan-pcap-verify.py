#!/usr/bin/env python3
"""Decode and correlate bounded Ethernet pcaps from the CNI EVPN VXLAN test."""

import argparse
import collections
import hashlib
import ipaddress
import json
import re
import struct
import sys
from pathlib import Path


PCAP_MAGICS = {
    b"\xd4\xc3\xb2\xa1": ("<", 1_000_000),
    b"\xa1\xb2\xc3\xd4": (">", 1_000_000),
    b"\x4d\x3c\xb2\xa1": ("<", 1_000_000_000),
    b"\xa1\xb2\x3c\x4d": (">", 1_000_000_000),
}


def mac_text(value):
    return ":".join(f"{part:02x}" for part in value)


def ipv4_text(value):
    return str(ipaddress.IPv4Address(value))


def read_pcap(path):
    data = Path(path).read_bytes()
    if len(data) < 24:
        raise ValueError(f"{path}: truncated pcap header")
    byte_order, fraction_scale = PCAP_MAGICS.get(data[:4], (None, None))
    if byte_order is None:
        raise ValueError(f"{path}: unsupported pcap magic")
    _, _, _, _, snaplen, link_type = struct.unpack_from(byte_order + "HHIIII", data, 4)
    if link_type != 1:
        raise ValueError(f"{path}: expected Ethernet link type 1, got {link_type}")
    offset = 24
    records = []
    while offset < len(data):
        if offset + 16 > len(data):
            raise ValueError(f"{path}: truncated packet header")
        seconds, fraction, captured_len, original_len = struct.unpack_from(
            byte_order + "IIII", data, offset
        )
        offset += 16
        if captured_len > snaplen or captured_len > len(data) - offset:
            raise ValueError(f"{path}: invalid/truncated packet length {captured_len}")
        if captured_len < original_len:
            raise ValueError(
                f"{path}: packet truncated to {captured_len} of {original_len} bytes"
            )
        if captured_len > original_len:
            raise ValueError(
                f"{path}: captured packet length {captured_len} exceeds original length {original_len}"
            )
        frame = data[offset : offset + captured_len]
        offset += captured_len
        records.append(
            {
                "timestamp": seconds + fraction / fraction_scale,
                "capturedLength": captured_len,
                "originalLength": original_len,
                "frame": frame,
            }
        )
    return data, records


def ipv4_packet(frame, offset):
    if len(frame) < offset + 20 or frame[offset] >> 4 != 4:
        return None
    header_len = (frame[offset] & 0x0F) * 4
    total_len = struct.unpack_from("!H", frame, offset + 2)[0]
    fragment = struct.unpack_from("!H", frame, offset + 6)[0] & 0x1FFF
    if (
        header_len < 20
        or len(frame) < offset + header_len
        or total_len < header_len
        or len(frame) < offset + total_len
    ):
        return None
    if fragment:
        return None
    end = min(len(frame), offset + total_len)
    return {
        "source": ipv4_text(frame[offset + 12 : offset + 16]),
        "destination": ipv4_text(frame[offset + 16 : offset + 20]),
        "protocol": frame[offset + 9],
        "payloadOffset": offset + header_len,
        "end": end,
    }


def ethernet_packet(frame, offset=0):
    if len(frame) < offset + 14:
        return None
    source_mac = mac_text(frame[offset + 6 : offset + 12])
    destination_mac = mac_text(frame[offset : offset + 6])
    ether_type = struct.unpack_from("!H", frame, offset + 12)[0]
    payload_offset = offset + 14
    while ether_type in (0x8100, 0x88A8, 0x9100):
        if len(frame) < payload_offset + 4:
            return None
        ether_type = struct.unpack_from("!H", frame, payload_offset + 2)[0]
        payload_offset += 4
    return source_mac, destination_mac, ether_type, payload_offset


def inner_packet(frame, offset=0):
    ethernet = ethernet_packet(frame, offset)
    if ethernet is None:
        return None
    source_mac, destination_mac, ether_type, payload_offset = ethernet
    result = {
        "sourceMac": source_mac,
        "destinationMac": destination_mac,
        "etherType": ether_type,
    }
    if ether_type == 0x0806:
        if len(frame) < payload_offset + 28:
            return None
        arp = frame[payload_offset : payload_offset + 28]
        result["arp"] = {
            "operation": struct.unpack_from("!H", arp, 6)[0],
            "senderMac": mac_text(arp[8:14]),
            "senderIp": ipv4_text(arp[14:18]),
            "targetMac": mac_text(arp[18:24]),
            "targetIp": ipv4_text(arp[24:28]),
        }
        return result
    if ether_type != 0x0800:
        return result
    ip = ipv4_packet(frame, payload_offset)
    if ip is None:
        return None
    result["sourceIp"] = ip["source"]
    result["destinationIp"] = ip["destination"]
    if ip["protocol"] != 1:
        return result
    if ip["end"] < ip["payloadOffset"] + 8:
        return None
    icmp_offset = ip["payloadOffset"]
    icmp_type, code, _, identifier, sequence = struct.unpack_from("!BBHHH", frame, icmp_offset)
    result["icmp"] = {
        "type": icmp_type,
        "code": code,
        "identifier": identifier,
        "sequence": sequence,
        "payload": frame[icmp_offset + 8 : ip["end"]],
    }
    return result


def marker_for(payload, markers):
    for label, marker in markers.items():
        if marker in payload:
            return label
    return None


def packet_label(inner, timestamp, markers, manifest):
    icmp = inner["icmp"]
    label = marker_for(icmp["payload"], markers)
    if label:
        return label
    stimulus = manifest.get("stimuli", {}).get("frr-to-pod")
    if not stimulus or "frr-to-pod" in markers:
        return None
    if not stimulus["startedAt"] <= timestamp <= stimulus["completedAt"]:
        return None
    if icmp["code"] != 0 or icmp["sequence"] not in (0, 1, 2):
        return None
    if icmp["type"] == 8:
        source, destination = manifest["remoteIp"], manifest["localIp"]
    elif icmp["type"] == 0:
        source, destination = manifest["localIp"], manifest["remoteIp"]
    else:
        return None
    if inner.get("sourceIp") == source and inner.get("destinationIp") == destination:
        return "frr-to-pod"
    return None


def icmp_evidence(icmp):
    return {
        **{key: value for key, value in icmp.items() if key != "payload"},
        "payloadSha256": hashlib.sha256(icmp["payload"]).hexdigest(),
    }


def expected_packets(manifest):
    local_ip, remote_ip = manifest["localIp"], manifest["remoteIp"]
    local_mac = manifest["localMac"].lower()
    remote_mac = manifest["remoteMac"].lower()
    expected = {}
    for label, source_ip, destination_ip, source_mac, destination_mac in (
        ("pod-to-frr", local_ip, remote_ip, local_mac, remote_mac),
        ("frr-to-pod", remote_ip, local_ip, remote_mac, local_mac),
    ):
        sequences = range(1, 4) if label == "pod-to-frr" else range(0, 3)
        for sequence in sequences:
            for icmp_type, src, dst, src_mac, dst_mac in (
                (8, source_ip, destination_ip, source_mac, destination_mac),
                (0, destination_ip, source_ip, destination_mac, source_mac),
            ):
                key = (label, icmp_type, sequence, src, dst, src_mac, dst_mac)
                expected[key] = True
    return expected


def marker_events(records, kind, markers, manifest):
    events = []
    alternate = []
    vxlan_count = 0
    sync_count = 0
    geneve_count = 0
    direct_count = 0
    for record in records:
        frame = record["frame"]
        if kind == "vxlan":
            ethernet = ethernet_packet(frame)
            if ethernet is None:
                continue
            if ethernet[2] == 0x0806:
                arp_frame = inner_packet(frame)
                arp = arp_frame.get("arp") if arp_frame else None
                if arp and (
                    arp["senderIp"] in (manifest["localIp"], manifest["remoteIp"])
                    or arp["targetIp"] in (manifest["localIp"], manifest["remoteIp"])
                    or arp["senderMac"].lower() in (manifest["localMac"].lower(), manifest["remoteMac"].lower())
                    or arp["targetMac"].lower() in (manifest["localMac"].lower(), manifest["remoteMac"].lower())
                ):
                    direct_count += 1
                continue
            if ethernet[2] != 0x0800:
                continue
            outer = ipv4_packet(frame, ethernet[3])
            if outer is None:
                continue
            is_endpoint_pair = {
                outer["source"],
                outer["destination"],
            } == {manifest["localIp"], manifest["remoteIp"]}
            if outer["protocol"] != 17:
                if is_endpoint_pair:
                    direct_count += 1
                direct = inner_packet(frame)
                if direct and direct.get("icmp"):
                    label = packet_label(direct, record["timestamp"], markers, manifest)
                    if label:
                        alternate.append(
                            {
                                "timestamp": record["timestamp"],
                                "kind": "unencapsulated-underlay",
                                "marker": label,
                                "sourceIp": direct.get("sourceIp"),
                                "destinationIp": direct.get("destinationIp"),
                            }
                        )
                continue
            udp_offset = outer["payloadOffset"]
            if outer["end"] < udp_offset + 8:
                continue
            source_port, destination_port, udp_len = struct.unpack_from("!HHH", frame, udp_offset)
            if udp_len < 8 or outer["end"] < udp_offset + min(udp_len, 8):
                continue
            payload_offset = udp_offset + 8
            if destination_port == 4789:
                vxlan_count += 1
                if outer["end"] < payload_offset + 8:
                    continue
                flags = frame[payload_offset]
                vni = int.from_bytes(frame[payload_offset + 4 : payload_offset + 7], "big")
                inner = inner_packet(frame, payload_offset + 8)
                tunnel = {
                    "outerSource": outer["source"],
                    "outerDestination": outer["destination"],
                    "udpSourcePort": source_port,
                    "udpDestinationPort": destination_port,
                    "vni": vni,
                    "vxlanIFlag": bool(flags & 0x08),
                }
                if inner and inner.get("icmp"):
                    event = {
                        "timestamp": record["timestamp"],
                        **tunnel,
                        **{k: v for k, v in inner.items() if k != "icmp"},
                        "icmp": icmp_evidence(inner["icmp"]),
                    }
                    label = packet_label(inner, record["timestamp"], markers, manifest)
                    if label:
                        event["marker"] = label
                        events.append(event)
                continue
            if destination_port == 60100:
                sync_count += 1
                if outer["end"] < payload_offset + 8:
                    continue
                vni = int.from_bytes(frame[payload_offset + 4 : payload_offset + 7], "big")
                inner = inner_packet(frame, payload_offset + 8)
                if inner and inner.get("icmp"):
                    label = packet_label(inner, record["timestamp"], markers, manifest)
                    if label:
                        alternate.append(
                            {
                                "timestamp": record["timestamp"],
                                "kind": "linux-vxlan-sync-60100",
                                "marker": label,
                                "vni": vni,
                                "outerSource": outer["source"],
                                "outerDestination": outer["destination"],
                            }
                        )
                continue
            if destination_port == 6081:
                geneve_count += 1
                if outer["end"] < payload_offset + 8:
                    continue
                option_len = (frame[payload_offset + 1] & 0x3F) * 4
                vni = int.from_bytes(frame[payload_offset + 4 : payload_offset + 7], "big")
                inner = inner_packet(frame, payload_offset + 8 + option_len)
                if inner and inner.get("icmp"):
                    label = packet_label(inner, record["timestamp"], markers, manifest)
                    if label:
                        alternate.append(
                            {
                                "timestamp": record["timestamp"],
                                "kind": "geneve-6081",
                                "marker": label,
                                "vni": vni,
                                "outerSource": outer["source"],
                                "outerDestination": outer["destination"],
                            }
                        )
                continue
            if is_endpoint_pair:
                direct_count += 1
            if destination_port == 4789:
                continue
            direct = inner_packet(frame)
            if direct and direct.get("icmp"):
                label = packet_label(direct, record["timestamp"], markers, manifest)
                if label:
                    alternate.append(
                        {
                            "timestamp": record["timestamp"],
                            "kind": "unencapsulated-underlay",
                            "marker": label,
                            "sourceIp": direct.get("sourceIp"),
                            "destinationIp": direct.get("destinationIp"),
                        }
                    )
        else:
            inner = inner_packet(frame)
            if not inner:
                continue
            if inner.get("arp"):
                events.append({"timestamp": record["timestamp"], "arp": inner["arp"], **{
                    k: v for k, v in inner.items() if k in ("sourceMac", "destinationMac")
                }})
            if inner.get("icmp"):
                event = {
                    "timestamp": record["timestamp"],
                    **{k: v for k, v in inner.items() if k != "icmp"},
                    "icmp": icmp_evidence(inner["icmp"]),
                }
                label = packet_label(inner, record["timestamp"], markers, manifest)
                if label:
                    event["marker"] = label
                    events.append(event)
    return events, alternate, vxlan_count, sync_count, geneve_count, direct_count


def event_key(event):
    icmp = event["icmp"]
    return (
        event["marker"],
        icmp["type"],
        icmp["sequence"],
        event.get("sourceIp"),
        event.get("destinationIp"),
        event.get("sourceMac", "").lower(),
        event.get("destinationMac", "").lower(),
    )


def ovs_port_stats(path, ofport):
    lines = Path(path).read_text(errors="replace").splitlines()
    block = []
    active = False
    for line in lines:
        match = re.match(r"\s*port\s+(\d+):", line)
        if match:
            if active:
                break
            active = int(match.group(1)) == ofport
        if active:
            block.append(line)
    text = " ".join(block)
    rx = re.search(r"\brx pkts=(\d+)", text)
    tx = re.search(r"\btx pkts=(\d+)", text)
    return {"rxPackets": int(rx.group(1)) if rx else None, "txPackets": int(tx.group(1)) if tx else None}


def ovs_actions(text):
    start = depth = 0
    for index, character in enumerate(text):
        if character == "(":
            depth += 1
        elif character == ")":
            depth -= 1
        elif character == "," and depth == 0:
            yield text[start:index].strip()
            start = index + 1
    yield text[start:].strip()


def tunnel_output_matches(actions, local_vtep, remote_vtep, vni, ofport):
    expected = {"src": int(ipaddress.IPv4Address(local_vtep)),
                "dst": int(ipaddress.IPv4Address(remote_vtep)), "id": vni}
    fields = {"NXM_NX_TUN_IPV4_SRC": "src", "NXM_NX_TUN_IPV4_DST": "dst",
              "NXM_NX_TUN_ID": "id"}
    tunnel = {}
    for action in ovs_actions(actions):
        match = re.fullmatch(r"set_field:([0-9a-fA-Fx.]+)->tun_(src|dst|id)", action)
        if match:
            value, field = match.groups()
            try:
                tunnel[field] = (
                    int(ipaddress.IPv4Address(value)) if field != "id" else int(value, 0)
                )
            except ValueError:
                tunnel.pop(field, None)
            continue
        match = re.fullmatch(
            r"load:(0x[0-9a-fA-F]+|[0-9]+)->"
            r"(NXM_NX_TUN_IPV4_SRC|NXM_NX_TUN_IPV4_DST|NXM_NX_TUN_ID)\[([^]]*)\]",
            action,
        )
        if match:
            value, field_name, bits = match.groups()
            field = fields[field_name]
            if bits in ("", "0..23" if field == "id" else "0..31"):
                tunnel[field] = int(value, 0)
            else:
                tunnel.pop(field, None)
            continue
        match = re.fullmatch(r"set_tunnel:(0x[0-9a-fA-F]+|[0-9]+)", action)
        if match:
            tunnel["id"] = int(match[1], 0)
        if action == f"output:{ofport}" and tunnel == expected:
            return True
    return False


def matching_flow_packets(path, local_vtep, remote_vtep, vni, ofport):
    total = 0
    matched = []
    for line in Path(path).read_text(errors="replace").splitlines():
        actions = line.partition("actions=")[2]
        if not actions:
            continue
        if not tunnel_output_matches(actions, local_vtep, remote_vtep, vni, ofport):
            continue
        packets = re.search(r"\bn_packets=(\d+)", line)
        if packets:
            total += int(packets.group(1))
            matched.append(line)
    return total, matched


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--manifest", required=True)
    parser.add_argument("--output", required=True)
    args = parser.parse_args()
    manifest = json.loads(Path(args.manifest).read_text())
    markers = {name: bytes.fromhex(value) for name, value in manifest["markers"].items()}
    expected = expected_packets(manifest)
    result = {
        "status": "PARTIAL",
        "manifest": manifest,
        "captures": {},
        "ovs": {},
        "alternativePathPackets": [],
        "errors": [],
    }
    identifiers_by_capture = {}
    payloads_by_capture = {}
    for name, capture in manifest["captures"].items():
        path = Path(capture["path"])
        try:
            raw, records = read_pcap(path)
            events, alternate, vxlan_count, sync_count, geneve_count, direct_count = marker_events(
                records, capture["kind"], markers, manifest
            )
            result["alternativePathPackets"].extend(
                [{"capture": name, **packet} for packet in alternate]
            )
            counts = collections.Counter()
            identifiers = collections.defaultdict(set)
            payloads = collections.defaultdict(set)
            invalid = []
            for event in events:
                if not event.get("marker") or "icmp" not in event:
                    continue
                key = event_key(event)
                if key not in expected:
                    invalid.append(event)
                    continue
                counts[key] += 1
                identifiers[key].add(event["icmp"]["identifier"])
                payloads[key].add(event["icmp"]["payloadSha256"])
                if capture["kind"] == "vxlan":
                    expected_source_vtep = (
                        manifest["localVtep"]
                        if event["sourceIp"] == manifest["localIp"]
                        else manifest["remoteVtep"]
                    )
                    expected_destination_vtep = (
                        manifest["remoteVtep"]
                        if event["destinationIp"] == manifest["remoteIp"]
                        else manifest["localVtep"]
                    )
                    if (
                        event["udpDestinationPort"] != manifest["udpPort"]
                        or event["vni"] != manifest["vni"]
                        or not event["vxlanIFlag"]
                        or event["outerSource"] != expected_source_vtep
                        or event["outerDestination"] != expected_destination_vtep
                    ):
                        invalid.append(event)
            missing = [key for key in expected if counts[key] == 0]
            capture_result = {
                "path": str(path),
                "sha256": hashlib.sha256(raw).hexdigest(),
                "bytes": len(raw),
                "records": len(records),
                "expectedIcmpPackets": sum(counts.values()),
                "packetIdentifiers": [
                    {
                        "marker": key[0],
                        "icmpType": key[1],
                        "sequence": key[2],
                        "identifiers": sorted(values),
                    }
                    for key, values in sorted(identifiers.items())
                ],
                "missing": [list(key) for key in missing],
                "invalid": invalid,
                "events": events,
                "vxlanUdp4789Records": vxlan_count,
                "linuxSyncUdp60100Records": sync_count,
                "geneveUdp6081Records": geneve_count,
                "directEndpointUnderlayRecords": direct_count,
            }
            if capture["kind"] == "vxlan":
                capture_result["vniRecords"] = {
                    str(manifest["vni"]): sum(
                        1
                        for event in events
                        if event.get("vni") == manifest["vni"]
                    )
                }
            result["captures"][name] = capture_result
            identifiers_by_capture[name] = identifiers
            payloads_by_capture[name] = payloads
            for key, values in identifiers.items():
                if len(values) != 1:
                    result["errors"].append(f"{name}: ambiguous ICMP identifiers for {key[:3]}")
                if key[1] == 8:
                    reply = (key[0], 0, key[2], key[4], key[3], key[6], key[5])
                    if identifiers.get(reply) != values or payloads.get(reply) != payloads[key]:
                        result["errors"].append(f"{name}: echo request/reply mismatch for {key[:3]}")
            if missing:
                result["errors"].append(f"{name}: missing {len(missing)} expected ICMP observations")
            if invalid:
                result["errors"].append(f"{name}: found {len(invalid)} invalid marked ICMP observations")
            if not records:
                result["errors"].append(f"{name}: pcap contains no records")
        except (OSError, ValueError, KeyError, struct.error) as error:
            result["errors"].append(f"{name}: {error}")
    baseline_identifiers = identifiers_by_capture.get("pod-facing", {})
    if "pod-facing" not in identifiers_by_capture:
        result["errors"].append("Pod-facing ICMP identifiers unavailable for cross-capture correlation")
    else:
        for name, observed in identifiers_by_capture.items():
            for key, baseline in baseline_identifiers.items():
                if observed.get(key) != baseline:
                    result["errors"].append(
                        f"{name}: ICMP identifier mismatch for {key[:3]} "
                        f"(pod-facing={sorted(baseline)}, observed={sorted(observed.get(key, set()))})"
                    )
                if payloads_by_capture[name].get(key) != payloads_by_capture["pod-facing"].get(key):
                    result["errors"].append(f"{name}: ICMP payload mismatch for {key[:3]}")
    if result["alternativePathPackets"]:
        result["errors"].append("marked ICMP packet observed on a non-OVS alternative tunnel/path")
    if any(
        capture.get("directEndpointUnderlayRecords", 0) > 0
        for capture in result["captures"].values()
        if capture.get("path") and capture.get("path", "").endswith(("worker-underlay.pcap", "frr-underlay.pcap"))
    ):
        result["errors"].append("direct endpoint traffic observed on an underlay outside OVS VXLAN")

    try:
        tunnel = Path(manifest["ovs"]["tunnelConfig"]).read_text(errors="replace")
        interface_valid = (
            "vxlan" in tunnel.lower()
            and re.search(rf"dst_port\s*=\s*['\"]?{manifest['udpPort']}['\"]?", tunnel)
        )
        remote_selection = (
            "remote_ip=flow" in tunnel
            or 'remote_ip="flow"' in tunnel
            or f'remote_ip="{manifest["remoteVtep"]}"' in tunnel
            or f"remote_ip={manifest['remoteVtep']}" in tunnel
        )
        key_selection = (
            "key=flow" in tunnel
            or 'key="flow"' in tunnel
            or f'key="{manifest["vni"]}"' in tunnel
            or f"key={manifest['vni']}" in tunnel
            or f"key=0x{manifest['vni']:x}" in tunnel
        )
        before_port = ovs_port_stats(
            manifest["ovs"]["portStatsBefore"], int(manifest["ovs"]["ofport"])
        )
        after_port = ovs_port_stats(
            manifest["ovs"]["portStatsAfter"], int(manifest["ovs"]["ofport"])
        )
        flow_before, flows_before = matching_flow_packets(
            manifest["ovs"]["flowsBefore"], manifest["localVtep"], manifest["remoteVtep"],
            manifest["vni"], int(manifest["ovs"]["ofport"])
        )
        flow_after, flows_after = matching_flow_packets(
            manifest["ovs"]["flowsAfter"], manifest["localVtep"], manifest["remoteVtep"],
            manifest["vni"], int(manifest["ovs"]["ofport"])
        )
        egress_flow_valid = bool(flows_after)
        result["ovs"] = {
            "tunnelConfig": tunnel,
            "typeAndOptionsValid": bool(
                interface_valid and remote_selection and key_selection and egress_flow_valid
            ),
            "egressFlowValid": egress_flow_valid,
            "remoteSelectionValid": bool(remote_selection),
            "vniKeySelectionValid": bool(key_selection),
            "ofport": int(manifest["ovs"]["ofport"]),
            "portStatsBefore": before_port,
            "portStatsAfter": after_port,
            "portRxDelta": (
                after_port["rxPackets"] - before_port["rxPackets"]
                if before_port["rxPackets"] is not None and after_port["rxPackets"] is not None
                else None
            ),
            "portTxDelta": (
                after_port["txPackets"] - before_port["txPackets"]
                if before_port["txPackets"] is not None and after_port["txPackets"] is not None
                else None
            ),
            "matchedFlowPacketsBefore": flow_before,
            "matchedFlowPacketsAfter": flow_after,
            "matchedFlowPacketsDelta": flow_after - flow_before,
            "matchingFlowsBefore": flows_before,
            "matchingFlowsAfter": flows_after,
        }
        if not result["ovs"]["typeAndOptionsValid"]:
            result["errors"].append(
                "OVS VXLAN interface/options or egress flow do not prove the expected VTEP/VNI"
            )
        if result["ovs"]["portRxDelta"] is None or result["ovs"]["portRxDelta"] <= 0:
            result["errors"].append("OVS tunnel port RX counter did not advance")
        if result["ovs"]["portTxDelta"] is None or result["ovs"]["portTxDelta"] <= 0:
            result["errors"].append("OVS tunnel port TX counter did not advance")
        if result["ovs"]["matchedFlowPacketsDelta"] <= 0:
            result["errors"].append("OVS remote-VTEP/VNI forwarding-flow counter did not advance")
    except (OSError, ValueError, KeyError, TypeError) as error:
        result["errors"].append(f"OVS attribution: {error}")

    expected_capture_count = 5
    if len(result["captures"]) != expected_capture_count:
        result["errors"].append(
            f"expected {expected_capture_count} usable capture points, found {len(result['captures'])}"
        )
    if not result["errors"]:
        result["status"] = "PASS"
    Path(args.output).write_text(json.dumps(result, indent=2) + "\n")
    print(f"packet-path-verdict={result['status']}")
    for error in result["errors"]:
        print(f"packet-path-incomplete={error}", file=sys.stderr)
    return 0 if result["status"] == "PASS" else 1


if __name__ == "__main__":
    raise SystemExit(main())
