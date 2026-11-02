#!/usr/bin/env python3
"""Fail-closed command capture and OVSDB evidence validation for CNI diagnostics."""

import argparse
from datetime import datetime, timezone
import hashlib
import json
from pathlib import Path
import re
import subprocess
import sys
import time


def _utc_now():
    return datetime.now(timezone.utc).isoformat()


def _redact(argv):
    redacted = []
    redact_next = False
    for arg in argv:
        if redact_next:
            redacted.append("<redacted>")
            redact_next = False
            continue
        if re.search(r"(?:password|passwd|token|secret|credential|private[-_]?key)", arg, re.I):
            if "=" in arg:
                redacted.append(arg.split("=", 1)[0] + "=<redacted>")
            else:
                redacted.append(arg)
                redact_next = True
            continue
        redacted.append(re.sub(r"(://[^:/@\s]+:)[^@\s]+@", r"\1<redacted>@", arg))
    return redacted


def _is_context_error(stderr):
    text = stderr.decode("utf-8", errors="replace").lower()
    markers = (
        "pod does not exist",
        "pods \"",
        "container not found",
        "no such container",
        "unable to upgrade connection",
        "unable to connect to the server",
        "context deadline exceeded",
        "the connection to the server",
        "not found: unknown",
    )
    return any(marker in text for marker in markers)


def _classify(returncode, stdout, stderr, expectation, command_found=True):
    if not command_found:
        return "COMMAND_NOT_FOUND", "command executable was not found"
    if returncode in (124, 137):
        return "COMMAND_TIMEOUT", f"command exited with timeout status {returncode}"
    if returncode != 0:
        if _is_context_error(stderr):
            return "EXEC_CONTEXT_FAILED", "execution target or transport was unavailable"
        text = stderr.decode("utf-8", errors="replace").lower()
        if any(marker in text for marker in ("unknown column", "no column named", "not a column")):
            return "UNSUPPORTED_FIELD", "database rejected a requested field"
        return "QUERY_FAILED", f"command exited with status {returncode}"
    if expectation == "required" and not stdout.strip():
        return "EMPTY_REQUIRED_OUTPUT", "successful command returned no required output"
    if expectation == "json":
        if not stdout.strip():
            return "EMPTY_REQUIRED_OUTPUT", "successful command returned no JSON document"
        try:
            json.loads(stdout)
        except (json.JSONDecodeError, UnicodeDecodeError) as error:
            return "PARSE_FAILED", f"JSON output is invalid: {error}"
    return "SUCCESS", "command and output contract passed"


def _digest(path):
    data = path.read_bytes()
    return {"bytes": len(data), "sha256": hashlib.sha256(data).hexdigest()}


def run_command(args):
    stdout_path = Path(args.stdout)
    stderr_path = Path(args.stderr)
    metadata_path = Path(args.metadata)
    status_path = Path(args.exit_status)
    paths = (stdout_path, stderr_path, metadata_path, status_path)
    start_time = _utc_now()
    start_ns = time.monotonic_ns()
    command_found = True
    try:
        if any(path.exists() for path in paths):
            raise FileExistsError("refusing to overwrite an existing diagnostic artifact")
        for path in paths:
            path.parent.mkdir(parents=True, exist_ok=True)
        with stdout_path.open("xb") as stdout_file, stderr_path.open("xb") as stderr_file:
            try:
                completed = subprocess.run(
                    args.command,
                    stdout=subprocess.PIPE,
                    stderr=subprocess.PIPE,
                    check=False,
                )
                returncode = completed.returncode
                stdout_file.write(completed.stdout)
                stderr_file.write(completed.stderr)
            except FileNotFoundError as error:
                command_found = False
                returncode = 127
                stderr_file.write(f"{error}\n".encode())
    except (OSError, ValueError) as error:
        print(
            json.dumps({
                "classification": "COLLECTOR_INTERNAL_ERROR",
                "detail": str(error),
            }),
            file=sys.stderr,
        )
        return 2
    end_ns = time.monotonic_ns()
    end_time = _utc_now()
    stdout_bytes = stdout_path.read_bytes()
    stderr_bytes = stderr_path.read_bytes()
    classification, detail = _classify(
        returncode, stdout_bytes, stderr_bytes, args.expectation, command_found
    )
    metadata = {
        "commandId": args.command_id,
        "executionContext": args.context,
        "argv": _redact(args.command),
        "startedAt": start_time,
        "endedAt": end_time,
        "durationNanoseconds": end_ns - start_ns,
        "exitStatus": returncode,
        "expectation": args.expectation,
        "timeout": classification == "COMMAND_TIMEOUT",
        "classification": classification,
        "detail": detail,
        "stdoutFile": str(stdout_path),
        "stderrFile": str(stderr_path),
        "stdout": _digest(stdout_path),
        "stderr": _digest(stderr_path),
    }
    try:
        with metadata_path.open("x", encoding="utf-8") as output:
            json.dump(metadata, output, indent=2, sort_keys=True)
            output.write("\n")
        with status_path.open("x", encoding="utf-8") as output:
            output.write(f"{returncode}\n")
    except OSError as error:
        print(
            json.dumps({
                "classification": "COLLECTOR_INTERNAL_ERROR",
                "detail": f"writing command metadata failed: {error}",
            }),
            file=sys.stderr,
        )
        return 2
    if classification != "SUCCESS":
        if args.expectation == "optional":
            print(
                f"{args.command_id}: optional evidence {classification}: {detail}",
                file=sys.stderr,
            )
            return 0
        print(
            f"{args.command_id}: {classification}: {detail}",
            file=sys.stderr,
        )
        return 1
    return 0


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


