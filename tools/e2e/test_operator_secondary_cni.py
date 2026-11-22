"""Offline orchestration tests; these do not execute Kubernetes or networking."""

import json
import hashlib
from pathlib import Path
import tempfile
import types
import unittest
import uuid
from unittest import mock
import contextlib
import io

import isolated_secondary_cni as isolated
import operator_secondary_cni as runner
from test_operator_ownership import external_fixture, inventory_fixture


def inventory_inputs():
    return {
        "clusterUID": "11111111-1111-4111-8111-111111111111",
        "installationUID": "22222222-2222-4222-8222-222222222222",
        "agentImage": "example.invalid/agent:fixture", "clientImage": "example.invalid/client:fixture",
        "ovnNBHost": "secondary-ovn.cni-test.svc", "ovnSBHost": "secondary-ovn.cni-test.svc",
        "remoteVTEP": "198.18.10.30",
        "workers": [{
            "node": {"metadata": {"name": "worker-" + suffix, "uid": str(uuid.UUID(int=number))}},
            "chassis": "chassis-" + suffix, "vtep": "198.18.10." + str(number + 1),
            "bootID": "33333333-3333-4333-8333-333333333333", "hostNetns": "net:[4026531993]",
            "serviceUID": str(uuid.UUID(int=number + 10)), "speakerCgroup": "/system.slice/gobgpd.service",
            **{name: "a" * 64 for name in ("ctrSHA256", "cniSHA256", "multusSHA256",
                "runtimeSHA256", "wrapperExecutableSHA256", "wrapperScriptSHA256", "gobgpdSHA256")}
        } for suffix, number in (("a", 1), ("b", 2))],
    }


def external_vxlan_link():
    return [{
        "ifname": "vxlan1000", "master": "br1000", "flags": ["BROADCAST", "UP"],
        "linkinfo": {"info_kind": "vxlan", "info_data": {
            "id": 1000, "local": runner.REMOTE_VTEP, "port": 4789,
        }},
    }]


