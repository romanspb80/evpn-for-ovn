"""Offline safety checks for the isolated secondary-CNI fixture."""

import copy
import json
import os
from pathlib import Path
import struct
import subprocess
import tempfile
import types
import unittest
from unittest import mock

import isolated_secondary_cni as isolated


class WorkloadFailureEvidenceTests(unittest.TestCase):
    def test_records_failed_and_timed_out_diagnostic_commands(self):
        fixture = isolated.IsolatedSecondaryCNI()
        fixture.runtime_pods = {node: "runtime-pod" for node in fixture.workers}
        calls = []

        def capture(args, **kwargs):
            calls.append(args)
            if "journalctl" in args and "kubelet" in args:
                raise subprocess.TimeoutExpired(args, 20)
            return types.SimpleNamespace(returncode=1, stdout="", stderr="unavailable")

        with mock.patch.object(isolated, "run", side_effect=capture), \
                mock.patch.object(fixture, "record") as record:
            fixture.capture_workload_failure()
        name, evidence = record.call_args.args
        self.assertEqual(name, "workload-failure-evidence.json")
        self.assertEqual(evidence["pod-a"]["returncode"], 1)
        self.assertIn("error", evidence[fixture.workers[0] + "-kubelet"])
        self.assertIn(fixture.workers[0] + "-cni-invocations", evidence)
        self.assertIn(fixture.workers[1] + "-node-agent-logs", evidence)
        self.assertIn("pod-b-events", evidence)
        self.assertIn("nad-b", evidence)
        self.assertIn("nb-switches", evidence)
        self.assertIn("sb-datapaths", evidence)
        self.assertIn(fixture.workers[0] + "-ovs-interfaces", evidence)
        self.assertIn(fixture.workers[1] + "-ovs-flows", evidence)
        self.assertTrue(all(call[0] in ("kubectl", "docker") for call in calls))

    def test_capture_happens_before_lab_teardown_and_preserves_failure(self):
        fixture = isolated.IsolatedSecondaryCNI()
        events = []
        with tempfile.TemporaryDirectory() as directory:
            fixture.directory = Path(directory)
            (fixture.directory / "lab.json").touch()
            with mock.patch.object(fixture, "initialize"), \
                    mock.patch.object(fixture, "build"), \
                    mock.patch.object(fixture, "provision"), \
                    mock.patch.object(fixture, "test_endpoints",
                                      side_effect=isolated.IsolationError("workload failed")), \
                    mock.patch.object(fixture, "record"), \
                    mock.patch.object(fixture, "capture_workload_failure",
                                      side_effect=lambda: events.append("capture")), \
                    mock.patch.object(fixture, "verify_host_dataplane"), \
                    mock.patch.object(isolated.lab, "load", return_value={"phase": "ready"}), \
                    mock.patch.object(isolated.lab, "collect"), \
                    mock.patch.object(isolated.lab, "down",
                                      side_effect=lambda _: events.append("down")):
                with self.assertRaisesRegex(isolated.IsolationError, "workload failed"):
                    fixture.run()
        self.assertEqual(events, ["capture", "down"])

    def test_unexpected_error_preserves_traceback_and_captures_before_cleanup(self):
        fixture = isolated.IsolatedSecondaryCNI()
        events = []
        records = {}
        with tempfile.TemporaryDirectory() as directory:
            fixture.directory = Path(directory)
            (fixture.directory / "lab.json").touch()
            with mock.patch.object(fixture, "initialize"), \
                    mock.patch.object(fixture, "build"), \
                    mock.patch.object(fixture, "provision"), \
                    mock.patch.object(fixture, "test_endpoints", side_effect=TypeError("bad keyword")), \
                    mock.patch.object(fixture, "record",
                                      side_effect=lambda name, data: records.update({name: data})), \
                    mock.patch.object(fixture, "capture_workload_failure",
                                      side_effect=lambda: events.append("capture")), \
                    mock.patch.object(fixture, "verify_host_dataplane"), \
                    mock.patch.object(isolated.lab, "load", return_value={"phase": "ready"}), \
                    mock.patch.object(isolated.lab, "collect"), \
                    mock.patch.object(isolated.lab, "down",
                                      side_effect=lambda _: events.append("down")):
                with self.assertRaisesRegex(TypeError, "bad keyword"):
                    fixture.run()
        self.assertEqual(events, ["capture", "down"])
        self.assertIn("TypeError: bad keyword", records["failure.json"]["traceback"])

    def test_worker_forwards_check_and_preserves_nonzero_output(self):
        fixture = isolated.IsolatedSecondaryCNI()
        failed = subprocess.CompletedProcess([], 3, "stdout", "stderr")
        with mock.patch.object(isolated, "run", return_value=failed) as command:
            result = fixture.worker("worker", "test", "-e", "/file", check=False, timeout=7)
            self.assertIs(result, failed)
            command.assert_called_once_with(
                ["docker", "exec", "worker", "test", "-e", "/file"],
                check=False, timeout=7)
        with mock.patch.object(isolated, "run",
                               side_effect=isolated.IsolationError("command failed")) as command:
            with self.assertRaisesRegex(isolated.IsolationError, "command failed"):
                fixture.worker("worker", "false")
            self.assertTrue(command.call_args.kwargs["check"])