def _table_rows(document):
    if not isinstance(document, dict):
        raise ValueError("OVSDB result must be a JSON object")
    headings = document.get("headings")
    data = document.get("data")
    if not isinstance(headings, list) or not headings:
        raise ValueError("OVSDB JSON has no column headings")
    if not isinstance(data, list):
        raise ValueError("OVSDB JSON has no row array")
    rows = []
    for row in data:
        if not isinstance(row, list) or len(row) != len(headings):
            raise ValueError("OVSDB row length does not match column headings")
        rows.append({key: _normalize(value) for key, value in zip(headings, row)})
    return rows


def _read_rows(path, required_columns=()):
    document = json.loads(Path(path).read_text(encoding="utf-8"))
    headings = document.get("headings") if isinstance(document, dict) else None
    if not isinstance(headings, list):
        raise ValueError("OVSDB JSON has no column headings")
    missing = sorted(set(required_columns) - set(headings))
    if missing:
        raise ValueError(f"OVSDB JSON is missing required columns: {', '.join(missing)}")
    return _table_rows(document)


def _uuid_members(value):
    if isinstance(value, str):
        return [value]
    return value if isinstance(value, list) else []


def verify_control(args):
    if bool(args.name_column) == bool(args.map_column):
        return _failure(
            "COLLECTOR_INTERNAL_ERROR",
            "provide exactly one positive-control identity selector",
        )
    required_columns = [args.uuid_column] if args.uuid_column else []
    if args.name_column:
        required_columns.append(args.name_column)
    if args.map_column:
        required_columns.append(args.map_column)
    if args.member_column:
        required_columns.append(args.member_column)
    try:
        rows = _read_rows(args.input, required_columns)
    except (OSError, UnicodeDecodeError, json.JSONDecodeError, ValueError) as error:
        return _failure("PARSE_FAILED", str(error))
    candidates = rows
    if args.name_column:
        candidates = [row for row in candidates if row.get(args.name_column) == args.name]
    if args.map_column:
        candidates = [
            row for row in candidates
            if isinstance(row.get(args.map_column), dict)
            and row[args.map_column].get(args.map_key) == args.map_value
        ]
    if len(candidates) != 1:
        return _failure(
            "OBJECT_ABSENT" if not candidates else "PARSE_FAILED",
            f"positive control expected exactly one matching row; found {len(candidates)}",
        )
    row = candidates[0]
    if args.uuid_column and not row.get(args.uuid_column):
        return _failure("PARSE_FAILED", f"positive control has no {args.uuid_column}")
    if args.member_column and args.member_uuid:
        members = _uuid_members(row.get(args.member_column))
        if args.member_uuid not in members:
            return _failure("OBJECT_ABSENT", "positive-control membership was not present")
    print(json.dumps({"classification": "SUCCESS", "row": row}, sort_keys=True))
    return 0


def classify_object(args):
    if bool(args.key_column) == bool(args.map_column):
        return _failure("COLLECTOR_INTERNAL_ERROR", "provide exactly one object identity selector")
    if args.key_column and args.key_value is None:
        return _failure("COLLECTOR_INTERNAL_ERROR", "key-value is required with key-column")
    if args.map_column and (args.map_key is None or args.map_value is None):
        return _failure("COLLECTOR_INTERNAL_ERROR", "map-key and map-value are required with map-column")
    try:
        identity_column = args.key_column or args.map_column
        rows = _read_rows(args.input, [identity_column])
    except (OSError, UnicodeDecodeError, json.JSONDecodeError, ValueError) as error:
        return _failure("PARSE_FAILED", str(error))
    if args.key_column:
        matches = [row for row in rows if row.get(args.key_column) == args.key_value]
        query = {args.key_column: args.key_value}
    else:
        matches = [
            row for row in rows
            if isinstance(row.get(args.map_column), dict)
            and row[args.map_column].get(args.map_key) == args.map_value
        ]
        query = {args.map_column: {args.map_key: args.map_value}}
    if len(matches) > 1:
        return _failure("PARSE_FAILED", f"duplicate {args.object_kind} identity")
    result = {
        "classification": "OBJECT_PRESENT" if matches else "OBJECT_ABSENT",
        "objectKind": args.object_kind,
        "query": query,
        "matches": matches,
    }
    print(json.dumps(result, indent=2, sort_keys=True))
    expected_present = args.expect == "present"
    if args.expect and bool(matches) != expected_present:
        return 1
    return 0


