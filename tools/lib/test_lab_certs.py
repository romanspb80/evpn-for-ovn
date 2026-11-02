import hashlib
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import unittest


ROOT = Path(__file__).resolve().parents[2]
SCRIPT = ROOT / "tools/generate-lab-certs.sh"
FILES = ("ca.key", "ca.pem", "server.key", "server.pem", "client.key", "client.pem")


@unittest.skipUnless(shutil.which("openssl"), "OpenSSL is required")
class LabCertificatesTest(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix="evpn-lab-certs-")
        self.addCleanup(self.temp.cleanup)
        self.directory = Path(self.temp.name)
        self.bundle = self.directory / "tls"

    def generate(self, *args, success=True):
        result = subprocess.run(
            ["bash", str(SCRIPT), "--output-dir", str(self.bundle), *args],
            capture_output=True, text=True,
        )
        self.assertEqual(result.returncode == 0, success, result.stderr)
        self.assertNotIn("PRIVATE KEY-----", result.stdout + result.stderr)
        return result

    def openssl(self, *args, success=True):
        result = subprocess.run(["openssl", *map(str, args)], capture_output=True, text=True)
        self.assertEqual(result.returncode == 0, success, result.stderr)
        return result.stdout

    def hashes(self):
        return {name: hashlib.sha256((self.bundle / name).read_bytes()).hexdigest() for name in FILES}

    def test_generation_validity_sans_eku_permissions_and_idempotency(self):
        self.generate("--dns-name", "custom.lab", "--ip-address", "192.0.2.10")
        before = self.hashes()
        for role, purpose in (("server", "sslserver"), ("client", "sslclient")):
            self.openssl("verify", "-purpose", purpose, "-CAfile", self.bundle / "ca.pem",
                         self.bundle / f"{role}.pem")
        self.assertIn("URI:urn:evpn-for-ovn:lab-client",
                      self.openssl("x509", "-in", self.bundle / "client.pem",
                                   "-noout", "-ext", "subjectAltName"))
        for option, name in (("-verify_hostname", "localhost"), ("-verify_hostname", "custom.lab"),
                             ("-verify_ip", "127.0.0.1"), ("-verify_ip", "192.0.2.10")):
            self.openssl("verify", "-CAfile", self.bundle / "ca.pem", option, name,
                         self.bundle / "server.pem")
        self.openssl("verify", "-CAfile", self.bundle / "ca.pem", "-purpose", "sslclient",
                     self.bundle / "server.pem", success=False)
        self.openssl("verify", "-CAfile", self.bundle / "ca.pem", "-purpose", "sslserver",
                     self.bundle / "client.pem", success=False)
        for role in ("ca", "server", "client"):
            self.assertEqual((self.bundle / f"{role}.key").stat().st_mode & 0o777, 0o600)
        (self.bundle / "server.key").chmod(0o644)
        self.generate("--dns-name", "custom.lab", "--ip-address", "192.0.2.10")
        self.assertEqual(before, self.hashes())
        self.assertEqual((self.bundle / "server.key").stat().st_mode & 0o777, 0o600)
        self.generate("--regenerate")
        self.assertNotEqual(before["ca.key"], self.hashes()["ca.key"])

    def test_invalid_partial_and_mismatched_bundles_are_not_overwritten(self):
        self.bundle.mkdir()
        (self.bundle / "ca.pem").write_text("not a certificate\n")
        self.generate(success=False)
        self.assertEqual((self.bundle / "ca.pem").read_text(), "not a certificate\n")
        self.generate("--regenerate")
        (self.bundle / "server.key").write_bytes((self.bundle / "client.key").read_bytes())
        before = self.hashes()
        self.generate(success=False)
        self.assertEqual(before, self.hashes())
        self.generate("--regenerate")
        before = self.hashes()
        self.generate("--dns-name", "missing.lab", success=False)
        self.assertEqual(before, self.hashes())

    def test_invalid_san_and_symlink_are_rejected(self):
        self.generate("--dns-name", "bad\nDNS.9=evil", success=False)
        self.assertFalse(self.bundle.exists())
        self.directory.joinpath("real").mkdir()
        self.bundle.symlink_to(self.directory / "real", target_is_directory=True)
        self.generate(success=False)
        self.assertFalse(list((self.directory / "real").iterdir()))

    def test_existing_generation_lock_and_file_symlink_are_rejected(self):
        self.generate()
        before = self.hashes()
        lock = self.bundle / ".generate.lock"
        lock.mkdir()
        self.generate("--regenerate", success=False)
        self.assertEqual(before, self.hashes())
        lock.rmdir()
        (self.bundle / "server.pem").unlink()
        (self.bundle / "server.pem").symlink_to(self.bundle / "client.pem")
        self.generate("--regenerate", success=False)
        self.assertTrue((self.bundle / "server.pem").is_symlink())
        self.assertEqual(before["client.pem"], self.hashes()["client.pem"])

    def test_bootstrap_generates_tls_before_containerlab_without_existing_files(self):
        # A minimal checkout with infrastructure commands intercepted before deployment.
        checkout = self.directory / "checkout"
        (checkout / "tools/e2e").mkdir(parents=True)
        (checkout / "deploy/containerlab").mkdir(parents=True)
        (checkout / "frr").mkdir()
        shutil.copyfile(SCRIPT, checkout / "tools/generate-lab-certs.sh")
        shutil.copyfile(ROOT / "tools/e2e/containerlab.py", checkout / "tools/e2e/containerlab.py")
        for name in ("daemons", "vtysh.conf"):
            shutil.copyfile(ROOT / "frr" / name, checkout / "frr" / name)
        bootstrap = checkout / "deploy/containerlab/deploy-kind-frr.sh"
        shutil.copyfile(ROOT / "deploy/containerlab/deploy-kind-frr.sh", bootstrap)
        commands = self.directory / "commands"
        commands.mkdir()
        docker = commands / "docker"
        docker.write_text(
            '#!/bin/sh\n'
            'if [ "$2" = inspect ]; then echo "No such resource" >&2; exit 1; fi\n'
            'exit 0\n'
        )
        docker.chmod(0o755)
        containerlab = commands / "containerlab"
        containerlab.write_text(
            '#!/bin/sh\n'
            'if [ "$1" = version ]; then echo "version: 0.79.0"; exit 0; fi\n'
            '[ "$1" = deploy ] || exit 98\n'
            '[ -s "$CLAB_CA_KEY_FILE" ] && [ -s "$CLAB_CA_CERT_FILE" ] || exit 99\n'
            'openssl verify -check_ss_sig -CAfile "$CLAB_CA_CERT_FILE" "$CLAB_CA_CERT_FILE" || exit 100\n'
            'echo "TLS_READY_DEPLOY_NOT_EXECUTED"\n'
            'exit 97\n'
        )
        containerlab.chmod(0o755)
        env = dict(os.environ)
        env.pop("LAB_TLS_DIR", None)
        env.pop("EVPN_E2E_LOCK_FD", None)
        env["HOME"] = str(self.directory / "home")
        env["EVIDENCE_DIR"] = str(self.directory / "lab")
        env["PATH"] = str(commands) + os.pathsep + env["PATH"]
        result = subprocess.run(["bash", str(bootstrap)], cwd=checkout, env=env,
                                capture_output=True, text=True)
        self.assertEqual(result.returncode, 1, result.stderr)
        self.assertIn("TLS_READY_DEPLOY_NOT_EXECUTED",
                      (self.directory / "lab/containerlab-deploy.log").read_text())
        self.assertTrue((self.directory / "home/.local/state/evpn-for-ovn/containerlab/tls/client.pem").is_file())
        self.assertFalse((checkout / "deploy/containerlab/.lab-tls").exists())


if __name__ == "__main__":
    unittest.main()