class SecondaryCentralManifestTests(unittest.TestCase):
    def service_documents(self, owner):
        service = {"metadata": {"labels": {"evpn.e2e/owner": owner}},
                   "spec": {"clusterIP": "10.96.40.2",
                            "ports": [{"name": "southbound", "port": 6642,
                                       "targetPort": 6642, "protocol": "TCP"}]}}
        pod = {"metadata": {"uid": "central-uid"},
               "status": {"podIP": "10.244.0.5"}}
        endpoints = {"subsets": [{"ports": [{"name": "southbound", "port": 6642,
                                              "protocol": "TCP"}],
                                  "addresses": [{"ip": "10.244.0.5",
                                                 "targetRef": {"kind": "Pod",
                                                               "uid": "central-uid"}}]}]}
        return service, endpoints, pod

    def test_numeric_sbdb_remote_requires_owned_service_and_ready_central_backend(self):
        service, endpoints, pod = self.service_documents("owner")
        self.assertEqual(isolated.secondary_sbdb_remote(service, endpoints, pod, "owner"),
                         "tcp:10.96.40.2:6642")
        service["spec"]["clusterIP"] = "fd00::7"
        self.assertEqual(isolated.secondary_sbdb_remote(service, endpoints, pod, "owner"),
                         "tcp:[fd00::7]:6642")
        for invalid in ("None", "", "0.0.0.0", "127.0.0.1", "224.0.0.1", "not-an-ip"):
            service["spec"]["clusterIP"] = invalid
            with self.subTest(cluster_ip=invalid), self.assertRaises(isolated.IsolationError):
                isolated.secondary_sbdb_remote(service, endpoints, pod, "owner")

    def test_rejects_wrong_port_owner_or_unready_foreign_backend(self):
        service, endpoints, pod = self.service_documents("owner")
        with self.assertRaisesRegex(isolated.IsolationError, "ownership mismatch"):
            isolated.secondary_sbdb_remote(service, endpoints, pod, "foreign")
        service["spec"]["ports"][0]["targetPort"] = 6641
        with self.assertRaisesRegex(isolated.IsolationError, "port mismatch"):
            isolated.secondary_sbdb_remote(service, endpoints, pod, "owner")
        service["spec"]["ports"][0]["targetPort"] = 6642
        endpoints["subsets"][0]["addresses"][0]["targetRef"]["uid"] = "foreign"
        with self.assertRaises(isolated.SecondaryEndpointNotReady):
            isolated.secondary_sbdb_remote(service, endpoints, pod, "owner")
        endpoints["subsets"][0]["notReadyAddresses"] = endpoints["subsets"][0].pop("addresses")
        with self.assertRaises(isolated.SecondaryEndpointNotReady):
            isolated.secondary_sbdb_remote(service, endpoints, pod, "owner")

    def test_central_runtime_directories_are_pod_owned_empty_dirs(self):
        lab = isolated.IsolatedSecondaryCNI()
        manifests = {}
        service, endpoints, pod = self.service_documents(lab.owner)
        pod["metadata"]["labels"] = {"evpn.e2e/owner": lab.owner}
        pod["spec"] = {"nodeName": lab.cluster + "-control-plane"}

        def kubectl(*args, **kwargs):
            if "service" in args and "get" in args:
                return types.SimpleNamespace(stdout=json.dumps(service))
            if "endpoints" in args:
                return types.SimpleNamespace(stdout=json.dumps(endpoints))
            if "pods" in args:
                return types.SimpleNamespace(stdout=json.dumps({"items": [pod]}))
            return types.SimpleNamespace(stdout="")

        with mock.patch.object(lab, "k", side_effect=kubectl), \
                mock.patch.object(lab, "record"), \
                mock.patch.object(lab, "apply", side_effect=lambda name, value: manifests.update({name: value})), \
                mock.patch.object(lab, "deploy_runtime_daemonset") as deploy_workers, \
                mock.patch.object(lab, "capture_runtime_identities"):
            lab.deploy_secondary_ovn()
        deploy_workers.assert_called_once_with("tcp:10.96.40.2:6642")

        spec = manifests["secondary-central.json"]["spec"]["template"]["spec"]
        volumes = {volume["name"]: volume for volume in spec["volumes"]}
        mounts = {mount["mountPath"]: mount["name"]
                  for mount in spec["containers"][0]["volumeMounts"]}
        for path in ("/var/run/ovn", "/var/lib/ovn"):
            self.assertEqual(volumes[mounts[path]]["emptyDir"], {})
            self.assertNotIn("hostPath", volumes[mounts[path]])
        self.assertEqual(spec["nodeSelector"]["kubernetes.io/hostname"],
                         lab.cluster + "-control-plane")
        self.assertNotIn(spec["nodeSelector"]["kubernetes.io/hostname"], lab.workers)
        self.assertNotIn("hostPID", spec)
        self.assertNotIn("hostNetwork", spec)
        self.assertIn({"key": "node-role.kubernetes.io/control-plane",
                       "operator": "Exists", "effect": "NoSchedule"}, spec["tolerations"])

    def test_workers_keep_host_pid_and_run_scoped_runtime_mounts(self):
        lab = isolated.IsolatedSecondaryCNI()
        manifests = {}
        pods = [{"spec": {"nodeName": node},
                 "metadata": {"name": f"worker-{index}", "uid": f"uid-{index}"},
                 "status": {"containerStatuses": []}}
                for index, node in enumerate(lab.workers)]

        def kubectl(*args, **kwargs):
            return types.SimpleNamespace(stdout=json.dumps({"items": pods}))

        with mock.patch.object(lab, "k", side_effect=kubectl), \
                mock.patch.object(lab, "record"), \
                mock.patch.object(lab, "worker",
                                  return_value=types.SimpleNamespace(stdout="shared\n")), \
                mock.patch.object(lab, "apply",
                                  side_effect=lambda name, value: manifests.update({name: value})):
            lab.deploy_runtime_daemonset("tcp:10.96.40.2:6642")
        spec = manifests["secondary-runtime.json"]["spec"]["template"]["spec"]
        self.assertTrue(spec["hostPID"])
        self.assertTrue(spec["hostNetwork"])
        ovn = next(container for container in spec["containers"] if container["name"] == "ovn")
        agent = next(container for container in spec["containers"] if container["name"] == "node-agent")
        self.assertEqual(next(m for m in agent["volumeMounts"] if m["name"] == "netns")[
            "mountPropagation"], "HostToContainer")
        self.assertNotIn("mountPropagation",
                         next(m for m in ovn["volumeMounts"] if m["name"] == "netns"))
        self.assertEqual(next(env["value"] for env in ovn["env"]
                              if env["name"] == "OVN_REMOTE"), "tcp:10.96.40.2:6642")
        probe = ovn["readinessProbe"]["exec"]["command"][-1]
        self.assertIn("grep -qx connected", probe)
        for value, expected in (("connected\n", 0), ("disconnected\n", 1),
                                ("not connected\n", 1), ("", 1)):
            result = subprocess.run(["grep", "-qx", "connected"], input=value,
                                    text=True, capture_output=True)
            self.assertEqual(result.returncode, expected)
        volumes = {volume["name"]: volume for volume in spec["volumes"]}
        for name in ("ovs-run", "ovs-db", "ovn-run", "identity"):
            self.assertTrue(volumes[name]["hostPath"]["path"].startswith(
                f"/run/evpn-secondary/{lab.cluster}/"))

    def test_worker_rejects_private_netns_source_before_deployment(self):
        lab = isolated.IsolatedSecondaryCNI()
        with mock.patch.object(lab, "worker",
                               return_value=types.SimpleNamespace(stdout="private\n")), \
                mock.patch.object(lab, "apply") as apply:
            with self.assertRaisesRegex(isolated.IsolationError, "source mount is not shared"):
                lab.deploy_runtime_daemonset("tcp:10.96.40.2:6642")
        apply.assert_not_called()


class DatapathLookupTests(unittest.TestCase):
    switch = "11111111-1111-1111-1111-111111111111"
    datapath = "22222222-2222-2222-2222-222222222222"

    def row(self, key=42, switch=None):
        return [["uuid", self.datapath],
                ["map", [["logical-switch", switch or self.switch], ["name", "ls-secondary"]]],
                key]

    def doc(self, rows):
        return json.dumps({"headings": ["_uuid", "external_ids", "tunnel_key"], "data": rows})

    def test_exact_logical_switch_uuid(self):
        result = isolated.datapath_tunnel_key(self.doc([self.row()]), self.switch)
        self.assertEqual(result["datapathUUID"], self.datapath)
        self.assertEqual(result["tunnelKey"], 42)
        self.assertEqual(result["externalIDs"]["logical-switch"], self.switch)

    def test_missing_or_ambiguous_datapath_rejected(self):
        for rows in ([], [self.row(), self.row()], [self.row(switch=self.datapath)]):
            with self.subTest(rows=rows), self.assertRaisesRegex(isolated.IsolationError, "found"):
                isolated.datapath_tunnel_key(self.doc(rows), self.switch)

    def test_invalid_key_rejected(self):
        for value in (None, 0, -1, 0x1000000, "42", True, ["set", []]):
            with self.subTest(value=value), self.assertRaisesRegex(isolated.IsolationError, "tunnel_key"):
                isolated.datapath_tunnel_key(self.doc([self.row(key=value)]), self.switch)

    def test_unexpected_output_rejected(self):
        for raw in ("", "42", "not-json", "{}", self.doc([[]]),
                    self.doc([[["uuid", self.datapath], ["map", "bad"], 42]])):
            with self.subTest(raw=raw), self.assertRaises(isolated.IsolationError):
                isolated.datapath_tunnel_key(raw, self.switch)
        with self.assertRaises(isolated.IsolationError):
            isolated.datapath_tunnel_key(self.doc([self.row()]), "uuid\nuuid")


class PortBindingTests(unittest.TestCase):
    def setUp(self):
        self.lab = isolated.IsolatedSecondaryCNI()
        self.uuid = ["uuid", "11111111-1111-1111-1111-111111111111"]
        self.binding = ["port-a", self.uuid, True, ["uuid", "22222222-2222-2222-2222-222222222222"]]
        self.chassis = [self.uuid, "chassis-a", "worker-a"]

    def response(self, bindings, chassis):
        def k(*args, **kwargs):
            table = args[-1]
            self.assertIn("--timeout=5", args)
            self.assertNotIn("--wait=hv", args)
            if table == "Port_Binding":
                doc = {"headings": ["logical_port", "chassis", "up", "_uuid"],
                       "data": bindings}
            else:
                doc = {"headings": ["_uuid", "name", "hostname"], "data": chassis}
            return types.SimpleNamespace(stdout=json.dumps(doc))
        return k

    def test_exact_up_binding_to_expected_worker(self):
        with mock.patch.object(self.lab, "k", side_effect=self.response(
                [self.binding], [self.chassis])):
            self.assertEqual(self.lab.wait_for_binding("port-a", "worker-a", "chassis-a"),
                             {"chassisUUID": self.uuid[1]})

    def test_missing_wrong_chassis_wrong_worker_and_down_timeout(self):
        for bindings, chassis in (
                ([], [self.chassis]), ([self.binding], []),
                ([["port-a", ["uuid", "other"], True, self.binding[3]]], [self.chassis]),
                ([self.binding], [[self.uuid, "chassis-a", "worker-b"]]),
                ([["port-a", self.uuid, False, self.binding[3]]], [self.chassis])):
            with self.subTest(bindings=bindings, chassis=chassis), \
                    mock.patch.object(self.lab, "k", side_effect=self.response(bindings, chassis)):
                with self.assertRaisesRegex(isolated.IsolationError, "timed out.*port-a.*worker-a"):
                    self.lab.wait_for_binding("port-a", "worker-a", "chassis-a", timeout=0)

    def test_duplicate_bindings_or_chassis_rejected(self):
        for bindings, chassis in (([self.binding, self.binding], [self.chassis]),
                                  ([self.binding], [self.chassis, self.chassis])):
            with self.subTest(bindings=bindings, chassis=chassis), \
                    mock.patch.object(self.lab, "k", side_effect=self.response(bindings, chassis)):
                with self.assertRaisesRegex(isolated.IsolationError, "ambiguous"):
                    self.lab.wait_for_binding("port-a", "worker-a", "chassis-a", timeout=0)

    def test_poll_retries_until_binding_becomes_ready(self):
        responses = [self.response([], [self.chassis]),
                     self.response([self.binding], [self.chassis])]
        def k(*args, **kwargs):
            response = responses[0]
            result = response(*args, **kwargs)
            if args[-1] == "Chassis" and len(responses) > 1:
                responses.pop(0)
            return result
        with mock.patch.object(self.lab, "k", side_effect=k), \
                mock.patch.object(isolated.time, "sleep"):
            self.assertEqual(self.lab.wait_for_binding("port-a", "worker-a", "chassis-a"),
                             {"chassisUUID": self.uuid[1]})


