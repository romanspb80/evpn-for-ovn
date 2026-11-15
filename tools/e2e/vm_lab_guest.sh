#!/usr/bin/env bash
set -euo pipefail

uuid="${1:?expected libvirt UUID required}"
scenario="${2:?scenario required}"
pin="${3:?ctr pin or - required}"
gobgpd_pin="${4:--}"
[[ "$uuid" =~ ^[0-9a-f-]{36}$ ]]
[[ "$(sudo cat /sys/class/dmi/id/product_uuid | tr '[:upper:]' '[:lower:]')" == "$uuid" ]] || {
    echo "Guest UUID does not match the host ownership receipt" >&2; exit 1;
}
case "$(systemd-detect-virt)" in kvm|qemu) ;; *) echo "Not a KVM/QEMU guest" >&2; exit 1;; esac
case "$scenario" in baseline|continuity|both|operator-inventory-type2-two-worker) ;;
    *) echo "Invalid scenario" >&2; exit 1;;
esac
umask 077
result="$(mktemp -d /tmp/evpn-vm-run-XXXXXXXX)"
blocked() {
    echo "BLOCKED: guest preflight failed; evidence: $result" >&2
    case "$scenario" in
        baseline) echo BLOCKED >"$result/baseline.status" ;;
        continuity) echo BLOCKED >"$result/continuity.status" ;;
        both) echo BLOCKED >"$result/baseline.status"; echo BLOCKED >"$result/continuity.status" ;;
        operator-inventory-type2-two-worker) echo BLOCKED >"$result/inventory.status" ;;
    esac
}
trap blocked ERR
[[ -z "${DOCKER_HOST:-}" && -z "${DOCKER_CONTEXT:-}" ]] || { echo "Docker override set" >&2; false; }
[[ "$(docker context show)" == default ]] || { echo "Docker context is not default" >&2; false; }
docker info >/dev/null
containers="$(docker ps -aq)"
clusters="$(kind get clusters)"
contexts="$(kubectl config get-contexts -o name)"
mounts="$(findmnt -rn -o FSTYPE)"
[[ -z "$containers" ]] || { echo "Guest Docker is not empty" >&2; false; }
[[ -z "$clusters" ]] || { echo "Guest kind is not empty" >&2; false; }
[[ -z "$contexts" ]] || { echo "Guest has existing Kubernetes contexts" >&2; false; }
if grep -qxE '9p|virtiofs' <<<"$mounts"; then
    echo "Guest has a host-shared filesystem" >&2; false
fi
[[ "$(docker image inspect evpn4ovn/ovn-ovs:kind-26.09 --format '{{.Id}}')" == \
  "sha256:c593278d6b9fffcd51a7b355bd80efca291eda4bc7848c3dd838078ef5109505" ]] || {
    echo "OVN image identity mismatch" >&2; false;
}
sudo modprobe openvswitch
if [[ "$scenario" == continuity || "$scenario" == both ||
      "$scenario" == operator-inventory-type2-two-worker ]]; then
    [[ "$pin" =~ ^[0-9a-f]{64}$ ]]
    printf '%s  %s\n' "$pin" "$HOME/evpn-trust/ctr" | sha256sum -c
fi
if [[ "$scenario" == operator-inventory-type2-two-worker ]]; then
    [[ "$gobgpd_pin" =~ ^[0-9a-f]{64}$ ]]
    printf '%s  %s\n' "$gobgpd_pin" "$HOME/evpn-trust/gobgpd" | sha256sum -c
    [[ "${EVPN_SOURCE_COMMIT:-}" =~ ^[0-9a-f]{40}$ ]]
    [[ "${EVPN_SOURCE_ARCHIVE_SHA256:-}" =~ ^[0-9a-f]{64}$ ]]
    [[ "${EVPN_SOURCE_ARCHIVE_FORMAT:-}" == tar ]]
    [[ -n "${EVPN_SOURCE_ARCHIVE_COMMAND:-}" ]]
    python3 - "$HOME/evpn-for-ovn-source.tar" \
        "$HOME/evpn-for-ovn-source-provenance.json" \
        "$HOME/evpn-for-ovn" "$EVPN_SOURCE_COMMIT" \
        "$EVPN_SOURCE_ARCHIVE_SHA256" "$EVPN_SOURCE_ARCHIVE_FORMAT" \
        "$EVPN_SOURCE_ARCHIVE_COMMAND" <<'PY'