def verify_ovs_attachment(args):
    try:
        interfaces = _read_rows(args.interfaces, ["_uuid", "name", "external_ids"])
        ports = _read_rows(args.ports, ["_uuid", "interfaces"])
        bridges = _read_rows(args.bridges, ["_uuid", "name", "ports"])
    except (OSError, UnicodeDecodeError, json.JSONDecodeError, ValueError) as error:
        return _failure("PARSE_FAILED", str(error))
    matches = [
        row for row in interfaces
        if isinstance(row.get("external_ids"), dict)
        and row["external_ids"].get("iface-id") == args.logical_port
    ]
    result = {
        "objectKind": "OVS_Interface",
        "query": {"external_ids": {"iface-id": args.logical_port}},
        "classification": "OBJECT_ABSENT",
        "interface": None,
        "port": None,
        "bridge": None,
    }
    if len(matches) > 1:
        return _failure("PARSE_FAILED", "duplicate OVS interfaces have the target iface-id")
    if matches:
        interface = matches[0]
        result["classification"] = "OBJECT_PRESENT"
        result["interface"] = interface
        interface_uuid = interface.get("_uuid")
        matching_ports = [
            row for row in ports
            if interface_uuid in _uuid_members(row.get("interfaces"))
        ]
        if len(matching_ports) != 1:
            return _failure(
                "PARSE_FAILED",
                f"expected one OVS Port containing interface UUID; found {len(matching_ports)}",
            )
        port = matching_ports[0]
        result["port"] = port
        matching_bridges = [
            row for row in bridges
            if row.get("name") == args.bridge
            and port.get("_uuid") in _uuid_members(row.get("ports"))
        ]
        if len(matching_bridges) != 1:
            return _failure(
                "OBJECT_ABSENT",
                f"OVS Port is not uniquely attached to expected bridge {args.bridge}",
            )
        result["bridge"] = matching_bridges[0]
    print(json.dumps(result, indent=2, sort_keys=True))
    if args.expect:
        expected_present = args.expect == "present"
        if bool(matches) != expected_present:
            return 1
    return 0


def verify_processes(args):
    try:
        text = Path(args.input).read_text(encoding="utf-8", errors="strict")
    except (OSError, UnicodeDecodeError) as error:
        return _failure("PARSE_FAILED", str(error))
    missing = [name for name in args.process if name not in text]
    if missing:
        return _failure("OBJECT_ABSENT", f"processes not observed: {', '.join(missing)}")
    print(json.dumps({"classification": "SUCCESS", "processes": args.process}))
    return 0


def _failure(classification, detail):
    print(json.dumps({"classification": classification, "detail": detail}), file=sys.stderr)
    return 1


def main():
    parser = argparse.ArgumentParser()
    commands = parser.add_subparsers(dest="action", required=True)
    run = commands.add_parser("run")
    run.add_argument("--command-id", required=True)
    run.add_argument("--context", required=True)
    run.add_argument("--expectation", choices=("required", "json", "optional", "status"), required=True)
    run.add_argument("--stdout", required=True)
    run.add_argument("--stderr", required=True)
    run.add_argument("--metadata", required=True)
    run.add_argument("--exit-status", required=True)
    run.add_argument("command", nargs=argparse.REMAINDER)
    run.set_defaults(func=run_command)

    control = commands.add_parser("verify-control")
    control.add_argument("--input", required=True)
    control.add_argument("--name-column")
    control.add_argument("--name")
    control.add_argument("--uuid-column")
    control.add_argument("--map-column")
    control.add_argument("--map-key")
    control.add_argument("--map-value")
    control.add_argument("--member-column")
    control.add_argument("--member-uuid")
    control.set_defaults(func=verify_control)

    absent = commands.add_parser("classify-object")
    absent.add_argument("--input", required=True)
    absent.add_argument("--object-kind", required=True)
    absent.add_argument("--key-column")
    absent.add_argument("--key-value")
    absent.add_argument("--map-column")
    absent.add_argument("--map-key")
    absent.add_argument("--map-value")
    absent.add_argument("--expect", choices=("present", "absent"))
    absent.set_defaults(func=classify_object)

    attachment = commands.add_parser("verify-ovs-attachment")
    attachment.add_argument("--interfaces", required=True)
    attachment.add_argument("--ports", required=True)
    attachment.add_argument("--bridges", required=True)
    attachment.add_argument("--logical-port", required=True)
    attachment.add_argument("--bridge", default="br-int")
    attachment.add_argument("--expect", choices=("present", "absent"))
    attachment.set_defaults(func=verify_ovs_attachment)

    processes = commands.add_parser("verify-processes")
    processes.add_argument("--input", required=True)
    processes.add_argument("--process", action="append", required=True)
    processes.set_defaults(func=verify_processes)

    args = parser.parse_args()
    if args.action == "run":
        if args.command and args.command[0] == "--":
            args.command = args.command[1:]
        if not args.command:
            parser.error("run requires a command after --")
    return args.func(args)


if __name__ == "__main__":
    raise SystemExit(main())