class PrimaryOwnershipTests(unittest.TestCase):
    def test_lab_rejects_missing_kernel_module_before_provisioning(self):
        lab = isolated.IsolatedSecondaryCNI()
        with mock.patch.object(isolated.shutil, "which", return_value="/usr/bin/test"), \
                mock.patch.object(Path, "exists", return_value=False), \
                mock.patch.object(Path, "is_dir", return_value=False), \
                mock.patch.object(isolated.lab, "init") as init:
            with self.assertRaisesRegex(isolated.IsolationError, "kernel module must be loaded"):
                lab.initialize()
        init.assert_not_called()

    def snapshot(self):
        return {
            "kindnet": {"uid": "kindnet-uid", "containerIDs": ["containerd://primary"]},
            "cni_config_sha256": "abc123",
            "links": isolated.primary_link_inventory([
                {"ifindex": 1, "ifname": "lo", "flags": ["LOOPBACK"], "mtu": 65536},
                {"ifindex": 2, "ifname": "eth0", "address": "02:00:00:00:00:01",
                 "ifalias": "", "mtu": 1500, "operstate": "UP",
                 "linkinfo": {"info_kind": "veth"}},
            ]),
            "addresses": {"lo": [], "eth0": [{"family": "inet", "local": "192.0.2.2",
                                                "prefixlen": 24, "scope": "global",
                                                "label": None}]},
            "routes": [{"dst": "default", "gateway": "192.0.2.1"}],
        }

    def test_primary_inventory_accepts_additive_secondary_links(self):
        before = self.snapshot()
        after = self.snapshot()
        after["links"]["br-int"] = {"ifindex": 3, "ifname": "br-int"}
        isolated.assert_primary_preserved(before, after)

    def test_primary_link_deletion_is_rejected(self):
        before = self.snapshot()
        after = self.snapshot()
        del after["links"]["eth0"]
        with self.assertRaisesRegex(isolated.IsolationError, "inventory changed"):
            isolated.assert_primary_preserved(before, after)

    def test_primary_link_mutation_is_rejected(self):
        before = self.snapshot()
        after = self.snapshot()
        after["links"]["eth0"]["ifindex"] = 99
        with self.assertRaisesRegex(isolated.IsolationError, "inventory changed"):
            isolated.assert_primary_preserved(before, after)

    def test_primary_cni_identity_change_is_rejected(self):
        before = self.snapshot()
        after = self.snapshot()
        after["kindnet"]["uid"] = "replacement"
        with self.assertRaisesRegex(isolated.IsolationError, "Pod identity changed"):
            isolated.assert_primary_preserved(before, after)

    def test_primary_cni_config_change_is_rejected(self):
        before = self.snapshot()
        after = self.snapshot()
        after["cni_config_sha256"] = "changed"
        with self.assertRaisesRegex(isolated.IsolationError, "conflist changed"):
            isolated.assert_primary_preserved(before, after)

    def test_primary_address_change_is_rejected(self):
        before = self.snapshot()
        after = self.snapshot()
        after["addresses"]["eth0"][0]["local"] = "192.0.2.99"
        with self.assertRaisesRegex(isolated.IsolationError, "addresses changed"):
            isolated.assert_primary_preserved(before, after)

    def test_missing_primary_address_is_rejected(self):
        before = self.snapshot()
        after = self.snapshot()
        after["addresses"]["eth0"] = []
        with self.assertRaisesRegex(isolated.IsolationError, "addresses changed"):
            isolated.assert_primary_restored(before, after)

    def test_primary_route_removal_is_rejected(self):
        before = self.snapshot()
        after = self.snapshot()
        after["routes"] = []
        with self.assertRaisesRegex(isolated.IsolationError, "routes changed"):
            isolated.assert_primary_preserved(before, after)

    def test_primary_route_addition_is_allowed_during_pod_lifecycle(self):
        before = self.snapshot()
        after = self.snapshot()
        after["routes"].append({"dst": "10.244.240.10", "dev": "eth0"})
        isolated.assert_primary_preserved(before, after)

    def test_primary_routes_allow_additional_entries_after_cni_del(self):
        before = self.snapshot()
        after = self.snapshot()
        after["routes"].append({"dst": "10.244.240.10", "dev": "eth0"})
        isolated.assert_primary_restored(before, after)

    def test_primary_identical_state_and_reordered_addresses_and_routes(self):
        before = self.snapshot()
        before["addresses"]["eth0"].append({
            "family": "inet6", "local": "fe80::1", "prefixlen": 64,
            "scope": "link", "label": None})
        before["routes"].append({"dst": "192.0.2.0/24", "dev": "eth0"})
        after = copy.deepcopy(before)
        isolated.assert_primary_restored(before, after)
        after["addresses"]["eth0"].reverse()
        after["routes"].reverse()
        isolated.assert_primary_restored(before, after)

    def test_additional_addresses_on_primary_and_unrelated_links_are_allowed(self):
        before = self.snapshot()
        after = self.snapshot()
        after["addresses"]["eth0"].append({
            "family": "inet6", "local": "fe80::1234", "prefixlen": 64,
            "scope": "link", "label": None})
        after["addresses"]["br-int"] = [{
            "family": "inet6", "local": "fe80::2", "prefixlen": 64,
            "scope": "link", "label": None}]
        isolated.assert_primary_restored(before, after)

    def test_changed_primary_prefix_is_rejected(self):
        before = self.snapshot()
        after = self.snapshot()
        after["addresses"]["eth0"][0]["prefixlen"] = 25
        with self.assertRaisesRegex(isolated.IsolationError, "addresses changed"):
            isolated.assert_primary_restored(before, after)

    def test_changed_primary_route_is_rejected(self):
        before = self.snapshot()
        after = self.snapshot()
        after["routes"][0]["gateway"] = "192.0.2.254"
        with self.assertRaisesRegex(isolated.IsolationError, "routes changed"):
            isolated.assert_primary_restored(before, after)

    def test_missing_primary_address_interface_is_rejected(self):
        before = self.snapshot()
        after = self.snapshot()
        del after["addresses"]["eth0"]
        with self.assertRaisesRegex(isolated.IsolationError, "interface missing"):
            isolated.assert_primary_restored(before, after)

    def test_primary_snapshot_is_recorded_before_failed_assertion(self):
        fixture = isolated.IsolatedSecondaryCNI()
        fixture.workers = ["node-a", "node-b"]
        fixture.primary_before = {node: self.snapshot() for node in fixture.workers}
        after = {node: self.snapshot() for node in fixture.workers}
        after["node-a"]["addresses"]["eth0"] = []
        with mock.patch.object(fixture, "primary_snapshot",
                               side_effect=lambda node, **kwargs: after[node]), \
                mock.patch.object(fixture, "record") as record:
            with self.assertRaisesRegex(isolated.IsolationError, "addresses changed"):
                fixture.verify_primary("primary-after-del.json")
        record.assert_called_once_with("primary-after-del.json", after)

    def test_logical_switch_port_name_matches_node_agent_identity(self):
        uid = "01234567-89ab-cdef-0123-456789abcdef"
        name = isolated.expected_lsp_name(uid)
        self.assertEqual(len(name), 63)
        self.assertEqual(name, "net1-secondary-isolation-s-secondary-isolations01234567-89ab-cd")

    def test_secondary_addresses_use_documentation_only_network(self):
        self.assertEqual(isolated.secondary_ip("a"), "192.0.2.10")
        self.assertEqual(isolated.secondary_ip("b"), "192.0.2.11")

    def test_host_preflight_accepts_only_empty_ovs_ovn_inventory(self):
        isolated.assert_host_dataplane_isolated({
            "processes": [], "sockets": [], "ovsLinks": {},
        })

    def test_host_preflight_rejects_existing_ovs_datapath(self):
        with self.assertRaisesRegex(isolated.IsolationError, "existing OVS/OVN"):
            isolated.assert_host_dataplane_isolated({
                "processes": [], "sockets": [],
                "ovsLinks": {"br-int": {"kind": "openvswitch"}},
            })


