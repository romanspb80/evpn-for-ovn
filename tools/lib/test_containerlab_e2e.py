"""Offline tests of Containerlab topology, ownership, failure and isolation contracts."""
import copy
import importlib.util
import json
import os
from pathlib import Path
import subprocess
import tempfile
import unittest
from unittest.mock import patch


ROOT = Path(__file__).resolve().parents[2]
SPEC = importlib.util.spec_from_file_location("evpn_containerlab", ROOT / "tools/e2e/containerlab.py")
LAB = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(LAB)


class ContainerlabTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)
        self.directory = Path(self.temporary.name) / "run"
        self.state = LAB.init(self.directory, "framework-netlink", "test-cluster",
                              "test-network", "test-frr")

    def objects(self):
        state = self.state
        objects = {}
        for index, name in enumerate(state["containers"]):
            labels = ({"io.x-k8s.kind.cluster": state["cluster"]}
                      if name in state["kind_nodes"]
                      else {"evpn.e2e.owner": state["owner"], "containerlab": state["name"]})
            objects["container", name] = {
                "Id": str(index + 1) * 64, "Config": {"Labels": labels},
            }
        objects["network", state["network"]] = {
            "Id": "f" * 64, "Labels": {"containerlab": ""},
            "IPAM": {"Config": [{"Subnet": state["subnet"]}]},
            "Containers": {item["Id"]: {} for (kind, _), item in objects.items()
                           if kind == "container"},
        }
        return objects

    def owned(self, objects):
        self.state["resources"] = LAB.inventory(self.state)
        LAB.write(self.directory / "lab.json", self.state)

    def membership(self, objects):
        identities = [objects["container", name]["Id"] for name in self.state["kind_nodes"]
                      if ("container", name) in objects]
        return subprocess.CompletedProcess([], 0, "\n".join(identities), "")

    def test_native_kind_and_linux_peer_share_one_topology(self):
        topology = json.loads((self.directory / "lab.clab.yaml").read_text())
        self.assertEqual(topology["prefix"], "")
        self.assertEqual(topology["mgmt"]["network"], "test-network")
        kind = topology["topology"]["nodes"]["test-cluster"]
        self.assertEqual(kind["kind"], "k8s-kind")
        self.assertEqual(kind["image"], LAB.NODE_IMAGE)
        self.assertEqual(kind["extras"]["k8s_kind"]["deploy"]["wait"], "180s")
        frr = topology["topology"]["nodes"]["test-frr"]
        self.assertEqual(frr["image"], LAB.FRR_IMAGE)
        self.assertFalse(frr["privileged"])
        self.assertTrue(frr["certificate"]["issue"])
        self.assertEqual(len(frr["binds"]), 3)
        self.assertTrue(all(bind.endswith(":ro") for bind in frr["binds"]))
        self.assertEqual(LAB.load(self.directory), self.state)

    def test_changed_topology_is_not_authorized(self):
        with (self.directory / "lab.clab.yaml").open("a") as stream:
            stream.write("\n")
        with self.assertRaisesRegex(LAB.LabError, "topology changed"):
            LAB.load(self.directory)

    def test_reserved_preflight_does_not_delete_anything(self):
        with patch.object(LAB, "command") as command, patch.object(LAB, "inspect") as inspect:
            LAB.down(self.directory)
        command.assert_not_called()
        inspect.assert_not_called()

    def test_exact_receipts_allow_teardown_and_missing_resources_are_idempotent(self):
        objects = self.objects()
        with patch.object(LAB, "inspect", side_effect=lambda kind, name: objects.get((kind, name))), \
             patch.object(LAB, "command", side_effect=lambda *a, **k: self.membership(objects)):
            self.owned(objects)
            self.assertEqual(LAB.verify_owned(self.state), self.state["resources"])
            objects.clear()
            self.assertEqual(LAB.verify_owned(self.state), {"containers": {}, "network": None})

    def test_changed_id_label_subnet_or_membership_blocks_teardown(self):
        for mutation in ("container-id", "network-id", "kind-label", "frr-label",
                         "network-label", "subnet", "attachment", "extra-kind-member"):
            with self.subTest(mutation=mutation):
                objects = self.objects()
                with patch.object(LAB, "inspect", side_effect=lambda kind, name: objects.get((kind, name))), \
                     patch.object(LAB, "command") as command:
                    command.return_value = self.membership(objects)
                    self.owned(objects)
                    if mutation == "container-id":
                        objects["container", "test-frr"]["Id"] = "0" * 64
                    elif mutation == "network-id":
                        objects["network", "test-network"]["Id"] = "0" * 64
                    elif mutation == "kind-label":
                        objects["container", "test-cluster-worker"]["Config"]["Labels"] = {}
                    elif mutation == "frr-label":
                        objects["container", "test-frr"]["Config"]["Labels"] = {}
                    elif mutation == "network-label":
                        objects["network", "test-network"]["Labels"] = {}
                    elif mutation == "subnet":
                        objects["network", "test-network"]["IPAM"]["Config"] = [{"Subnet": "192.0.2.0/24"}]
                    elif mutation == "attachment":
                        objects["network", "test-network"]["Containers"]["foreign"] = {}
                    else:
                        command.return_value.stdout += "\n" + "0" * 64
                    with self.assertRaises(LAB.LabError):
                        LAB.verify_owned(self.state)
                    self.assertFalse(any(call.args[0][:2] == ["containerlab", "destroy"]
                                         for call in command.call_args_list))

    def test_preexisting_target_is_never_claimed(self):
        with patch.object(LAB, "command", return_value=subprocess.CompletedProcess([], 0, "version: 0.79.0", "")), \
             patch.object(LAB, "inspect", return_value={"Id": "foreign"}):
            with self.assertRaisesRegex(LAB.LabError, "pre-existing container"):
                LAB.up(self.directory)
        self.assertEqual(LAB.load(self.directory)["phase"], "reserved")

    def test_deploy_failure_keeps_partial_receipts_for_scoped_cleanup(self):
        calls = []
        objects = self.objects()
        partial = {"containers": {"test-cluster-worker": "a" * 64}, "network": "f" * 64}

        def command(args, **kwargs):
            calls.append(args)
            if args[:2] == ["containerlab", "version"]:
                return subprocess.CompletedProcess(args, 0, "version: 0.79.0", "")
            if args[:2] == ["containerlab", "deploy"]:
                return subprocess.CompletedProcess(args, 42, "", "injected failure")
            return subprocess.CompletedProcess(args, 0, "", "")

        with patch.object(LAB, "STATE_ROOT", Path(self.temporary.name)), \
             patch.object(LAB, "command", side_effect=command), \
             patch.object(LAB, "inspect", return_value=None), \
             patch.object(LAB, "tls_env", return_value=({}, Path(self.temporary.name))), \
             patch.object(LAB, "inventory", return_value=partial):
            with self.assertRaisesRegex(LAB.LabError, "deploy failed"):
                LAB.up(self.directory)
        state = LAB.load(self.directory)
        self.assertEqual(state["phase"], "deploying")
        self.assertEqual(state["resources"], partial)
        self.assertIn("injected failure", (self.directory / "containerlab-deploy.log").read_text())
        self.assertFalse(any("--all" in call or "--reconfigure" in call for call in calls))

    def test_retained_lab_lease_blocks_other_suites(self):
        with patch.object(LAB, "STATE_ROOT", Path(self.temporary.name)):
            LAB.lease(self.directory)
            with self.assertRaisesRegex(LAB.LabError, "another E2E lab"):
                LAB.lease(self.directory.parent / "foreign")
            LAB.lease(self.directory, release=True)
            LAB.lease(self.directory.parent / "foreign")

    def test_docker_inspection_error_is_not_resource_absence(self):
        with patch.object(LAB, "command", return_value=subprocess.CompletedProcess([], 1, "", "permission denied")):
            with self.assertRaisesRegex(LAB.LabError, "permission denied"):
                LAB.inspect("container", "test")

    def test_source_tree_evidence_is_rejected_before_creation(self):
        with self.assertRaisesRegex(LAB.LabError, "outside the source tree"):
            LAB.init(ROOT / "generated-e2e", "cni", "test", "test-network")

    def test_successful_deploy_with_missing_or_stopped_nodes_is_not_ready(self):
        for missing in (True, False):
            with self.subTest(missing=missing):
                state = copy.deepcopy(self.state)
                state["resources"] = {
                    "containers": {} if missing else {name: "a" * 64 for name in state["containers"]},
                    "network": "f" * 64,
                }
                with patch.object(LAB, "STATE_ROOT", Path(self.temporary.name)), \
                     patch.object(LAB, "command", side_effect=lambda args, **kwargs:
                                  subprocess.CompletedProcess(args, 0,
                                      "version: 0.79.0" if args[:2] == ["containerlab", "version"] else "", "")), \
                     patch.object(LAB, "inspect", side_effect=[None] * 4 + [
                         {"State": {"Running": False}}]), \
                     patch.object(LAB, "tls_env", return_value=({}, Path(self.temporary.name))), \
                     patch.object(LAB, "inventory", return_value=state["resources"]):
                    LAB.write(self.directory / "lab.json", self.state)
                    with self.assertRaisesRegex(LAB.LabError, "missing|not running"):
                        LAB.up(self.directory)
                self.assertEqual(LAB.load(self.directory)["phase"], "deploying")

    def test_auxiliary_requires_matching_labels_and_exact_identity(self):
        objects = self.objects()
        with patch.object(LAB, "STATE_ROOT", Path(self.temporary.name)), \
             patch.object(LAB, "inspect", side_effect=lambda kind, name: objects.get((kind, name))), \
             patch.object(LAB, "command", side_effect=lambda *a, **k: self.membership(objects)):
            self.owned(objects)
            auxiliary = {"Id": "e" * 64, "Name": "/test-helper",
                         "Config": {"Labels": {"containerlab": self.state["name"],
                                               "evpn.e2e.owner": self.state["owner"]}}}
            objects["container", "test-helper"] = auxiliary
            objects["network", "test-network"]["Containers"][auxiliary["Id"]] = {}
            with self.assertRaisesRegex(LAB.LabError, "unowned"):
                LAB.verify_owned(self.state)
            LAB.register_auxiliary(self.directory, "test-helper")
            state = LAB.load(self.directory)
            self.assertEqual(state["auxiliary"], ["test-helper"])
            self.assertEqual(LAB.verify_owned(state)["containers"]["test-helper"], auxiliary["Id"])
            auxiliary["Id"] = "d" * 64
            with self.assertRaisesRegex(LAB.LabError, "identity changed"):
                LAB.register_auxiliary(self.directory, "test-helper")
            auxiliary["Config"]["Labels"] = {}
            with self.assertRaisesRegex(LAB.LabError, "ownership labels"):
                LAB.register_auxiliary(self.directory, "test-helper")

    def test_exclusive_lock_is_inherited_but_blocks_independent_operations(self):
        with patch.object(LAB, "STATE_ROOT", Path(self.temporary.name)), \
             patch.dict(os.environ):
            os.environ.pop("EVPN_E2E_LOCK_FD", None)
            with LAB.exclusive():
                with LAB.exclusive():
                    LAB.command(["python3", "-c",
                                 "import fcntl,os; fcntl.flock(int(os.environ['EVPN_E2E_LOCK_FD']), "
                                 "fcntl.LOCK_EX | fcntl.LOCK_NB)"])
                result = LAB.command(
                    ["python3", "-c",
                     "import fcntl,sys; f=open(sys.argv[1],'a'); "
                     "fcntl.flock(f, fcntl.LOCK_EX | fcntl.LOCK_NB)",
                     str(Path(self.temporary.name) / "e2e.lock")], check=False)
                self.assertNotEqual(result.returncode, 0)
                self.assertIn("BlockingIOError", result.stderr)
            self.assertNotIn("EVPN_E2E_LOCK_FD", os.environ)


if __name__ == "__main__":
    unittest.main()
