#!/usr/bin/env python3
"""Verify runtime receipts and real GoBGP/FRR Type-2 observations."""

import ipaddress
import json
import re
import sys
from pathlib import Path


MAC_A = "0a:58:f0:01:00:0a"
MAC_B = "0a:58:f0:01:00:0b"
IP_A = "10.244.240.10"
IP_B = "10.244.240.11"
IP6_A = "2001:db8:77::10"
IP6_B = "2001:db8:77::11"
BASE = {(MAC_A, ""), (MAC_A, IP_A), (MAC_B, ""), (MAC_B, IP_B)}


def expected(variant):
    return {
        "base": BASE,
        "dual-a": BASE | {(MAC_A, IP6_A)},
        "dual-b": BASE | {(MAC_B, IP6_B)},
        "mac-a": BASE - {(MAC_A, IP_A)},
    }[variant]


def receipts(status, variant, vtep, frozen=False):
    result = status["Result"]
    assert not result.get("DryRun")
    assert result.get("OwnershipUnverified", False) is frozen
    assert status["SpeakerProcess"]["PID"] > 0
    applied = {}
    for row in result["Applied"]:
        ep = row["Endpoint"]
        ips = ep.get("IPAddresses") or []
        assert len(ips) <= 1
        ip = str(ipaddress.ip_address(ips[0])) if ips else ""
        key = (ep["MACAddress"].lower(), ip)
        assert key not in applied and bool(ep["MACOnly"]) == (not ip)
        assert ep["VNI"] == 1000 and ep["RouteDistinguisher"] == vtep + ":1000"
        assert ep["RouteTargets"] == ["64512:1000"] and ep["LocalVTEP"] == vtep
        assert ep["EndpointID"] and ep["Incarnation"] and row["PathUUID"]
        applied[key] = row["PathUUID"]
    assert set(applied) == expected(variant), applied
    assert len(set(applied.values())) == len(applied)
    return applied


def verify(status, gobgp, frr, variant, vtep, foreign_vtep, frozen=False):
    if frozen:
        assert "endpoint observation failed" in status["Error"], status["Error"]
    else:
        assert not status["Error"], status["Error"]
    applied = receipts(status, variant, vtep, frozen=frozen)
    rd = vtep + ":1000"
    local = set()
    foreign = set()
    for line in gobgp.splitlines():
        match = re.search(
            r"\[type:macadv\]\[rd:([^]]+)\]\[etag:(\d+)\]\[mac:([^]]+)\]"
            r"(?:\[ip:([^]]+)\])?\s+\[(\d+)\]\s+(\S+).*"
            r"\{Extcomms: ([^}]+)\}", line,
        )
        if "[type:macadv][rd:" + rd + "]" in line:
            assert match is not None, line
        if not match:
            continue
        route_rd, tag, mac, ip, vni, hop, communities = match.groups()
        ip = str(ipaddress.ip_address(ip)) if ip else ""
        key = (mac.lower(), ip)
        if route_rd == rd:
            assert tag == "0" and vni == "1000" and hop == vtep
            assert "[64512:1000]" in communities and "[VXLAN]" in communities
            assert key not in local
            local.add(key)
        if mac.lower() == "0a:58:f0:01:00:0c":
            assert route_rd == "198.18.0.3:1000" and tag == "0"
            assert vni == "1000" and hop == foreign_vtep
            assert "[64512:1000]" in communities and "[VXLAN]" in communities
            assert key not in foreign
            foreign.add(key)
    assert local == expected(variant), local
    assert foreign == {("0a:58:f0:01:00:0c", ""), ("0a:58:f0:01:00:0c", "10.244.240.12")}, foreign
    # FRR table records contain NLRI, next hop, and extended communities on
    # consecutive lines. GoBGP separately supplies the encoded label/VNI.
    seen = set()
    lines = frr.splitlines()
    for index, line in enumerate(lines):
        match = re.search(
            r"\[2\]:\[0\]:\[48\]:\[([^]]+)\](?::\[(32|128)\]:\[([^]]+)\])?\s+RD\s+(\S+)",
            line,
        )
        if "[2]:" in line and " RD " + rd in line:
            assert match is not None, line
        if not match or match.group(4) != rd:
            continue
        mac, length, ip, _ = match.groups()
        if ip:
            parsed = ipaddress.ip_address(ip)
            assert parsed.max_prefixlen == int(length)
            ip = str(parsed)
        key = (mac.lower(), ip or "")
        assert index + 2 < len(lines)
        assert lines[index + 1].split()[0] == vtep
        assert {"RT:64512:1000", "ET:8"} <= set(lines[index + 2].split())
        assert key not in seen
        seen.add(key)
    assert seen == expected(variant), seen
    return applied


def verify_kernel(fdb, neighbors4, neighbors6, variant):
    entries = neighbors4 + neighbors6
    observed = set()
    for row in entries:
        if row.get("lladdr", "").lower() in (MAC_A, MAC_B):
            assert row.get("state") == ["NOARP"] and "extern_learn" not in row, row
            observed.add((row["lladdr"].lower(), str(ipaddress.ip_address(row["dst"]))))
    assert observed == {(mac, ip) for mac, ip in expected(variant) if ip}, observed
    assert any(row.get("dst") == "10.244.240.12" and
               row.get("lladdr", "").lower() == "0a:58:f0:01:00:0c" for row in entries)
    for mac in (MAC_A, MAC_B):
        assert any(row.get("mac", "").lower() == mac for row in fdb), (mac, fdb)


def main():
    mode, variant, vtep, foreign_vtep, directory = sys.argv[1:6]
    reference = sys.argv[6] if len(sys.argv) > 6 else None
    root = Path(directory)
    status = json.loads((root / (mode + "-status.json")).read_text())
    paths = verify(status, (root / (mode + "-gobgp.txt")).read_text(),
                   (root / (mode + "-frr.txt")).read_text(),
                   variant, vtep, foreign_vtep, frozen=reference is not None)
    verify_kernel(*(json.loads((root / (mode + "-" + name + ".json")).read_text())
                    for name in ("fdb", "neighbors4", "neighbors6")), variant)
    if reference:
        previous = json.loads((root / (reference + "-status.json")).read_text())
        assert paths == receipts(previous, variant, vtep)
        assert status["SpeakerProcess"] == previous["SpeakerProcess"]
        def exact_rows(value):
            return sorted(json.dumps(row, sort_keys=True) for row in value["Result"]["Applied"])
        assert exact_rows(status) == exact_rows(previous)
    (root / (mode + "-verified.json")).write_text(json.dumps(
        {"variant": variant, "receipts": [{"mac": k[0], "ip": k[1], "uuid": v}
                                          for k, v in sorted(paths.items())]},
        indent=2) + "\n")


if __name__ == "__main__":
    main()