class ProcessNamespaceTests(unittest.TestCase):
    def test_permission_denied_uses_read_only_privileged_namespace_lookup(self):
        response = types.SimpleNamespace(returncode=0, stdout="12345\n", stderr="")
        with mock.patch.object(isolated.os, "stat", side_effect=PermissionError), \
                mock.patch.object(isolated, "run", return_value=response) as command:
            self.assertEqual(isolated.process_netns("123"), (12345, "observed"))
        self.assertEqual(command.call_args.args[0],
                         ["sudo", "-n", "stat", "-Lc", "%i", "/proc/123/ns/net"])

    def test_namespace_uncertainty_remains_host_or_unknown(self):
        response = types.SimpleNamespace(returncode=1, stdout="", stderr="Permission denied")
        with mock.patch.object(isolated.os, "stat", side_effect=PermissionError), \
                mock.patch.object(isolated.Path, "exists", return_value=True), \
                mock.patch.object(isolated, "run", return_value=response):
            self.assertEqual(isolated.process_netns("123"), (None, "permission-denied"))
        with mock.patch.object(isolated.os, "stat", side_effect=FileNotFoundError), \
                mock.patch.object(isolated.Path, "exists", return_value=True):
            self.assertEqual(isolated.process_netns("123"), (None, "namespace-unavailable"))
        with mock.patch.object(isolated.os, "stat", side_effect=FileNotFoundError), \
                mock.patch.object(isolated.Path, "exists", return_value=False):
            self.assertEqual(isolated.process_netns("123"), (None, "terminated"))

    def test_unverified_nonhost_process_is_not_claimed_owned(self):
        responses = [
            types.SimpleNamespace(stdout="123 ovsdb-server\n124 ovn-northd\n"),
            types.SimpleNamespace(stdout="[]"),
        ]
        with mock.patch.object(isolated, "run", side_effect=responses), \
                mock.patch.object(isolated.os, "stat", return_value=types.SimpleNamespace(st_ino=10)), \
                mock.patch.object(isolated, "process_netns", side_effect=[(20, "observed"),
                                                                         (None, "permission-denied")]), \
                mock.patch.object(isolated.Path, "exists", return_value=False):
            inventory = isolated.host_dataplane_inventory()
        self.assertEqual(inventory["processes"][0]["ownership"], "non-host-unverified")
        self.assertEqual(inventory["hostProcesses"], [inventory["processes"][1]])
        with self.assertRaises(isolated.IsolationError):
            isolated.assert_host_dataplane_isolated(inventory)


class SecondaryRuntimeSafetyTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.root = Path(self.temp.name)
        self.env = os.environ.copy()
        self.ovs_run = self.root / "openvswitch"
        self.ovs_db = self.root / "ovs-db"
        self.ovn_run = self.root / "ovn"
        self.identity = self.root / "identity"
        self.sys_class = self.root / "sys-class-net"
        self.ovs_module = self.root / "openvswitch-module"
        for directory in (self.ovs_run, self.ovs_db, self.ovn_run,
                          self.identity, self.sys_class, self.ovs_module):
            directory.mkdir()
        self.proc = self.root / "proc"
        (self.proc / "self/ns").mkdir(parents=True)
        (self.proc / "1/ns").mkdir(parents=True)
        (self.proc / "self/ns/net").symlink_to("net:[42]")
        (self.proc / "1/ns/net").symlink_to("net:[42]")
        self.ps = self.executable("ps", "exit 0\n")
        self.ip = self.executable("ip", 'printf "1: lo: <LOOPBACK> mtu 65536\\n"\n')
        self.ovs_ctl_log = self.root / "ovs-ctl.log"
        self.ovs_ctl = self.executable("ovs-ctl", 'printf "%s\\n" "$*" >> "$OVS_CTL_LOG"\n')
        self.vswitchd = self.executable(
            "ovs-vswitchd", 'printf "%s\\n" "$*" >> "$OVS_VSWITCHD_LOG"\n')
        self.ovn_ctl = self.executable("ovn-ctl", "exit 0\n")
        self.preflight = Path(__file__).parents[2] / "deploy/containerlab/secondary-ovn-worker-preflight.sh"
        self.worker = Path(__file__).parents[2] / "deploy/containerlab/secondary-ovn-worker.sh"
        self.env.update({
            "OVS_RUN_DIR": str(self.ovs_run),
            "OVS_DB_DIR": str(self.ovs_db),
            "OVN_RUN_DIR": str(self.ovn_run),
            "IDENTITY_DIR": str(self.identity),
            "PROC_ROOT": str(self.proc),
            "SYS_CLASS_NET_ROOT": str(self.sys_class),
            "PS": str(self.ps),
            "IP": str(self.ip),
            "OVS_CTL": str(self.ovs_ctl),
            "OVS_VSWITCHD": str(self.vswitchd),
            "OVS_VSWITCHD_LOG": str(self.root / "ovs-vswitchd.log"),
            "OVS_MODULE_DIR": str(self.ovs_module),
            "OVN_CTL": str(self.ovn_ctl),
            "PREFLIGHT_SCRIPT": str(self.preflight),
            "OVS_CTL_LOG": str(self.ovs_ctl_log),
            "SECONDARY_OWNER": "secondary-test-owner",
            "OVN_REMOTE": "tcp:secondary-ovn:6642",
            "OVN_ENCAP_IP": "192.0.2.10",
        })

    def tearDown(self):
        self.temp.cleanup()

    def executable(self, name, body):
        path = self.root / name
        path.write_text("#!/bin/sh\nset -eu\n" + body)
        path.chmod(0o755)
        return path

    def run_script(self, path, *, check=False):
        return subprocess.run(
            ["/bin/sh", str(path)], env=self.env, text=True,
            stdout=subprocess.PIPE, stderr=subprocess.PIPE, check=check, timeout=10)

    def snapshot_dirs(self):
        return {
            directory: sorted(str(path.relative_to(directory))
                              for path in directory.rglob("*"))
            for directory in (self.ovs_run, self.ovs_db, self.ovn_run, self.identity)
        }

    def test_worker_preflight_rejects_foreign_br_int_before_ovs_start(self):
        (self.sys_class / "br-int").mkdir()
        before = self.snapshot_dirs()
        result = self.run_script(self.worker)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("conflicting OVS interface", result.stderr)
        self.assertFalse(self.ovs_ctl_log.exists())
        self.assertEqual(self.snapshot_dirs(), before)

    def test_worker_exports_default_directories_to_child_preflight(self):
        source = self.worker.read_text()
        for variable, default, directory in (
                ("OVS_RUN_DIR", "/var/run/openvswitch", self.ovs_run),
                ("OVS_DB_DIR", "/etc/openvswitch", self.ovs_db),
                ("OVN_RUN_DIR", "/var/run/ovn", self.ovn_run),
                ("IDENTITY_DIR", "/run/evpn-secondary/identity", self.identity)):
            self.env.pop(variable)
            source = source.replace(f"${{{variable}:-{default}}}", f"${{{variable}:-{directory}}}")
        worker = self.root / "worker-defaults.sh"
        worker.write_text(source)
        result = self.run_script(worker)
        self.assertNotIn("missing isolated", result.stderr)
        self.assertTrue(self.ovs_ctl_log.exists())

    def test_worker_rejects_missing_directory_before_start(self):
        self.ovs_run.rmdir()
        result = self.run_script(self.worker)
        self.assertIn("isolated runtime directory is missing", result.stderr)
        self.assertFalse(self.ovs_ctl_log.exists())

    def test_worker_preflight_rejects_conflicting_process_before_ovs_start(self):
        self.ps.write_text(
            "#!/bin/sh\nprintf ' 42 ovs-vswitchd\\n'\n")
        self.ps.chmod(0o755)
        before = self.snapshot_dirs()
        result = self.run_script(self.worker)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("conflicting OVS/OVN process", result.stderr)
        self.assertFalse(self.ovs_ctl_log.exists())
        self.assertEqual(self.snapshot_dirs(), before)

    def test_worker_preflight_reports_exact_foreign_process_identity(self):
        process = self.proc / "42"
        (process / "ns").mkdir(parents=True)
        (process / "ns/net").symlink_to("net:[43]")
        (process / "ns/pid").symlink_to("pid:[44]")
        (process / "exe").symlink_to("/usr/sbin/ovs-vswitchd")
        (process / "stat").write_text(
            "42 (ovs-vswitchd) S 9 " + " ".join(["0"] * 17) + " 12345\n")
        (process / "cmdline").write_bytes(b"ovs-vswitchd\0--pidfile=/foreign/ovs.pid\0")
        (process / "cgroup").write_text("0::/foreign-container\n")
        self.ps.write_text("#!/bin/sh\nprintf ' 42 ovs-vswitchd\\n'\n")
        result = self.run_script(self.worker)
        self.assertNotEqual(result.returncode, 0)
        for expected in ("pid=42 name=ovs-vswitchd", "net namespace=net:[43]",
                         "pid namespace=pid:[44]", "parent=9 start_ticks=12345",
                         "executable=/usr/sbin/ovs-vswitchd", "--pidfile=/foreign/ovs.pid",
                         "/foreign-container"):
            self.assertIn(expected, result.stderr)
        self.assertFalse(self.ovs_ctl_log.exists())

    def test_worker_preflight_rejects_ambiguous_process_identity(self):
        self.ps.write_text("#!/bin/sh\nprintf ' 42 ovsdb-server\\n'\n")
        result = self.run_script(self.worker)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("start_ticks=unavailable", result.stderr)
        self.assertIn("net namespace=unavailable", result.stderr)
        self.assertFalse(self.ovs_ctl_log.exists())

    def test_worker_preflight_accepts_empty_runtime_before_startup(self):
        result = self.run_script(self.preflight)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertFalse(self.ovs_ctl_log.exists())

    def test_worker_refuses_missing_kernel_module_before_mutation(self):
        self.ovs_module.rmdir()
        before = self.snapshot_dirs()
        result = self.run_script(self.worker)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("kernel module is not loaded", result.stderr)
        self.assertFalse(self.ovs_ctl_log.exists())
        self.assertFalse((self.root / "ovs-vswitchd.log").exists())
        self.assertEqual(self.snapshot_dirs(), before)

    def test_worker_starts_vswitchd_without_module_helper(self):
        result = self.run_script(self.worker)
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(self.ovs_ctl_log.read_text().splitlines(),
                         ["--no-ovs-vswitchd --system-id=random start"])
        args = (self.root / "ovs-vswitchd.log").read_text()
        self.assertIn("--pidfile=/var/run/openvswitch/ovs-vswitchd.pid", args)
        self.assertIn(f"unix:{self.ovs_run}/db.sock", args)

    def test_worker_preflight_refuses_receipted_partial_startup(self):
        (self.identity / "worker-owner").write_text(
            "owner=secondary-test-owner\nphase=ovs-running\novsdb=42:12345\n")
        (self.ovs_run / "ovsdb-server.pid").write_text("42\n")
        self.ps.write_text("#!/bin/sh\nprintf ' 42 ovsdb-server\\n'\n")
        before = self.snapshot_dirs()
        result = self.run_script(self.worker)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("runtime directory is not empty", result.stderr)
        self.assertFalse(self.ovs_ctl_log.exists())
        self.assertEqual(self.snapshot_dirs(), before)

    def test_worker_preflight_refuses_pid_reuse_with_stale_receipt(self):
        (self.identity / "worker-owner").write_text(
            "owner=secondary-test-owner\nphase=ovs-running\novsdb=42:12345\n")
        (self.ovs_run / "ovsdb-server.pid").write_text("42\n")
        process = self.proc / "42"
        process.mkdir()
        (process / "stat").write_text(
            "42 (ovsdb-server) S 9 " + " ".join(["0"] * 17) + " 99999\n")
        self.ps.write_text("#!/bin/sh\nprintf ' 42 ovsdb-server\\n'\n")
        result = self.run_script(self.worker)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("runtime directory is not empty", result.stderr)
        self.assertFalse(self.ovs_ctl_log.exists())

    def test_worker_preflight_rejects_conflicting_socket_before_ovs_start(self):
        (self.ovs_run / "db.sock").touch()
        before = self.snapshot_dirs()
        result = self.run_script(self.worker)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("runtime directory is not empty", result.stderr)
        self.assertFalse(self.ovs_ctl_log.exists())
        self.assertEqual(self.snapshot_dirs(), before)

    def test_worker_preflight_rejects_stale_ownership_receipt(self):
        (self.identity / "worker-owner").write_text("owner=stale\n")
        before = self.snapshot_dirs()
        result = self.run_script(self.worker)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("runtime directory is not empty", result.stderr)
        self.assertFalse(self.ovs_ctl_log.exists())
        self.assertEqual(self.snapshot_dirs(), before)

    def test_worker_preflight_fails_closed_when_inspection_tool_is_missing(self):
        self.env["PS"] = str(self.root / "missing-ps")
        before = self.snapshot_dirs()
        result = self.run_script(self.worker)
        self.assertNotEqual(result.returncode, 0)
        self.assertFalse(self.ovs_ctl_log.exists())
        self.assertEqual(self.snapshot_dirs(), before)

    def test_worker_preflight_fails_closed_when_link_inspection_fails(self):
        self.ip.write_text("#!/bin/sh\nexit 1\n")
        self.ip.chmod(0o755)
        before = self.snapshot_dirs()
        result = self.run_script(self.worker)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("cannot inspect links", result.stderr)
        self.assertFalse(self.ovs_ctl_log.exists())
        self.assertEqual(self.snapshot_dirs(), before)

    def test_worker_start_failure_does_not_run_unowned_stop(self):
        self.ovs_ctl.write_text(
            "#!/bin/sh\nset -eu\nprintf '%s\\n' \"$*\" >> \"$OVS_CTL_LOG\"\n"
            "case \"$*\" in *start*) exit 1;; esac\n")
        self.ovs_ctl.chmod(0o755)
        result = self.run_script(self.worker)
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(self.ovs_ctl_log.read_text().splitlines(),
                         ["--no-ovs-vswitchd --system-id=random start"])
        receipt = (self.identity / "worker-owner").read_text()
        self.assertIn("owner=secondary-test-owner", receipt)
        self.assertIn("phase=starting", receipt)

    def test_worker_vswitchd_failure_preserves_partial_runtime(self):
        self.vswitchd.write_text("#!/bin/sh\nexit 1\n")
        before = self.snapshot_dirs()
        result = self.run_script(self.worker)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("ovs-vswitchd startup failed", result.stderr)
        self.assertEqual(self.ovs_ctl_log.read_text().splitlines(),
                         ["--no-ovs-vswitchd --system-id=random start"])
        self.assertEqual(self.snapshot_dirs()[self.ovs_run], before[self.ovs_run])
        self.assertIn("phase=starting", (self.identity / "worker-owner").read_text())

    def test_missing_southbound_database_stops_only_receipted_ovs_processes(self):
        processes = (
            (201, "ovsdb-server", "ovsdb-server.pid"),
            (202, "ovs-vswitchd", "ovs-vswitchd.pid"),
        )
        ovs_ctl_body = [
            "#!/bin/sh", "set -eu",
            'printf "%s\\n" "$*" >> "$OVS_CTL_LOG"',
            'case "$*" in *start*)',
        ]
        for pid, name, pidfile in processes:
            proc = self.proc / str(pid)
            stat_values = " ".join(["S"] + [str(value) for value in range(1, 20)])
            ovs_ctl_body.extend([
                f'mkdir -p "{proc}"',
                f"printf '%s\\n' '{name}' > '{proc / 'comm'}'",
                f"printf '%s\\0' '{name}' "
                f"'--pidfile=/var/run/openvswitch/{pidfile}' > '{proc / 'cmdline'}'",
                f"printf '%s\\n' '{pid} ({name}) {stat_values}' > '{proc / 'stat'}'",
                f"printf '%s\\n' '{pid}' > '{self.ovs_run / pidfile}'",
            ])
        ovs_ctl_body.extend(["exit 0;;", "esac", "exit 0"])
        self.ovs_ctl.write_text("\n".join(ovs_ctl_body) + "\n")
        self.ovs_ctl.chmod(0o755)
        self.env["OVS_VSCTL"] = str(self.executable(
            "ovs-vsctl", 'case "$1" in list-br) exit 0;; esac\nexit 0\n'))
        self.ovn_ctl.write_text(
            "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$OVN_CTL_LOG\"\nexit 1\n")
        self.ovn_ctl.chmod(0o755)
        self.env["OVN_CTL_LOG"] = str(self.root / "ovn-ctl.log")
        self.env["PATH"] = f"{self.root}:{self.env.get('PATH', '/usr/bin:/bin')}"
        result = self.run_script(self.worker)
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(self.ovs_ctl_log.read_text().splitlines(),
                         ["--no-ovs-vswitchd --system-id=random start", "stop"], result.stderr)
        self.assertTrue((self.root / "ovn-ctl.log").exists(), result.stderr)
        self.assertEqual((self.root / "ovn-ctl.log").read_text().splitlines(),
                         ["start_controller"], result.stderr)

    def test_changed_controller_identity_prevents_ovs_cleanup(self):
        processes = (
            (201, "ovsdb-server", "ovsdb-server.pid"),
            (202, "ovs-vswitchd", "ovs-vswitchd.pid"),
        )
        ovs_ctl_body = [
            "#!/bin/sh", "set -eu",
            'printf "%s\\n" "$*" >> "$OVS_CTL_LOG"',
            'case "$*" in *start*)',
        ]
        for pid, name, pidfile in processes:
            proc = self.proc / str(pid)
            stat_values = " ".join(["S"] + [str(value) for value in range(1, 20)])
            ovs_ctl_body.extend([
                f'mkdir -p "{proc}"',
                f"printf '%s\\n' '{name}' > '{proc / 'comm'}'",
                f"printf '%s\\0' '{name}' "
                f"'--pidfile=/var/run/openvswitch/{pidfile}' > '{proc / 'cmdline'}'",
                f"printf '%s\\n' '{pid} ({name}) {stat_values}' > '{proc / 'stat'}'",
                f"printf '%s\\n' '{pid}' > '{self.ovs_run / pidfile}'",
            ])
        ovs_ctl_body.extend(["exit 0;;", "esac", "exit 0"])
        self.ovs_ctl.write_text("\n".join(ovs_ctl_body) + "\n")
        self.ovs_ctl.chmod(0o755)
        self.env["OVS_VSCTL"] = str(self.executable(
            "ovs-vsctl", 'case "$1" in list-br) exit 0;; esac\nexit 0\n'))

        controller_pidfile = self.ovn_run / "ovn-controller.pid"
        controller_proc = self.proc / "203"
        stat_values = " ".join(["S"] + [str(value) for value in range(1, 20)])
        self.ovn_ctl.write_text(
            "#!/bin/sh\nset -eu\n"
            'printf "%s\\n" "$*" >> "$OVN_CTL_LOG"\n'
            f"mkdir -p '{controller_proc}'\n"
            f"printf '%s\\n' ovn-controller > '{controller_proc / 'comm'}'\n"
            f"printf '%s\\0' ovn-controller "
            "'--pidfile=/var/run/ovn/ovn-controller.pid' "
            f"> '{controller_proc / 'cmdline'}'\n"
            f"printf '%s\\n' '203 (ovn-controller) {stat_values}' "
            f"> '{controller_proc / 'stat'}'\n"
            f"printf '%s\\n' 203 > '{controller_pidfile}'\n")
        self.ovn_ctl.chmod(0o755)
        self.executable(
            "sleep", f"printf '999\\n' > '{controller_pidfile}'\nexit 0\n")
        self.env["PATH"] = f"{self.root}:{self.env.get('PATH', '/usr/bin:/bin')}"
        self.env["OVN_CTL_LOG"] = str(self.root / "ovn-ctl.log")
        result = self.run_script(self.worker)
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(self.ovs_ctl_log.read_text().splitlines(),
                         ["--no-ovs-vswitchd --system-id=random start"])
        self.assertEqual((self.root / "ovn-ctl.log").read_text().splitlines(),
                         ["start_controller"])
        self.assertIn("identity changed", result.stderr)

    def test_network_namespace_ambiguity_fails_closed(self):
        (self.proc / "1/ns/net").unlink()
        (self.proc / "1/ns/net").symlink_to("net:[99]")
        before = self.snapshot_dirs()
        result = self.run_script(self.worker)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("ambiguous worker network namespace", result.stderr)
        self.assertFalse(self.ovs_ctl_log.exists())
        self.assertEqual(self.snapshot_dirs(), before)


class CentralRuntimeSafetyTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.root = Path(self.temp.name)
        self.ovn_run = self.root / "ovn-run"
        self.ovn_data = self.root / "ovn-data"
        self.proc = self.root / "proc"
        self.bin = self.root / "bin"
        self.log = self.root / "ovn-ctl.log"
        for directory in (self.ovn_run, self.ovn_data, self.proc, self.bin):
            directory.mkdir()
        self.owner_receipt = self.root / "owner-receipt"
        self.ovn_ctl = self.write_ovn_ctl()
        self.ps = self.executable("ps", "exit 0\n")
        self.ovn_nbctl = self.executable("ovn-nbctl", "exit 0\n")
        self.ovn_sbctl = self.executable("ovn-sbctl", "exit 0\n")
        self.executable("sleep", "exit 0\n")
        self.script = Path(__file__).parents[2] / "deploy/containerlab/secondary-ovn-central.sh"
        original_path = os.environ.get("PATH", "/usr/bin:/bin")
        self.env = {
            **os.environ,
            "PATH": f"{self.bin}:{original_path}",
            "OVN_RUN_DIR": str(self.ovn_run),
            "OVN_DATA_DIR": str(self.ovn_data),
            "PROC_ROOT": str(self.proc),
            "OWNER_RECEIPT": str(self.owner_receipt),
            "OVN_CTL": str(self.ovn_ctl),
            "OVN_NBCTL": str(self.ovn_nbctl),
            "OVN_SBCTL": str(self.ovn_sbctl),
            "PS": str(self.ps),
            "OVN_CTL_LOG": str(self.log),
            "SECONDARY_OWNER": "secondary-test-owner",
        }

    def tearDown(self):
        self.temp.cleanup()

    def executable(self, name, body):
        path = self.bin / name
        path.write_text("#!/bin/sh\nset -eu\n" + body)
        path.chmod(0o755)
        return path

    def write_ovn_ctl(self, *, partial=False):
        pids = (101, 102, 103)
        names = ("ovsdb-server", "ovsdb-server", "ovn-northd")
        files = ("ovnnb_db.pid", "ovnsb_db.pid", "ovn-northd.pid")
        path = self.bin / "ovn-ctl"
        lines = [
            "#!/bin/sh", "set -eu",
            'printf "%s\\n" "$*" >> "$OVN_CTL_LOG"',
            'case "$*" in',
            '*start_northd*)',
        ]
        for index, (pid, name, pidfile) in enumerate(zip(pids, names, files)):
            if partial and index > 0:
                break
            proc = self.proc / str(pid)
            comm = proc / "comm"
            cmdline = proc / "cmdline"
            stat = proc / "stat"
            stat_values = " ".join(["S"] + [str(value) for value in range(1, 20)])
            lines.extend([
                f'mkdir -p "{proc}"',
                f"printf '%s\\n' '{name}' > '{comm}'",
                f"printf '%s\\0' '{name}' '--pidfile=/var/run/ovn/{pidfile}' > '{cmdline}'",
                f"printf '%s\\n' '{pid} ({name}) {stat_values}' > '{stat}'",
                f"printf '%s\\n' '{pid}' > '{self.ovn_run / pidfile}'",
            ])
        lines.extend([
            "exit \"${OVN_START_STATUS:-0}\"",
            ";;",
            "esac",
            "exit 0",
        ])
        path.write_text("\n".join(lines) + "\n")
        path.chmod(0o755)
        return path

    def run_central(self, *, start_status=None):
        env = dict(self.env)
        if start_status is not None:
            env["OVN_START_STATUS"] = str(start_status)
        return subprocess.run(
            ["/bin/sh", str(self.script)], env=env, text=True,
            stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=10)

    def test_existing_database_fails_before_ovn_start(self):
        (self.ovn_data / "ovnnb_db.db").touch()
        before = sorted(path.name for path in self.ovn_run.iterdir())
        result = self.run_central()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("conflicting secondary OVN resource", result.stderr)
        self.assertFalse(self.log.exists())
        self.assertEqual(sorted(path.name for path in self.ovn_run.iterdir()), before)

    def test_stale_central_receipt_fails_before_ovn_start(self):
        self.owner_receipt.write_text("owner=stale\n")
        result = self.run_central()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("stale secondary OVN ownership receipt", result.stderr)
        self.assertFalse(self.log.exists())

    def test_central_preflight_fails_closed_when_process_inspection_fails(self):
        self.ps.write_text("#!/bin/sh\nexit 1\n")
        self.ps.chmod(0o755)
        result = self.run_central()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("cannot inspect processes", result.stderr)
        self.assertFalse(self.log.exists())

    def test_partial_ovn_startup_failure_does_not_stop_unverified_processes(self):
        self.ovn_ctl = self.write_ovn_ctl(partial=True)
        self.env["OVN_CTL"] = str(self.ovn_ctl)
        result = self.run_central(start_status=1)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("secondary OVN provisioning failed", result.stderr)
        self.assertEqual(self.log.read_text().splitlines(), [
            "--db-nb-create-insecure-remote=yes --db-sb-create-insecure-remote=yes start_northd",
        ])
        receipt = self.owner_receipt.read_text()
        self.assertIn("owner=secondary-test-owner", receipt)
        self.assertIn("nb=101:", receipt)
        self.assertNotIn("northd=", receipt)

    def test_successful_ovn_runtime_is_stopped_only_after_exact_identity_check(self):
        result = self.run_central()
        self.assertEqual(result.returncode, 0, result.stderr)
        calls = self.log.read_text().splitlines()
        self.assertEqual(len(calls), 2)
        self.assertTrue(calls[0].endswith("start_northd"))
        self.assertEqual(calls[1], "stop")

    def test_changed_process_identity_refuses_ovn_stop(self):
        nb_pid_file = self.ovn_run / "ovnnb_db.pid"
        self.ovn_nbctl = self.executable(
            "ovn-nbctl", f"printf '999\\n' > '{nb_pid_file}'\n")
        self.env["OVN_NBCTL"] = str(self.ovn_nbctl)
        result = self.run_central()
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(len(self.log.read_text().splitlines()), 1)
        self.assertIn("identity is incomplete or changed", result.stderr)


