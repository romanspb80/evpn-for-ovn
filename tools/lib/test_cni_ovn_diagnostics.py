#!/usr/bin/env python3
"""Offline tests for fail-closed OVN diagnostic command capture."""

import hashlib
import json
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest


ROOT = Path(__file__).resolve().parents[2]
TOOL = ROOT / "tools/lib/cni_ovn_diagnostics.py"
INTEROP = ROOT / "tools/lib/cni-evpn-frr-interop-test.sh"


def ovsdb_table(headings, rows):
    return {"headings": headings, "data": rows}


class DiagnosticCaptureTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.root = Path(self.temp.name)

    def tearDown(self):
        self.temp.cleanup()

    def run_capture(self, command, expectation="required", command_id="fixture",
                    stdout_path=None):
        base = self.root / command_id
        stdout_path = stdout_path or base.with_suffix(".stdout")
        stderr_path = base.with_suffix(".stderr")
        metadata_path = base.with_suffix(".command.json")
        status_path = base.with_suffix(".exit-status")
        result = subprocess.run(
            [
                sys.executable, str(TOOL), "run",
                "--command-id", command_id,
                "--context", "offline mocked context",
                "--expectation", expectation,
                "--stdout", str(stdout_path),
                "--stderr", str(stderr_path),
                "--metadata", str(metadata_path),
                "--exit-status", str(status_path),
                "--", *command,
            ],
            text=True,
            capture_output=True,
            check=False,
        )
        return result, stdout_path, stderr_path, metadata_path, status_path

    def test_success_records_command_context_timing_streams_and_hashes(self):
        payload = "answer\n"
        result, stdout, stderr, metadata, status = self.run_capture(
            [sys.executable, "-c", "import sys; print('answer'); print('warning', file=sys.stderr)"]
        )
        self.assertEqual(result.returncode, 0, result.stderr)
        document = json.loads(metadata.read_text())
        self.assertEqual(document["classification"], "SUCCESS")
        self.assertEqual(document["executionContext"], "offline mocked context")
        self.assertEqual(document["expectation"], "required")
        self.assertEqual(document["exitStatus"], 0)
        self.assertGreater(document["durationNanoseconds"], 0)
        self.assertEqual(document["stdout"]["bytes"], len(payload))
        self.assertEqual(document["stdout"]["sha256"], hashlib.sha256(payload.encode()).hexdigest())
        self.assertEqual(document["stderr"]["bytes"], len(b"warning\n"))
        self.assertEqual(stdout.read_text(), payload)
        self.assertEqual(stderr.read_text(), "warning\n")
        self.assertEqual(status.read_text().strip(), "0")

    def test_zero_exit_with_empty_required_stdout_fails_closed(self):
        result, _, _, metadata, _ = self.run_capture(
            [sys.executable, "-c", "pass"]
        )
        self.assertEqual(result.returncode, 1)
        self.assertEqual(json.loads(metadata.read_text())["classification"], "EMPTY_REQUIRED_OUTPUT")

    def test_status_only_command_accepts_empty_success_but_not_failed_query(self):
        success, _, _, metadata, _ = self.run_capture(
            [sys.executable, "-c", "pass"], expectation="status", command_id="sync-success"
        )
        self.assertEqual(success.returncode, 0, success.stderr)
        self.assertEqual(json.loads(metadata.read_text())["classification"], "SUCCESS")
        failed, _, _, metadata, _ = self.run_capture(
            [sys.executable, "-c", "raise SystemExit(124)"],
            expectation="status", command_id="sync-timeout",
        )
        self.assertNotEqual(failed.returncode, 0)
        self.assertEqual(json.loads(metadata.read_text())["classification"], "COMMAND_TIMEOUT")

    def test_descriptor_sensitive_launcher_receives_pipes_not_regular_files(self):
        command = [
            sys.executable, "-c",
            "import json,os,stat,sys; "
            "sys.exit(0) if stat.S_ISREG(os.fstat(1).st_mode) else None; "
            "print(json.dumps({'clientVersion': 'fixture'})); "
            "print('launcher diagnostic', file=sys.stderr)",
        ]
        result, stdout, stderr, metadata, _ = self.run_capture(
            command, expectation="json", command_id="launcher"
        )
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(json.loads(stdout.read_text()), {"clientVersion": "fixture"})
        self.assertEqual(stderr.read_text(), "launcher diagnostic\n")
        self.assertEqual(json.loads(metadata.read_text())["classification"], "SUCCESS")

    def test_nonzero_with_empty_stderr_is_query_failure_not_absence(self):
        result, _, stderr, metadata, status = self.run_capture(
            [sys.executable, "-c", "raise SystemExit(1)"]
        )
        self.assertEqual(result.returncode, 1)
        self.assertEqual(stderr.read_bytes(), b"")
        record = json.loads(metadata.read_text())
        self.assertEqual(record["classification"], "QUERY_FAILED")
        self.assertEqual(record["exitStatus"], 1)
        self.assertEqual(status.read_text().strip(), "1")

    def test_timeout_and_unavailable_execution_context_are_distinct(self):
        timeout_result, _, _, timeout_metadata, _ = self.run_capture(
            [sys.executable, "-c", "raise SystemExit(124)"], command_id="timeout"
        )
        self.assertEqual(timeout_result.returncode, 1)
        self.assertEqual(json.loads(timeout_metadata.read_text())["classification"], "COMMAND_TIMEOUT")

        context_result, _, _, context_metadata, _ = self.run_capture(
            [
                sys.executable, "-c",
                """import sys; print('Error from server (NotFound): pods "ovn-runtime" not found', file=sys.stderr); raise SystemExit(1)""",
            ],
            command_id="context",
        )
        self.assertEqual(context_result.returncode, 1)
        self.assertEqual(
            json.loads(context_metadata.read_text())["classification"],
            "EXEC_CONTEXT_FAILED",
        )

    def test_missing_executable_and_unsupported_field_are_classified(self):
        missing, _, _, missing_metadata, _ = self.run_capture(
            [str(self.root / "not-installed")], command_id="missing"
        )
        self.assertEqual(missing.returncode, 1)
        self.assertEqual(json.loads(missing_metadata.read_text())["classification"], "COMMAND_NOT_FOUND")

        unsupported, _, _, unsupported_metadata, _ = self.run_capture(
            [
                sys.executable, "-c",
                "import sys; print('unknown column nb_cfg', file=sys.stderr); raise SystemExit(1)",
            ],
            command_id="unsupported",
        )
        self.assertEqual(unsupported.returncode, 1)
        self.assertEqual(
            json.loads(unsupported_metadata.read_text())["classification"],
            "UNSUPPORTED_FIELD",
        )

    def test_stdout_redirection_failure_does_not_run_command(self):
        blocker = self.root / "not-a-directory"
        blocker.write_text("preserve")
        marker = self.root / "should-not-exist"
        result, _, _, _, _ = self.run_capture(
            [sys.executable, "-c", f"from pathlib import Path; Path({str(marker)!r}).touch()"],
            command_id="redirection",
            stdout_path=blocker / "output",
        )
        self.assertEqual(result.returncode, 2)
        self.assertEqual(
            json.loads(result.stderr)["classification"],
            "COLLECTOR_INTERNAL_ERROR",
        )
        self.assertFalse(marker.exists())
        self.assertEqual(blocker.read_text(), "preserve")

    def test_no_pipeline_or_command_substitution_can_turn_failure_into_success(self):
        pipeline, _, _, pipeline_metadata, _ = self.run_capture(
            ["/bin/bash", "-o", "pipefail", "-c", "false | cat"],
            expectation="required",
            command_id="pipeline",
        )
        self.assertEqual(pipeline.returncode, 1)
        self.assertEqual(json.loads(pipeline_metadata.read_text())["classification"], "QUERY_FAILED")

        substitution, stdout, _, substitution_metadata, _ = self.run_capture(
            ["/bin/bash", "-c", "value=$(printf source-output); printf transformed"],
            command_id="substitution",
        )
        self.assertEqual(substitution.returncode, 0, substitution.stderr)
        self.assertEqual(stdout.read_text(), "transformed")
        self.assertEqual(json.loads(substitution_metadata.read_text())["stdout"]["bytes"], 11)

        discarded, _, _, discarded_metadata, _ = self.run_capture(
            ["/bin/bash", "-c", "value=$(printf hidden-output); :"],
            command_id="discarded",
        )
        self.assertEqual(discarded.returncode, 1)
        self.assertEqual(
            json.loads(discarded_metadata.read_text())["classification"],
            "EMPTY_REQUIRED_OUTPUT",
        )

    def test_json_parse_failure_and_optional_empty_output(self):
        bad_json, _, _, bad_metadata, _ = self.run_capture(
            [sys.executable, "-c", "print('{')"], expectation="json", command_id="bad-json"
        )
        self.assertEqual(bad_json.returncode, 1)
        self.assertEqual(json.loads(bad_metadata.read_text())["classification"], "PARSE_FAILED")

        optional, stdout, _, optional_metadata, _ = self.run_capture(
            [sys.executable, "-c", "pass"], expectation="optional", command_id="optional"
        )
        self.assertEqual(optional.returncode, 0)
        self.assertEqual(stdout.read_bytes(), b"")
        self.assertEqual(json.loads(optional_metadata.read_text())["classification"], "SUCCESS")

    def test_optional_command_failure_is_recorded_and_not_silently_reported_as_success(self):
        result, _, _, metadata, _ = self.run_capture(
            [sys.executable, "-c", "raise SystemExit(1)"],
            expectation="optional",
            command_id="optional-failure",
        )
        self.assertEqual(result.returncode, 0)
        self.assertIn("optional evidence QUERY_FAILED", result.stderr)
        self.assertEqual(
            json.loads(metadata.read_text())["classification"],
            "QUERY_FAILED",
        )

    def test_required_port_binding_absence_is_machine_readable_only_after_successful_query(self):
        query, stdout, _, metadata, _ = self.run_capture(
            [
                sys.executable, "-c",
                "import json; print(json.dumps({'headings':['logical_port'], 'data':[]}))",
            ],
            expectation="json",
            command_id="bindings",
        )
        self.assertEqual(query.returncode, 0, query.stderr)
        absent = subprocess.run(
            [
                sys.executable, str(TOOL), "classify-object",
                "--input", str(stdout),
                "--object-kind", "Port_Binding",
                "--key-column", "logical_port",
                "--key-value", "replacement-lsp",
            ],
            text=True,
            capture_output=True,
            check=False,
        )
        self.assertEqual(absent.returncode, 0, absent.stderr)
        self.assertEqual(json.loads(absent.stdout)["classification"], "OBJECT_ABSENT")
        self.assertEqual(json.loads(metadata.read_text())["classification"], "SUCCESS")

    def test_positive_control_rejects_wrong_execution_context_or_missing_object(self):
        fixture = self.root / "switches.json"
        fixture.write_text(json.dumps(ovsdb_table(["_uuid", "name"], [["uuid-known", "other-switch"]])))
        result = subprocess.run(
            [
                sys.executable, str(TOOL), "verify-control",
                "--input", str(fixture), "--name-column", "name",
                "--name", "ls24b", "--uuid-column", "_uuid",
            ],
            text=True,
            capture_output=True,
            check=False,
        )
        self.assertEqual(result.returncode, 1)
        self.assertEqual(json.loads(result.stderr)["classification"], "OBJECT_ABSENT")

    def test_missing_identity_column_is_parse_failure_not_object_absence(self):
        fixture = self.root / "incomplete-bindings.json"
        fixture.write_text(json.dumps(ovsdb_table(["datapath"], [])))
        result = subprocess.run(
            [
                sys.executable, str(TOOL), "classify-object",
                "--input", str(fixture),
                "--object-kind", "Port_Binding",
                "--key-column", "logical_port",
                "--key-value", "replacement-lsp",
            ],
            text=True,
            capture_output=True,
            check=False,
        )
        self.assertEqual(result.returncode, 1)
        self.assertEqual(json.loads(result.stderr)["classification"], "PARSE_FAILED")

    def test_positive_control_validates_nb_sb_chassis_and_ovs_rows(self):
        cases = (
            (["_uuid", "name", "ports"], [["switch-id", "ls24b", ["set", [["uuid", "lsp-id"]]]]],
             ["--name-column", "name", "--name", "ls24b", "--uuid-column", "_uuid"]),
            (["_uuid", "name", "ports"], [["switch-id", "ls24b", ["uuid", "lsp-id"]]],
             ["--name-column", "name", "--name", "ls24b", "--uuid-column", "_uuid",
              "--member-column", "ports", "--member-uuid", "lsp-id"]),
            (["_uuid", "external_ids"], [["dp-id", ["map", [["logical-switch", "switch-id"]]]]],
             ["--map-column", "external_ids", "--map-key", "logical-switch",
              "--map-value", "switch-id", "--uuid-column", "_uuid"]),
            (["_uuid", "name"], [["chassis-id", "chassis0"]],
             ["--name-column", "name", "--name", "chassis0", "--uuid-column", "_uuid"]),
            (["_uuid", "name"], [["bridge-id", "br-int"]],
             ["--name-column", "name", "--name", "br-int", "--uuid-column", "_uuid"]),
        )
        for index, (headings, rows, selectors) in enumerate(cases):
            with self.subTest(control=index):
                fixture = self.root / f"control-{index}.json"
                fixture.write_text(json.dumps(ovsdb_table(headings, rows)))
                result = subprocess.run(
                    [sys.executable, str(TOOL), "verify-control", "--input", str(fixture), *selectors],
                    text=True,
                    capture_output=True,
                    check=False,
                )
                self.assertEqual(result.returncode, 0, result.stderr)

    def test_ovs_attachment_requires_iface_id_port_and_bridge_membership(self):
        inputs = {
            "interfaces": ovsdb_table(
                ["_uuid", "name", "external_ids"],
                [["if-id", "veth0", ["map", [["iface-id", "replacement-lsp"]]]]],
            ),
            "ports": ovsdb_table(
                ["_uuid", "name", "interfaces"],
                [["port-id", "replacement-lsp", ["set", [["uuid", "if-id"]]]]],
            ),
            "bridges": ovsdb_table(
                ["_uuid", "name", "ports"],
                [["bridge-id", "br-int", ["set", [["uuid", "port-id"]]]]],
            ),
        }
        paths = {}
        for key, document in inputs.items():
            path = self.root / f"{key}.json"
            path.write_text(json.dumps(document))
            paths[key] = path
        command = [
            sys.executable, str(TOOL), "verify-ovs-attachment",
            "--interfaces", str(paths["interfaces"]),
            "--ports", str(paths["ports"]),
            "--bridges", str(paths["bridges"]),
            "--logical-port", "replacement-lsp",
            "--expect", "present",
        ]
        result = subprocess.run(command, text=True, capture_output=True, check=False)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(json.loads(result.stdout)["bridge"]["name"], "br-int")

        for column, key, member in (("interfaces", "ports", "if-id"),
                                    ("ports", "bridges", "port-id")):
            index = inputs[key]["headings"].index(column)
            inputs[key]["data"][0][index] = ["uuid", member]
            paths[key].write_text(json.dumps(inputs[key]))
        result = subprocess.run(command, text=True, capture_output=True, check=False)
        self.assertEqual(result.returncode, 0, result.stderr)

        inputs["bridges"] = ovsdb_table(
            ["_uuid", "name", "ports"], [["bridge-id", "br-int", ["set", []]]]
        )
        paths["bridges"].write_text(json.dumps(inputs["bridges"]))
        result = subprocess.run(command, text=True, capture_output=True, check=False)
        self.assertEqual(result.returncode, 1)
        self.assertEqual(json.loads(result.stderr)["classification"], "OBJECT_ABSENT")

    def test_existing_diagnostic_artifact_is_never_overwritten(self):
        result, stdout, _, _, _ = self.run_capture(
            [sys.executable, "-c", "print('first')"], command_id="preserved"
        )
        self.assertEqual(result.returncode, 0, result.stderr)
        original = stdout.read_bytes()
        second, _, _, _, _ = self.run_capture(
            [sys.executable, "-c", "print('second')"], command_id="preserved"
        )
        self.assertEqual(second.returncode, 2)
        self.assertIn("refusing to overwrite", second.stderr)
        self.assertEqual(stdout.read_bytes(), original)

    def test_capture_calls_are_context_labeled_and_stop_on_missing_required_evidence(self):
        source = INTEROP.read_text()
        diagnostic = source[
            source.index("interop_collect_replacement_binding_diagnostics() {"):
            source.index("\ninterop_diagnostic_preflight() {")
        ]
        self.assertIn('"kubectl exec OVN runtime Pod ${OVN_POD} in cni-test" json', diagnostic)
        self.assertIn('"docker exec kind worker ${WORKER}" json', diagnostic)
        self.assertIn('"${context}" optional', diagnostic)
        self.assertIn("|| return 1", diagnostic)
        self.assertIn("diagnostic-control-nb-switch.json", source)
        self.assertIn("diagnostic-control-sb-datapaths.json", source)
        self.assertIn("diagnostic-control-chassis.json", source)
        self.assertIn("diagnostic-control-ovs-bridges.json", source)

    def test_global_table_artifacts_have_diagnostic_specific_names(self):
        source = INTEROP.read_text()
        diagnostic = source[
            source.index("interop_collect_replacement_binding_diagnostics() {"):
            source.index("\ninterop_diagnostic_preflight() {")
        ]
        self.assertIn('"ovn-nb-global-diagnostic-${suffix}.txt"', diagnostic)
        self.assertIn('"ovn-sb-global-diagnostic-${suffix}.txt"', diagnostic)
        self.assertNotIn('"ovn-nb-global-${suffix}.txt"', diagnostic)
        self.assertNotIn('"ovn-sb-global-${suffix}.txt"', diagnostic)

    def test_preflight_failure_stops_before_pod_lifecycle(self):
        marker = self.root / "lifecycle-started"
        script = f"""
KIND_CLUSTER=offline-fixture
EVIDENCE_DIR={str(self.root)!r}
source {str(INTEROP)!r}
interop_diagnostic_preflight() {{ return 1; }}
k() {{ : >{str(marker)!r}; }}
interop_capture_binding_snapshot() {{ : >{str(marker)!r}; }}
interop_binding_diagnostic
"""
        result = subprocess.run(["bash", "-c", script], text=True, capture_output=True, check=False)
        self.assertNotEqual(result.returncode, 0)
        self.assertFalse(marker.exists())

    def test_deleted_snapshot_does_not_require_describing_nonexistent_pod(self):
        commands = self.root / "commands"
        script = f"""
KIND_CLUSTER=offline-fixture
ROOT={str(ROOT)!r}
EVIDENCE_DIR={str(self.root)!r}
OVN_POD=ovn-runtime
AGENT_POD=node-agent
WORKER=worker
source {str(INTEROP)!r}
interop_capture_diagnostic() {{ printf '%s\\n' "$*" >>{str(commands)!r}; }}
python3() {{ return 0; }}
interop_collect_replacement_binding_diagnostics deleted
"""
        result = subprocess.run(["bash", "-c", script], text=True, capture_output=True, check=False)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertNotIn("describe pod pod-a", commands.read_text())
        self.assertIn("get pods -o json", commands.read_text())

    def test_diagnostic_runner_gates_and_during_poll_snapshot(self):
        source = INTEROP.read_text()
        runner = (ROOT / "tools/run-cni-mvp-2.4b-multus.sh").read_text()
        diagnostic = source[source.index("interop_binding_diagnostic() {"):
                            source.index("\ninterop_verify() {")]
        self.assertIn('interop_capture_binding_snapshot replacement-30s "${new_uid}"', diagnostic)
        self.assertNotIn("interop_capture_start", diagnostic)
        self.assertNotIn("interop_verify_lifecycle", diagnostic)
        self.assertIn('[[ "${state}" == BOUND ]]', source)
        self.assertIn('find Port_Binding "logical_port=${logical_port}"', source)
        self.assertIn("binding diagnostic mode requires FRR interop", runner)
        self.assertIn("interop_binding_diagnostic\n      exit 0", runner)
        gate = runner.index('if [[ "${EVPN_FRR_INTEROP_BINDING_DIAGNOSTIC}" == 1 ]]; then\n  interop_diagnostic_preflight')
        self.assertLess(gate, runner.index("\ncreate_pod a\n"))


if __name__ == "__main__":
    unittest.main()