class OperatorRunnerTests(unittest.TestCase):
    def test_worker_cni_executable_staging_repairs_copy_ownership_before_activation(self):
        instance = isolated.IsolatedSecondaryCNI()
        with tempfile.TemporaryDirectory() as directory:
            source = Path(directory) / "evpn-cni.real"
            source.write_bytes(b"pinned executable")
            digest = hashlib.sha256(source.read_bytes()).hexdigest()
            with mock.patch.object(isolated.lab, "command") as command, \
                    mock.patch.object(instance, "docker") as docker, \
                    mock.patch.object(instance, "worker") as worker:
                instance.stage_worker_executable(
                    "worker-a", source, "/opt/cni/bin/evpn-cni.real")
            args = command.call_args.args[0]
            self.assertEqual(args[:2], ["bash", "-ec"])
            self.assertIn("stage_worker_cni_file", args[2])
            self.assertEqual(args[4:], [
                str(Path(isolated.ROOT) / "tools/lib/cni-agent-image-test.sh"),
                "worker-a", str(source), "/opt/cni/bin/evpn-cni.real", "17", digest, "real",
            ])
            command.assert_called_once_with(args, timeout=300)
            docker.assert_not_called()
            commands = [call.args[1:] for call in worker.call_args_list]
            self.assertEqual(len(commands), 1)
            self.assertEqual(commands[0][:2], ("sh", "-ec"))
            self.assertIn("0:0:755:17", commands[0][2])
            self.assertIn(digest, commands[0][2])

    def test_inventory_capture_names_match_published_inventory_prefix(self):
        instance = runner.OperatorInventoryType2("ctr", "a" * 64, "gobgpd", "b" * 64)
        endpoints = {
            "worker-a": {"underlayInterface": "eth0", "vtep": "198.18.30.2"},
            "worker-b": {"underlayInterface": "eth0", "vtep": "198.18.30.3"},
        }
        with tempfile.TemporaryDirectory() as directory:
            instance.directory = Path(directory)
            instance.frr = "external-frr"
            instance.cluster = "inventory-cluster"
            scripts = []

            def command(args, **kwargs):
                if args[0] == "docker" and args[1] == "inspect":
                    return types.SimpleNamespace(stdout={
                        "worker-a": "101\n", "worker-b": "102\n", "external-frr": "103\n",
                    }[args[-1]])
                scripts.append(args[2])
                (instance.directory / "inventory-capture-state.sh").write_text("owned state\n")
                return types.SimpleNamespace(returncode=0, stdout="", stderr="")

            with mock.patch.object(runner.lab, "command", side_effect=command), \
                    mock.patch.object(instance, "docker", return_value=types.SimpleNamespace(stdout="[]")), \
                    mock.patch.object(instance, "interface_for_address", return_value="eth0"), \
                    mock.patch.object(instance, "record"):
                instance.start_inventory_capture(endpoints)

        self.assertEqual(len(scripts), 1)
        script = scripts[0]
        self.assertIn("INTEROP_CAPTURE_OUTPUT_NAMES=(worker-a-underlay worker-b-underlay frr-underlay)", script)
        self.assertIn("declare -p INTEROP_CAPTURE_PHASE", script)
        self.assertIn("interop_capture_start_one inventory-worker-a-underlay 101", script)
        self.assertIn("interop_capture_start_one inventory-worker-b-underlay 102", script)
        self.assertIn("interop_capture_start_one inventory-frr-underlay 103", script)

    def test_inventory_forwarding_records_fdb_before_refusing_wrong_vtep(self):
        instance = runner.OperatorInventoryType2("ctr", "a" * 64, "gobgpd", "b" * 64)
        endpoint = {"node": "worker-a", "ip": "10.244.240.11",
                    "mac": "02:00:00:00:00:11", "vtep": "198.18.30.2"}
        conflict = {"mac": endpoint["mac"], "dst": "198.18.30.99",
                    "state": "permanent", "flags": ["self"]}
        responses = [
            "[]",
            json.dumps([{"addr_info": [{"local": runner.REMOTE_IP}]}]),
            json.dumps([{"address": runner.REMOTE_MAC}]),
            json.dumps([{"dst": endpoint["ip"], "dev": "frr-work0",
                         "lladdr": endpoint["mac"]}]),
            json.dumps([conflict]),
            json.dumps(external_vxlan_link()),
        ]
        with mock.patch.object(instance, "docker", side_effect=lambda *args: types.SimpleNamespace(
                stdout=responses.pop(0))), \
                mock.patch.object(instance, "interface_for_address", return_value="eth0"), \
                mock.patch.object(instance, "record") as record:
            with self.assertRaisesRegex(isolated.IsolationError, "conflicting FDB entry"):
                instance.seed_forwarding_state({"worker-a": endpoint})
        self.assertEqual(record.call_args.args[0], "inventory-forwarding-fixture-observations.json")
        observation = record.call_args.args[1]
        self.assertEqual(observation["workerEndpoints"][0]["fdbRows"], [conflict])

    def test_inventory_fdb_accepts_externally_learned_entry_and_bridge_rows(self):
        mac, vtep = "02:00:00:00:00:11", "198.18.30.2"
        rows = [
            {"mac": mac, "dst": vtep, "flags": ["self", "extern_learn"], "state": ""},
            {"mac": mac, "dev": "vxlan1000", "master": "br1000", "vlan": 1},
            {"mac": mac, "master": "br1000", "vlan": 1},
        ]
        result = runner.classify_external_vxlan_fdb(rows, mac, vtep)
        self.assertEqual(result["status"], "observed-externally-learned")
        self.assertEqual(len(result["bridgeMasterRows"]), 2)

    def test_inventory_fdb_rejects_multiple_device_local_entries(self):
        mac, vtep = "02:00:00:00:00:11", "198.18.30.2"
        rows = [
            {"mac": mac, "dst": vtep, "flags": ["self", "extern_learn"], "state": ""},
            {"mac": mac, "dst": "198.18.30.99", "flags": ["self"], "state": "permanent"},
        ]
        with self.assertRaisesRegex(isolated.IsolationError, "conflicting FDB entries"):
            runner.classify_external_vxlan_fdb(rows, mac, vtep)

    def test_inventory_fdb_reports_missing_device_local_entry(self):
        result = runner.classify_external_vxlan_fdb(
            [{"mac": "02:00:00:00:00:11", "master": "br1000", "vlan": 1}],
            "02:00:00:00:00:11", "198.18.30.2",
        )
        self.assertEqual(result["status"], "MISSING")
        self.assertEqual(len(result["bridgeMasterRows"]), 1)

    def test_external_workload_bridge_fdb_accepts_observed_bridge_row(self):
        mac = runner.REMOTE_MAC
        row = {"mac": mac, "ifname": "frr-host0", "flags": [],
               "master": runner.EXTERNAL_BRIDGE, "state": ""}
        result = runner.classify_external_workload_bridge_fdb([row], mac)
        self.assertEqual(result, {"status": "OBSERVED_EXISTING", "entry": row})

    def test_external_workload_bridge_fdb_rejects_wrong_interface_or_bridge(self):
        mac = runner.REMOTE_MAC
        for field, value in (("ifname", "other0"), ("master", "other-bridge")):
            with self.subTest(field=field):
                row = {"mac": mac, "ifname": "frr-host0", "flags": [],
                       "master": runner.EXTERNAL_BRIDGE, "state": ""}
                row[field] = value
                with self.assertRaisesRegex(isolated.IsolationError, "conflicting workload FDB"):
                    runner.classify_external_workload_bridge_fdb([row], mac)

    def test_external_workload_bridge_fdb_rejects_duplicate_or_malformed_rows(self):
        mac = runner.REMOTE_MAC
        row = {"mac": mac, "ifname": "frr-host0", "flags": [],
               "master": runner.EXTERNAL_BRIDGE, "state": ""}
        with self.assertRaisesRegex(isolated.IsolationError, "conflicting workload FDB"):
            runner.classify_external_workload_bridge_fdb([row, dict(row)], mac)
        malformed = dict(row, flags="master")
        with self.assertRaisesRegex(isolated.IsolationError, "conflicting workload FDB"):
            runner.classify_external_workload_bridge_fdb([malformed], mac)

    def test_external_workload_bridge_fdb_reports_missing_exact_entry(self):
        result = runner.classify_external_workload_bridge_fdb([], runner.REMOTE_MAC)
        self.assertEqual(result, {"status": "MISSING", "entry": None})

    def test_inventory_fdb_rejects_malformed_route_learned_state(self):
        with self.assertRaisesRegex(isolated.IsolationError, "malformed or ambiguous"):
            runner.classify_external_vxlan_fdb([{
                "mac": "02:00:00:00:00:11", "dst": "198.18.30.2",
                "flags": ["extern_learn"], "state": "permanent",
            }], "02:00:00:00:00:11", "198.18.30.2")

    def test_inventory_vxlan_identity_requires_interface_vni_and_underlay(self):
        identity = runner.validate_external_vxlan_identity(external_vxlan_link())
        self.assertEqual(identity["vni"], 1000)
        wrong = external_vxlan_link()
        wrong[0]["linkinfo"]["info_data"]["id"] = 1001
        with self.assertRaisesRegex(isolated.IsolationError, "interface/VNI identity"):
            runner.validate_external_vxlan_identity(wrong)

    def test_inventory_fdb_cleanup_preserves_route_learned_and_bridge_entries(self):
        instance = runner.OperatorInventoryType2("ctr", "a" * 64, "gobgpd", "b" * 64)
        instance.frr = "frr"
        seeds = {"workerEndpoints": {"worker-a": {"externalRemoteFDB": {
            "mac": "02:00:00:00:00:11", "device": "vxlan1000", "vni": 1000,
            "remoteVTEP": "198.18.30.2", "addedByTest": False,
            "observedState": "observed-externally-learned",
        }}}}
        with mock.patch.object(instance, "docker") as docker, \
                mock.patch.object(instance, "record"):
            result = instance.cleanup_inventory_forwarding_fdb(seeds)
        self.assertEqual(result["worker-a"]["status"], "PRESERVED_EXISTING")
        docker.assert_not_called()

    def test_inventory_fdb_cleanup_deletes_only_fixture_owned_device_row(self):
        instance = runner.OperatorInventoryType2("ctr", "a" * 64, "gobgpd", "b" * 64)
        instance.frr = "frr"
        bridge_row = {"mac": "02:00:00:00:00:11", "master": "br1000", "vlan": 1}
        owned_row = {"mac": "02:00:00:00:00:11", "dst": "198.18.30.2",
                     "flags": ["self"], "state": "permanent"}
        responses = [json.dumps([bridge_row, owned_row]), json.dumps([bridge_row])]
        seeds = {"workerEndpoints": {"worker-a": {"externalRemoteFDB": {
            "mac": owned_row["mac"], "device": "vxlan1000", "vni": 1000,
            "remoteVTEP": "198.18.30.2", "addedByTest": True,
        }}}}
        with mock.patch.object(instance, "docker", side_effect=lambda *args: (
                types.SimpleNamespace(stdout=responses.pop(0)) if args[2:4] == ("bridge", "-j")
                else types.SimpleNamespace(stdout=""))) as docker, \
                mock.patch.object(instance, "record"):
            result = instance.cleanup_inventory_forwarding_fdb(seeds)
        self.assertEqual(result["worker-a"]["status"], "REMOVED_EXACT_ENTRY")
        delete = next(call.args for call in docker.call_args_list
                      if call.args[2:4] == ("bridge", "fdb") and "del" in call.args)
        self.assertEqual(delete[4:], ("del", owned_row["mac"], "dev", "vxlan1000", "dst",
                                      "198.18.30.2", "self"))

    def test_inventory_bridge_fdb_cleanup_preserves_observed_existing_entry(self):
        instance = runner.OperatorInventoryType2("ctr", "a" * 64, "gobgpd", "b" * 64)
        instance.frr = "frr"
        entry = {"mac": runner.REMOTE_MAC, "ifname": "frr-host0", "flags": [],
                 "master": runner.EXTERNAL_BRIDGE, "state": ""}
        seeds = {"remoteEndpoint": {"bridgeFDB": {
            "mac": runner.REMOTE_MAC, "device": "frr-host0", "bridge": runner.EXTERNAL_BRIDGE,
            "entry": entry, "addedByTest": False,
        }}}
        with mock.patch.object(instance, "docker") as docker, \
                mock.patch.object(instance, "record"):
            result = instance.cleanup_inventory_forwarding_fdb(seeds)
        self.assertEqual(result["remoteEndpointBridge"]["status"], "PRESERVED_EXISTING")
        docker.assert_not_called()

    def test_inventory_bridge_fdb_cleanup_removes_only_exact_fixture_entry(self):
        instance = runner.OperatorInventoryType2("ctr", "a" * 64, "gobgpd", "b" * 64)
        instance.frr = "frr"
        entry = {"mac": runner.REMOTE_MAC, "ifname": "frr-host0", "flags": ["master"],
                 "master": runner.EXTERNAL_BRIDGE, "state": "permanent"}
        seeds = {"remoteEndpoint": {"bridgeFDB": {
            "mac": runner.REMOTE_MAC, "device": "frr-host0", "bridge": runner.EXTERNAL_BRIDGE,
            "entry": entry, "addedByTest": True,
        }}}
        responses = [json.dumps([entry]), "[]"]
        def docker(*args):
            output = responses.pop(0) if args[2:4] == ("bridge", "-j") else ""
            return types.SimpleNamespace(stdout=output)
        with mock.patch.object(instance, "docker", side_effect=docker) as docker_mock, \
                mock.patch.object(instance, "record"):
            result = instance.cleanup_inventory_forwarding_fdb(seeds)
        self.assertEqual(result["remoteEndpointBridge"]["status"], "REMOVED_EXACT_ENTRY")
        delete = next(call.args for call in docker_mock.call_args_list
                      if call.args[2:4] == ("bridge", "fdb") and "del" in call.args)
        self.assertEqual(delete[4:], ("del", runner.REMOTE_MAC, "dev", "frr-host0", "master"))

    def test_inventory_bridge_fdb_cleanup_refuses_changed_entry_identity(self):
        instance = runner.OperatorInventoryType2("ctr", "a" * 64, "gobgpd", "b" * 64)
        instance.frr = "frr"
        owned_entry = {"mac": runner.REMOTE_MAC, "ifname": "frr-host0", "flags": ["master"],
                       "master": runner.EXTERNAL_BRIDGE, "state": "permanent"}
        changed_entry = dict(owned_entry, flags=[])
        seeds = {"remoteEndpoint": {"bridgeFDB": {
            "mac": runner.REMOTE_MAC, "device": "frr-host0", "bridge": runner.EXTERNAL_BRIDGE,
            "entry": owned_entry, "addedByTest": True,
        }}}
        with mock.patch.object(instance, "docker", return_value=types.SimpleNamespace(
                stdout=json.dumps([changed_entry]))) as docker, \
                mock.patch.object(instance, "record"):
            with self.assertRaisesRegex(isolated.IsolationError, "identity changed"):
                instance.cleanup_inventory_forwarding_fdb(seeds)
        self.assertEqual(docker.call_count, 1)

    def test_worker_cni_executable_staging_rejects_unexpected_destination(self):
        instance = isolated.IsolatedSecondaryCNI()
        with tempfile.TemporaryDirectory() as directory:
            source = Path(directory) / "artifact"
            source.write_bytes(b"artifact")
            with mock.patch.object(instance, "docker") as docker, \
                    mock.patch.object(instance, "worker") as worker:
                with self.assertRaisesRegex(isolated.IsolationError, "unexpected worker"):
                    instance.stage_worker_executable("worker-a", source, "/tmp/foreign")
            docker.assert_not_called()
            worker.assert_not_called()

    def test_inventory_failure_capture_selects_resources_and_both_journals(self):
        instance = runner.OperatorInventoryType2("ctr", "a" * 64, "gobgpd", "b" * 64)
        core, _, ds, pods, nodes, _, cm, journals = inventory_fixture()
        instance.workers = [n["metadata"]["name"] for n in nodes]
        for pod in pods:
            pod["metadata"]["name"] = pod["spec"]["nodeName"] + "-agent"
        def kubectl(*args, **kwargs):
            if "evpncore" in args:
                body = core
            elif "evpn-worker-inventory" in args:
                body = cm
            elif "daemonset" in args:
                body = ds
            elif "pods" in args:
                body = {"items": pods}
            else:
                body = {}
            return types.SimpleNamespace(returncode=0, stdout=json.dumps(body), stderr="")
        def worker(node, *args, **kwargs):
            if node == "worker-b":
                return types.SimpleNamespace(returncode=1, stdout="", stderr="missing")
            return types.SimpleNamespace(returncode=0, stdout=journals[node], stderr="")
        with mock.patch.object(instance, "k", side_effect=kubectl) as k, \
                mock.patch.object(instance, "worker", side_effect=worker), \
                mock.patch.object(instance, "record") as record:
            instance.capture_workload_failure()
        commands = [call.args for call in k.call_args_list]
        self.assertIn(("-n", "evpn-system", "get", "evpncore", "evpn-core", "-o", "json"), commands)
        self.assertFalse(any("secondary" in command for command in commands))
        evidence = record.call_args.args[1]
        self.assertTrue(evidence["comparison"]["workers"]["worker-a"]["available"])
        self.assertFalse(evidence["comparison"]["workers"]["worker-b"]["available"])
        self.assertEqual(evidence["comparison"]["workers"]["worker-b"]["parseError"], "missing or invalid journal")
        self.assertIn("podLogs:worker-a-agent", evidence)
        self.assertIn("podLogs:worker-b-agent", evidence)

    def test_inventory_comparison_handles_uncommitted_null_state(self):
        core, _, _, _, _, _, cm, journals = inventory_fixture()
        envelope = json.loads(journals["worker-a"])
        envelope["Record"]["State"] = None
        result = runner.inventory_evidence_comparisons(core, cm, {
            "worker-a": {"returncode": 0, "stdout": json.dumps(envelope)}})
        self.assertEqual(result["workers"]["worker-a"]["state"], "uncommitted")

    def test_inventory_comparison_exposes_revision_and_digest_mismatches(self):
        core, _, _, _, _, _, cm, journals = inventory_fixture()
        cm["data"]["revision"] = "9"
        cm["data"]["sha256"] = "wrong"
        entries = {node: {"returncode": 0, "stdout": raw} for node, raw in journals.items()}
        result = runner.inventory_evidence_comparisons(core, cm, entries)
        self.assertEqual(result["publication"]["expectedRevision"], 1)
        self.assertEqual(result["publication"]["observedRevision"], "9")
        self.assertNotEqual(result["publication"]["computedDigest"], result["publication"]["observedDigest"])
        self.assertEqual(result["workers"]["worker-a"]["expectedDigest"],
                         result["workers"]["worker-a"]["observedDigest"])

    def test_inventory_redacts_secrets_and_multiline_private_keys(self):
        secret = "very-private-value"
        evidence = {"env": [{"name": "FOO", "value": secret}],
                    "nested": json.dumps({"password": secret, "uid": "visible"}),
                    "logs": "normal line\n-----BEGIN PRIVATE KEY-----\n" + secret +
                            "\n-----END PRIVATE KEY-----\nauthorization: Bearer " + secret +
                            "\nBearer " + secret + "\n"}
        redacted = json.dumps(runner.redact_inventory_evidence(evidence))
        self.assertNotIn(secret, redacted)
        self.assertIn("visible", redacted)
        self.assertIn("normal line", redacted)

    def test_inventory_configuration_renders_each_real_identity_without_peer_crosslink(self):
        inputs = inventory_inputs()
        result = runner.inventory_configuration(inputs)
        self.assertEqual(result["scenario"], runner.INVENTORY_SCENARIO)
        self.assertEqual(result["status"], "CONFIGURATION_ONLY")
        core = result["manifests"]["evpncore.yaml"]
        self.assertNotIn("<NODE", core)
        self.assertIn(inputs["workers"][0]["node"]["metadata"]["uid"], core)
        self.assertIn(inputs["workers"][1]["chassis"], core)
        for w in inputs["workers"]:
            name, address = w["node"]["metadata"]["name"], w["vtep"]
            cfg = result["workers"][name]
            self.assertEqual(cfg["export.json"]["Recovery"]["Domain"]["NodeUID"], w["node"]["metadata"]["uid"])
            self.assertEqual(cfg["import.json"]["Import"]["Networks"][0]["AllowedVTEPs"], [inputs["remoteVTEP"]])
            self.assertEqual(cfg["cni.json"]["Networks"], {"tenant-a": "tenant-a"})
            self.assertEqual(cfg["provider.json"]["standalone"]["goBGPConfigSHA256"],
                             hashlib.sha256(cfg["gobgpd.yaml"].encode()).hexdigest())
            self.assertIn(inputs["remoteVTEP"], cfg["gobgpd.yaml"])
            other_vtep = next(other["vtep"] for other in inputs["workers"] if other is not w)
            self.assertNotIn(f'neighbor-address: "{other_vtep}"', cfg["gobgpd.yaml"])
            self.assertEqual(cfg["provider.json"]["standalone"]["routerID"], address)
        self.assertIn("neighbor 198.18.10.2 remote-as", result["manifests"]["frr.conf"])
        self.assertIn("neighbor 198.18.10.3 remote-as", result["manifests"]["frr.conf"])
        self.assertIn("  enabled: false\n", result["manifests"]["evpncore-disabled.yaml"])
        for w in inputs["workers"]:
            cmds = result["datapathCommands"]["workers"][w["node"]["metadata"]["name"]]
            self.assertIn(["ip", "link", "add", "evpnvx1000", "type", "vxlan", "id", "1000",
                           "local", w["vtep"], "dstport", "60100", "nolearning"], cmds)
        self.assertIn(["ip", "link", "add", "vxlan1000", "type", "vxlan", "id", "1000",
                       "local", inputs["remoteVTEP"], "dstport", "4789", "nolearning"],
                      result["datapathCommands"]["externalFRR"])

    def test_export_recovery_namespace_uses_fstat_device_and_inode_identity(self):
        cni_namespace = "net:[4026531993]"
        export_namespace = runner.export_recovery_namespace_identity(
            cni_namespace, "4:4026531993")
        self.assertEqual(export_namespace, "NS(4:4026531993)")
        self.assertNotEqual(export_namespace, cni_namespace)
        with self.assertRaisesRegex(isolated.IsolationError, "identities differ"):
            runner.export_recovery_namespace_identity(cni_namespace, "4:4026531994")

    def test_inventory_configuration_refuses_untrusted_shapes_and_duplicate_identities(self):
        for field in ("name", "uid", "chassis", "vtep", "serviceUID"):
            with self.subTest(field=field):
                inputs = inventory_inputs()
                a, b = inputs["workers"]
                if field in ("name", "uid"):
                    b["node"]["metadata"][field] = a["node"]["metadata"][field]
                else:
                    b[field] = a[field]
                with self.assertRaisesRegex(isolated.IsolationError, "duplicate"):
                    runner.inventory_configuration(inputs)
        inputs = inventory_inputs()
        inputs["workers"][0]["gobgpdSHA256"] = "<HASH>"
        with self.assertRaisesRegex(isolated.IsolationError, "gobgpdSHA256"):
            runner.inventory_configuration(inputs)

    def test_inventory_addresses_are_not_rewritten_after_template_expansion(self):
        inputs = inventory_inputs()
        inputs["workers"][0]["vtep"] = "192.0.2.12"
        inputs["workers"][1]["vtep"] = "192.0.2.11"
        inputs["remoteVTEP"] = "192.0.2.30"
        result = runner.inventory_configuration(inputs)
        for w in inputs["workers"]:
            cfg = result["workers"][w["node"]["metadata"]["name"]]
            self.assertEqual(cfg["export.json"]["RouterID"], w["vtep"])
            self.assertEqual(cfg["import.json"]["Import"]["LocalVTEP"], w["vtep"])
            self.assertIn(f'router-id: "{w["vtep"]}"', cfg["gobgpd.yaml"])

    def test_inventory_check_is_offline_and_live_mode_refuses_to_launch(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "input.json"
            path.write_text(json.dumps(inventory_inputs()))
            with mock.patch.object(runner.lab, "exclusive") as exclusive, \
                    mock.patch.object(runner.OperatorSecondaryCNI, "run") as run, \
                    contextlib.redirect_stdout(io.StringIO()) as output:
                runner.main(["--scenario", runner.INVENTORY_SCENARIO, "--check-config", str(path)])
                self.assertEqual(json.loads(output.getvalue())["status"], "CONFIGURATION_ONLY")
                exclusive.assert_not_called()
                run.assert_not_called()
            with mock.patch.object(runner.lab, "exclusive") as exclusive, \
                    contextlib.redirect_stderr(io.StringIO()):
                with self.assertRaises(SystemExit):
                    runner.main(["--scenario", runner.INVENTORY_SCENARIO, "--confirm-disposable-host"])
                exclusive.assert_not_called()

    def test_inventory_collector_checks_each_host_fence_and_agent(self):
        instance = runner.OperatorSecondaryCNI()
        core, lease, ds, pods, nodes, selector, cm, journals = inventory_fixture()
        instance.workers = [n["metadata"]["name"] for n in nodes]
        agent = core["spec"]["nodeAgent"]
        agent.update(exportConfigPath="/run/export.json", remoteConfigPath="/run/import.json",
                     type2JournalHostPath="/var/lib/evpn-worker")
        template = ds["spec"]["template"]["spec"]
        template.update(serviceAccountName="evpn-provider-observer", automountServiceAccountToken=True)
        args = template["containers"][0]["args"]
        args.extend(["--evpn-export-config=/run/export.json", "--evpn-remote-config=/run/import.json"])
        for pod in pods:
            pod["metadata"]["name"] = pod["spec"]["nodeName"] + "-agent"
            pod["spec"].update(serviceAccountName="evpn-provider-observer", automountServiceAccountToken=True)
            pod["spec"]["containers"] = [{"name": "agent", "args": list(args)}]
        responses = [core, lease, ds, {"items": pods}, *nodes, cm]
        def worker(node, *args):
            return types.SimpleNamespace(stdout=journals[node])
        def kubectl(*args):
            pod_name = args[3]
            return types.SimpleNamespace(stdout=pod_name.removesuffix("-agent") + "\n")
        with mock.patch.object(instance, "get_json", side_effect=responses), \
                mock.patch.object(instance, "worker", side_effect=worker) as read, \
                mock.patch.object(instance, "k", side_effect=kubectl), \
                mock.patch.object(instance, "record") as record:
            instance.verify_inventory_operator("cni-test", "secondary", selector)
        self.assertEqual(read.call_count, 2)
        self.assertEqual(record.call_args.args[1]["runtimeHealth"], "UNCONFIRMED")

    def test_inventory_collector_rejects_changed_discovered_chassis_before_evidence(self):
        instance = runner.OperatorSecondaryCNI()
        core, _, _, _, nodes, _, _, _ = inventory_fixture()
        expected = {n["metadata"]["name"]: {"nodeUID": n["metadata"]["uid"],
                                             "chassis": "different-chassis"}
                    for n in nodes}
        with mock.patch.object(instance, "get_json", return_value=core), \
                mock.patch.object(instance, "worker") as worker:
            with self.assertRaisesRegex(isolated.IsolationError, "discovered identities"):
                instance.verify_inventory_operator(
                    "cni-test", "secondary", {}, expected_identities=expected,
                    expected_installation=core["metadata"]["uid"],
                    expected_cluster=core["spec"]["workerInventory"]["clusterUID"])
            worker.assert_not_called()

    def test_inventory_entry_point_runs_explicit_authorized_scenario(self):
        with tempfile.TemporaryDirectory() as directory:
            binary = Path(directory) / "binary"
            binary.write_bytes(b"fixture")
            digest = hashlib.sha256(binary.read_bytes()).hexdigest()
            argv = ["--scenario", runner.INVENTORY_SCENARIO, "--confirm-disposable-host",
                    "--continuity-ctr", str(binary), "--continuity-ctr-sha256", digest,
                    "--inventory-gobgpd", str(binary), "--inventory-gobgpd-sha256", digest]
            with mock.patch.object(runner.lab, "exclusive") as exclusive, \
                    mock.patch.object(runner.OperatorInventoryType2, "run") as run:
                runner.main(argv)
                exclusive.assert_called_once()
                run.assert_called_once()

    def test_inventory_peerpidfd_probe_runs_in_each_ready_agent_container(self):
        instance = runner.OperatorInventoryType2("ctr", "a" * 64, "gobgpd", "b" * 64)
        instance.workers = ["worker-a", "worker-b"]
        pods = {"items": [{
            "metadata": {"name": node + "-agent"},
            "spec": {"nodeName": node},
            "status": {"phase": "Running", "containerStatuses": [
                {"name": "agent", "ready": True}]},
        } for node in instance.workers]}
        probe = {"operation": "getsockopt(SOL_SOCKET, SO_PEERPIDFD)",
                 "socket": "AF_UNIX/SOCK_STREAM", "peerPidFD": 4,
                 "descriptor": "anon_inode:[pidfd]"}
        with mock.patch.object(instance, "get_json", return_value=pods), \
                mock.patch.object(instance, "k", return_value=types.SimpleNamespace(
                    stdout=json.dumps(probe))) as kubectl, \
                mock.patch.object(instance, "record") as record:
            instance.verify_agent_peerpidfd()
        self.assertEqual(len(kubectl.call_args_list), 2)
        self.assertEqual([call.args[3] for call in kubectl.call_args_list],
                         ["worker-a-agent", "worker-b-agent"])
        self.assertEqual([call.args[0] for call in record.call_args_list],
                         ["peerpidfd-container-worker-a.json",
                          "peerpidfd-container-worker-b.json"])

    def test_inventory_peerpidfd_probe_rejects_missing_kernel_operation(self):
        instance = runner.OperatorInventoryType2("ctr", "a" * 64, "gobgpd", "b" * 64)
        instance.workers = ["worker-a"]
        pods = {"items": [{
            "metadata": {"name": "worker-a-agent"},
            "spec": {"nodeName": "worker-a"},
            "status": {"phase": "Running", "containerStatuses": [
                {"name": "agent", "ready": True}]},
        }]}
        with mock.patch.object(instance, "get_json", return_value=pods), \
                mock.patch.object(instance, "k", return_value=types.SimpleNamespace(
                    stdout=json.dumps({"operation": "pidfd_open"}))):
            with self.assertRaisesRegex(isolated.IsolationError, "peer pidfd probe failed"):
                instance.verify_agent_peerpidfd()

    def test_inventory_forwarding_parsers_require_tunnel_counters_and_zero_loss(self):
        ports = ("OFPST_PORT reply (xid=0x2): 4 ports\n"
                 "  port  2: rx pkts=7, bytes=600, drop=0, errs=0\n"
                 "           tx pkts=9, bytes=800, drop=0, errs=0\n")
        self.assertEqual(runner.parse_ovs_port_counters(ports, 2),
                         {"rxPackets": 7, "txPackets": 9})
        with self.assertRaisesRegex(isolated.IsolationError, "counters are unavailable"):
            runner.parse_ovs_port_counters(ports, 3)
        self.assertEqual(
            runner.parse_inventory_ping_result(
                "3 packets transmitted, 3 received, 0% packet loss\n", 0
            )["status"],
            "PASS",
        )
        self.assertEqual(
            runner.parse_inventory_ping_result(
                "3 packets transmitted, 2 received, 33% packet loss\n", 1
            )["status"],
            "FAIL",
        )
        invalid_ping = runner.parse_inventory_ping_result(
            "", 2
        )
        self.assertEqual(invalid_ping["status"], "SETUP_FAILURE")
        self.assertIsNone(invalid_ping["transmitted"])
        self.assertEqual(
            runner.parse_inventory_ping_result(
                "ping: unrecognized option: e\n", 1
            )["status"],
            "SETUP_FAILURE",
        )
        setup_error = runner.inventory_ping_failure(
            "worker-a", "external-to-workload", invalid_ping, 1,
            "ping: unrecognized option: e\n",
        )
        self.assertEqual(setup_error.failure_class, "WORKLOAD_STIMULUS_SETUP_FAILURE")
        self.assertIn("unrecognized option: e", str(setup_error))
        self.assertEqual(
            runner.inventory_forwarding_failure_status(True, setup_error.failure_class), "BLOCKED"
        )
        packet_loss = runner.parse_inventory_ping_result(
            "3 packets transmitted, 0 received, 100% packet loss\n", 1
        )
        loss_error = runner.inventory_ping_failure(
            "worker-a", "external-to-workload", packet_loss, 1, ""
        )
        self.assertEqual(loss_error.failure_class, "FUNCTIONAL_DATAPLANE_FAILURE")
        self.assertEqual(
            runner.inventory_forwarding_failure_status(True, loss_error.failure_class), "FAIL"
        )

    def test_linux_link_counter_parser_requires_exact_link_and_complete_stats(self):
        stats = {"rx": {"packets": 7, "bytes": 600, "errors": 0, "dropped": 1},
                 "tx": {"packets": 9, "bytes": 800, "errors": 0, "dropped": 2}}
        output = json.dumps([{"ifname": "vxlan1000", "ifindex": 4, "stats64": stats}])
        self.assertEqual(
            runner.parse_linux_link_counters(output, "vxlan1000"),
            {"ifname": "vxlan1000", "ifindex": 4, **stats},
        )
        with self.assertRaisesRegex(isolated.IsolationError, "ambiguous link identity"):
            runner.parse_linux_link_counters(output, "eth0")
        incomplete = json.dumps([{"ifname": "vxlan1000", "ifindex": 4,
                                  "stats64": {"rx": stats["rx"], "tx": {"packets": 1}}}])
        with self.assertRaisesRegex(isolated.IsolationError, "tx bytes counter"):
            runner.parse_linux_link_counters(incomplete, "vxlan1000")

    def test_inventory_capture_statistics_require_kernel_zero_drop_and_all_counters(self):
        stats = runner.parse_capture_statistics(
            "12 packets captured\n12 packets received by filter\n0 packets dropped by kernel\n"
        )
        self.assertEqual(stats, {
            "captured": 12,
            "receivedByFilter": 12,
            "droppedByKernel": 0,
        })
        with self.assertRaisesRegex(isolated.IsolationError, "dropped packets"):
            runner.parse_capture_statistics(
                "12 packets captured\n12 packets received by filter\n1 packets dropped by kernel\n"
            )
        with self.assertRaisesRegex(isolated.IsolationError, "missing receivedByFilter"):
            runner.parse_capture_statistics("12 packets captured\n0 packets dropped by kernel\n")
        self.assertTrue(runner.validate_capture_statistics(stats, 12))
        with self.assertRaisesRegex(isolated.IsolationError, "complete bounded capture"):
            runner.validate_capture_statistics(
                {"captured": 11, "receivedByFilter": 12, "droppedByKernel": 0}, 12
            )

    def test_inventory_workload_identity_parser_and_counter_delta_require_exact_identity(self):
        stats = {
            "rx": {"packets": 10, "bytes": 1000, "errors": 0, "dropped": 0},
            "tx": {"packets": 10, "bytes": 1000, "errors": 0, "dropped": 0},
        }
        before = runner.parse_inventory_workload_snapshot(
            "net:[4026532912]",
            json.dumps([{"ifname": "net1", "ifindex": 12, "address": "02:00:00:00:00:11", "stats64": stats}]),
            json.dumps([{"addr_info": [{"family": "inet", "local": "10.244.240.11"}]}]),
            "net1",
            "02:00:00:00:00:11",
            "10.244.240.11",
        )
        stats_after = {
            "rx": {"packets": 13, "bytes": 1300, "errors": 0, "dropped": 0},
            "tx": {"packets": 14, "bytes": 1400, "errors": 0, "dropped": 0},
        }
        after = runner.parse_inventory_workload_snapshot(
            "net:[4026532912]",
            json.dumps([{"ifname": "net1", "ifindex": 12, "address": "02:00:00:00:00:11", "stats64": stats_after}]),
            json.dumps([{"addr_info": [{"family": "inet", "local": "10.244.240.11"}]}]),
            "net1",
            "02:00:00:00:00:11",
            "10.244.240.11",
        )
        delta = runner.inventory_workload_counter_delta(before, after)
        self.assertEqual(delta["rx"]["packets"], 3)
        self.assertEqual(delta["tx"]["packets"], 4)
        changed = dict(after)
        changed["namespace"] = "net:[4026533000]"
        with self.assertRaisesRegex(isolated.IsolationError, "identity changed"):
            runner.inventory_workload_counter_delta(before, changed)
        with self.assertRaisesRegex(isolated.IsolationError, "did not advance"):
            runner.inventory_workload_counter_delta(before, before)

    def test_worker_linux_integration_counter_deltas_allow_zero(self):
        instance = runner.OperatorInventoryType2("ctr", "a" * 64, "gobgpd", "b" * 64)
        instance.workers = ["worker-a"]
        counters = {
            "rx": {"packets": 5, "bytes": 500, "errors": 0, "dropped": 0},
            "tx": {"packets": 6, "bytes": 600, "errors": 0, "dropped": 0},
        }
        snapshot = {
            "bridge": {"name": "evpnbr1000", "counters": {"ifindex": 10, **counters}},
            "vxlan": {"name": "evpnvx1000", "counters": {"ifindex": 11, **counters}},
            "underlay": {"name": "eth0", "counters": {"ifindex": 2, **counters}},
        }
        before = {"worker-a": snapshot}
        after = json.loads(json.dumps(before))
        delta = instance.worker_linux_counter_delta(before, after)
        self.assertEqual(delta["worker-a"]["vxlan"]["rx"]["packets"], 0)
        self.assertEqual(delta["worker-a"]["bridge"]["tx"]["packets"], 0)

    def test_expected_ovs_counters_must_advance(self):
        before = {"rxPackets": 2, "txPackets": 3}
        after = {"rxPackets": 5, "txPackets": 7}
        self.assertEqual(
            runner.inventory_ovs_counter_delta(before, after, 4, 8),
            {"rxPackets": 3, "txPackets": 4, "remoteType2FlowPackets": 4},
        )
        with self.assertRaisesRegex(isolated.IsolationError, "did not advance"):
            runner.inventory_ovs_counter_delta(before, before, 4, 4)

    def test_inventory_workload_packet_correlation_requires_request_reply_identity_pairs(self):
        endpoint = {
            "node": "worker-a", "ip": "10.244.240.11",
            "icmpIdentifiers": {"workerToExternal": 20000, "externalToWorker": 30000},
        }
        packets = []
        for identifier in (1, 2, 3):
            packets.extend([
                {"source": endpoint["ip"], "destination": runner.REMOTE_IP, "icmpType": 8,
                 "icmpIdentifier": 20000, "icmpSequence": identifier},
                {"source": runner.REMOTE_IP, "destination": endpoint["ip"], "icmpType": 0,
                 "icmpIdentifier": 20000, "icmpSequence": identifier},
                {"source": runner.REMOTE_IP, "destination": endpoint["ip"], "icmpType": 8,
                 "icmpIdentifier": 30000, "icmpSequence": identifier},
                {"source": endpoint["ip"], "destination": runner.REMOTE_IP, "icmpType": 0,
                 "icmpIdentifier": 30000, "icmpSequence": identifier},
            ])
        summary = runner.verify_inventory_workload_packets(packets, endpoint)
        self.assertEqual(summary["status"], "PASS")
        broken = [*packets, {
            "source": endpoint["ip"], "destination": runner.REMOTE_IP, "icmpType": 8,
            "icmpIdentifier": 29999, "icmpSequence": 1,
        }]
        with self.assertRaisesRegex(isolated.IsolationError, "unrelated ICMP identities"):
            runner.verify_inventory_workload_packets(broken, endpoint)

    def test_inventory_evidence_manifest_enforces_allowlisted_regular_files(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory)
            required_underlay = [
                "inventory-worker-a-underlay.pcap",
                "inventory-worker-b-underlay.pcap",
                "inventory-frr-underlay.pcap",
            ]
            for name in required_underlay:
                (path / name).write_bytes(b"fixture pcap")
            artifacts = runner.evidence_file_manifest(path, required_underlay)
            self.assertEqual(
                [artifact["path"] for artifact in artifacts], sorted(required_underlay)
            )
            with self.assertRaisesRegex(isolated.IsolationError, "unsafe artifact path"):
                runner.evidence_file_manifest(path, ["../escape"])
            with self.assertRaisesRegex(isolated.IsolationError, "unsafe artifact path"):
                runner.evidence_file_manifest(path, ["private-key.pem"])
            (path / "inventory-worker-b-net1.pcap").symlink_to(
                "inventory-worker-a-net1.pcap"
            )
            with self.assertRaisesRegex(isolated.IsolationError, "missing or unsafe"):
                runner.evidence_file_manifest(path, ["inventory-worker-b-net1.pcap"])

    def test_inventory_ovs_evidence_uses_worker_aliases_for_complete_final_manifest(self):
        instance = runner.OperatorInventoryType2("ctr", "a" * 64, "gobgpd", "b" * 64)
        instance.workers = ["cni-sec-8fe5b811-worker", "cni-sec-8fe5b811-worker2"]
        remote_flow = f"cookie=0x1, n_packets=4, dl_dst={runner.REMOTE_MAC} actions=output:5"

        def runtime_outputs(endpoint_mac, chassis):
            values = ("vxlan0", "br-int", "5", "{}", "{}", chassis,
                      f"{remote_flow}\ncookie=0x2, n_packets=3, dl_dst={endpoint_mac} actions=output:1",
                      "  port  5: rx pkts=7, bytes=0\n           tx pkts=9, bytes=0")
            return [types.SimpleNamespace(stdout=value) for value in values]

        with tempfile.TemporaryDirectory() as directory:
            instance.directory = Path(directory)
            outputs = []
            for _phase in ("before", "after"):
                for index, _node in enumerate(instance.workers):
                    outputs.extend(runtime_outputs(f"02:00:00:00:00:1{index + 1}", f"chassis-{index}"))
            with mock.patch.object(instance, "runtime", side_effect=outputs):
                for phase in ("before", "after"):
                    for index, node in enumerate(instance.workers):
                        snapshot = instance.collect_ovs_dataplane(
                            node, {"mac": f"02:00:00:00:00:1{index + 1}"}, f"chassis-{index}", phase=phase
                        )
                        self.assertEqual(snapshot["node"], node)
                        self.assertEqual(snapshot["workerAlias"], "worker-a" if index == 0 else "worker-b")
            ovs_required = {
                f"ovs-dataplane{suffix}-worker-{alias}.json"
                for suffix in ("", "-after") for alias in ("a", "b")
            }
            self.assertEqual({path.name for path in instance.directory.iterdir()}, ovs_required)
            recorded = json.loads((instance.directory / "ovs-dataplane-after-worker-b.json").read_text())
            self.assertEqual(recorded["node"], instance.workers[1])
            for name in runner.INVENTORY_EVIDENCE_ARTIFACTS - ovs_required:
                (instance.directory / name).write_bytes(b"fixture")
            artifacts = runner.evidence_file_manifest(
                instance.directory, sorted(runner.INVENTORY_EVIDENCE_ARTIFACTS)
            )
            self.assertEqual({item["path"] for item in artifacts}, set(runner.INVENTORY_EVIDENCE_ARTIFACTS))
            (instance.directory / "ovs-dataplane-worker-a.json").unlink()
            with self.assertRaisesRegex(isolated.IsolationError,
                                        "missing or unsafe: ovs-dataplane-worker-a.json"):
                runner.evidence_file_manifest(instance.directory, sorted(runner.INVENTORY_EVIDENCE_ARTIFACTS))
        with self.assertRaisesRegex(isolated.IsolationError, "not an inventory worker"), \
                mock.patch.object(instance, "runtime", side_effect=runtime_outputs("02:00:00:00:00:11", "x")):
            instance.collect_ovs_dataplane("foreign-node", {"mac": "02:00:00:00:00:11"}, "x")

    def test_inventory_manifest_has_separate_final_checksum(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory)
            manifest = path / "inventory-evidence-manifest.json"
            manifest.write_text('{"status":"PASS"}\n')
            digest = runner.write_inventory_manifest_checksum(path)
            checksum = (path / "inventory-evidence-manifest.sha256").read_text()
            self.assertEqual(digest, hashlib.sha256(manifest.read_bytes()).hexdigest())
            self.assertTrue(checksum.startswith(digest + "  "))
            with self.assertRaisesRegex(isolated.IsolationError, "pre-existing"):
                runner.write_inventory_manifest_checksum(path)

    def test_inventory_source_provenance_requires_receipt_archive_and_matching_hash(self):
        provenance = {
            "commit": "a" * 40,
            "archiveSHA256": hashlib.sha256(b"archive").hexdigest(),
            "archiveFormat": "tar",
            "archiveCommand": "git archive --format=tar " + "a" * 40,
        }
        environment = {
            "EVPN_SOURCE_COMMIT": provenance["commit"],
            "EVPN_SOURCE_ARCHIVE_SHA256": provenance["archiveSHA256"],
            "EVPN_SOURCE_ARCHIVE_FORMAT": provenance["archiveFormat"],
            "EVPN_SOURCE_ARCHIVE_COMMAND": provenance["archiveCommand"],
        }
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory)
            receipt = path / "provenance.json"
            archive = path / "source.tar"
            receipt.write_text(json.dumps(provenance))
            archive.write_bytes(b"archive")
            self.assertEqual(
                runner.verify_inventory_source_provenance(receipt, archive, environment),
                provenance,
            )
            archive.write_bytes(b"tampered")
            with self.assertRaisesRegex(isolated.IsolationError, "SHA256 mismatch"):
                runner.verify_inventory_source_provenance(receipt, archive, environment)
            archive.write_bytes(b"archive")
            mismatched_environment = dict(environment, EVPN_SOURCE_COMMIT="b" * 40)
            with self.assertRaisesRegex(isolated.IsolationError, "receipt"):
                runner.verify_inventory_source_provenance(
                    receipt, archive, mismatched_environment
                )
            with self.assertRaisesRegex(isolated.IsolationError, "missing"):
                runner.verify_inventory_source_provenance(receipt, archive, {})
            worktree = dict(provenance, archiveKind="worktree", workingTreeDiffSHA256="d" * 64,
                            archiveCommand="git ls-files --cached --others --exclude-standard -z | sort -zu | tar --null -T - -cf -")
            worktree_environment = dict(environment, EVPN_SOURCE_ARCHIVE_KIND="worktree",
                                       EVPN_SOURCE_WORKTREE_DIFF_SHA256="d" * 64,
                                       EVPN_SOURCE_ARCHIVE_COMMAND=worktree["archiveCommand"])
            receipt.write_text(json.dumps(worktree))
            self.assertEqual(runner.verify_inventory_source_provenance(receipt, archive, worktree_environment), worktree)
            with self.assertRaisesRegex(isolated.IsolationError, "receipt"):
                runner.verify_inventory_source_provenance(receipt, archive, environment)
            worktree_environment.pop("EVPN_SOURCE_WORKTREE_DIFF_SHA256")
            with self.assertRaisesRegex(isolated.IsolationError, "diff provenance"):
                runner.verify_inventory_source_provenance(receipt, archive, worktree_environment)
            receipt.unlink()
            with self.assertRaisesRegex(isolated.IsolationError, "missing/unsafe"):
                runner.verify_inventory_source_provenance(receipt, archive, environment)

    def test_inventory_capture_verification_correlates_both_icmp_directions(self):
        import external_frr

        def capture(path, **expected):
            local = expected["local_vtep"]
            remote = expected["remote_vtep"]
            local_mac, remote_mac = expected["local_mac"], expected["remote_mac"]
            local_ip, remote_ip = expected["local_ip"], expected["remote_ip"]
            worker_identifier = 20000 if local_ip.endswith(".11") else 20001
            external_identifier = 30000 if local_ip.endswith(".11") else 30001
            packets = []
            for identifier, source, destination, source_mac, destination_mac, source_ip, destination_ip, kind in (
                    (worker_identifier, local, remote, local_mac, remote_mac, local_ip, remote_ip, 8),
                    (worker_identifier, remote, local, remote_mac, local_mac, remote_ip, local_ip, 0),
                    (external_identifier, remote, local, remote_mac, local_mac, remote_ip, local_ip, 8),
                    (external_identifier, local, remote, local_mac, remote_mac, local_ip, remote_ip, 0)):
                for sequence in range(1, 4):
                    packets.append({
                        "outerSource": source, "outerDestination": destination,
                        "sourceMac": source_mac, "destinationMac": destination_mac,
                        "sourceIp": source_ip, "destinationIp": destination_ip,
                        "vni": expected["vni"], "type": kind, "id": identifier,
                        "seq": sequence,
                    })
            return {"sha256": "a" * 64, "packets": packets}

        endpoints = [
            {"node": "worker-a", "ip": "10.244.240.11", "mac": "02:00:00:00:00:11",
             "vtep": "198.18.30.2", "capture": "inventory-worker-a-underlay.pcap",
             "icmpIdentifiers": {"workerToExternal": 20000, "externalToWorker": 30000}},
            {"node": "worker-b", "ip": "10.244.240.12", "mac": "02:00:00:00:00:12",
             "vtep": "198.18.30.3", "capture": "inventory-worker-b-underlay.pcap",
             "icmpIdentifiers": {"workerToExternal": 20001, "externalToWorker": 30001}},
        ]
        with tempfile.TemporaryDirectory() as directory, \
                mock.patch.object(external_frr, "decode_vxlan", side_effect=capture) as decode:
            for name in ("inventory-frr-underlay.pcap", "inventory-worker-a-underlay.pcap",
                         "inventory-worker-b-underlay.pcap"):
                (Path(directory) / name).write_bytes(b"fixture")
            result = runner.verify_inventory_forwarding_captures(Path(directory), endpoints)
        self.assertEqual(result["status"], "PASS")
        self.assertEqual(result["workers"]["worker-a"]["directions"]["workerToExternal"],
                         {"requestCount": 3, "replyCount": 3})
        self.assertEqual(decode.call_count, 4)
        endpoints[0]["icmpIdentifiers"]["workerToExternal"] = 29999
        with tempfile.TemporaryDirectory() as directory, \
                mock.patch.object(external_frr, "decode_vxlan", side_effect=capture):
            for name in ("inventory-frr-underlay.pcap", "inventory-worker-a-underlay.pcap",
                         "inventory-worker-b-underlay.pcap"):
                (Path(directory) / name).write_bytes(b"fixture")
            with self.assertRaisesRegex(
                isolated.IsolationError, "incomplete workerToExternal VXLAN packet identities"
            ):
                runner.verify_inventory_forwarding_captures(Path(directory), endpoints)

    def test_inventory_capture_verification_rejects_worker_frr_identity_mismatch(self):
        import external_frr

        def capture_with_worker_mismatch(path, **expected):
            local = expected["local_vtep"]
            remote = expected["remote_vtep"]
            local_mac, remote_mac = expected["local_mac"], expected["remote_mac"]
            local_ip, remote_ip = expected["local_ip"], expected["remote_ip"]
            packets = []
            for identifier, source, destination, source_mac, destination_mac, source_ip, destination_ip, kind in (
                    (20000, local, remote, local_mac, remote_mac, local_ip, remote_ip, 8),
                    (20000, remote, local, remote_mac, local_mac, remote_ip, local_ip, 0),
                    (30000, remote, local, remote_mac, local_mac, remote_ip, local_ip, 8),
                    (30000, local, remote, local_mac, remote_mac, local_ip, remote_ip, 0)):
                for sequence in range(1, 4):
                    packets.append({
                        "outerSource": source, "outerDestination": destination,
                        "sourceMac": source_mac, "destinationMac": destination_mac,
                        "sourceIp": source_ip, "destinationIp": destination_ip,
                        "vni": expected["vni"], "type": kind, "id": identifier,
                        "seq": sequence,
                    })
            result = {"sha256": "a" * 64, "packets": packets}
            if Path(path).name == "inventory-worker-a-underlay.pcap":
                for packet in result["packets"]:
                    if packet["id"] == 20000 and packet["seq"] == 1:
                        packet["id"] = 29999
                        break
            return result

        endpoints = [
            {"node": "worker-a", "ip": "10.244.240.11", "mac": "02:00:00:00:00:11",
             "vtep": "198.18.30.2", "capture": "inventory-worker-a-underlay.pcap",
             "icmpIdentifiers": {"workerToExternal": 20000, "externalToWorker": 30000}},
        ]
        with tempfile.TemporaryDirectory() as directory, \
                mock.patch.object(
                    external_frr, "decode_vxlan", side_effect=capture_with_worker_mismatch
                ):
            for name in ("inventory-frr-underlay.pcap", "inventory-worker-a-underlay.pcap"):
                (Path(directory) / name).write_bytes(b"fixture")
            with self.assertRaisesRegex(
                isolated.IsolationError, "worker capture has incomplete workerToExternal"
            ):
                runner.verify_inventory_forwarding_captures(Path(directory), endpoints)

    def test_inventory_scenario_orders_real_discovery_provision_and_activation(self):
        instance = runner.OperatorInventoryType2("ctr", "a" * 64, "gobgpd", "b" * 64)
        identities = {"worker-a": {"nodeUID": "real-a", "chassis": "chassis-a"},
                      "worker-b": {"nodeUID": "real-b", "chassis": "chassis-b"}}
        instance.workers = list(identities)
        rendered = {"workers": {"worker-a": {"vtep": "198.18.30.2"},
                                "worker-b": {"vtep": "198.18.30.3"}}}
        endpoints = {"worker-a": {"pod": "a"}, "worker-b": {"pod": "b"}}
        calls = []
        with mock.patch.object(instance, "install_operator", side_effect=lambda **kw: calls.append(("install", kw))), \
                mock.patch.object(instance, "discover_inventory_workers", side_effect=lambda: calls.append("discover") or identities), \
                mock.patch.object(instance, "prepare_disabled_core", side_effect=lambda: calls.append("disabled") or "real-installation"), \
                mock.patch.object(instance, "prepare_inventory_workers", side_effect=lambda ids, uid: calls.append(("provision", ids, uid)) or rendered), \
                mock.patch.object(instance, "provision_external_frr", side_effect=lambda value: calls.append(("frr", value))), \
                mock.patch.object(instance, "activate_inventory_core", side_effect=lambda value: calls.append(("activate", value))), \
                mock.patch.object(instance, "await_inventory_acceptance", side_effect=lambda ids, uid: calls.append(("acceptance", ids, uid))), \
                mock.patch.object(instance, "verify_agent_peerpidfd", side_effect=lambda: calls.append("peerpidfd")), \
                mock.patch.object(instance, "await_external_import", side_effect=lambda *args: calls.append(("import", *args))), \
                mock.patch.object(instance, "create_inventory_endpoints", side_effect=lambda *args: calls.append(("endpoints", *args)) or endpoints), \
                mock.patch.object(instance, "await_local_export", side_effect=lambda *args: calls.append(("export", *args)) or "path-uuid"), \
                mock.patch.object(instance, "verify_speaker_rib", side_effect=lambda *args: calls.append(("rib", *args))), \
                mock.patch.object(instance, "collect_ovs_dataplane",
                                  side_effect=lambda node, *args, **kwargs:
                                  calls.append(("ovs", node)) or {"node": node}), \
                mock.patch.object(instance, "verify_inventory_forwarding",
                                  side_effect=lambda endpoints, ids, ovs:
                                  calls.append(("forwarding", endpoints, ids))):
            instance.test_endpoints()
        self.assertEqual(calls, [("install", {"create_baseline_core": False}), "discover", "disabled",
                                ("provision", identities, "real-installation"),
                                ("frr", rendered),
                                 ("activate", rendered), ("acceptance", identities, "real-installation"),
                                 "peerpidfd",
                                 ("import", "worker-a", identities["worker-a"], "real-installation", "198.18.30.2"),
                                 ("import", "worker-b", identities["worker-b"], "real-installation", "198.18.30.3"),
                                 ("endpoints", rendered, identities),
                                 ("export", "worker-a", endpoints["worker-a"], identities["worker-a"], "real-installation"),
                                 ("rib", "worker-a", endpoints["worker-a"], identities["worker-a"], "real-installation", "path-uuid"),
                                 ("export", "worker-b", endpoints["worker-b"], identities["worker-b"], "real-installation"),
                                 ("rib", "worker-b", endpoints["worker-b"], identities["worker-b"], "real-installation", "path-uuid"),
                                 ("ovs", "worker-a"),
                                 ("ovs", "worker-b"),
                                 ("forwarding", endpoints, identities)])

    def test_local_rib_rejects_mismatched_paths(self):
        endpoint = {"vtep": "198.18.30.2", "mac": "02:00:00:00:00:11", "ip": "10.244.240.11"}
        path = {"Local": True, "PathUUID": "receipt", "NLRI": {
            "RD": "198.18.30.2:1000", "MAC": endpoint["mac"], "IP": endpoint["ip"],
            "EthernetTag": 0, "ESI": "00000000000000000000", "Labels": [1000]},
            "PathAttributes": {"Origin": "igp", "NextHop": endpoint["vtep"], "ExtendedCommunities": [
                {"Kind": "route-target", "Value": "64512:1000"}, {"Kind": "encapsulation", "Value": "vxlan"}]}}
        self.assertEqual(runner.verify_local_rib([path], endpoint, "receipt"), path)
        for field, value in (("Labels", [2000]), ("MAC", "02:00:00:00:00:12"),
                             ("IP", "10.244.240.12"), ("RD", "198.18.30.3:1000")):
            with self.subTest(field=field):
                original = path["NLRI"][field]
                path["NLRI"][field] = value
                with self.assertRaises(isolated.IsolationError):
                    runner.verify_local_rib([path], endpoint, "receipt")
                path["NLRI"][field] = original
        path["PathAttributes"]["NextHop"] = "198.18.30.3"
        with self.assertRaises(isolated.IsolationError):
            runner.verify_local_rib([path], endpoint, "receipt")
        path["PathAttributes"]["NextHop"] = endpoint["vtep"]
        path["PathUUID"] = "foreign"
        with self.assertRaises(isolated.IsolationError):
            runner.verify_local_rib([path], endpoint, "receipt")

    def test_speaker_rib_uses_attested_pid_and_receipt(self):
        instance = runner.OperatorInventoryType2("ctr", "a" * 64, "gobgpd", "b" * 64)
        endpoint = {"vtep": "198.18.30.2", "mac": "02:00:00:00:00:11", "ip": "10.244.240.11"}
        identity = {"nodeUID": "node-a"}
        speaker = {"PID": 42, "StartTicks": "123", "standalone": {
            "nodeUID": "node-a", "installationUID": "installation", "routerID": endpoint["vtep"]}}
        status = {"SpeakerProcess": speaker, "Result": {"OwnershipUnverified": False,
                                                          "Applied": [{"PathUUID": "receipt"}]}}
        with mock.patch.object(instance, "worker", side_effect=[
                types.SimpleNamespace(stdout=json.dumps(status)),
                types.SimpleNamespace(stdout="[]"),
                types.SimpleNamespace(stdout=json.dumps(status))]) as worker, \
                mock.patch.object(runner, "verify_local_rib", return_value={"path": "ok"}) as verify, \
                mock.patch.object(instance, "record") as record:
            instance.verify_speaker_rib("worker-a", endpoint, identity, "installation", "receipt")
        self.assertIn("-pid", worker.call_args_list[1].args)
        self.assertIn("42", worker.call_args_list[1].args)
        verify.assert_called_once_with([], endpoint, "receipt")
        record.assert_called_once()

    def test_ovs_collection_rejects_missing_endpoint_flow(self):
        instance = runner.OperatorInventoryType2("ctr", "a" * 64, "gobgpd", "b" * 64)
        output = ("vxlan0", "br-int", "5", "{}", "{}", "chassis-a", "no endpoint flow", "ports")
        with mock.patch.object(instance, "runtime",
                               side_effect=[types.SimpleNamespace(stdout=x) for x in output]), \
                mock.patch.object(instance, "record") as record:
            with self.assertRaises(isolated.IsolationError):
                instance.collect_ovs_dataplane(
                    "worker-a", {"mac": "02:00:00:00:00:11"}, "chassis-a"
                )
            record.assert_not_called()

    def test_local_export_requires_exact_owned_route_and_frr_peer(self):
        endpoint = {"node": "worker-a", "podUID": "pod-a", "mac": "02:00:00:00:00:11",
                    "ip": "10.244.240.11", "vtep": "198.18.30.2"}
        identity = {"nodeUID": "node-a", "chassis": "chassis-a"}
        route = {"NetworkID": "tenant-a", "EndpointID": "tenant-a|secondary|pod-a",
                 "Incarnation": "incarnation", "MACAddress": endpoint["mac"],
                 "IPAddresses": [endpoint["ip"]], "VNI": 1000,
                 "RouteDistinguisher": "198.18.30.2:1000", "RouteTargets": ["64512:1000"],
                 "LocalVTEP": endpoint["vtep"], "NodeName": endpoint["node"],
                 "Chassis": identity["chassis"], "Advertise": True}
        owned = {"Endpoint": route, "PathUUID": "owned-uuid"}
        status = {"Result": {"RecoveryState": "RUNNING", "OwnershipUnverified": False, "Applied": [owned]},
                  "SpeakerProcess": {"standalone": {"nodeUID": "node-a", "installationUID": "installation",
                                                      "routerID": endpoint["vtep"]}}}
        journal = {"Record": {"Domain": {"InstallationUID": "installation", "NodeUID": "node-a"},
                              "Pending": None, "State": {"Receipts": [{"Owned": owned}]}}}
        path = {"routeType": 2, "mac": endpoint["mac"], "ip": endpoint["ip"], "ethTag": 0,
                "peerId": endpoint["vtep"], "nexthops": [{"ip": endpoint["vtep"]}],
                "extendedCommunity": {"string": "RT:64512:1000 ET:8"}}
        rib = {"198.18.30.2:1000": {"route": {"paths": [[path]]}}}
        self.assertEqual(runner.verify_local_export_evidence(status, journal, rib, endpoint, identity, "installation"),
                         "owned-uuid")
        for field, value in (("peerId", "198.18.30.3"), ("ip", "10.244.240.12"),
                             ("extendedCommunity", {"string": "ET:8"})):
            with self.subTest(field=field):
                original = path[field]
                path[field] = value
                with self.assertRaises(isolated.IsolationError):
                    runner.verify_local_export_evidence(status, journal, rib, endpoint, identity, "installation")
                path[field] = original
        journal["Record"]["State"]["Receipts"][0]["Owned"] = {"PathUUID": "different"}
        with self.assertRaises(isolated.IsolationError):
            runner.verify_local_export_evidence(status, journal, rib, endpoint, identity, "installation")
        journal["Record"]["State"]["Receipts"][0]["Owned"] = owned
        status["SpeakerProcess"]["standalone"]["nodeUID"] = "node-b"
        with self.assertRaises(isolated.IsolationError):
            runner.verify_local_export_evidence(status, journal, rib, endpoint, identity, "installation")

    def test_inventory_scenario_stops_after_every_failed_stage(self):
        stages = ("install_operator", "discover_inventory_workers", "prepare_disabled_core",
                  "prepare_inventory_workers", "provision_external_frr", "activate_inventory_core",
                  "await_inventory_acceptance", "verify_agent_peerpidfd")
        for failed in range(len(stages)):
            with self.subTest(stage=stages[failed]):
                instance = runner.OperatorInventoryType2("ctr", "a" * 64, "gobgpd", "b" * 64)
                patches = [mock.patch.object(instance, stage) for stage in stages]
                mocks = [patch.start() for patch in patches]
                self.addCleanup(lambda patches=patches: [p.stop() for p in reversed(patches)])
                mocks[failed].side_effect = isolated.IsolationError("stage failed")
                with self.assertRaisesRegex(isolated.IsolationError, "stage failed"):
                    instance.test_endpoints()
                for later in mocks[failed + 1:]:
                    later.assert_not_called()

    def test_inventory_stops_if_first_worker_import_fails(self):
        instance = runner.OperatorInventoryType2("ctr", "a" * 64, "gobgpd", "b" * 64)
        instance.workers = ["worker-a", "worker-b"]
        identities = {node: {"nodeUID": node + "-uid"} for node in instance.workers}
        rendered = {"workers": {node: {"vtep": address} for node, address in
                                zip(instance.workers, ("198.18.30.2", "198.18.30.3"))}}
        with mock.patch.object(instance, "install_operator"), \
                mock.patch.object(instance, "discover_inventory_workers", return_value=identities), \
                mock.patch.object(instance, "prepare_disabled_core", return_value="installation"), \
                mock.patch.object(instance, "prepare_inventory_workers", return_value=rendered), \
                mock.patch.object(instance, "provision_external_frr"), \
                mock.patch.object(instance, "activate_inventory_core"), \
                mock.patch.object(instance, "await_inventory_acceptance"), \
                mock.patch.object(instance, "verify_agent_peerpidfd"), \
                mock.patch.object(instance, "create_inventory_endpoints") as endpoints, \
                mock.patch.object(instance, "await_external_import",
                                  side_effect=isolated.IsolationError("missing receipt")) as check:
            with self.assertRaisesRegex(isolated.IsolationError, "missing receipt"):
                instance.test_endpoints()
            check.assert_called_once_with("worker-a", identities["worker-a"], "installation", "198.18.30.2")
            endpoints.assert_not_called()

    def test_inventory_endpoint_creation_uses_cni_receipts_on_each_worker(self):
        instance = runner.OperatorInventoryType2("ctr", "a" * 64, "gobgpd", "b" * 64)
        instance.workers = ["worker-a", "worker-b"]
        uids = ["11111111-1111-4111-8111-111111111111",
                "22222222-2222-4222-8222-222222222222"]
        lsp_uuids = ["33333333-3333-4333-8333-333333333333",
                     "44444444-4444-4444-8444-444444444444"]
        identities = {node: {"chassis": "chassis-" + node, "chassisUUID": "sb-" + node}
                      for node in instance.workers}
        rendered = {"workers": {node: {"vtep": address, "cni.json": {"Domain": {}}}
                                for node, address in zip(instance.workers, ("198.18.30.2", "198.18.30.3"))}}
        pods = {}
        for i, node in enumerate(instance.workers):
            name = "tenant-a-worker-" + ("a" if i == 0 else "b")
            pods[name] = {"metadata": {"uid": uids[i], "labels": {"evpn.e2e/owner": instance.owner},
                                      "annotations": {"k8s.v1.cni.cncf.io/network-status":
                                                      json.dumps([{"interface": "net1", "ips": [f"10.244.240.{11 + i}"]}])}},
                          "spec": {"nodeName": node}}
        def kubectl(*args, **kwargs):
            if "Logical_Switch_Port" in args:
                port = args[-1].removeprefix("name=")
                index = [isolated.expected_lsp_name(uid, "tenant-a", "secondary") for uid in uids].index(port)
                return types.SimpleNamespace(stdout=lsp_uuids[index])
            if "Logical_Switch" in args:
                return types.SimpleNamespace(stdout=", ".join(lsp_uuids))
            return types.SimpleNamespace(stdout="")
        def journal(node, uid, state):
            index = instance.workers.index(node)
            self.assertEqual((uid, state), (uids[index], "active"))
            return {"Record": {}}, {"Receipt": {"LSPUUID": lsp_uuids[index]}}
        with mock.patch.object(instance, "apply_text") as create, \
                mock.patch.object(instance, "k", side_effect=kubectl), \
                mock.patch.object(instance, "get_json", side_effect=lambda *args: pods[args[-1]]), \
                mock.patch.object(instance, "wait_for_binding",
                                  side_effect=lambda port, node, chassis: {"chassisUUID": identities[node]["chassisUUID"]}), \
                mock.patch.object(instance, "wait_journal", side_effect=journal) as verify, \
                mock.patch.object(instance, "record"):
            result = instance.create_inventory_endpoints(rendered, identities)
        self.assertEqual(set(result), set(instance.workers))
        self.assertEqual(verify.call_count, 2)
        for call in create.call_args_list:
            self.assertTrue(call.kwargs["create"])
            nad, pod = json.loads(call.args[1])["items"]
            config = json.loads(nad["spec"]["config"])
            self.assertEqual((config["networkID"], config["logicalSwitch"]), ("tenant-a", "tenant-a"))
            self.assertTrue(config["creationIdentity"])
            self.assertEqual(pod["metadata"]["namespace"], "evpn-system")

    def test_inventory_activation_uses_operator_reservations_then_gated_cr(self):
        instance = runner.OperatorInventoryType2("ctr", "a" * 64, "gobgpd", "b" * 64)
        result = runner.inventory_configuration(inventory_inputs())
        with mock.patch.object(instance, "apply_text") as apply:
            instance.activate_inventory_core(result)
        self.assertEqual([call.args[0] for call in apply.call_args_list],
                         ["inventory-reservations.yaml", "inventory-rbac.yaml", "inventory-core.yaml"])
        self.assertTrue(apply.call_args_list[0].kwargs["create"])
        core = apply.call_args_list[-1].args[1]
        self.assertIn(f'evpn.e2e/owner: "{instance.owner}"', core)
        self.assertIn("workerInventory:", core)
        self.assertIn("  enabled: true", core)

    def test_inventory_worker_provision_renders_discovered_identity_and_observed_speaker(self):
        inputs = inventory_inputs()
        with tempfile.TemporaryDirectory() as directory:
            binary = Path(directory) / "binary"
            binary.write_bytes(b"fixture")
            digest = hashlib.sha256(binary.read_bytes()).hexdigest()
            instance = runner.OperatorInventoryType2(binary, digest, binary, digest)
            instance.directory = Path(directory)
            instance.workers = [w["node"]["metadata"]["name"] for w in inputs["workers"]]
            for worker, vtep in zip(inputs["workers"], ("198.18.30.2", "198.18.30.3")):
                worker["vtep"] = vtep
            identities = {w["node"]["metadata"]["name"]:
                          {"nodeUID": w["node"]["metadata"]["uid"], "chassis": w["chassis"]}
                          for w in inputs["workers"]}
            objects = {w["node"]["metadata"]["name"]: {
                **w["node"], "status": {"addresses": [{"type": "InternalIP", "address": w["vtep"]}]}}
                       for w in inputs["workers"]}
            def worker(node, *args):
                if args[:4] == ("ip", "-j", "-4", "addr"):
                    address = objects[node]["status"]["addresses"][0]["address"]
                    out = json.dumps([{"addr_info": [{"local": address}]}])
                elif args[0] == "sha256sum":
                    out = (digest if args[1] in ("/opt/evpn/bin/ctr", "/usr/local/bin/gobgpd")
                           else "a" * 64) + "  " + args[1]
                elif args[:2] == ("systemctl", "show"):
                    out = "/system.slice/evpn-e2e-gobgpd.service" if args[2] == "-p" and args[3] == "ControlGroup" else "123"
                elif args == ("cat", "/proc/sys/kernel/random/boot_id"):
                    out = "33333333-3333-4333-8333-333333333333"
                elif args == ("readlink", "/proc/self/ns/net"):
                    out = "net:[4026531993]"
                elif args == ("stat", "-Lc", "%d:%i", "/proc/self/ns/net"):
                    out = "4:4026531993"
                elif args[:2] == ("sh", "-ec") and "command -v containerd" in args[2]:
                    out = "/usr/bin/containerd"
                else:
                    out = ""
                return types.SimpleNamespace(stdout=out)
            events = []
            def observed_worker(node, *args):
                events.append(("worker", node, args))
                return worker(node, *args)
            def observed_docker(*args):
                events.append(("docker", args))
            def observed_stream(args, **kwargs):
                events.append(("stream", args, kwargs))
                return types.SimpleNamespace(returncode=0, stderr="")
            with mock.patch.object(instance, "get_json", side_effect=lambda *args: objects[args[-1]]), \
                    mock.patch.object(instance, "worker", side_effect=observed_worker), \
                    mock.patch.object(instance, "docker", side_effect=observed_docker), mock.patch.object(instance, "runtime"), \
                    mock.patch.object(runner.fixture.subprocess, "run", side_effect=observed_stream), \
                    mock.patch.object(instance, "k"):
                result = instance.prepare_inventory_workers(identities, inputs["installationUID"])
            for name in instance.workers:
                bootstrap = next(i for i, event in enumerate(events)
                                 if event[0] == "worker" and event[1] == name and
                                 event[2][:2] == ("sh", "-ec") and
                                 "install -d -o 0 -g 0 -m 700 /run/evpn-node-agent" in event[2][2])
                copy = next(i for i, event in enumerate(events)
                            if event[0] == "stream" and event[1][3] == name and
                            event[1][-1] == "/run/evpn-node-agent/export.json")
                self.assertLess(bootstrap, copy)
                self.assertIn("0:0:700", events[bootstrap][2][2])
                self.assertIn("umask 077; set -C", events[copy][1][-3])
                self.assertEqual(json.loads(events[copy][2]["input"]),
                                 result["workers"][name]["export.json"])
                for destination in ("/etc/evpn/gobgpd.yaml",
                                    "/etc/evpn/provider-attestation/provider.json"):
                    copied = next(i for i, event in enumerate(events) if event[0] == "docker" and
                                  event[1][0] == "cp" and event[1][-1] == f"{name}:{destination}")
                    owned = next(i for i, event in enumerate(events) if event[0] == "worker" and
                                 event[1] == name and event[2] == ("chown", "root:root", destination))
                    secured = next(i for i, event in enumerate(events) if event[0] == "worker" and
                                   event[1] == name and event[2] == ("chmod", "600", destination))
                    self.assertLess(copied, owned)
                    self.assertLess(owned, secured)
                self.assertEqual(result["workers"][name]["nodeUID"], identities[name]["nodeUID"])
                self.assertEqual(result["workers"][name]["chassis"], identities[name]["chassis"])
                self.assertEqual(result["workers"][name]["provider.json"]["standalone"]["nodeUID"],
                                 identities[name]["nodeUID"])
                installed = (Path(directory) / f"{name}-gobgpd.yaml").read_text()
                self.assertEqual(installed, result["workers"][name]["gobgpd.yaml"])

    def test_inventory_worker_bootstrap_failure_stops_config_copy(self):
        with tempfile.TemporaryDirectory() as directory:
            binary = Path(directory) / "binary"
            binary.write_bytes(b"fixture")
            digest = hashlib.sha256(binary.read_bytes()).hexdigest()
            instance = runner.OperatorInventoryType2(binary, digest, binary, digest)
            node = instance.workers[0]
            identity = {node: {"nodeUID": "real-node-uid"}}

            def worker(_node, *args):
                if args[:4] == ("ip", "-j", "-4", "addr"):
                    return types.SimpleNamespace(stdout='[{"addr_info":[{"local":"198.18.30.2"}]}]')
                self.assertEqual(args[:2], ("sh", "-ec"))
                self.assertIn("test ! -L /run/evpn-node-agent", args[2])
                self.assertIn("0:0:700", args[2])
                self.assertIn("install -d -o 0 -g 0 -m 700 /run/evpn-node-agent", args[2])
                raise isolated.IsolationError("runtime directory rejected")

            with mock.patch.object(instance, "get_json", return_value={"metadata": {"uid": "real-node-uid"}}), \
                    mock.patch.object(instance, "worker", side_effect=worker), \
                    mock.patch.object(instance, "docker") as docker:
                with self.assertRaisesRegex(isolated.IsolationError, "runtime directory rejected"):
                    instance.prepare_inventory_workers(identity, "installation")
                docker.assert_not_called()

    def test_inventory_external_peer_uses_owned_lab_and_no_agent(self):
        inputs = inventory_inputs()
        for worker, vtep in zip(inputs["workers"], ("198.18.30.2", "198.18.30.3")):
            worker["vtep"] = vtep
        inputs["remoteVTEP"] = "198.18.30.4"
        rendered = runner.inventory_configuration(inputs)
        with tempfile.TemporaryDirectory() as directory:
            instance = runner.OperatorInventoryType2("ctr", "a" * 64, "gobgpd", "b" * 64)
            instance.directory = Path(directory)
            instance.workers = [w["node"]["metadata"]["name"] for w in inputs["workers"]]
            state = {"profile": "cni-inventory-frr", "frr": instance.frr, "phase": "ready"}
            with mock.patch.object(runner.lab, "load", return_value=state), \
                    mock.patch.object(runner.lab, "verify_owned") as verify, \
                    mock.patch.object(instance, "docker") as docker, \
                    mock.patch.object(instance, "record") as record:
                instance.provision_external_frr(rendered)
            verify.assert_called_once_with(state)
            commands = [call.args for call in docker.call_args_list]
            self.assertTrue(any("vxlan1000" in cmd and "4789" in cmd for cmd in commands))
            self.assertTrue(any("neighbor 198.18.30.2 remote-as 64512" in cmd and
                                "neighbor 198.18.30.3 remote-as 64512" in cmd for cmd in commands))
            self.assertFalse(any("evpn-node-agent" in str(cmd) for cmd in commands))
            self.assertIn("not established", record.call_args.args[1]["scope"])

    def test_inventory_external_peer_refuses_unowned_lab(self):
        instance = runner.OperatorInventoryType2("ctr", "a" * 64, "gobgpd", "b" * 64)
        with mock.patch.object(runner.lab, "load", return_value={
                "profile": "cni-inventory-frr", "frr": "foreign", "phase": "ready"}), \
                mock.patch.object(instance, "docker") as docker:
            with self.assertRaisesRegex(isolated.IsolationError, "not in the ready owned lab"):
                instance.provision_external_frr({})
            docker.assert_not_called()

    def test_external_import_evidence_rejects_unconfirmed_or_foreign_state(self):
        identity = {"nodeUID": "node-a"}
        export = {"SpeakerProcess": {"PID": 123, "StartTicks": 1,
                  "standalone": {"nodeUID": "node-a", "installationUID": "installation",
                                 "routerID": "198.18.30.2"}}}
        receipt = {"Network": {"NetworkID": "tenant-a", "LogicalSwitch": "tenant-a", "VNI": 1000,
                               "BridgeDevice": "evpnbr1000", "VXLANDevice": "evpnvx1000"},
                   "Endpoint": {"MAC": "02:00:00:00:00:30", "IP": "10.244.240.30",
                                "RemoteVTEP": "198.18.30.4", "ValidationState": "validated",
                                "Routes": [{"Route": {"Classification": "remote",
                                                       "Source": {"NeighborIP": "198.18.30.4"}}}]},
                   "FDB": {"ACK": True, "Status": "confirmed"},
                   "Neighbor": {"ACK": True, "Status": "confirmed"}}
        status = {"RecoveryState": "RUNNING", "Snapshot": {"Authoritative": True}, "Receipts": [receipt]}
        journal = {"Record": {"Pending": None, "Domain": {"InstallationUID": "installation", "NodeUID": "node-a"},
                              "State": {"Receipts": {"owned": receipt}}}}
        fdb = [{"mac": "02:00:00:00:00:30", "dst": "198.18.30.4", "state": "permanent",
                "flags": ["self", "extern_learn"]}]
        neighbors = [{"dst": "10.244.240.30", "lladdr": "02:00:00:00:00:30", "state": ["NOARP"]}]
        runner.verify_external_import_evidence(status, export, journal, fdb, neighbors,
                                               identity, "installation", "198.18.30.2")
        for component in ("speaker", "snapshot", "fdb", "neighbor"):
            with self.subTest(component=component):
                import copy
                exp, st, fs, ns = copy.deepcopy((export, status, fdb, neighbors))
                if component == "speaker":
                    exp["SpeakerProcess"]["standalone"]["nodeUID"] = "foreign"
                elif component == "snapshot":
                    st["Snapshot"]["Authoritative"] = False
                elif component == "fdb":
                    fs.clear()
                else:
                    ns.clear()
                with self.assertRaises(isolated.IsolationError):
                    runner.verify_external_import_evidence(st, exp, journal, fs, ns,
                                                           identity, "installation", "198.18.30.2")

    def test_requires_explicit_disposable_host_confirmation(self):
        with mock.patch.object(runner.lab, "exclusive") as exclusive, \
                contextlib.redirect_stderr(io.StringIO()):
            with self.assertRaises(SystemExit):
                runner.main([])
            exclusive.assert_not_called()

    def test_external_worker_has_no_agent_and_uses_local_paths(self):
        instance = runner.OperatorSecondaryCNI()
        ds = {"spec": {"template": {"spec": {
            "containers": [{"name": "ovn"}, {"name": "node-agent"}],
            "volumes": [{"name": "ovs-run", "hostPath": {"path": "/old/ovs"}},
                        {"name": "ovn-run", "hostPath": {"path": "/old/ovn"}}]}}}}
        with mock.patch.object(instance, "worker") as worker:
            actual = instance.configure_runtime_daemonset(ds)
        pod = actual["spec"]["template"]["spec"]
        self.assertEqual([c["name"] for c in pod["containers"]], ["ovn"])
        self.assertEqual([v["hostPath"]["path"] for v in pod["volumes"]],
                         ["/run/openvswitch", "/run/ovn"])
        self.assertEqual(worker.call_count, 2)
        self.assertEqual(instance.agent_socket(), "/run/evpn-node-agent/agent.sock")

    def test_install_uses_real_operator_manifest_and_cr_not_agent_daemonset(self):
        instance = runner.OperatorSecondaryCNI()
        with tempfile.TemporaryDirectory() as directory:
            instance.directory = Path(directory)
            with mock.patch.object(instance, "k") as k, \
                    mock.patch.object(instance, "apply") as apply:
                instance.install_operator()
            manifest = (instance.directory / "operator-install.yaml").read_text()
            self.assertIn(instance.image, manifest)
            self.assertIn("kind: CustomResourceDefinition", manifest)
            self.assertIn("kind: ClusterRoleBinding", manifest)
            core = apply.call_args.args[1]
            self.assertEqual(core["kind"], "EvpnCore")
            self.assertEqual(core["spec"]["dataplaneMode"], "external-ovn")
            self.assertEqual(core["spec"]["nodeAgent"]["nodeSelector"],
                             {"evpn.e2e/owner": instance.owner})
            self.assertFalse(any("daemonset" in call.args for call in k.call_args_list))
            self.assertEqual(apply.call_count, 1)

    def test_operator_can_be_installed_without_ungated_core(self):
        instance = runner.OperatorSecondaryCNI()
        with tempfile.TemporaryDirectory() as directory:
            instance.directory = Path(directory)
            with mock.patch.object(instance, "k") as k, mock.patch.object(instance, "apply") as apply:
                instance.install_operator(create_baseline_core=False)
            apply.assert_not_called()
            self.assertTrue(any("crd/evpncores.evpn.romanspb80.github.io" in call.args
                                for call in k.call_args_list))

    def test_discovers_two_worker_node_uids_and_sb_chassis(self):
        instance = runner.OperatorSecondaryCNI()
        nodes = [{"metadata": {"name": name, "uid": str(uuid.UUID(int=i)),
                               "labels": {"evpn.e2e/owner": instance.owner}},
                  "status": {"conditions": [{"type": "Ready", "status": "True"}]}}
                 for i, name in enumerate(instance.workers, 1)]
        with mock.patch.object(instance, "get_json", side_effect=nodes), \
                mock.patch.object(instance, "runtime", side_effect=[
                    types.SimpleNamespace(stdout='"chassis-a"\n'),
                    types.SimpleNamespace(stdout='"chassis-b"\n')]), \
                mock.patch.object(instance, "k", side_effect=[
                    types.SimpleNamespace(stdout=str(uuid.UUID(int=3)) + "\n"),
                    types.SimpleNamespace(stdout=str(uuid.UUID(int=4)) + "\n")]), \
                mock.patch.object(instance, "record") as record:
            result = instance.discover_inventory_workers()
        self.assertEqual(result[instance.workers[0]]["nodeUID"], nodes[0]["metadata"]["uid"])
        self.assertEqual(result[instance.workers[1]]["chassis"], "chassis-b")
        record.assert_called_once_with("inventory-worker-identities.json", result)

    def test_discovery_rejects_duplicate_or_missing_chassis(self):
        for sb_results, expected in ((["", str(uuid.UUID(int=4))], "matching SB chassis"),
                                     ([str(uuid.UUID(int=3)), str(uuid.UUID(int=3))], "duplicate")):
            with self.subTest(expected=expected):
                instance = runner.OperatorSecondaryCNI()
                nodes = [{"metadata": {"name": name, "uid": str(uuid.UUID(int=i)),
                                       "labels": {"evpn.e2e/owner": instance.owner}},
                          "status": {"conditions": [{"type": "Ready", "status": "True"}]}}
                         for i, name in enumerate(instance.workers, 1)]
                with mock.patch.object(instance, "get_json", side_effect=nodes), \
                        mock.patch.object(instance, "runtime", side_effect=[
                            types.SimpleNamespace(stdout="chassis-a"),
                            types.SimpleNamespace(stdout="chassis-b")]), \
                        mock.patch.object(instance, "k", side_effect=[
                            types.SimpleNamespace(stdout=value) for value in sb_results]), \
                        mock.patch.object(instance, "record") as record:
                    with self.assertRaisesRegex(isolated.IsolationError, expected):
                        instance.discover_inventory_workers()
                    record.assert_not_called()

    def test_existing_worker_generator_emits_only_external_ovn(self):
        instance = runner.OperatorSecondaryCNI()
        pods = [{"spec": {"nodeName": node},
                 "metadata": {"name": f"worker-{i}", "uid": f"uid-{i}"},
                 "status": {"containerStatuses": []}}
                for i, node in enumerate(instance.workers)]
        manifests = {}
        with mock.patch.object(instance, "worker", return_value=types.SimpleNamespace(stdout="shared\n")), \
                mock.patch.object(instance, "k", return_value=types.SimpleNamespace(
                    stdout=json.dumps({"items": pods}))), \
                mock.patch.object(instance, "record"), \
                mock.patch.object(instance, "apply",
                                  side_effect=lambda name, value: manifests.update({name: value})):
            instance.deploy_runtime_daemonset("tcp:10.96.40.2:6642")
        ds = manifests["secondary-runtime.json"]
        self.assertEqual(ds["metadata"]["name"], "secondary-ovn-worker")
        self.assertEqual([c["name"] for c in ds["spec"]["template"]["spec"]["containers"]], ["ovn"])
        self.assertEqual(set(instance.runtime_pods), set(instance.workers))

    def test_live_objects_are_passed_to_ownership_verifier_and_persisted(self):
        instance = runner.OperatorSecondaryCNI()
        core, lease, ds, pods, nodes, selector = external_fixture()
        instance.owner = selector["evpn.e2e/owner"]
        instance.workers = [n["metadata"]["name"] for n in nodes]
        core["spec"]["nodeAgent"].update(providerAttestation=False, cniContinuityConfigPath="")
        ds["spec"]["template"]["spec"]["containers"][0]["args"][-1] = "--cni-continuity-config="
        responses = [core, lease, ds, {"items": pods}, *nodes]
        with mock.patch.object(instance, "get_json", side_effect=responses), \
                mock.patch.object(instance, "record") as record, \
                mock.patch.object(runner.operator_ownership, "verify_external_ovn",
                                  wraps=runner.operator_ownership.verify_external_ovn) as verify:
            instance.verify_operator()
        self.assertEqual(verify.call_args.kwargs, {"require_continuity": False})
        self.assertIs(verify.call_args.args[0], core)
        self.assertEqual(record.call_args.args[1]["pods"], pods)

    def test_foreign_pod_evidence_never_passes(self):
        instance = runner.OperatorSecondaryCNI()
        core, lease, ds, pods, nodes, selector = external_fixture()
        instance.owner = selector["evpn.e2e/owner"]
        instance.workers = [n["metadata"]["name"] for n in nodes]
        pods[0]["metadata"]["ownerReferences"][0]["uid"] = "foreign"
        responses = [core, lease, ds, {"items": pods}, *nodes] * 180
        with mock.patch.object(instance, "get_json", side_effect=responses), \
                mock.patch.object(runner.time, "sleep"), \
                mock.patch.object(instance, "record") as record:
            with self.assertRaisesRegex(isolated.IsolationError, "Pod ownership"):
                instance.verify_operator()
            record.assert_not_called()

    def test_changed_external_resources_prevent_baseline(self):
        instance = runner.OperatorSecondaryCNI()
        instance.external_before = {"uid": "before"}
        with mock.patch.object(instance, "k"), mock.patch.object(instance, "verify_operator"), \
                mock.patch.object(instance, "external_inventory", return_value={"uid": "after"}), \
                mock.patch.object(isolated.IsolatedSecondaryCNI, "test_endpoints") as baseline:
            with self.assertRaisesRegex(isolated.IsolationError, "external OVN"):
                instance.test_endpoints()
            baseline.assert_not_called()

    def test_endpoint_deletion_stops_when_survivor_is_missing(self):
        instance = runner.OperatorSecondaryCNI()
        pods = {"b": {"metadata": {"uid": "pod-b-uid"},
                      "spec": {"nodeName": instance.workers[1]}}}
        with mock.patch.object(instance, "k", return_value=types.SimpleNamespace(stdout="")) as k, \
                mock.patch.object(instance, "runtime",
                                  return_value=types.SimpleNamespace(stdout="")), \
                mock.patch.object(instance, "record") as record:
            with self.assertRaisesRegex(isolated.IsolationError, "affected pod-b"):
                instance.delete_pods(pods)
        self.assertFalse(any("pod-b" in call.args and "delete" in call.args
                             for call in k.call_args_list))
        record.assert_not_called()

    def test_baseline_and_continuity_results_are_separate(self):
        instance = runner.OperatorSecondaryCNI()
        instance.external_before = {"uid": "stable"}
        with tempfile.TemporaryDirectory() as directory:
            instance.directory = Path(directory)
            (instance.directory / "secondary-runtime-ownership.json").write_text('{"id":"stable"}')
            with mock.patch.object(instance, "k"), mock.patch.object(instance, "verify_operator"), \
                    mock.patch.object(instance, "verify_node_local"), \
                    mock.patch.object(instance, "capture_runtime_identities"), \
                    mock.patch.object(instance, "external_inventory", return_value={"uid": "stable"}), \
                    mock.patch.object(instance, "record") as record, \
                    mock.patch.object(isolated.IsolatedSecondaryCNI, "test_endpoints") as baseline:
                instance.test_endpoints()
        baseline.assert_called_once_with()
        self.assertEqual(record.call_args.args[1]["status"], "BLOCKED")
        self.assertEqual(instance.result_details()["overallAcceptance"], "BLOCKED")

    def test_baseline_failure_does_not_report_continuity_or_pass(self):
        instance = runner.OperatorSecondaryCNI()
        instance.external_before = {"uid": "stable"}
        with tempfile.TemporaryDirectory() as directory:
            instance.directory = Path(directory)
            (instance.directory / "secondary-runtime-ownership.json").write_text("{}")
            with mock.patch.object(instance, "k"), mock.patch.object(instance, "verify_operator"), \
                    mock.patch.object(instance, "external_inventory", return_value={"uid": "stable"}), \
                    mock.patch.object(instance, "record") as record, \
                    mock.patch.object(isolated.IsolatedSecondaryCNI, "test_endpoints",
                                      side_effect=isolated.IsolationError("CNI failed")):
                with self.assertRaisesRegex(isolated.IsolationError, "CNI failed"):
                    instance.test_endpoints()
                self.assertNotIn("continuity-stage.json",
                                 [call.args[0] for call in record.call_args_list])

    def test_journal_requires_exact_domain_receipt_and_integrity(self):
        instance = runner.OperatorSecondaryCNI()
        node = instance.workers[0]
        domain = {"InstallationUID": "installation", "NodeUID": "node-a",
                  "BootID": "boot", "NetworkNamespace": "net:[1]"}
        instance.continuity_configs = {node: {"Domain": domain}}
        receipt = {"SwitchUUID": "switch", "LSPUUID": "lsp", "InterfaceUUID": "if",
                   "PortUUID": "port", "BridgeUUID": "bridge", "NetNSDevice": 10, "NetNSInode": 20}
        ack = {"Identity": {"workload_uid": "pod"}, "Runtime": {"container_id": "sandbox"},
               "Receipt": receipt, "State": "active", "CreatedAt": "2026-01-01",
               "VerifiedAt": "2026-01-02", "Admission": {"PodUID": "pod",
                   "ContainerID": "sandbox", "NetNSDevice": 10, "NetNSInode": 20}}
        record = {"Domain": {**domain, "ConfigDigest": "a" * 64}, "Pending": None,
                  "State": {"Endpoints": {"key": ack}}}
        def encoded():
            raw = json.dumps(record, separators=(",", ":"), ensure_ascii=False)
            return '{"Record":' + raw + ',"SHA256":"' + hashlib.sha256(raw.encode()).hexdigest() + '"}'
        def read():
            return types.SimpleNamespace(stdout=encoded())
        with mock.patch.object(instance, "worker", side_effect=lambda *a: read()):
            instance.journal(node, "pod", "active")
            record["Domain"]["NodeUID"] = "foreign"
            with self.assertRaisesRegex(isolated.IsolationError, "domain"):
                instance.journal(node, "pod", "active")
            record["Domain"]["NodeUID"] = "node-a"
            record["Pending"] = {"Operation": "cni-add"}
            with self.assertRaisesRegex(isolated.IsolationError, "pending"):
                instance.journal(node, "pod", "active")
            record["Pending"] = None
            ack["Admission"]["PodUID"] = "other"
            with self.assertRaisesRegex(isolated.IsolationError, "receipt"):
                instance.journal(node, "pod", "active")
        with mock.patch.object(instance, "worker",
                               return_value=types.SimpleNamespace(stdout=encoded().replace("node-a", "node-b"))):
            with self.assertRaisesRegex(isolated.IsolationError, "checksum"):
                instance.journal(node, "pod", "active")

    def test_restart_rejects_replacing_both_agents(self):
        instance = runner.OperatorSecondaryCNI()
        instance.authenticated = True
        a, b = instance.workers
        instance.cni_attachment_ids = {"b": {"wrapperCallDirectory": "/run/call"}}
        pods = {"a": {"metadata": {"uid": "u-a"}}, "b": {"metadata": {"uid": "u-b"}}}
        old = [{"spec": {"nodeName": n}, "metadata": {"uid": n, "name": n}}
               for n in instance.workers]
        new = [{"spec": {"nodeName": n}, "metadata": {"uid": n + "-new", "name": n}}
               for n in instance.workers]
        with mock.patch.object(instance, "verify_operator", side_effect=[(None, None, old, None),
                                                                          (None, None, new, None)]), \
                mock.patch.object(instance, "wait_journal", return_value=({}, {"Receipt": {}})), \
                mock.patch.object(instance, "record"), mock.patch.object(instance, "k"), \
                mock.patch.object(instance, "worker", return_value=types.SimpleNamespace(stdout="lock")):
            with self.assertRaisesRegex(isolated.IsolationError, "only worker A"):
                instance.endpoints_before_delete(pods)

    def test_rejects_remote_docker_before_initializing_lab(self):
        instance = runner.OperatorSecondaryCNI()
        document = [{"Endpoints": {"docker": {"Host": "tcp://remote:2376"}}}]
        with mock.patch.object(instance, "docker", return_value=types.SimpleNamespace(
                stdout=json.dumps(document))), \
                mock.patch.object(isolated.IsolatedSecondaryCNI, "initialize") as initialize:
            with self.assertRaisesRegex(isolated.IsolationError, "local default"):
                instance.initialize()
            initialize.assert_not_called()

    def test_bad_ctr_pin_fails_before_rbac_or_worker_mutations(self):
        with tempfile.TemporaryDirectory() as directory:
            ctr = Path(directory) / "ctr"
            ctr.write_bytes(b"not-the-pinned-binary")
            instance = runner.OperatorSecondaryCNI(ctr, "0" * 64)
            nodes = [{"metadata": {"name": name, "uid": name + "-uid"}}
                     for name in instance.workers]
            with mock.patch.object(instance, "verify_operator",
                                   return_value=({"metadata": {"uid": "installation"}}, None, None, nodes)), \
                    mock.patch.object(instance, "apply") as apply, \
                    mock.patch.object(instance, "worker") as worker:
                with self.assertRaisesRegex(isolated.IsolationError, "SHA256 mismatch"):
                    instance.provision_continuity()
                apply.assert_not_called()
                worker.assert_not_called()

    def test_provisioning_binds_distinct_nodes_and_limited_observer_rbac(self):
        with tempfile.TemporaryDirectory() as directory:
            ctr = Path(directory) / "ctr"
            ctr.write_bytes(b"test-binary")
            digest = hashlib.sha256(ctr.read_bytes()).hexdigest()
            instance = runner.OperatorSecondaryCNI(ctr, digest)
            instance.directory = Path(directory)
            nodes = [{"metadata": {"name": name, "uid": name + "-uid"}}
                     for name in instance.workers]
            def worker(node, *args, **kwargs):
                if args[0] == "sha256sum":
                    text = digest + "  " + args[1]
                elif args[:2] == ("cat", "/proc/sys/kernel/random/boot_id"):
                    text = "boot-" + node
                elif args[:2] == ("readlink", "/proc/self/ns/net"):
                    text = "net:[" + node + "]"
                elif args[:2] == ("sh", "-ec") and args[2] == "command -v containerd":
                    text = "/usr/bin/containerd"
                else:
                    text = ""
                return types.SimpleNamespace(stdout=text)
            def journal(node):
                return {"Record": {"Domain": {"ConfigDigest": node}}}
            with mock.patch.object(instance, "verify_operator",
                                   return_value=({"metadata": {"uid": "installation"}}, None, None, nodes)), \
                    mock.patch.object(instance, "verify_node_local"), \
                    mock.patch.object(instance, "worker", side_effect=worker), \
                    mock.patch.object(instance, "docker"), mock.patch.object(instance, "k") as k, \
                    mock.patch.object(instance, "apply") as apply, \
                    mock.patch.object(instance, "record"), mock.patch.object(instance, "read_journal", side_effect=journal):
                instance.provision_continuity()
            configs = instance.continuity_configs
            self.assertEqual(configs[instance.workers[0]]["Domain"]["NodeUID"],
                             instance.workers[0] + "-uid")
            self.assertNotEqual(configs[instance.workers[0]]["Domain"], configs[instance.workers[1]]["Domain"])
            self.assertEqual(configs[instance.workers[0]]["Networks"],
                             {"secondary-isolation": "ls-secondary"})
            rules = apply.call_args.args[1]["items"]
            cluster_role = next(item for item in rules if item["kind"] == "ClusterRole")
            self.assertEqual(cluster_role["rules"][0]["resourceNames"], instance.workers)
            role = next(item for item in rules if item["kind"] == "Role")
            self.assertEqual(role["metadata"]["namespace"], "cni-test")
            self.assertEqual(role["rules"][0]["verbs"], ["get"])
            self.assertIn("cniContinuityConfigPath", k.call_args.args[-1])

    def test_inventory_result_requires_live_forwarding_for_pass(self):
        instance = runner.OperatorInventoryType2(None, None, "gobgpd", None)

        self.assertEqual(instance.result_status(), "OPERATOR_INVENTORY_TWO_WORKER_PARTIAL")
        self.assertEqual(instance.result_details()["liveEVPNType2"], "OBSERVED")

        instance.forwarding_status = "PASS"
        self.assertEqual(instance.result_status(), "OPERATOR_INVENTORY_TWO_WORKER_PASS")
        details = instance.result_details()
        self.assertEqual(details["liveEVPNType2"], "PASS")
        self.assertEqual(details["vxlanForwarding"], "PASS")
        self.assertEqual(details["recovery"], "NOT_TESTED")

    def test_inventory_capture_start_failure_records_stderr_command_and_exit_status(self):
        instance = runner.OperatorInventoryType2(None, None, "gobgpd", None)
        instance.kubeconfig = Path("/run-owned/kubeconfig")
        instance.context = "kind-owned-cluster"
        instance.workers = ["worker-a", "worker-b"]
        instance.frr = "frr"
        instance.directory = mock.MagicMock()
        pcap_path = mock.MagicMock()
        pcap_path.exists.return_value = False
        pcap_path.is_symlink.return_value = False
        pcap_path.open.return_value = io.BytesIO()
        instance.directory.__truediv__.return_value = pcap_path
        process = mock.MagicMock()
        process.stderr = io.StringIO("tcpdump: net1: No such device\n")
        process.returncode = 2
        process.poll.return_value = 2
        process.wait.return_value = 2
        endpoints = {
            "worker-a": {"ip": "10.244.1.2", "pod": "client-a"},
            "worker-b": {"ip": "10.244.2.2", "pod": "client-b"},
        }

        with mock.patch.dict(runner.fixture.os.environ, {"KUBECONFIG": "/ambient/kubeconfig"}), \
                mock.patch.object(instance, "ensure_inventory_frr_workload_tools"), \
                mock.patch.object(runner.subprocess, "Popen", return_value=process) as popen, \
                mock.patch.object(instance, "record") as record:
            with self.assertRaisesRegex(
                    runner.InventoryEvidenceError, "inventory-worker-a-net1: capture did not become ready"):
                instance.start_inventory_workload_captures(endpoints)

        command = popen.call_args.args[0]
        self.assertEqual(
            command[:6],
            ["kubectl", "--kubeconfig", str(instance.kubeconfig), "--context", instance.context, "-n"],
        )
        self.assertNotIn("/ambient/kubeconfig", command)
        timeout_index = command.index("timeout")
        self.assertEqual(
            command[timeout_index:timeout_index + 7],
            ["timeout", "-s", "INT", "-k", "5", str(runner.INVENTORY_CAPTURE_MAX_SECONDS), "tcpdump"],
        )
        self.assertIn("-i", command)
        self.assertIn("net1", command)
        tcpdump_index = command.index("tcpdump")
        self.assertEqual(
            command[command.index("-c", tcpdump_index) + 1],
            str(runner.INVENTORY_CAPTURE_LIMIT),
        )
        record.assert_called_once()
        name, evidence = record.call_args.args
        self.assertEqual(name, "inventory-capture-startup-failure.json")
        self.assertEqual(evidence["capture"], "inventory-worker-a-net1")
        self.assertEqual(evidence["command"], command)
        self.assertEqual(evidence["trailingStderr"], "tcpdump: net1: No such device\n")
        self.assertEqual(evidence["returncode"], 2)
        self.assertIsNone(evidence["signal"])

    def test_inventory_workload_capture_timeout_prefix_is_portable_for_all_targets(self):
        instance = runner.OperatorInventoryType2(None, None, "gobgpd", None)
        instance.kubeconfig = Path("/run-owned/kubeconfig")
        instance.context = "kind-owned-cluster"
        instance.workers = ["worker-a", "worker-b"]
        instance.frr = "frr"
        instance.directory = mock.MagicMock()
        pcap_path = mock.MagicMock()
        pcap_path.exists.return_value = False
        pcap_path.is_symlink.return_value = False
        pcap_path.open.side_effect = lambda *_args, **_kwargs: io.BytesIO()
        instance.directory.__truediv__.return_value = pcap_path
        processes = []

        def ready_process(*_args, **_kwargs):
            process = mock.MagicMock()
            process.stderr = io.StringIO("listening on net1\n")
            process.poll.return_value = None
            processes.append(process)
            return process

        endpoints = {
            "worker-a": {"ip": "10.244.1.2", "pod": "client-a"},
            "worker-b": {"ip": "10.244.2.2", "pod": "client-b"},
        }
        with mock.patch.object(instance, "ensure_inventory_frr_workload_tools") as ensure_tool, \
                mock.patch.object(runner.subprocess, "Popen", side_effect=ready_process) as popen, \
                mock.patch.object(runner.select, "select", return_value=([object()], [], [])):
            captures = instance.start_inventory_workload_captures(endpoints)

        ensure_tool.assert_called_once_with()
        expected_prefix = [
            "timeout", "-s", "INT", "-k", "5", str(runner.INVENTORY_CAPTURE_MAX_SECONDS), "tcpdump",
        ]
        self.assertEqual(len(captures), 3)
        self.assertEqual(popen.call_count, 3)
        expected_counts = [runner.INVENTORY_CAPTURE_LIMIT, runner.INVENTORY_CAPTURE_LIMIT,
                           runner.INVENTORY_CAPTURE_LIMIT * len(instance.workers)]
        for index, call in enumerate(popen.call_args_list):
            command = call.args[0]
            timeout_index = command.index("timeout")
            self.assertEqual(command[timeout_index:timeout_index + len(expected_prefix)], expected_prefix)
            tcpdump_index = command.index("tcpdump", timeout_index)
            self.assertEqual(command[command.index("-c", tcpdump_index) + 1], str(expected_counts[index]))
            if index < len(instance.workers):
                endpoint_ip = endpoints[instance.workers[index]]["ip"]
                self.assertEqual(command[-1],
                                 f"icmp and ((src host {endpoint_ip} and dst host {runner.REMOTE_IP}) or "
                                 f"(src host {runner.REMOTE_IP} and dst host {endpoint_ip}))")
                self.assertEqual(command[command.index("-i", tcpdump_index) + 1], "net1")
            else:
                self.assertEqual(command[-1],
                                 f"icmp and host {runner.REMOTE_IP} and "
                                 "(host 10.244.1.2 or host 10.244.2.2)")
                self.assertEqual(command[command.index("-i", tcpdump_index) + 1], "frr-work0")

    def test_inventory_workload_capture_verification_uses_endpoints_with_icmp_identifiers(self):
        instance = runner.OperatorInventoryType2(None, None, "gobgpd", None)
        instance.workers = ["worker-a", "worker-b"]
        instance.frr = "run-owned-frr"
        instance.directory = Path("/run-owned/inventory-evidence")
        endpoints = {
            "worker-a": {"pod": "client-a", "podUID": "uid-a", "ip": "10.244.1.2",
                         "mac": "02:00:00:00:00:0a", "vtep": "172.18.0.2"},
            "worker-b": {"pod": "client-b", "podUID": "uid-b", "ip": "10.244.2.2",
                         "mac": "02:00:00:00:00:0b", "vtep": "172.18.0.3"},
        }

        class StopReached(Exception):
            pass

        passing = types.SimpleNamespace(returncode=0, stdout="", stderr="")
        with mock.patch.object(instance, "record"), \
                mock.patch.object(instance, "seed_forwarding_state"), \
                mock.patch.object(instance, "collect_linux_forwarding_counters", return_value={}), \
                mock.patch.object(instance, "collect_worker_linux_state", return_value={}), \
                mock.patch.object(instance, "observe_inventory_workload_interfaces", return_value={}), \
                mock.patch.object(instance, "start_inventory_capture"), \
                mock.patch.object(instance, "stop_inventory_capture"), \
                mock.patch.object(instance, "start_inventory_workload_captures", return_value=[]), \
                mock.patch.object(instance, "k", return_value=passing), \
                mock.patch.object(runner.lab, "command", return_value=passing), \
                mock.patch.object(runner, "parse_inventory_ping_result",
                                  return_value={"status": "PASS"}), \
                mock.patch.object(instance, "stop_inventory_workload_captures",
                                  side_effect=StopReached) as stop:
            with self.assertRaises(StopReached):
                instance.verify_inventory_forwarding(endpoints, {}, {})

        captures, verified = stop.call_args.args
        self.assertEqual(captures, [])
        for index, node in enumerate(instance.workers):
            self.assertEqual(verified[node]["node"], node)
            self.assertEqual(verified[node]["icmpIdentifiers"], {
                "workerToExternal": 20000 + index, "externalToWorker": 30000 + index,
            })
            for key, value in endpoints[node].items():
                self.assertEqual(verified[node][key], value)
            self.assertNotIn("icmpIdentifiers", endpoints[node])

    def test_inventory_frr_workload_tools_are_provisioned_only_in_verified_run_owned_container(self):
        instance = runner.OperatorInventoryType2(None, None, "gobgpd", None)
        instance.directory = Path("/run-owned/inventory-evidence")
        instance.frr = "run-owned-frr"
        state = {"profile": "cni-inventory-frr", "frr": instance.frr, "phase": "ready"}
        result = types.SimpleNamespace(
            returncode=0,
            stdout=("provisioning=apk-add-tcpdump-iputils-ping\n"
                    "tcpdump-path=/usr/sbin/tcpdump\n"
                    "tcpdump-version=tcpdump version 4.99.5\n"
                    "tcpdump-package=tcpdump-4.99.5-r0\n"
                    "ping-path=/usr/bin/ping\n"
                    "ping-version=ping from iputils 20250605\n"
                    "ping-package=iputils-ping-20250605-r0\n"
                    "ping-identifier-option=-e identifier\n"
                    "alpine-release=3.22.1\n"),
            stderr="",
        )
        with mock.patch.object(runner.lab, "load", return_value=state) as load, \
                mock.patch.object(runner.lab, "verify_owned", return_value={
                    "containers": {instance.frr: "container-sha"}
                }) as verify_owned, \
                mock.patch.object(instance, "docker", return_value=result) as docker, \
                mock.patch.object(instance, "record") as record:
            instance.ensure_inventory_frr_workload_tools()

        load.assert_called_once_with(instance.directory)
        verify_owned.assert_called_once_with(state)
        docker.assert_called_once()
        args, kwargs = docker.call_args
        self.assertEqual(args[:2], ("exec", instance.frr))
        self.assertIn("apk add --no-cache tcpdump iputils-ping", args[-1])
        self.assertIn("if ! printf '%s\\n' \"$ping_help\" | grep -Eq", args[-1])
        self.assertIn("needs_install=1", args[-1])
        self.assertIn("timeout -s INT -k 5 90", args[-1])
        self.assertIn("tcpdump --version", args[-1])
        self.assertIn("apk info -v tcpdump", args[-1])
        self.assertIn("ping -h", args[-1])
        self.assertIn("ping -V", args[-1])
        self.assertIn("apk info -v iputils-ping", args[-1])
        self.assertIn("grep -Eq '(^|[[:space:]])-e([[:space:],]|$)'", args[-1])
        self.assertEqual(kwargs, {"check": False, "timeout": 120})
        name, evidence = record.call_args.args
        self.assertEqual(name, "inventory-frr-workload-tools.json")
        self.assertEqual(evidence["containerID"], "container-sha")
        self.assertIn("tcpdump-package=tcpdump-4.99.5-r0", evidence["stdout"])
        self.assertIn("ping-package=iputils-ping-20250605-r0", evidence["stdout"])
        self.assertIn("ping-identifier-option=-e identifier", evidence["stdout"])

    def test_inventory_frr_workload_tools_refuse_unowned_lab_before_install(self):
        instance = runner.OperatorInventoryType2(None, None, "gobgpd", None)
        instance.directory = Path("/run-owned/inventory-evidence")
        instance.frr = "foreign-frr"
        state = {"profile": "foreign-profile", "frr": instance.frr, "phase": "ready"}
        with mock.patch.object(runner.lab, "load", return_value=state), \
                mock.patch.object(instance, "docker") as docker:
            with self.assertRaisesRegex(runner.fixture.IsolationError, "refusing capture-tool provisioning"):
                instance.ensure_inventory_frr_workload_tools()
        docker.assert_not_called()