class GeneveEvidenceTests(unittest.TestCase):
    VTEPS = {"node-a": "198.51.100.1", "node-b": "198.51.100.2"}
    VNI = 4096

    @staticmethod
    def ipv4(source, destination, protocol, payload):
        source_bytes = bytes(map(int, source.split(".")))
        destination_bytes = bytes(map(int, destination.split(".")))
        return (struct.pack("!BBHHHBBH4s4s", 0x45, 0, 20 + len(payload), 1, 0,
                            64, protocol, 0, source_bytes, destination_bytes) + payload)

    @classmethod
    def icmp_packet(cls, probe, *, reply=False, sequence=1, wrong_ip=False):
        source = probe["destinationIP"] if reply else probe["sourceIP"]
        destination = probe["sourceIP"] if reply else probe["destinationIP"]
        if wrong_ip:
            destination = "192.0.2.99"
        marker = probe["marker"]
        payload = (marker * ((56 + len(marker) - 1) // len(marker)))[:56]
        payload = bytes([sequence]) * 16 + payload[16:]
        icmp = struct.pack("!BBHHH", 0 if reply else 8, 0, 0,
                           probe["identifier"], sequence) + payload
        packet = cls.ipv4(source, destination, 1, icmp)
        return b"\x02" * 6 + b"\x01" * 6 + b"\x08\x00" + packet

    @classmethod
    def geneve_frame(cls, probe, *, reply=False, sequence=1, vni=None,
                     outer_source=None, outer_destination=None, wrong_ip=False):
        source_node = probe["destinationNode"] if reply else probe["sourceNode"]
        destination_node = probe["sourceNode"] if reply else probe["destinationNode"]
        inner = cls.icmp_packet(probe, reply=reply, sequence=sequence, wrong_ip=wrong_ip)
        geneve = (b"\x00\x00\x65\x58" +
                  int(cls.VNI if vni is None else vni).to_bytes(3, "big") + b"\x00" + inner)
        udp = struct.pack("!HHHH", 6082, 6081, 8 + len(geneve), 0) + geneve
        outer = cls.ipv4(
            outer_source or cls.VTEPS[source_node],
            outer_destination or cls.VTEPS[destination_node], 17, udp)
        return b"\x02" * 6 + b"\x01" * 6 + b"\x08\x00" + outer

    @staticmethod
    def write_pcap(path, frames, link_type=1):
        with open(path, "wb") as stream:
            stream.write(struct.pack(
                "<IHHIIII", 0xA1B2C3D4, 2, 4, 0, 0, 65535, link_type))
            for frame in frames:
                stream.write(struct.pack("<IIII", 1, 0, len(frame), len(frame)))
                stream.write(frame)

    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.root = Path(self.temp.name)
        self.probes = []
        for name, source, destination, source_node, destination_node in (
                ("a-to-b", "a", "b", "node-a", "node-b"),
                ("b-to-a", "b", "a", "node-b", "node-a")):
            marker = (b"EVPN" + name.encode())
            identifier = 0x1200 if source == "a" else 0x3400
            self.probes.append({
                "name": name, "source": source, "destination": destination,
                "sourceIP": "192.0.2.10" if source == "a" else "192.0.2.11",
                "destinationIP": "192.0.2.10" if destination == "a" else "192.0.2.11",
                "sourceNode": source_node, "destinationNode": destination_node,
                "identifier": identifier, "marker": marker,
            })
        self.geneve = {
            node: [
                self.geneve_frame(probe, reply=reply, sequence=sequence)
                for probe in self.probes
                for reply in (False, True)
                for sequence in (1, 2, 3)
            ]
            for node in self.VTEPS
        }
        self.receivers = {
            suffix: [self.icmp_packet(probe, reply=reply, sequence=sequence)
                     for probe in self.probes for reply in (False, True)
                     for sequence in (1, 2, 3)]
            for suffix in ("a", "b")
        }

    def tearDown(self):
        self.temp.cleanup()

    def parse_geneve(self, packets):
        path = self.root / "geneve.pcap"
        self.write_pcap(path, packets)
        return isolated.decode_geneve_pcap(path)

    def parse_receiver(self, packets):
        path = self.root / "receiver.pcap"
        self.write_pcap(path, packets)
        return isolated.decode_icmp_pcap(path)

    def assert_rejected(self, captures=None, probes=None, vteps=None,
                        vni=None, receivers=None):
        decoded = {
            node: self.parse_geneve(packets)
            for node, packets in (self.geneve if captures is None else captures).items()
        }
        decoded_receivers = {
            suffix: self.parse_receiver(packets)
            for suffix, packets in (self.receivers if receivers is None else receivers).items()
        }
        with self.assertRaises(isolated.IsolationError):
            isolated.assert_geneve_probe_evidence(
                decoded, probes or self.probes, vteps or self.VTEPS,
                self.VNI if vni is None else vni, decoded_receivers)

    def test_bidirectional_geneve_and_net1_receiver_evidence_passes(self):
        decoded = {node: self.parse_geneve(packets)
                   for node, packets in self.geneve.items()}
        receivers = {suffix: self.parse_receiver(packets)
                     for suffix, packets in self.receivers.items()}
        isolated.assert_geneve_probe_evidence(
            decoded, self.probes, self.VTEPS, self.VNI, receivers)

    def test_linux_cooked_v1_and_v2_geneve_captures_are_decoded(self):
        for link_type, prefix in (
                (113, b"\x00" * 14 + b"\x08\x00"),
                (276, b"\x08\x00" + b"\x00" * 18)):
            path = self.root / f"geneve-{link_type}.pcap"
            frames = [
                prefix + frame[14:]
                for frame in self.geneve["node-a"]
            ]
            self.write_pcap(path, frames, link_type=link_type)
            decoded = isolated.decode_geneve_pcap(path)
            self.assertEqual(len(decoded), len(self.geneve["node-a"]))
            self.assertEqual(decoded[0]["vni"], self.VNI)

    def test_arbitrary_udp_6081_is_not_geneve_evidence(self):
        frame = b"\x02" * 6 + b"\x01" * 6 + b"\x08\x00"
        frame += self.ipv4("198.51.100.1", "198.51.100.2", 17,
                           struct.pack("!HHHH", 6082, 6081, 12, 0) + b"fake")
        self.assert_rejected(captures={node: [frame] for node in self.VTEPS})

    def test_wrong_vni_is_rejected(self):
        packets = {
            node: [self.geneve_frame(probe, reply=reply, sequence=sequence, vni=99)
                   for probe in self.probes for reply in (False, True)
                   for sequence in (1, 2, 3)]
            for node in self.VTEPS
        }
        self.assert_rejected(captures=packets)

    def test_conflicting_vni_evidence_is_rejected_even_with_valid_packets(self):
        packets = {
            node: self.geneve[node] + [
                self.geneve_frame(self.probes[0], vni=99)
            ]
            for node in self.VTEPS
        }
        self.assert_rejected(captures=packets)

    def test_wrong_vtep_is_rejected(self):
        packets = {
            node: [self.geneve_frame(
                probe, reply=reply, sequence=sequence,
                outer_source="203.0.113.1" if not reply else None)
                   for probe in self.probes for reply in (False, True)
                   for sequence in (1, 2, 3)]
            for node in self.VTEPS
        }
        self.assert_rejected(captures=packets)

    def test_wrong_inner_endpoint_is_rejected(self):
        packets = {
            node: [self.geneve_frame(
                probe, reply=reply, sequence=sequence,
                wrong_ip=(probe["name"] == "a-to-b" and not reply))
                   for probe in self.probes for reply in (False, True)
                   for sequence in (1, 2, 3)]
            for node in self.VTEPS
        }
        self.assert_rejected(captures=packets)

    def test_missing_direction_or_receiver_capture_is_rejected(self):
        probe = self.probes[0]
        packets = {
            node: [self.geneve_frame(probe, reply=reply, sequence=sequence)
                   for reply in (False, True) for sequence in (1, 2, 3)]
            for node in self.VTEPS
        }
        self.assert_rejected(captures=packets)
        self.assert_rejected(receivers={"b": self.receivers["b"]})

    def test_short_receiver_capture_is_rejected(self):
        self.assert_rejected(receivers={"a": self.receivers["a"],
                                        "b": self.receivers["b"][:2]})

    def test_marker_at_pattern_offset_is_required(self):
        frame = self.icmp_packet(self.probes[0])
        payload = frame[42:]
        expected = (self.probes[0]["marker"] * 8)[:56]
        self.assertEqual(payload[16:], expected[16:])
        changed = frame[:58] + b"X" + frame[59:]
        receivers = dict(self.receivers)
        receivers["a"] = [changed] + receivers["a"][1:]
        self.assert_rejected(receivers=receivers)

    def test_duplicate_packet_is_rejected(self):
        receivers = dict(self.receivers)
        receivers["a"] = self.receivers["a"] + [self.receivers["a"][0]]
        self.assert_rejected(receivers=receivers)
        captures = dict(self.geneve)
        captures["node-a"] = self.geneve["node-a"] + [self.geneve["node-a"][0]]
        self.assert_rejected(captures=captures)

    def test_wrong_identifier_and_sequence_are_rejected(self):
        for offset, value in ((38, b"\x99\x99"), (40, b"\x00\x04")):
            frame = self.receivers["a"][0]
            receivers = dict(self.receivers)
            receivers["a"] = [frame[:offset] + value + frame[offset + 2:]] + self.receivers["a"][1:]
            self.assert_rejected(receivers=receivers)

    def test_unrelated_geneve_traffic_does_not_replace_probe(self):
        unrelated = dict(self.probes[0], identifier=0x9999)
        captures = {node: [self.geneve_frame(unrelated)] for node in self.VTEPS}
        self.assert_rejected(captures=captures)

    def test_truncated_pcap_is_rejected_as_inconclusive(self):
        path = self.root / "truncated.pcap"
        path.write_bytes(
            struct.pack("<IHHIIII", 0xA1B2C3D4, 2, 4, 0, 0, 65535, 1) + b"\x00" * 4)
        with self.assertRaisesRegex(isolated.IsolationError, "truncated"):
            isolated.decode_geneve_pcap(path)


class OwnershipReceiptTests(unittest.TestCase):
    def setUp(self):
        self.reference = "evpn4ovn/isolated-cni-agent:run-123"
        self.owner = "secondary-run-123"
        self.image_id = "sha256:0123456789"
        self.receipt = {
            "owner": self.owner, "reference": self.reference, "imageID": self.image_id,
        }
        self.image = {
            "Id": self.image_id,
            "RepoTags": [self.reference, "shared/agent:stable"],
            "Config": {"Labels": {
                "evpn.e2e.owner": self.owner,
                "evpn.e2e.reference": self.reference,
            }},
        }

    def test_image_cleanup_removes_only_the_run_owned_tag(self):
        removed = []
        outcome = isolated.cleanup_image_reference(
            self.receipt, self.image, self.reference, self.owner, removed.append)
        self.assertEqual(removed, [self.reference])
        self.assertEqual(outcome, {
            "removedReference": self.reference,
            "preservedTags": ["shared/agent:stable"],
        })

    def test_image_cleanup_is_idempotent_when_owned_tag_is_absent(self):
        removed = []
        outcome = isolated.cleanup_image_reference(
            self.receipt, None, self.reference, self.owner, removed.append)
        self.assertEqual(outcome, "already-absent")
        self.assertEqual(removed, [])

    def test_image_cleanup_refuses_stale_or_missing_receipts(self):
        removed = []
        with self.assertRaisesRegex(isolated.IsolationError, "receipt"):
            isolated.cleanup_image_reference(
                None, self.image, self.reference, self.owner, removed.append)
        stale = {**self.receipt, "owner": "another-run"}
        with self.assertRaisesRegex(isolated.IsolationError, "receipt"):
            isolated.cleanup_image_reference(
                stale, self.image, self.reference, self.owner, removed.append)
        self.assertEqual(removed, [])

    def test_image_cleanup_refuses_changed_identity_without_removing_any_tag(self):
        removed = []
        changed = {**self.image, "Id": "sha256:changed"}
        with self.assertRaisesRegex(isolated.IsolationError, "identity"):
            isolated.cleanup_image_reference(
                self.receipt, changed, self.reference, self.owner, removed.append)
        self.assertEqual(removed, [])

    def test_foreign_witness_is_unavailable_without_preexisting_inventory(self):
        report = isolated.foreign_resource_report([])
        self.assertEqual(report["status"], "FOREIGN_WITNESS_NOT_AVAILABLE")
        self.assertEqual(report["resources"], [])

    def test_foreign_witness_only_reports_preexisting_resources(self):
        before = [{"scope": "worker", "links": {"br-int": {"ifindex": 7}}}]
        report = isolated.foreign_resource_report(before)
        self.assertEqual(report, {
            "status": "FOREIGN_WITNESS_CAPTURED", "resources": before,
        })
        compared = isolated.foreign_resource_report(before, comparison_complete=True)
        self.assertEqual(compared, {"status": "PRESERVED", "resources": before})

    def test_foreign_resource_comparison_requires_exact_preexisting_identity(self):
        before = [{"scope": "worker", "links": {"br-int": {"ifindex": 7}}}]
        isolated.assert_foreign_resources_preserved(
            before, {"worker": {"links": {"br-int": {"ifindex": 7}}}})
        with self.assertRaisesRegex(isolated.IsolationError, "changed"):
            isolated.assert_foreign_resources_preserved(
                before, {"worker": {"links": {"br-int": {"ifindex": 8}}}})
        with self.assertRaisesRegex(isolated.IsolationError, "disappeared"):
            isolated.assert_foreign_resources_preserved(before, {})


if __name__ == "__main__":
    unittest.main()
