#!/usr/bin/env python3
"""Run actual worker scripts and wrapper in a temporary filesystem via Docker mocks."""

import hashlib
import json
import os
from pathlib import Path
import shlex
import subprocess
import tempfile
import unittest


LIB = Path(__file__).resolve().parent
ROOT = LIB.parents[1]
HELPER = LIB / "cni-agent-image-test.sh"
WORKER_SCRIPT = LIB / "cni-worker-artifact.sh"
RUNNER = ROOT / "tools/run-cni-mvp-2.4b-multus.sh"
VERSION = b'{"cniVersion":"1.1.0","supportedVersions":["1.0.0"]}\n'

DOCKER_MOCK = r'''#!/usr/bin/env python3
import json, os, shutil, subprocess, sys
from pathlib import Path
root = Path(os.environ["MOCK_ROOT"])
args = sys.argv[1:]
mode = os.environ.get("MOCK_MODE", "valid")
with (root / "calls").open("a") as log:
    log.write(json.dumps(args) + "\n")
def mapped(value):
    return value.replace("/opt/cni/bin", str(root / "worker"))
if args[:1] == ["cp"]:
    assert len(args) == 3 and args[2].startswith("worker:/opt/cni/bin/.evpn-cni-stage.")
    target = Path(mapped(args[2].split(":", 1)[1]))
    shutil.copyfile(args[1], target)
    if mode == "copy-fail":
        target.write_bytes(b"partial")
        sys.exit(37)
    if mode == "zero": target.write_bytes(b"")
    if mode == "truncated": target.write_bytes(target.read_bytes()[:10])
    if mode == "wrong-sha":
        data = bytearray(target.read_bytes()); data[-1] ^= 1; target.write_bytes(data)
    if mode == "wrong-arch":
        data = bytearray(target.read_bytes()); data[18:20] = b"\xb7\x00"; target.write_bytes(data)
    if mode == "symlink":
        target.unlink(); target.symlink_to(root / "host")
    if mode == "raced-destination":
        (root / "worker/evpn-cni.real").write_bytes(b"foreign executable")
    sys.exit(0)
if args[:5] == ["exec", "-i", "worker", "sh", "-s"]:
    action = args[6]
    script = sys.stdin.read()
    if mode == "wrong-temp" and action == "prepare":
        print("/unowned/wrong-directory"); sys.exit(0)
    env = dict(os.environ)
    env["PATH"] = str(root / "worker-tools") + ":" + env["PATH"]
    result = subprocess.run(
        ["/bin/sh", "-s", "--", *[mapped(arg) for arg in args[6:]]],
        input=mapped(script), text=True, env=env,
        stdout=subprocess.PIPE if action == "prepare" else None)
    if action == "prepare":
        print(result.stdout.replace(str(root / "worker"), "/opt/cni/bin"), end="")
    sys.exit(result.returncode)
if args[:5] == ["exec", "-i", "worker", "env", "-i"]:
    assert args[-1] == "/opt/cni/bin/evpn-cni"
    assert "CNI_COMMAND=VERSION" in args and "CNI_PATH=/opt/cni/bin" in args
    env = dict(arg.split("=", 1) for arg in args[5:-1])
    env["MOCK_ROOT"] = str(root)
    env["MOCK_VERSION_RC"] = os.environ.get("MOCK_VERSION_RC", "0")
    env["MOCK_VERSION_STDERR"] = os.environ.get("MOCK_VERSION_STDERR", "")
    result = subprocess.run([mapped(args[-1])], env=env, input=sys.stdin.buffer.read())
    sys.exit(result.returncode)
raise SystemExit("UNEXPECTED infrastructure operation: " + repr(args))
'''


class WorkerStagingTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory(prefix="cni-worker-offline-")
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name)
        self.worker = self.root / "worker"
        self.worker.mkdir()
        self.tools = self.root / "worker-tools"
        self.tools.mkdir()
        self.bin = self.root / "bin"
        self.bin.mkdir()
        docker = self.bin / "docker"
        docker.write_text(DOCKER_MOCK)
        docker.chmod(0o755)
        for name in ("kind", "kubectl", "sudo", "systemctl", "tcpdump"):
            denied = self.bin / name
            denied.write_text("#!/bin/sh\necho 'UNEXPECTED infrastructure command' >&2\nexit 99\n")
            denied.chmod(0o755)
        self.data = b"\x7fELF\x02\x01\x01" + b"\0" * 9 + b"\x02\0\x3e\0" + b"mock-cni"
        self.host = self.root / "host"
        self.host.write_bytes(self.data)
        self.host.chmod(0o755)
        self.size = len(self.data)
        self.sha = hashlib.sha256(self.data).hexdigest()
        self.env = {
            **os.environ, "MOCK_ROOT": str(self.root),
            "PATH": str(self.bin) + ":" + os.environ["PATH"],
        }

    def shell(self, body, mode="valid", **env):
        result = subprocess.run(
            ["bash", "-c", f"set -euo pipefail\nsource {shlex.quote(str(HELPER))}\n" + body],
            env={**self.env, "MOCK_MODE": mode, **env}, capture_output=True, text=True,
        )
        self.assertNotIn("UNEXPECTED", result.stderr)
        return result

    def stage(self, mode="valid", destination="/opt/cni/bin/evpn-cni.real"):
        return self.shell(
            f"stage_worker_cni_file worker {shlex.quote(str(self.host))} "
            f"{shlex.quote(destination)} {self.size} {self.sha} real\n"
            f"touch {shlex.quote(str(self.root / 'pod-created'))}", mode,
        )

    def assert_blocked(self, result):
        self.assertNotEqual(result.returncode, 0)
        self.assertFalse((self.root / "pod-created").exists())
        self.assertEqual(list(self.worker.glob(".evpn-cni-stage.*")), [])

    def verify_final(self):
        return self.shell(
            "docker exec -i worker sh -s -- verify /opt/cni/bin/evpn-cni.real "
            f"real {self.size} {self.sha} < {shlex.quote(str(WORKER_SCRIPT))}"
        )

    def test_correct_binary_is_verified_and_published_before_pod(self):
        result = self.stage()
        self.assertEqual(result.returncode, 0, result.stderr)
        final = self.worker / "evpn-cni.real"
        self.assertEqual(final.read_bytes(), self.data)
        self.assertEqual(final.stat().st_mode & 0o777, 0o755)
        self.assertTrue((self.root / "pod-created").exists())
        self.assertEqual(list(self.worker.glob(".evpn-cni-stage.*")), [])
        self.assertIn(f"path={final}", result.stdout)
        calls = [json.loads(line) for line in (self.root / "calls").read_text().splitlines()]
        self.assertEqual([call[0] for call in calls], ["exec", "cp", "exec", "exec"])
        self.assertNotEqual(calls[1][2], "worker:/opt/cni/bin/evpn-cni.real")
        self.assertEqual(calls[2][6], "publish")
        self.assertEqual(calls[3][6], "cleanup")

    def test_failed_worker_copy_does_not_publish_and_preserves_status(self):
        result = self.stage("copy-fail")
        self.assertEqual(result.returncode, 37, result.stderr)
        self.assert_blocked(result)
        self.assertFalse((self.worker / "evpn-cni.real").exists())

    def test_zero_byte_copy_is_rejected(self):
        result = self.stage("zero")
        self.assert_blocked(result)
        self.assertIn("size mismatch", result.stderr)

    def test_truncated_copy_is_rejected(self):
        result = self.stage("truncated")
        self.assert_blocked(result)
        self.assertIn("size mismatch", result.stderr)

    def test_wrong_hash_is_rejected(self):
        result = self.stage("wrong-sha")
        self.assert_blocked(result)
        self.assertIn("SHA-256 mismatch", result.stderr)

    def test_symlink_copy_is_rejected_without_deleting_target(self):
        result = self.stage("symlink")
        self.assert_blocked(result)
        self.assertIn("non-symlink", result.stderr)
        self.assertEqual(self.host.read_bytes(), self.data)

    def test_wrong_architecture_even_with_matching_hash_is_rejected(self):
        data = bytearray(self.data)
        data[18:20] = b"\xb7\0"
        self.host.write_bytes(data)
        self.sha = hashlib.sha256(data).hexdigest()
        result = self.stage()
        self.assert_blocked(result)
        self.assertIn("ELF64", result.stderr)

    def test_wrong_destination_path_is_rejected_before_copy(self):
        result = self.stage(destination="/opt/cni/bin/wrong")
        self.assert_blocked(result)
        self.assertIn("unexpected destination", result.stderr)
        self.assertNotIn('"cp"', (self.root / "calls").read_text())

    def test_unknown_existing_destination_is_untouched(self):
        final = self.worker / "evpn-cni.real"
        final.write_bytes(b"foreign")
        result = self.stage()
        self.assert_blocked(result)
        self.assertEqual(final.read_bytes(), b"foreign")
        self.assertIn("unknown ownership", result.stderr)

    def test_dangling_destination_symlink_is_preserved(self):
        final = self.worker / "evpn-cni.real"
        final.symlink_to(self.root / "unrelated")
        result = self.stage()
        self.assert_blocked(result)
        self.assertTrue(final.is_symlink())

    def test_destination_created_during_copy_is_not_overwritten(self):
        result = self.stage("raced-destination")
        self.assert_blocked(result)
        self.assertEqual((self.worker / "evpn-cni.real").read_bytes(), b"foreign executable")

    def test_unexpected_temp_path_blocks_copy_and_cleanup(self):
        result = self.stage("wrong-temp")
        self.assert_blocked(result)
        self.assertIn("unexpected temporary path", result.stderr)
        self.assertNotIn('"cp"', (self.root / "calls").read_text())

    def test_nonexecutable_published_file_is_rejected(self):
        final = self.worker / "evpn-cni.real"
        final.write_bytes(self.data)
        final.chmod(0o644)
        result = self.verify_final()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("mode-0755", result.stderr)

    def test_missing_published_file_is_rejected(self):
        result = self.verify_final()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("non-symlink", result.stderr)

    def test_failed_chmod_prevents_publication(self):
        chmod = self.tools / "chmod"
        chmod.write_text("#!/bin/sh\nexit 43\n")
        chmod.chmod(0o755)
        result = self.stage()
        self.assert_blocked(result)
        self.assertEqual(result.returncode, 43)

    def test_published_replacement_is_rechecked(self):
        mv = self.tools / "mv"
        mv.write_text('#!/bin/sh\n/bin/mv "$@"\nfor last; do :; done\n: > "$last"\n')
        mv.chmod(0o755)
        result = self.stage()
        self.assert_blocked(result)
        self.assertIn("size mismatch", result.stderr)

    def test_published_permissions_are_rechecked(self):
        mv = self.tools / "mv"
        mv.write_text('#!/bin/sh\n/bin/mv "$@"\nfor last; do :; done\n/bin/chmod 644 "$last"\n')
        mv.chmod(0o755)
        result = self.stage()
        self.assert_blocked(result)
        self.assertIn("mode-0755", result.stderr)

    def test_atomic_publication_does_not_replace_a_raced_destination(self):
        mv = self.tools / "mv"
        mv.write_text('#!/bin/sh\nfor last; do :; done\nprintf foreign > "$last"\n/bin/mv "$@"\n')
        mv.chmod(0o755)
        result = self.stage()
        self.assert_blocked(result)
        self.assertTrue("destination appeared" in result.stderr or "publication failed" in result.stderr)
        self.assertEqual((self.worker / "evpn-cni.real").read_bytes(), b"foreign")

    def test_failed_rename_does_not_publish(self):
        mv = self.tools / "mv"
        mv.write_text("#!/bin/sh\nexit 45\n")
        mv.chmod(0o755)
        result = self.stage()
        self.assert_blocked(result)
        self.assertEqual(result.returncode, 45)
        self.assertFalse((self.worker / "evpn-cni.real").exists())

    def test_cross_filesystem_publication_is_rejected(self):
        stat = self.tools / "stat"
        stat.write_text(
            '#!/bin/sh\nif [ "$2" = "%d" ]; then\n'
            '  for last; do :; done\n'
            '  case "$last" in *.evpn-cni-stage.*) echo 999; exit;; esac\n'
            'fi\nexec /usr/bin/stat "$@"\n'
        )
        stat.chmod(0o755)
        result = self.stage()
        self.assert_blocked(result)
        self.assertIn("cross filesystems", result.stderr)

    def test_missing_utility_fails_with_explicit_diagnostic(self):
        result = subprocess.run(
            ["/bin/sh", "-s", "--", "verify", "/opt/cni/bin/evpn-cni.real",
             "real", str(self.size), self.sha], input=WORKER_SCRIPT.read_text(),
            text=True, capture_output=True, env={**os.environ, "PATH": str(self.tools)},
        )
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("required utility unavailable", result.stderr)

    def test_unrelated_temporary_files_survive_cleanup(self):
        unrelated = self.worker / ".evpn-cni-stage.unrelated"
        unrelated.mkdir()
        (unrelated / "artifact").write_bytes(b"foreign")
        result = self.stage("copy-fail")
        self.assertEqual(result.returncode, 37)
        self.assertEqual((unrelated / "artifact").read_bytes(), b"foreign")

    def prepare_wrapper(self, output=VERSION):
        runner = RUNNER.read_text()
        wrapper = runner.split('cat > "${EVIDENCE_DIR}/cni-wrapper" <<\'EOF\'\n', 1)[1].split("\nEOF", 1)[0]
        self.assertIn("/opt/cni/bin/evpn-cni.real", wrapper)
        self.assertNotIn("/usr/local/bin/evpn-node-agent", wrapper)
        self.assertEqual(wrapper.count("/opt/cni/bin/evpn-cni.real"), 1)
        calls = self.root / "wrapper-calls"
        calls.mkdir(exist_ok=True)
        real = self.root / "mock-real"
        real.write_text(
            "#!/usr/bin/env python3\nimport json, os, pathlib, sys\n"
            f"root = pathlib.Path({str(self.root)!r})\n"
            '(root / "wrapper-stdin").write_bytes(sys.stdin.buffer.read())\n'
            '(root / "wrapper-env").write_text(json.dumps(dict(os.environ)))\n'
            'sys.stdout.buffer.write((root / "version-response").read_bytes())\n'
            'sys.stderr.write(os.environ.get("MOCK_VERSION_STDERR", ""))\n'
            'raise SystemExit(int(os.environ.get("MOCK_VERSION_RC", "0")))\n'
        )
        real.chmod(0o755)
        wrapper = wrapper.replace("/opt/cni/bin/evpn-cni.real", str(real))
        wrapper = wrapper.replace("/run/evpn-cni-evidence", str(calls))
        path = self.root / "cni-wrapper"
        path.write_text(wrapper + "\n")
        path.chmod(0o755)
        (self.root / "version-response").write_bytes(output)
        return path

    def test_wrapper_preserves_stdin_cni_environment_stdout_and_stderr(self):
        wrapper = self.prepare_wrapper()
        stdin = b'{"some":"input"}\n\n\0not-text'
        result = subprocess.run(
            [str(wrapper)], input=stdin, capture_output=True,
            env={**self.env, "CNI_COMMAND": "VERSION", "CNI_ARGS": "unchanged=value",
                 "MOCK_VERSION_STDERR": "diagnostic\n"},
        )
        self.assertEqual(result.returncode, 0)
        self.assertEqual(result.stdout, VERSION)
        self.assertEqual(result.stderr, b"diagnostic\n")
        self.assertEqual((self.root / "wrapper-stdin").read_bytes(), stdin)
        env = json.loads((self.root / "wrapper-env").read_text())
        self.assertEqual(env["CNI_COMMAND"], "VERSION")
        self.assertEqual(env["CNI_ARGS"], "unchanged=value")

    def test_wrapper_propagates_nonzero_real_exit_without_stdout_contamination(self):
        wrapper = self.prepare_wrapper(output=b"")
        result = subprocess.run(
            [str(wrapper)], input=b"", capture_output=True,
            env={**self.env, "MOCK_VERSION_RC": "29", "MOCK_VERSION_STDERR": "failed\n"},
        )
        self.assertEqual(result.returncode, 29)
        self.assertEqual(result.stdout, b"")
        self.assertEqual(result.stderr, b"failed\n")

    def run_version(self, output=VERSION, rc=0, stderr=""):
        wrapper = self.prepare_wrapper(output)
        (self.worker / "evpn-cni").write_bytes(wrapper.read_bytes())
        (self.worker / "evpn-cni").chmod(0o755)
        final = self.worker / "evpn-cni.real"
        final.write_bytes(self.data)
        final.chmod(0o755)
        return self.shell(
            f"verify_worker_cni_version worker {shlex.quote(str(self.root))} {self.size} {self.sha}\n"
            f"touch {shlex.quote(str(self.root / 'pod-created'))}",
            MOCK_VERSION_RC=str(rc), MOCK_VERSION_STDERR=stderr,
        )

    def test_valid_version_json_passes_through_installed_wrapper(self):
        result = self.run_version()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual((self.root / "cni-version.json").read_bytes(), VERSION)
        self.assertEqual((self.root / "cni-version.exit-code").read_text(), "0\n")
        self.assertEqual((self.root / "wrapper-stdin").read_bytes(), b"")
        self.assertTrue((self.root / "pod-created").exists())

    def test_empty_version_stdout_fails(self):
        result = self.run_version(output=b"")
        self.assert_blocked(result)
        self.assertIn("VERSION validation failed", result.stderr)

    def test_malformed_version_json_fails(self):
        result = self.run_version(output=b"{broken")
        self.assert_blocked(result)

    def test_missing_version_fields_fail(self):
        for output in (b"{}", b'{"cniVersion":"1.1.0"}', b'{"supportedVersions":["1.0.0"]}'):
            with self.subTest(output=output):
                result = self.run_version(output=output)
                self.assert_blocked(result)

    def test_wrong_supported_versions_fail(self):
        result = self.run_version(output=b'{"cniVersion":"1.1.0","supportedVersions":["0.4.0"]}')
        self.assert_blocked(result)

    def test_stdout_contamination_fails(self):
        result = self.run_version(output=b"log line\n" + VERSION)
        self.assert_blocked(result)

    def test_trailing_stdout_contamination_fails(self):
        result = self.run_version(output=VERSION + b'{"second":"object"}')
        self.assert_blocked(result)

    def test_wrong_version_field_types_fail(self):
        result = self.run_version(output=b'{"cniVersion":1.1,"supportedVersions":"1.0.0"}')
        self.assert_blocked(result)

    def test_wrapper_is_published_separately_and_real_is_preserved(self):
        result = self.stage()
        self.assertEqual(result.returncode, 0, result.stderr)
        wrapper = self.prepare_wrapper()
        sha = hashlib.sha256(wrapper.read_bytes()).hexdigest()
        result = self.shell(
            f"stage_worker_cni_file worker {shlex.quote(str(wrapper))} "
            f"/opt/cni/bin/evpn-cni {wrapper.stat().st_size} {sha} wrapper"
        )
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual((self.worker / "evpn-cni").read_bytes(), wrapper.read_bytes())
        self.assertEqual((self.worker / "evpn-cni.real").read_bytes(), self.data)

    def test_version_cannot_run_after_failed_final_integrity(self):
        wrapper = self.prepare_wrapper()
        (self.worker / "evpn-cni").write_bytes(wrapper.read_bytes())
        (self.worker / "evpn-cni").chmod(0o755)
        result = self.shell(
            f"verify_worker_cni_version worker {shlex.quote(str(self.root))} {self.size} {self.sha}"
        )
        self.assertNotEqual(result.returncode, 0)
        self.assertFalse((self.root / "wrapper-stdin").exists())
        self.assertNotIn('"env"', (self.root / "calls").read_text())

    def test_version_cannot_run_with_replaced_wrapper(self):
        self.prepare_wrapper()
        (self.worker / "evpn-cni").write_bytes(b"wrong wrapper")
        (self.worker / "evpn-cni").chmod(0o755)
        result = self.shell(
            f"verify_worker_cni_version worker {shlex.quote(str(self.root))} {self.size} {self.sha}"
        )
        self.assertNotEqual(result.returncode, 0)
        self.assertFalse((self.root / "wrapper-stdin").exists())
        self.assertNotIn('"env"', (self.root / "calls").read_text())

    def test_nonzero_version_exit_preserves_status_and_stderr(self):
        result = self.run_version(rc=31, stderr="mock failure\n")
        self.assert_blocked(result)
        self.assertEqual(result.returncode, 31)
        self.assertIn("mock failure", result.stderr)
        self.assertEqual((self.root / "cni-version.exit-code").read_text(), "31\n")
        self.assertEqual((self.root / "cni-version.stderr").read_text(), "mock failure\n")

    def test_stderr_is_retained_and_not_mixed_into_valid_json(self):
        result = self.run_version(stderr="diagnostic only\n")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual((self.root / "cni-version.json").read_bytes(), VERSION)
        self.assertEqual((self.root / "cni-version.stderr").read_text(), "diagnostic only\n")

    def test_runner_orders_integrity_and_version_before_workload_creation(self):
        text = RUNNER.read_text()
        self.assertLess(text.index("stage_worker_cni_file"), text.index("verify_worker_cni_version"))
        self.assertLess(text.index("verify_worker_cni_version"), text.index("create_pod a"))
        self.assertIn("CNI_SIZE=15242639", text)
        self.assertIn("CNI_SHA=39a499c490738a3c1d26a82623e7964bc790f30952eab3f6491ab6e8c9afad98", text)


if __name__ == "__main__":
    unittest.main()
