#!/usr/bin/env python3
"""Classify an OVN Southbound Port_Binding snapshot for one replacement LSP."""

import argparse
from datetime import datetime, timezone
import json
from pathlib import Path
import sys


def _normalize(value):
    if not isinstance(value, list) or not value:
        return value
    tag = value[0]
    if tag in ("uuid", "named-uuid"):
        return value[1]
    if tag == "set":
        return [_normalize(item) for item in value[1]]
    if tag == "map":
        return {_normalize(key): _normalize(item) for key, item in value[1]}
    return value


def _rows(table):
    headings = table.get("headings")
    if not headings:
        raise ValueError("OVSDB JSON has no column headings")
    return [
        {heading: _normalize(value) for heading, value in zip(headings, row)}
        for row in table.get("data", [])
    ]


def classify_binding(document, logical_port, local_chassis, expected_datapath):
    matches = [
        row for row in _rows(document)
        if row.get("logical_port") == logical_port
    ]
    if not matches:
        return "ABSENT", matches
    if len(matches) != 1:
        return "DUPLICATE", matches
    binding = matches[0]
    if binding.get("datapath") != expected_datapath:
        return "WRONG_DATAPATH", matches
    if binding.get("type", ""):
        return "WRONG_TYPE", matches
    chassis = binding.get("chassis")
    if not chassis:
        return "UNBOUND", matches
    if chassis != local_chassis:
        return "WRONG_CHASSIS", matches
    if binding.get("up") is not True:
        return "DOWN", matches
    return "BOUND", matches


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--logical-port", required=True)
    parser.add_argument("--local-chassis", required=True)
    parser.add_argument("--expected-datapath", required=True)
    parser.add_argument("--timeline", type=Path, required=True)
    args = parser.parse_args()

    document = json.load(sys.stdin)
    state, matches = classify_binding(
        document, args.logical_port, args.local_chassis, args.expected_datapath
    )
    record = {
        "observedAt": datetime.now(timezone.utc).isoformat(),
        "logicalPort": args.logical_port,
        "state": state,
        "bindings": matches,
    }
    with args.timeline.open("a", encoding="utf-8") as timeline:
        timeline.write(json.dumps(record, sort_keys=True) + "\n")
    print(state)


if __name__ == "__main__":
    main()