import hashlib,json,pathlib,re,stat,sys,tarfile
archive_path,receipt_path,source_path=map(pathlib.Path,sys.argv[1:4])
commit,digest,archive_format,command=sys.argv[4:]
if any(path.is_symlink() or not path.is_file() for path in (archive_path,receipt_path)):
    raise SystemExit("source archive or provenance receipt is missing/unsafe")
receipt=json.loads(receipt_path.read_text())
expected={"commit":commit,"archiveSHA256":digest,"archiveFormat":archive_format,
          "archiveCommand":command}
if receipt != expected or not re.fullmatch(r"[0-9a-f]{40}",commit) or \
   not re.fullmatch(r"[0-9a-f]{64}",digest) or archive_format != "tar" or \
   command != f"git archive --format=tar {commit}":
    raise SystemExit("source provenance does not match the recorded committed archive")
with archive_path.open("rb") as stream:
    if hashlib.file_digest(stream,"sha256").hexdigest()!=digest:
        raise SystemExit("uploaded source archive SHA256 mismatch")
if source_path.is_symlink() or not source_path.is_dir():
    raise SystemExit("extracted source directory is missing/unsafe")
expected_paths=set()
with tarfile.open(archive_path,"r:") as archive:
    members=archive.getmembers()
    for member in members:
        path=pathlib.PurePosixPath(member.name)
        if path.is_absolute() or ".." in path.parts or not (member.isfile() or member.isdir()):
            raise SystemExit(f"unsafe source archive entry: {member.name}")
        normalized=path.as_posix().rstrip("/")
        if normalized:
            expected_paths.add(normalized)
        for parent in path.parents:
            if parent.as_posix() != ".":
                expected_paths.add(parent.as_posix())
    actual_paths=set()
    for path in source_path.rglob("*"):
        relative=path.relative_to(source_path).as_posix()
        if path.is_symlink() or not (path.is_file() or path.is_dir()):
            raise SystemExit(f"unsafe extracted source entry: {relative}")
        actual_paths.add(relative)
    if actual_paths != expected_paths:
        raise SystemExit("extracted source paths differ from the committed archive")
    for member in members:
        if not member.isfile():
            continue
        path=source_path.joinpath(*pathlib.PurePosixPath(member.name).parts)
        with archive.extractfile(member) as expected_stream, path.open("rb") as actual_stream:
            if hashlib.file_digest(expected_stream,"sha256").digest()!= \
               hashlib.file_digest(actual_stream,"sha256").digest():
                raise SystemExit(f"extracted source content differs: {member.name}")
PY
    python3 - "$result/source-provenance.json" "$EVPN_SOURCE_COMMIT" \
        "$EVPN_SOURCE_ARCHIVE_SHA256" "$EVPN_SOURCE_ARCHIVE_FORMAT" \
        "$EVPN_SOURCE_ARCHIVE_COMMAND" <<'PY'
