import importlib.util
import hashlib
import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch
import os


path = Path(__file__).resolve().parents[1] / "vm-e2e-lab.py"
spec = importlib.util.spec_from_file_location("vm_e2e_lab", path)
lab = importlib.util.module_from_spec(spec)
spec.loader.exec_module(lab)

NAME = "evpn-vm-123456abcdef"
XML = f"""<domain><name>{NAME}</name><devices>
<disk type="file" device="disk"><source file="/pool/{NAME}.img"/></disk>
<interface type="network"><source network="isolated"/></interface>
</devices></domain>"""


class DomainSafetyTests(unittest.TestCase):
    def test_source_archive_matches_a_reproducible_clean_commit_archive(self):
        with tempfile.TemporaryDirectory() as temp:
            repository = Path(temp) / "repository"
            repository.mkdir()
            lab.subprocess.run(["git", "init", "-q", str(repository)], check=True)
            lab.subprocess.run(
                ["git", "-C", str(repository), "config", "user.email", "test@example.invalid"],
                check=True,
            )
            lab.subprocess.run(
                ["git", "-C", str(repository), "config", "user.name", "Test"],
                check=True,
            )
            (repository / "source.txt").write_text("committed source\n")
            lab.subprocess.run(["git", "-C", str(repository), "add", "source.txt"], check=True)
            lab.subprocess.run(
                ["git", "-C", str(repository), "commit", "-qm", "source"],
                check=True,
            )
            commit = lab.command(["git", "rev-parse", "HEAD"], cwd=repository).strip()
            archive_bytes = lab.subprocess.run(
                ["git", "archive", "--format=tar", commit], cwd=repository,
                check=True, stdout=lab.subprocess.PIPE,
            ).stdout
            archive_path = Path(temp) / "source.tar"
            archive_path.write_bytes(archive_bytes)
            first = lab.source_archive_provenance(archive_path, repository)
            second = lab.source_archive_provenance(archive_path, repository)
        self.assertEqual(first, second)
        self.assertEqual(first["commit"], commit)
        self.assertEqual(first["archiveFormat"], "tar")
        self.assertEqual(first["archiveCommand"], f"git archive --format=tar {commit}")
        self.assertEqual(first["archiveSHA256"], hashlib.sha256(archive_bytes).hexdigest())

    def test_source_archive_rejects_dirty_checkout(self):
        with tempfile.TemporaryDirectory() as temp:
            repository = Path(temp) / "repository"
            repository.mkdir()
            lab.subprocess.run(["git", "init", "-q", str(repository)], check=True)
            lab.subprocess.run(
                ["git", "-C", str(repository), "config", "user.email", "test@example.invalid"],
                check=True,
            )
            lab.subprocess.run(
                ["git", "-C", str(repository), "config", "user.name", "Test"],
                check=True,
            )
            source = repository / "source.txt"
            source.write_text("committed source\n")
            lab.subprocess.run(["git", "-C", str(repository), "add", "source.txt"], check=True)
            lab.subprocess.run(
                ["git", "-C", str(repository), "commit", "-qm", "source"],
                check=True,
            )
            archive_path = Path(temp) / "source.tar"
            archive_path.write_bytes(lab.subprocess.run(
                ["git", "archive", "--format=tar", "HEAD"], cwd=repository,
                check=True, stdout=lab.subprocess.PIPE,
            ).stdout)
            source.write_text("uncommitted source\n")
            with self.assertRaisesRegex(lab.LabError, "must be clean"):
                lab.source_archive_provenance(archive_path, repository)

    def test_source_archive_rejects_hash_mismatch(self):
        with tempfile.TemporaryDirectory() as temp:
            repository = Path(temp) / "repository"
            repository.mkdir()
            lab.subprocess.run(["git", "init", "-q", str(repository)], check=True)
            lab.subprocess.run(
                ["git", "-C", str(repository), "config", "user.email", "test@example.invalid"],
                check=True,
            )
            lab.subprocess.run(
                ["git", "-C", str(repository), "config", "user.name", "Test"],
                check=True,
            )
            source = repository / "source.txt"
            source.write_text("committed source\n")
            lab.subprocess.run(["git", "-C", str(repository), "add", "source.txt"], check=True)
            lab.subprocess.run(
                ["git", "-C", str(repository), "commit", "-qm", "source"],
                check=True,
            )
            archive_path = Path(temp) / "source.tar"
            archive_path.write_bytes(b"not the committed archive")
            with self.assertRaisesRegex(lab.LabError, "does not match"):
                lab.source_archive_provenance(archive_path, repository)

    def test_kernel_reconnect_waits_for_the_pinned_release_not_just_ssh(self):
        config = {"name": NAME, "pool": "pool", "network": "isolated",
                  "gateway": "192.168.248.1", "network_address": "192.168.248.0/24",
                  "cpus": 4, "memory": 12288, "disk": 128}
        with tempfile.TemporaryDirectory() as temp:
            directory = Path(temp)
            responses = [
                lab.subprocess.CompletedProcess([], 0, "running-kernel=5.15.0-91-generic\n"),
                lab.subprocess.CompletedProcess(
                    [], 0, "running-kernel=6.8.0-149-generic\nEVPN_GUEST_KERNEL_VERIFIED\n"),
            ]
            with (patch.object(lab, "guest_command", side_effect=responses) as guest,
                  patch.object(lab.time, "sleep") as sleep):
                lab.wait_for_inventory_kernel(directory, config, directory)
            self.assertEqual(guest.call_count, 2)
            sleep.assert_called_once_with(5)
            self.assertIn("running-kernel=5.15.0-91-generic",
                          (directory / "kernel-reconnect.log").read_text())

    def test_peerpidfd_probe_result_ignores_vagrant_warnings_and_build_output(self):
        result = {
            "operation": "getsockopt(SOL_SOCKET, SO_PEERPIDFD)",
            "socket": "AF_UNIX/SOCK_STREAM",
            "level": 1,
            "option": 77,
            "peerPidFD": 5,
            "descriptor": "anon_inode:[pidfd]",
        }
        output = (
            "[fog][WARNING] Unrecognized arguments: libvirt_ip_command\n"
            "go: downloading golang.org/x/sys v0.40.0\n"
            f"{json.dumps(result)}\n"
        )
        self.assertEqual(lab.peerpidfd_probe_result(output), result)

    def test_peerpidfd_probe_result_rejects_missing_or_ambiguous_json(self):
        with self.assertRaisesRegex(lab.LabError, "exactly one JSON result"):
            lab.peerpidfd_probe_result("Vagrant warning\nnot JSON\n")
        result = json.dumps({
            "operation": "getsockopt(SOL_SOCKET, SO_PEERPIDFD)",
        })
        with self.assertRaisesRegex(lab.LabError, "exactly one JSON result"):
            lab.peerpidfd_probe_result(f"{result}\n{result}\n")

    def test_evidence_collection_reads_root_owned_guest_artifacts(self):
        command = lab.evidence_collection_command()
        self.assertTrue(command.startswith("sudo bash -ec "))
        self.assertIn("cni-sec-*-evidence", command)
        self.assertIn("tar -C /tmp --null -T - -cf -", command)

    def test_base_box_cache_is_checked_against_the_verified_disk_fingerprint(self):
        with tempfile.TemporaryDirectory() as temp:
            path = Path(temp) / lab.BOX_CACHE_PATH
            path.parent.mkdir(parents=True)
            path.write_bytes(b"verified base disk")
            digest = hashlib.sha256(path.read_bytes()).hexdigest()
            with patch.object(lab, "BOX_DISK_SHA256", digest):
                result = lab.verify_base_box(temp)
            self.assertEqual(result["cachedDiskSHA256"], digest)
            self.assertEqual(result["archiveSHA256"], lab.BOX_ARCHIVE_SHA256)

    def test_base_box_cache_rejects_changed_disk(self):
        with tempfile.TemporaryDirectory() as temp:
            path = Path(temp) / lab.BOX_CACHE_PATH
            path.parent.mkdir(parents=True)
            path.write_bytes(b"not the verified box")
            with self.assertRaisesRegex(lab.LabError, "SHA256 mismatch"):
                lab.verify_base_box(temp)

    def test_inventory_kernel_prerequisite_pins_signed_hwe_packages_and_reboots(self):
        script = lab.inventory_kernel_install_script()
        self.assertIn("apt-get update -o APT::Update::Error-Mode=any", script)
        for package in lab.KERNEL_PACKAGES:
            self.assertIn(f"{package}={lab.KERNEL_PACKAGE_VERSION}", script)
        self.assertIn("systemd-run --unit=evpn-hwe-kernel-reboot", script)
        self.assertIn("6.8.0-149-generic", script)
        self.assertNotIn("pidfd_open", script)

    def test_vagrant_cannot_redirect_state_directory(self):
        config = {"name": NAME, "pool": "pool", "network": "isolated",
                  "gateway": "192.168.248.1", "network_address": "192.168.248.0/24",
                  "cpus": 4, "memory": 12288, "disk": 128}
        with patch.dict(os.environ, {"VAGRANT_CWD": "/foreign",
                                     "VAGRANT_DOTFILE_PATH": "/foreign/.vagrant",
                                     "VAGRANT_VAGRANTFILE": "other"}):
            env = lab.environment(config)
        for key in ("VAGRANT_CWD", "VAGRANT_DOTFILE_PATH", "VAGRANT_VAGRANTFILE"):
            self.assertNotIn(key, env)

    def test_exact_owned_disk_and_network(self):
        self.assertEqual(lab.properties(XML, NAME, "/pool", "isolated"), [f"/pool/{NAME}.img"])

    def test_foreign_disk_rejected(self):
        with self.assertRaises(lab.LabError):
            lab.properties(XML.replace(f"/pool/{NAME}.img", "/pool/foreign.img"), NAME, "/pool", "isolated")

    def test_host_filesystem_rejected(self):
        with self.assertRaises(lab.LabError):
            lab.properties(XML.replace("</devices>", "<filesystem/></devices>"), NAME, "/pool", "isolated")

    def test_foreign_network_rejected(self):
        with self.assertRaises(lab.LabError):
            lab.properties(XML, NAME, "/pool", "foreign")

    def test_missing_receipt_fails_closed(self):
        with self.assertRaises(lab.LabError):
            lab.checked_domain(Path("/does-not-exist"), {"name": NAME})

    def test_refuses_changed_vagrant_configuration(self):
        with tempfile.TemporaryDirectory() as temp:
            directory = Path(temp)
            (directory / "Vagrantfile").write_text("changed")
            with self.assertRaisesRegex(lab.LabError, "Vagrant configuration changed"):
                lab.checked_domain(directory, {"name": NAME, "uuid": "id",
                                                "disks": [], "templates": {"Vagrantfile": "bad"}})

    def test_refuses_changed_vagrant_machine_id(self):
        with tempfile.TemporaryDirectory() as temp:
            directory = Path(temp)
            id_path = directory / ".vagrant" / "machines" / NAME / "libvirt"
            id_path.mkdir(parents=True)
            (id_path / "id").write_text("foreign")
            with patch.object(lab, "virsh") as virsh:
                with self.assertRaisesRegex(lab.LabError, "machine ID"):
                    lab.checked_domain(directory, {"name": NAME, "uuid": "expected",
                                                    "disks": [], "templates": {}})
                virsh.assert_not_called()


if __name__ == "__main__":
    unittest.main()
