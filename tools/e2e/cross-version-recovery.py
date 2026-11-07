#!/usr/bin/env python3
"""Run test-only durable recovery helpers across independently compiled binaries."""

import argparse
import hashlib
import json
import os
from pathlib import Path
import selectors
import shutil
import subprocess
import tempfile


CASES = {
    "ownership": ("TestCrossVersionJournal", [
        "ack", "pending", "truncated", "checksum", "unknown-envelope",
        "unknown-record", "extra-value", "foreign", "overlap",
        "crash-before-rename", "crash-after-sync",
    ]),
    "nodeagent": ("TestCrossVersionCNI", [
        "pending", "active", "rejected", "incomplete", "identity-conflict",
        "uuid-replaced", "unacknowledged", "foreign-domain",
    ]),
    "evpn": ("TestCrossVersionEVPN", [
        "export-ack", "export-crash-ack", "export-ambiguous",
        "export-incarnation", "export-domain", "export-foreign",
        "import-ack", "import-crash-ack", "import-crash-mutation", "import-ambiguous", "import-quarantine",
        "import-foreign", "import-domain",
    ]),
}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--old", required=True, type=Path)
    parser.add_argument("--new", required=True, type=Path)
    parser.add_argument("--evidence", required=True, type=Path)
    args = parser.parse_args()
    if os.geteuid() != 0:
        parser.error("root-owned executable fixtures required")
    args.evidence.mkdir(mode=0o700, parents=True, exist_ok=False)
    results = []
    binaries = {}
    for version, directory in (("go1.25.0", args.old), ("go1.27.3", args.new)):
        for package in CASES:
            binary = (directory / (package + ".test")).resolve(strict=True)
            st = binary.stat()
            if st.st_uid != 0 or st.st_mode & 0o022:
                raise RuntimeError(f"unsafe test executable ownership: {binary}")
            binaries[version, package] = binary
            digest = hashlib.sha256(binary.read_bytes()).hexdigest()
            results.append({"binary": str(binary), "compiler": version, "sha256": digest})
    # /tmp must be executable; the caller verifies mount policy before launch.
    root = Path(tempfile.mkdtemp(prefix="x", dir="/tmp"))
    root_stat = root.lstat()
    if root_stat.st_uid != 0 or root_stat.st_mode & 0o077 or Path("/tmp").is_symlink():
        raise RuntimeError("unsafe fixture root or symlink parent")
    env = dict(os.environ, TMPDIR=str(root), GOTMPDIR=str(root), GOTOOLCHAIN="local")

    def command(binary, test):
        return [str(binary), "-test.v", "-test.timeout=30s", "-test.run=^" + test + "$"]

    def run(binary, test, mode, case, directory, label, expected=0):
        child_env = dict(env, EVPN_CROSS_MODE=mode, EVPN_CROSS_CASE=case, EVPN_CROSS_DIR=str(directory))
        p = subprocess.run(command(binary, test), env=child_env, capture_output=True, text=True, timeout=40)
        (args.evidence / (label + ".log")).write_text(p.stdout + p.stderr)
        version = next(version for (version, _), path in binaries.items() if path == binary)
        marker = f"COMPILER {version} MODE {mode} CASE {case}"
        if p.returncode != expected or marker not in p.stdout or "--- SKIP:" in p.stdout:
            raise RuntimeError(f"{label}: exit={p.returncode}, expected={expected}\n{p.stdout}{p.stderr}")
        results.append({"process": label, "exit": p.returncode, "expected": expected, "compiler": version})

    try:
        for direction, writer_version, reader_version in (
            ("upgrade", "go1.25.0", "go1.27.3"),
            ("rollback", "go1.27.3", "go1.25.0"),
        ):
            for package, (test, cases) in CASES.items():
                writer, reader = binaries[writer_version, package], binaries[reader_version, package]
                if writer == reader:
                    raise RuntimeError("writer and reader must be distinct binaries")
                for case in cases:
                    fixture = root / f"{direction}-{package}-{case}"
                    fixture.mkdir(mode=0o700)
                    label = f"{direction}-{package}-{case}"
                    if case == "overlap":
                        child_env = dict(env, EVPN_CROSS_MODE="write", EVPN_CROSS_CASE=case, EVPN_CROSS_DIR=str(fixture))
                        p = subprocess.Popen(command(writer, test), env=child_env, stdin=subprocess.PIPE,
                                             stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=False)
                        output = bytearray()
                        try:
                            with selectors.DefaultSelector() as sel:
                                sel.register(p.stdout, selectors.EVENT_READ)
                                while b"\nREADY\n" not in output:
                                    if not sel.select(15):
                                        raise RuntimeError("writer readiness timeout")
                                    data = os.read(p.stdout.fileno(), 4096)
                                    if not data:
                                        raise RuntimeError(f"writer exited before ready: {output!r}")
                                    output.extend(data)
                            run(reader, test, "read", case, fixture, label + "-read")
                            p.stdin.write(b"x")
                            p.stdin.flush()
                            rest, _ = p.communicate(timeout=15)
                            output.extend(rest)
                            if p.returncode != 0 or f"COMPILER {writer_version}".encode() not in output:
                                raise RuntimeError(f"overlap writer failed: {output!r}")
                            (args.evidence / (label + "-write.log")).write_bytes(output)
                            results.append({"process": label + "-write", "exit": 0, "compiler": writer_version})
                        finally:
                            if p.poll() is None:
                                p.kill()
                                p.wait()
                    else:
                        crash = case in ("crash-before-rename", "crash-after-sync", "export-ambiguous",
                                         "export-crash-ack", "import-crash-mutation", "import-crash-ack")
                        run(writer, test, "write", case, fixture, label + "-write", 42 if crash else 0)
                        run(reader, test, "read", case, fixture, label + "-read")
                    results.append({"case": label, "status": "PASS"})
                    print("PASS", label, flush=True)
    finally:
        shutil.rmtree(root)
        (args.evidence / "results.json").write_text(json.dumps(results, indent=2) + "\n")
        (args.evidence / "cleanup.json").write_text(json.dumps({"fixture_root": str(root), "removed": not root.exists()}) + "\n")


if __name__ == "__main__":
    main()