import json,sys
json.dump({
    "commit": sys.argv[2],
    "archiveSHA256": sys.argv[3],
    "archiveFormat": sys.argv[4],
    "archiveCommand": sys.argv[5],
}, open(sys.argv[1], "w"), separators=(",", ":"))
PY
    [[ "$(uname -r)" == "6.8.0-149-generic" ]]
    dpkg-query -W -f='${Package} ${Version}\n' \
        linux-image-6.8.0-149-generic \
        linux-modules-6.8.0-149-generic \
        linux-modules-extra-6.8.0-149-generic >"$result/kernel-packages.txt"
    grep -Fqx "linux-image-6.8.0-149-generic 6.8.0-149.149~22.04.1" "$result/kernel-packages.txt"
    grep -Fqx "linux-modules-6.8.0-149-generic 6.8.0-149.149~22.04.1" "$result/kernel-packages.txt"
    grep -Fqx "linux-modules-extra-6.8.0-149-generic 6.8.0-149.149~22.04.1" "$result/kernel-packages.txt"
    apt-mark showhold >"$result/kernel-holds.txt"
    grep -Fqx linux-image-6.8.0-149-generic "$result/kernel-holds.txt"
    grep -Fqx linux-modules-6.8.0-149-generic "$result/kernel-holds.txt"
    grep -Fqx linux-modules-extra-6.8.0-149-generic "$result/kernel-holds.txt"
    (cd "$HOME/evpn-for-ovn" &&
        go build -o "$result/peerpidfd-probe" ./tools/e2e/peerpidfdprobe)
    "$result/peerpidfd-probe" >"$result/peerpidfd-guest.json"
fi

{
    date -u
    uname -a
    docker version
    kind version
    kubectl version --client
    containerlab version
    go version
    if [[ "$scenario" == operator-inventory-type2-two-worker ]]; then
        dpkg-query -W -f='${Package} ${Version}\n' \
            linux-image-6.8.0-149-generic \
            linux-modules-6.8.0-149-generic \
            linux-modules-extra-6.8.0-149-generic
        apt-mark showhold
        cat "$result/kernel-holds.txt"
        cat "$result/peerpidfd-guest.json"
        cat "$result/source-provenance.json"
    fi
} >"$result/environment.txt" 2>&1
trap - ERR
cd "$HOME/evpn-for-ovn"
status=0
run_one() {
    local name="$1"
    shift
    local code=0
    bash tools/run-operator-secondary-cni-e2e.sh --confirm-disposable-host "$@" \
        >"$result/$name.log" 2>&1 || code=$?
    local marker
    if [[ "$name" == baseline ]]; then
        marker=OPERATOR_SECONDARY_CNI_BASELINE_PASS
    else
        case "$name" in
            continuity) marker=OPERATOR_SECONDARY_CNI_AUTHENTICATED_PASS ;;
            inventory) marker=OPERATOR_INVENTORY_TWO_WORKER_PASS ;;
        esac
    fi
    if [[ "$code" == 0 ]] && grep -q "^$marker evidence=" "$result/$name.log"; then
        printf 'PASS\n' >"$result/$name.status"
    elif [[ "$name" == inventory ]] &&
         grep -q "dataplane forwarding evidence is not implemented" "$result/$name.log"; then
        printf 'BLOCKED: endpoint/export assertions reached; forwarding evidence is not implemented\n' \
            >"$result/$name.status"
        status=1
    else
        printf 'FAIL exit=%s expected-marker=%s\n' "$code" "$marker" >"$result/$name.status"
        status=1
    fi
    cat "$result/$name.status"
    echo "Log: $result/$name.log"
}
case "$scenario" in
    baseline) run_one baseline ;;
    continuity) run_one continuity --continuity-ctr "$HOME/evpn-trust/ctr" --continuity-ctr-sha256 "$pin" ;;
    both)
        run_one baseline
        run_one continuity --continuity-ctr "$HOME/evpn-trust/ctr" --continuity-ctr-sha256 "$pin"
        ;;
    operator-inventory-type2-two-worker)
        run_one inventory --scenario operator-inventory-type2-two-worker \
            --confirm-disposable-host \
            --continuity-ctr "$HOME/evpn-trust/ctr" \
            --continuity-ctr-sha256 "$pin" \
            --inventory-gobgpd "$HOME/evpn-trust/gobgpd" \
            --inventory-gobgpd-sha256 "$gobgpd_pin"
        ;;
    *) echo "Invalid scenario" >&2; exit 1 ;;
esac
exit "$status"
