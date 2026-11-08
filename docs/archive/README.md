# Project history and evidence provenance

[Project history](../../README.md#project-history-and-evolution) |
[Implementation status](../../IMPLEMENTATION_README.md) |
[Release provenance](../roadmap/release.md)

This archive index preserves navigation to historical engineering material; it
does not fabricate a local copy of evidence or resurrect obsolete installation
instructions. No archive directory existed in the reviewed checkout before G.8.
The original implementation remains in the deliberately separate historical
[`master` branch](https://github.com/romanspb80/evpn-for-ovn/tree/master).
The modern default `main` is a redesigned Go implementation, not a rewrite of
that branch's commits.

## Independently checked historical records

| Record | What it establishes |
|---|---|
| [2018-12-20 README commit](https://github.com/romanspb80/evpn-for-ovn/commit/0ef54bd48547d7bd984124ef5c4d63540516f6b6) | Project activity in 2018; not proof of the exact earliest start date. |
| [2018-12-23 Ryu applications](https://github.com/romanspb80/evpn-for-ovn/commit/113bcc3bf332b802fa84509223348798d60c34a5) and [README at that commit](https://github.com/romanspb80/evpn-for-ovn/blob/113bcc3bf332b802fa84509223348798d60c34a5/README.md) | Python/Ryu EVPN-VXLAN prototype for OVN/OpenStack; REST/RPC separation and RabbitMQ/oslo.messaging. |
| [Historical README snapshot](https://github.com/romanspb80/evpn-for-ovn/blob/2db621f16cf676b39264350f186110d0d28ae996/README.md) and [OVSDB-agent dependencies](https://github.com/romanspb80/evpn-for-ovn/blob/2db621f16cf676b39264350f186110d0d28ae996/docker_files/ovsdb-agent/requirements.txt) | Later Python prototype used Ryu, ovsdbapp and messaging components; its future plans mention GoBGP/libovsdb and CNI. These are historical plans, not proof of modern capabilities. |
| Modern root `38f0821d053b814be2016113d354b80262fafe40` (2026-11-02) | Locally verified parentless Go-framework root; modern and historical implementation histories are deliberately independent. |

Remote history was read through GitHub without fetching or changing refs. This
checkout has no local `master` or `origin/master`; no such ref was created.
Historical dates establish a 2018 origin, not a precise initial-development day.

## Historical and current runtime evidence

- [OVN crash/reference reproduction](../issues/ovn-port-binding-reproducer.md)
  retains the historical isolated-lab record and its limitations.
- [Testing Guide](../../TESTING_GUIDE.md) retains earlier implementation test
  summaries and the verified G.7b.5a evidence manifest.
- Local `/tmp` evidence paths are ephemeral, not public downloadable artifacts.
  Before release or upstream submission, the maintainer must retain a sanitized,
  reproducible bundle with image/source provenance and access-independent links.

Do not delete older evidence merely because it describes the Python prototype
or an earlier Go architecture. Do not execute historical recipes against the
host's OVN/OVS installation.
