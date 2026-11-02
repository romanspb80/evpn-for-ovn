#!/usr/bin/env python3
"""Parse CNI invocation identity from environment and Kubernetes CNI_ARGS."""

import argparse
from pathlib import Path
import re
import sys


IDENTITY_KEYS = frozenset({
    "K8S_POD_INFRA_CONTAINER_ID",
    "K8S_POD_NAME",
    "K8S_POD_NAMESPACE",
    "K8S_POD_UID",
})


class CniIdentityError(ValueError):
    """Raised when CNI invocation identity evidence is ambiguous or invalid."""


def parse_environment(text):
    environment = {}
    for line_number, line in enumerate(text.splitlines(), start=1):
        if not line:
            continue
        if "=" not in line:
            raise CniIdentityError(f"environment line {line_number} is malformed")
        key, value = line.split("=", 1)
        if not re.fullmatch(r"[A-Za-z_][A-Za-z0-9_]*", key):
            raise CniIdentityError(f"environment line {line_number} has an invalid key")
        if key in environment:
            raise CniIdentityError(f"environment contains duplicate key {key}")
        environment[key] = value
    return environment


def parse_cni_args_identity(value):
    identity = {}
    if not value:
        raise CniIdentityError("CNI_ARGS is empty")
    for index, field in enumerate(value.split(";"), start=1):
        if not field or "=" not in field:
            raise CniIdentityError(f"CNI_ARGS field {index} is malformed")
        key, item = field.split("=", 1)
        if not key or any(character.isspace() for character in key):
            raise CniIdentityError(f"CNI_ARGS field {index} has an invalid key")
        if key in IDENTITY_KEYS and not item:
            raise CniIdentityError(f"CNI_ARGS identity value {key} is empty")
        if key in identity:
            if identity[key] != item:
                raise CniIdentityError(f"CNI_ARGS contains conflicting values for {key}")
            continue
        if key in IDENTITY_KEYS:
            identity[key] = item
    return identity


def parse_invocation_identity(environment):
    args_identity = {}
    if "CNI_ARGS" in environment:
        args_identity = parse_cni_args_identity(environment["CNI_ARGS"])

    identity = dict(args_identity)
    for key in IDENTITY_KEYS:
        if key not in environment:
            continue
        value = environment[key]
        if not value:
            raise CniIdentityError(f"environment identity value {key} is empty")
        if key in args_identity and args_identity[key] != value:
            raise CniIdentityError(f"conflicting standalone and CNI_ARGS values for {key}")
        identity[key] = value

    uid = identity.get("K8S_POD_UID")
    if not uid:
        raise CniIdentityError("CNI invocation is missing K8S_POD_UID")
    return identity


def read_call(call_dir):
    call = Path(call_dir)
    try:
        environment = parse_environment((call / "env").read_text(encoding="utf-8"))
        status = (call / "rc").read_text(encoding="utf-8").strip()
    except (OSError, UnicodeDecodeError) as error:
        raise CniIdentityError(f"CNI invocation evidence is incomplete: {error}") from error
    return environment, status


def select_successful_call(calls_dir, uid, command):
    calls = Path(calls_dir)
    try:
        candidates = sorted(path for path in calls.glob("call.*") if path.is_dir())
    except OSError as error:
        raise CniIdentityError(f"CNI invocation directory is unreadable: {error}") from error

    matches = []
    for call in candidates:
        environment, status = read_call(call)
        if environment.get("CNI_COMMAND") != command or status != "0":
            continue
        identity = parse_invocation_identity(environment)
        if identity["K8S_POD_UID"] == uid:
            matches.append(call)
    if len(matches) != 1:
        raise CniIdentityError(
            f"expected one successful {command} call for UID {uid}; found {len(matches)}"
        )
    return matches[0]


def main():
    parser = argparse.ArgumentParser()
    commands = parser.add_subparsers(dest="action", required=True)
    select = commands.add_parser("select-successful")
    select.add_argument("--calls-dir", required=True)
    select.add_argument("--uid", required=True)
    select.add_argument("--command", required=True)
    args = parser.parse_args()
    try:
        call = select_successful_call(args.calls_dir, args.uid, args.command)
    except CniIdentityError as error:
        print(str(error), file=sys.stderr)
        return 2
    print(call)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
