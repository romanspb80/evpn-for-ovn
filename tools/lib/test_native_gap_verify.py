import copy
import json
from pathlib import Path
import re
import subprocess
import sys
import unittest

from tools.lib import native_gap_verify as verify


VTEP = "198.18.0.2"
FOREIGN = "198.18.0.4"


def fixture(variant):
    rows = []
    gobgp = []
    frr = []
    for mac, ip in sorted(verify.expected(variant)):
        rows.append({"Endpoint": {"MACAddress": mac, "IPAddresses": [ip] if ip else None,
                    "MACOnly": not ip, "VNI": 1000, "RouteDistinguisher": VTEP + ":1000",
                    "RouteTargets": ["64512:1000"], "LocalVTEP": VTEP,
                    "EndpointID": "endpoint-" + mac, "Incarnation": "pb"},
                    "PathUUID": mac + ip})
        suffix = "[ip:" + ip + "]" if ip else ""
        gobgp.append(f"*> [type:macadv][rd:{VTEP}:1000][etag:0][mac:{mac}]{suffix} "
                     f"[1000] {VTEP} [{{Origin: i}} {{Extcomms: [64512:1000], [VXLAN]}}]")
        bits = 128 if ":" in ip else 32
        suffix = f":[{bits}]:[{ip}]" if ip else ""
        frr.extend((f" *>i [2]:[0]:[48]:[{mac}]{suffix} RD {VTEP}:1000",
                    f"                    {VTEP} 100 0 i", "                    RT:64512:1000 ET:8"))
    for ip in ("", "10.244.240.12"):
        suffix = "[ip:" + ip + "]" if ip else ""
        gobgp.append(f"*> [type:macadv][rd:198.18.0.3:1000][etag:0][mac:0a:58:f0:01:00:0c]{suffix} "
                     f"[1000] {FOREIGN} [{{Extcomms: [VXLAN], [64512:1000]}}]")
    return ({"Error": "", "Result": {"Applied": rows}, "SpeakerProcess": {"PID": 42}},
            "\n".join(gobgp), "\n".join(frr))


class NativeGapVerificationTests(unittest.TestCase):
    def test_capture_interface_selector(self):
        helper = Path(__file__).with_name("framework-native-gap-cases.sh").read_text()
        snippets = re.findall(r"python3 -c '([^']*)'", helper)
        self.assertTrue(snippets)
        for snippet in snippets:
            compile(snippet, str(helper), "exec")
        interfaces = [
            {"ifname": "lo", "addr_info": [{"local": "127.0.0.1"}]},
            {"ifname": "eth0", "addr_info": [{"local": VTEP}]},
        ]
        result = subprocess.run(
            [sys.executable, "-c", snippets[0], VTEP],
            input=json.dumps(interfaces), text=True, capture_output=True, check=True,
        )
        self.assertEqual(result.stdout.strip(), "eth0")
        result = subprocess.run(
            [sys.executable, "-c", snippets[0], FOREIGN],
            input=json.dumps(interfaces), text=True, capture_output=True,
        )
        self.assertNotEqual(result.returncode, 0)

    def test_exact_dual_stack_and_base(self):
        for variant in ("base", "dual-a", "dual-b", "mac-a"):
            status, gobgp, frr = fixture(variant)
            actual = verify.verify(status, gobgp, frr, variant, VTEP, FOREIGN)
            self.assertEqual(set(actual), verify.expected(variant))

    def test_rejects_wrong_rt_next_hop_label_and_missing_ipv6(self):
        status, gobgp, frr = fixture("dual-a")
        for bad in (gobgp.replace("[64512:1000]", "[64512:999]"),
                    gobgp.replace("[1000] " + VTEP, "[1001] " + VTEP),
                    gobgp.replace("[1000] " + VTEP, "[1000] 198.18.0.9"),
                    "\n".join(line for line in gobgp.splitlines() if verify.IP6_A not in line)):
            with self.assertRaises(AssertionError):
                verify.verify(status, bad, frr, "dual-a", VTEP, FOREIGN)

    def test_rejects_missing_foreign_route_and_unhealthy_speaker(self):
        status, gobgp, frr = fixture("dual-a")
        with self.assertRaises(AssertionError):
            verify.verify(status, "\n".join(line for line in gobgp.splitlines()
                                            if "00:0c" not in line), frr,
                          "dual-a", VTEP, FOREIGN)
        status["Error"] = "endpoint observation failed: SB timed out"
        with self.assertRaises(AssertionError):
            verify.verify(status, gobgp, frr, "dual-a", VTEP, FOREIGN)
        with self.assertRaises(AssertionError):
            verify.verify(status, gobgp, frr, "dual-a", VTEP, FOREIGN, frozen=True)
        status["Result"]["OwnershipUnverified"] = True
        verify.verify(status, gobgp, frr, "dual-a", VTEP, FOREIGN, frozen=True)
        status["Error"] = ""
        with self.assertRaises(AssertionError):
            verify.verify(status, gobgp, frr, "dual-a", VTEP, FOREIGN)
        status["Error"] = "unrelated error"
        with self.assertRaises(AssertionError):
            verify.verify(status, gobgp, frr, "dual-a", VTEP, FOREIGN, frozen=True)

    def test_rejects_changed_receipt_and_frr_attributes(self):
        status, gobgp, frr = fixture("dual-a")
        changed = copy.deepcopy(status)
        changed["Result"]["Applied"][0]["Endpoint"]["VNI"] = 200
        with self.assertRaises(AssertionError):
            verify.verify(changed, gobgp, frr, "dual-a", VTEP, FOREIGN)
        with self.assertRaises(AssertionError):
            verify.verify(status, gobgp, frr.replace("RT:64512:1000", "RT:64512:999"),
                          "dual-a", VTEP, FOREIGN)

    def test_kernel_requires_exact_local_and_foreign(self):
        fdb = [{"mac": verify.MAC_A}, {"mac": verify.MAC_B}]
        n4 = [{"dst": ip, "lladdr": mac, "state": ["NOARP"]}
              for mac, ip in ((verify.MAC_A, verify.IP_A), (verify.MAC_B, verify.IP_B))]
        n4.append({"dst": "10.244.240.12", "lladdr": "0a:58:f0:01:00:0c"})
        n6 = [{"dst": verify.IP6_A, "lladdr": verify.MAC_A, "state": ["NOARP"]}]
        verify.verify_kernel(fdb, n4, n6, "dual-a")
        with self.assertRaises(AssertionError):
            verify.verify_kernel(fdb, n4, [], "dual-a")
        with self.assertRaises(AssertionError):
            verify.verify_kernel(fdb, n4[:-1], n6, "dual-a")


if __name__ == "__main__":
    unittest.main()
