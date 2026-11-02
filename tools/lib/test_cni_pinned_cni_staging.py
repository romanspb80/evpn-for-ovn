#!/usr/bin/env python3
"""Offline tests for the pinned CNI staging helper and its copy/Pod gate."""

import hashlib
import os
from pathlib import Path
import subprocess
import tempfile
import unittest


LIB = Path(__file__).resolve().parent
ROOT = LIB.parents[1]
HELPER = LIB / "cni-agent-image-test.sh"


def elf_fixture(machine=62, payload=b"fixture-cni"):
    header = bytearray(20)
    header[:4] = b"\x7fELF"
    header[4] = 2
    header[5] = 1
    header[6] = 1
    header[16:18] = (2).to_bytes(2, "little")
    header[18:20] = machine.to_bytes(2, "little")
    return bytes(header) + payload


class PinnedCNIStagingTests(unittest.TestCase):
    def run_staging(self, data=None, mode="valid", command_status=0,
                    expected_size=None, expected_sha=None, old_destination=None,
                    read_only_destination=False, source_missing=False):
        temporary = tempfile.TemporaryDirectory(prefix="cni-pinned-stage-")
        self.addCleanup(temporary.cleanup)
        root = Path(temporary.name)
        root.chmod(0o777 if read_only_destination else 0o755)
        source = root / "source"
        source.write_bytes(elf_fixture() if data is None else data)
        source.chmod(0o644)
        destination_dir = root / "destination"
        destination_dir.mkdir()
        destination = destination_dir / "evpn-cni"
        if old_destination is not None:
            destination.write_bytes(old_destination)
            destination.chmod(0o755)
        if read_only_destination:
            destination_dir.chmod(0o555)
        missing = root / "missing-source"
        expected = source.read_bytes()
        size = len(expected) if expected_size is None else expected_size
        digest = hashlib.sha256(expected).hexdigest() if expected_sha is None else expected_sha
        (root / "pod-created").touch()
        (root / "pod-created").unlink()

        body = r'''
set -euo pipefail
source "$MOCK_HELPER"
ag() {
  printf '%s\n' "$*" >> "$MOCK_ROOT/ag.log"
  case "$MOCK_MODE" in
    valid) cat "$MOCK_SOURCE";;
    empty) return 0;;
    partial) head -c 10 "$MOCK_SOURCE";;
    extraction-error) cat "$MOCK_SOURCE"; return "$MOCK_COMMAND_STATUS";;
    interrupt) kill -INT "$BASHPID"; return 0;;
    unreadable) echo "mock source permission denied" >&2; return 13;;
    missing) cat "$MOCK_MISSING_SOURCE";;
    *) echo "unexpected mock mode: $MOCK_MODE" >&2; return 99;;
  esac
}
docker() {
  printf '%s\n' "$*" >> "$MOCK_ROOT/worker-copy.log"
}
stage_pinned_cni_binary "$MOCK_DESTINATION" /usr/local/bin/evpn-cni \
  "$MOCK_EXPECTED_SIZE" "$MOCK_EXPECTED_SHA"
docker cp "$MOCK_DESTINATION" worker:/opt/cni/bin/evpn-cni.real
touch "$MOCK_ROOT/pod-created"
'''
        env = {
            **os.environ,
            "MOCK_HELPER": str(HELPER),
            "MOCK_ROOT": str(root),
            "MOCK_SOURCE": str(source),
            "MOCK_MISSING_SOURCE": str(missing),
            "MOCK_DESTINATION": str(destination),
            "MOCK_MODE": mode,
            "MOCK_COMMAND_STATUS": str(command_status),
            "MOCK_EXPECTED_SIZE": str(size),
            "MOCK_EXPECTED_SHA": digest,
        }
        preexec_fn = None
        if read_only_destination and os.geteuid() == 0:
            preexec_fn = lambda: os.setuid(65534)
        result = subprocess.run(
            ["bash", "-c", body],
            text=True,
            capture_output=True,
            env=env,
            preexec_fn=preexec_fn,
        )
        worker_calls = (root / "worker-copy.log").read_text().splitlines() \
            if (root / "worker-copy.log").exists() else []
        return result, root, destination, worker_calls

    def assert_no_temp_files(self, destination):
        self.assertEqual(list(destination.parent.glob(destination.name + ".tmp.*")), [])

    def test_exact_binary_is_published_executable_then_worker_copy_and_pod_are_allowed(self):
        data = elf_fixture()
        result, root, destination, worker_calls = self.run_staging(data=data)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(destination.read_bytes(), data)
        self.assertEqual(destination.stat().st_mode & 0o777, 0o755)
        self.assertEqual(len(worker_calls), 1)
        self.assertTrue((root / "pod-created").exists())
        self.assert_no_temp_files(destination)

    def test_zero_byte_successful_extraction_blocks_worker_and_pod(self):
        old = elf_fixture(payload=b"previous valid binary")
        result, root, destination, worker_calls = self.run_staging(
            mode="empty", old_destination=old
        )
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("artifact is empty", result.stderr)
        self.assertEqual(destination.read_bytes(), old)
        self.assertEqual(worker_calls, [])
        self.assertFalse((root / "pod-created").exists())
        self.assert_no_temp_files(destination)

    def test_truncated_successful_extraction_is_rejected(self):
        old = elf_fixture(payload=b"previous valid binary")
        result, root, destination, worker_calls = self.run_staging(
            mode="partial", old_destination=old
        )
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("size mismatch", result.stderr)
        self.assertEqual(destination.read_bytes(), old)
        self.assertEqual(worker_calls, [])
        self.assertFalse((root / "pod-created").exists())
        self.assert_no_temp_files(destination)

    def test_nonzero_extraction_status_is_preserved(self):
        old = elf_fixture(payload=b"previous valid binary")
        result, root, destination, worker_calls = self.run_staging(
            mode="extraction-error", command_status=37, old_destination=old
        )
        self.assertEqual(result.returncode, 37)
        self.assertIn("failed at extraction", result.stderr)
        self.assertEqual(destination.read_bytes(), old)
        self.assertEqual(worker_calls, [])
        self.assertFalse((root / "pod-created").exists())
        self.assert_no_temp_files(destination)

    def test_interrupted_extraction_preserves_status_and_cleans_temporary_file(self):
        old = elf_fixture(payload=b"previous valid binary")
        result, root, destination, worker_calls = self.run_staging(
            mode="interrupt", old_destination=old
        )
        self.assertEqual(result.returncode, 130)
        self.assertEqual(destination.read_bytes(), old)
        self.assertEqual(worker_calls, [])
        self.assertFalse((root / "pod-created").exists())
        self.assert_no_temp_files(destination)

    def test_missing_source_is_rejected_and_previous_destination_preserved(self):
        old = elf_fixture(payload=b"previous valid binary")
        result, root, destination, worker_calls = self.run_staging(
            mode="missing", old_destination=old, source_missing=True
        )
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("failed at extraction", result.stderr)
        self.assertEqual(destination.read_bytes(), old)
        self.assertEqual(worker_calls, [])
        self.assertFalse((root / "pod-created").exists())
        self.assert_no_temp_files(destination)

    def test_unreadable_source_status_is_preserved(self):
        result, root, destination, worker_calls = self.run_staging(
            mode="unreadable", old_destination=elf_fixture(payload=b"old")
        )
        self.assertEqual(result.returncode, 13)
        self.assertIn("mock source permission denied", result.stderr)
        self.assertEqual(worker_calls, [])
        self.assertFalse((root / "pod-created").exists())
        self.assert_no_temp_files(destination)

    def test_destination_temp_creation_failure_blocks_copy(self):
        result, root, destination, worker_calls = self.run_staging(
            read_only_destination=True
        )
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("temporary-file creation", result.stderr)
        self.assertFalse(destination.exists())
        self.assertEqual(worker_calls, [])
        self.assertFalse((root / "pod-created").exists())

    def test_old_valid_destination_survives_wrong_hash(self):
        old = elf_fixture(payload=b"old verified")
        result, root, destination, worker_calls = self.run_staging(
            old_destination=old, expected_sha="0" * 64
        )
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("SHA-256 mismatch", result.stderr)
        self.assertEqual(destination.read_bytes(), old)
        self.assertEqual(worker_calls, [])
        self.assertFalse((root / "pod-created").exists())
        self.assert_no_temp_files(destination)

    def test_wrong_expected_size_is_rejected(self):
        result, root, destination, worker_calls = self.run_staging(
            expected_size=len(elf_fixture()) + 1
        )
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("size mismatch", result.stderr)
        self.assertFalse(destination.exists())
        self.assertEqual(worker_calls, [])
        self.assertFalse((root / "pod-created").exists())
        self.assert_no_temp_files(destination)

    def test_wrong_elf_architecture_is_rejected_even_with_matching_hash(self):
        wrong_arch = elf_fixture(machine=183)
        result, root, destination, worker_calls = self.run_staging(data=wrong_arch)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("ELF64 little-endian x86-64", result.stderr)
        self.assertFalse(destination.exists())
        self.assertEqual(worker_calls, [])
        self.assertFalse((root / "pod-created").exists())
        self.assert_no_temp_files(destination)

    def test_non_elf_bytes_are_rejected(self):
        result, root, destination, worker_calls = self.run_staging(data=b"not an ELF")
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("ELF64 little-endian x86-64", result.stderr)
        self.assertFalse(destination.exists())
        self.assertEqual(worker_calls, [])
        self.assertFalse((root / "pod-created").exists())
        self.assert_no_temp_files(destination)

    def test_failed_staging_never_invokes_real_infrastructure(self):
        result, root, destination, worker_calls = self.run_staging(mode="empty")
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(worker_calls, [])
        self.assertFalse((root / "pod-created").exists())
        self.assertTrue((root / "ag.log").exists())
        self.assert_no_temp_files(destination)


if __name__ == "__main__":
    unittest.main()