class InventoryGapTests(unittest.TestCase):
    def test_candidate_rejection_requires_current_generation_condition(self):
        import inventory_trust_cases

        core = {"metadata": {"generation": 5}, "status": {
            "phase": "Degraded", "reason": "human-readable validation error", "observedGeneration": 5,
            "conditions": [{"type": "Degraded", "status": "True", "reason": "ReconcileFailed", "observedGeneration": 5}]}}
        self.assertTrue(inventory_trust_cases.candidate_rejected(core))
        core["status"]["conditions"][0]["observedGeneration"] = 4
        self.assertFalse(inventory_trust_cases.candidate_rejected(core))
        core["status"]["conditions"][0]["observedGeneration"] = 5
        core["status"]["observedGeneration"] = 4
        self.assertFalse(inventory_trust_cases.candidate_rejected(core))

    def test_recovery_capture_payload_requires_exact_marker_and_reply(self):
        import inventory_trust_cases

        payload = ((b"\x00" * 16) + (20000).to_bytes(2, "big") * 20).hex()
        packets = [{"icmpIdentifier": 20000, "icmpSequence": sequence, "payload": payload}
                   for sequence in (1, 2, 3) for _ in range(2)]
        inventory_trust_cases.assert_probe_payloads(packets)
        changed = json.loads(json.dumps(packets))
        changed[-1]["payload"] = "00" * 56
        with self.assertRaisesRegex(isolated.IsolationError, "marker"):
            inventory_trust_cases.assert_probe_payloads(changed)
        with self.assertRaisesRegex(isolated.IsolationError, "request/reply"):
            inventory_trust_cases.assert_probe_payloads(packets[:-1])

    def test_inventory_evidence_root_keeps_owned_paths_together(self):
        with tempfile.TemporaryDirectory() as directory, mock.patch.dict(
                runner.os.environ, {"EVPN_INVENTORY_EVIDENCE_ROOT": directory}):
            instance = runner.OperatorInventoryType2(None, None, "gobgpd", None)
            self.assertEqual(instance.directory.parent, Path(directory))
            self.assertEqual(instance.kubeconfig, instance.directory / "kubeconfig")
            self.assertEqual(instance.image_receipt, instance.directory / "image-ownership.json")

    def test_gap_selector_is_explicit_and_bounded(self):
        self.assertEqual(runner.parse_inventory_gap_cases("h052,M010"), ("H052", "M010"))
        for value in ("", "H052,H052", "M011", "H052,UNKNOWN"):
            with self.assertRaises(runner.argparse.ArgumentTypeError):
                runner.parse_inventory_gap_cases(value)
        instance = runner.OperatorInventoryType2(None, None, "gobgpd", None)
        self.assertFalse(instance.gap_cases)

    def test_gap_selector_refuses_other_scenarios_and_offline_modes(self):
        with contextlib.redirect_stderr(io.StringIO()):
            for args in (("--inventory-gap-cases", "H052"),
                         ("--scenario", runner.INVENTORY_SCENARIO, "--check-config", "absent.json",
                          "--inventory-gap-cases", "H054")):
                with self.assertRaises(SystemExit):
                    runner.main(list(args))

    def test_baseline_manifest_does_not_require_opt_in_gap_artifacts(self):
        self.assertFalse(any(name.startswith(("inventory-gap-", "inventory-probe-", "inventory-cross-worker-"))
                             for name in runner.INVENTORY_BASE_ARTIFACTS))
        self.assertNotIn("inventory-worker-a-geneve-underlay.pcap", runner.INVENTORY_BASE_ARTIFACTS)
        self.assertEqual(runner.INVENTORY_EVIDENCE_ARTIFACTS,
                         runner.INVENTORY_BASE_ARTIFACTS.union(*runner.INVENTORY_GAP_ARTIFACTS.values()))
        for case, identifiers in runner.INVENTORY_PROBE_IDS.items():
            for identifier in identifiers:
                self.assertIn(f"inventory-probe-{identifier}.json", runner.INVENTORY_GAP_ARTIFACTS[case])

    def test_negative_publications_preserve_original_and_name_each_axis(self):
        data = {"inventory.json": json.dumps({"revision": 1, "workers": [
            {"nodeUID": "original", "localVTEP": "198.18.30.2"}]}), "revision": "1", "sha256": "original"}
        original = dict(data)
        cases = runner.inventory_negative_publications(data)
        self.assertEqual(data, original)
        self.assertEqual(set(cases), {"untrusted-worker", "untrusted-vtep", "untrusted-policy-generation"})
        for case in cases.values():
            self.assertEqual(case["sha256"], hashlib.sha256(case["inventory.json"].encode()).hexdigest())
            self.assertNotEqual(case, original)

    def test_geneve_counters_require_identity_and_both_directions(self):
        before = {"actual-worker": {"tunnels": {"ovn-peer": {
            "ofport": 5, "options": ["map", [["remote_ip", "172.18.0.3"]]],
            "counters": {"rxPackets": 10, "txPackets": 20}}}}}
        after = json.loads(json.dumps(before))
        after["actual-worker"]["tunnels"]["ovn-peer"]["counters"] = {"rxPackets": 16, "txPackets": 26}
        delta = runner.inventory_geneve_counter_deltas(before, after)
        self.assertEqual(delta["actual-worker"]["ovn-peer"], {"rxPackets": 6, "txPackets": 6})
        for key, value in (("rxPackets", 10), ("txPackets", 19)):
            bad = json.loads(json.dumps(after))
            bad["actual-worker"]["tunnels"]["ovn-peer"]["counters"][key] = value
            with self.assertRaises(runner.InventoryEvidenceError):
                runner.inventory_geneve_counter_deltas(before, bad)
        after["actual-worker"]["tunnels"]["ovn-peer"]["ofport"] = 6
        with self.assertRaisesRegex(runner.InventoryEvidenceError, "identity"):
            runner.inventory_geneve_counter_deltas(before, after)

    @staticmethod
    def packet_fixture():
        endpoints = {node: {"ip": ip, "mac": mac} for node, (ip, mac) in
                     zip(("actual-a", "actual-b"), runner.INVENTORY_ENDPOINTS)}
        identifiers = {("actual-a", "actual-b"): 41000, ("actual-b", "actual-a"): 41001}
        packets, geneve = [], []
        for (src, dst), identifier in identifiers.items():
            for kind, source, destination in ((8, src, dst), (0, dst, src)):
                for seq in (1, 2, 3):
                    packets.append({"source": endpoints[source]["ip"], "destination": endpoints[destination]["ip"],
                                    "icmpType": kind, "icmpIdentifier": identifier, "icmpSequence": seq})
                    geneve.append({"innerSource": endpoints[source]["ip"], "innerDestination": endpoints[destination]["ip"],
                                   "outerSource": "172.18.0.2", "outerDestination": "172.18.0.3", "vni": 7,
                                   "icmpType": kind, "icmpIdentifier": identifier, "icmpSequence": seq})
        identity = {"datapath": {"tunnelKey": 7}, "outerIPs": {"actual-a": "172.18.0.2", "actual-b": "172.18.0.3"}}
        return endpoints, identifiers, packets, geneve, identity

    def verify_packets(self, packets=None, geneve=None, identity=None, missing=None):
        endpoints, identifiers, expected_packets, expected_geneve, expected_identity = self.packet_fixture()
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            for alias in ("a", "b"):
                for name in (f"inventory-cross-worker-{alias}-net1.pcap", f"inventory-worker-{alias}-geneve-underlay.pcap"):
                    if name != missing:
                        (root / name).write_bytes(b"mock pcap bytes")
            with mock.patch.object(runner.fixture, "decode_icmp_pcap", return_value=expected_packets if packets is None else packets), \
                    mock.patch.object(runner.fixture, "decode_geneve_pcap", return_value=expected_geneve if geneve is None else geneve):
                return runner.verify_inventory_cross_worker_captures(
                    root, endpoints, identifiers, expected_identity if identity is None else identity)

    def test_cross_worker_exact_packets_report_geneve_not_vxlan(self):
        result = self.verify_packets()
        self.assertEqual(result["overlayProtocol"], "Geneve")
        self.assertEqual(result["observedGeneveVNIs"], [7])
        self.assertIn("shared OVN", result["protocolClarification"])

    def test_cross_worker_rejects_missing_reverse_wrong_id_and_duplicates(self):
        _, _, packets, _, _ = self.packet_fixture()
        for candidate in (packets[:6], packets + packets[:1], [{**packet, "icmpIdentifier": 999} for packet in packets]):
            with self.assertRaises(runner.InventoryEvidenceError):
                self.verify_packets(packets=candidate)

    def test_cross_worker_rejects_wrong_vni_outer_identity_and_missing_capture(self):
        _, _, _, geneve, _ = self.packet_fixture()
        for field, value in (("vni", 1000), ("outerSource", "198.18.30.4"), ("icmpSequence", 9)):
            with self.assertRaises(runner.InventoryEvidenceError):
                self.verify_packets(geneve=[{**packet, field: value} for packet in geneve])
        with self.assertRaises(runner.InventoryEvidenceError):
            self.verify_packets(missing="inventory-worker-b-geneve-underlay.pcap")

    def test_gap_probe_propagates_identifier_and_uses_owned_kubectl_wrapper(self):
        instance = runner.OperatorInventoryType2(None, None, "gobgpd", None)
        endpoint = {"pod": "owned-pod", "podUID": "uid", "node": "actual-worker"}
        instance.inventory_identities = {"actual-worker": {"chassis": "chassis"}}
        ovs = {"interface": "vxlan0", "ofport": 5, "ownerEvidence": {"systemID": "chassis"},
               "tunnelCounters": {"rxPackets": 10, "txPackets": 20},
               "flowCounters": {runner.REMOTE_MAC: {"packets": 10}}}
        after = {**ovs, "tunnelCounters": {"rxPackets": 13, "txPackets": 23},
                 "flowCounters": {runner.REMOTE_MAC: {"packets": 13}}}
        with mock.patch.object(instance, "k", return_value=types.SimpleNamespace(
                stdout="3 packets transmitted, 3 received, 0% packet loss", stderr="", returncode=0)) as invoke, \
                mock.patch.object(instance, "collect_ovs_dataplane", side_effect=[ovs, after]), \
                mock.patch.object(instance, "record") as record:
            result = instance.inventory_probe(endpoint, runner.REMOTE_IP, 42001)
        self.assertEqual(result["identifier"], 42001)
        self.assertEqual(invoke.call_args.args[:6], ("-n", "evpn-system", "exec", "owned-pod", "-c", "client"))
        self.assertIn("42001", invoke.call_args.args)
        self.assertEqual(result["ovsCounterDelta"]["rxPackets"], 3)
        self.assertEqual(record.call_args.args[0], "inventory-probe-42001.json")

    def test_gap_probe_failure_is_not_reported_pass(self):
        instance = runner.OperatorInventoryType2(None, None, "gobgpd", None)
        instance.inventory_identities = {"n": {"chassis": "chassis"}}
        with mock.patch.object(instance, "k", return_value=types.SimpleNamespace(
                stdout="3 packets transmitted, 0 received, 100% packet loss", stderr="", returncode=1)), \
                mock.patch.object(instance, "collect_ovs_dataplane", return_value={}), \
                mock.patch.object(instance, "record") as record:
            with self.assertRaises(runner.InventoryEvidenceError):
                instance.inventory_probe({"pod": "p", "podUID": "u", "node": "n"}, runner.REMOTE_IP, 45000)
        self.assertEqual(record.call_args.args[1]["status"], "FAIL")

    @staticmethod
    def survivor_fixture():
        endpoint = {"vtep": "198.18.30.3", "mac": "02:00:00:00:00:12", "ip": "10.244.240.12"}
        path = {"routeType": 2, "mac": endpoint["mac"], "ip": endpoint["ip"], "peerId": endpoint["vtep"],
                "nexthops": [{"ip": endpoint["vtep"]}], "extendedCommunity": {"string": "RT:64512:1000 ET:8"}}
        worker = {"endpoint": endpoint,
                  "fdb": [{"mac": runner.REMOTE_MAC, "dst": runner.REMOTE_VTEP, "state": "permanent", "flags": ["self", "extern_learn"]}],
                  "neighbors": [{"dst": runner.REMOTE_IP, "lladdr": runner.REMOTE_MAC, "state": ["NOARP"]}],
                  "export": {"Result": {"Applied": [{"PathUUID": "owned"}]}},
                  "cni": {"Record": {"State": {"Endpoints": {"endpoint": {"Receipt": {"LSPUUID": "lsp"}}}}}}}
        return {"workers": {"worker-b": worker}, "frrRIB": {endpoint["vtep"] + ":1000": {"nlri": {"paths": [[path]]}}}}

    def test_survivor_assertion_checks_route_kernel_export_and_cni_receipts(self):
        before = self.survivor_fixture()
        runner.assert_inventory_survivor(before, json.loads(json.dumps(before)), "worker-b")
        variants = []
        for field in ("fdb", "neighbors"):
            after = json.loads(json.dumps(before))
            after["workers"]["worker-b"][field] = []
            variants.append(after)
        after = json.loads(json.dumps(before))
        after["frrRIB"] = {}
        variants.append(after)
        after = json.loads(json.dumps(before))
        after["workers"]["worker-b"]["export"]["Result"]["Applied"][0]["PathUUID"] = "foreign"
        variants.append(after)
        after = json.loads(json.dumps(before))
        after["workers"]["worker-b"]["cni"]["Record"]["State"]["Endpoints"]["endpoint"]["Receipt"]["LSPUUID"] = "replacement"
        variants.append(after)
        for after in variants:
            with self.assertRaises(runner.InventoryEvidenceError):
                runner.assert_inventory_survivor(before, after, "worker-b")

    def test_negative_no_mutation_checks_new_foreign_rows_not_just_existing_endpoint(self):
        before = self.survivor_fixture()
        before["workers"]["worker-b"]["routes"] = [{"dst": "198.18.30.0/24", "dev": "eth1"}]
        runner.assert_inventory_no_mutation(before, json.loads(json.dumps(before)))
        for table in ("fdb", "neighbors", "routes"):
            after = json.loads(json.dumps(before))
            after["workers"]["worker-b"][table].append({"dst": "198.18.30.254"})
            with self.assertRaises(runner.InventoryEvidenceError):
                runner.assert_inventory_no_mutation(before, after)
        after = json.loads(json.dumps(before))
        path = {"routeType": 2, "mac": "02:00:00:00:00:99", "ip": "10.244.240.99"}
        after["frrRIB"]["198.18.30.254:1000"] = {"hostile-nlri": {"paths": [[path]]}}
        with self.assertRaisesRegex(runner.InventoryEvidenceError, "RIB changed"):
            runner.assert_inventory_no_mutation(before, after)

    def test_recovery_reverses_roles_and_restores_each_speaker_and_bgp_session(self):
        instance = runner.OperatorInventoryType2(None, None, "gobgpd", None)
        instance.workers = ["worker-a", "worker-b"]
        endpoints = {node: {"node": node, "podUID": node, "vtep": f"198.18.30.{index + 2}"}
                     for index, node in enumerate(instance.workers)}
        epoch = {node: 0 for node in instance.workers}
        pids = {node: 100 + index for index, node in enumerate(instance.workers)}
        paused = set()

        def snapshot(*_):
            return {"workers": {node: {"agentPods": [{"metadata": {"name": node + "-agent", "uid": str(epoch[node])}}],
                                       "speakerPID": str(pids[node]),
                                       "export": {"SpeakerProcess": {"PID": pids[node], "StartTicks": str(pids[node])},
                                                  "InventoryTrust": {"Phase": "TEMPORARILY_UNAVAILABLE" if node in paused else "RECOVERY_AUTHORIZED",
                                                                     "Security": False}}}
                                for node in instance.workers}}

        def k(*args, **_):
            if args[2:4] == ("delete", "pod"):
                epoch[args[4].removesuffix("-agent")] += 1

        def worker(node, *args, **_):
            if args[:2] == ("kill", "-STOP"):
                paused.add(node)
            elif args[:2] == ("kill", "-CONT"):
                paused.remove(node)
            elif args[1] == "start":
                pids[node] += 10

        with mock.patch.object(instance, "gap_snapshot", side_effect=snapshot), \
                mock.patch.object(instance, "await_gap_routes", side_effect=snapshot), \
                mock.patch.object(instance, "inventory_probe", return_value={"status": "PASS"}) as probes, \
                mock.patch.object(instance, "k", side_effect=k), \
                mock.patch.object(instance, "worker", side_effect=worker) as workers, \
                mock.patch.object(instance, "docker") as docker, \
                mock.patch.object(instance, "record") as record, \
                mock.patch.object(instance, "await_inventory_acceptance"), \
                mock.patch.object(instance, "await_external_import"), \
                mock.patch.object(instance, "await_local_export"), \
                mock.patch.object(instance, "run_inventory_endpoint_outage", return_value={"status": "PASS"}) as lifecycle, \
                mock.patch.object(instance, "verify_inventory_recovered_dataplane", return_value={"status": "PASS"}), \
                mock.patch.object(runner, "assert_inventory_survivor"):
            instance.run_inventory_recovery_gaps(endpoints, {node: {} for node in instance.workers}, "installation")
        report = record.call_args.args[1]
        self.assertEqual(report["status"], "PASS")
        self.assertEqual(set(report["roles"]), set(instance.workers))
        self.assertEqual({call.args[2] for call in probes.call_args_list}, set(runner.INVENTORY_PROBE_IDS["H053"]))
        self.assertEqual(workers.call_count, 8)
        self.assertEqual(docker.call_count, 4)
        self.assertEqual(lifecycle.call_count, 2)

    def test_recovery_failure_records_failed_case_and_starts_stopped_speaker(self):
        instance = runner.OperatorInventoryType2(None, None, "gobgpd", None)
        instance.workers = ["a", "b"]
        initial = {"workers": {node: {"agentPods": [{"metadata": {"name": node, "uid": "old"}}],
                                       "speakerPID": "100",
                                       "export": {"SpeakerProcess": {"PID": 100, "StartTicks": "1"},
                                                  "InventoryTrust": {"Phase": "RECOVERY_AUTHORIZED", "Security": False}}}
                               for node in instance.workers}}
        replacement = json.loads(json.dumps(initial))
        replacement["workers"]["a"]["agentPods"][0]["metadata"]["uid"] = "new"
        paused = json.loads(json.dumps(replacement))
        paused["workers"]["a"]["export"]["InventoryTrust"]["Phase"] = "TEMPORARILY_UNAVAILABLE"
        with mock.patch.object(instance, "gap_snapshot", side_effect=[initial, initial, replacement, paused, replacement, replacement]), \
                mock.patch.object(instance, "await_gap_routes", side_effect=[replacement, runner.InventoryEvidenceError("ROUTE_STATE_MISMATCH", "withdrawal missing")]), \
                mock.patch.object(instance, "inventory_probe", return_value={}), \
                mock.patch.object(instance, "k"), mock.patch.object(instance, "worker") as worker, \
                mock.patch.object(instance, "record") as record, \
                mock.patch.object(instance, "await_inventory_acceptance"), \
                mock.patch.object(instance, "await_external_import"), \
                mock.patch.object(instance, "await_local_export"), \
                mock.patch.object(runner, "assert_inventory_survivor"):
            with self.assertRaises(runner.InventoryEvidenceError):
                instance.run_inventory_recovery_gaps({node: {"vtep": node} for node in instance.workers}, {node: {} for node in instance.workers}, "installation")
        worker.assert_any_call("a", "systemctl", "start", "evpn-e2e-gobgpd.service")
        self.assertEqual(record.call_args.args[1]["status"], "FAIL")
        self.assertIn("speakerStarted", record.call_args.args[1]["roles"]["a"])
        self.assertEqual(record.call_args.args[1]["failureSnapshot"], replacement)


if __name__ == "__main__":
    unittest.main()
