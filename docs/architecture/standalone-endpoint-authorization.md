# Standalone endpoint authorization

Standalone export is an opt-in Framework path for environments without the
Kubernetes CNI authority. It is not enabled by default and is not a general
platform installation method.

## Required authority

The administrator supplies protected authorization policy and provider
attestation configuration. Files and parent directories must have the
ownership and permission properties required by the implementation. The
runtime binds the policy to installation, node and chassis identity, accepts
revisions through a durable fence, and intersects authorized endpoints with
current OVN state. OVN external IDs alone do not authorize advertisement.

The runtime attests the actual local GoBGP Unix API peer and process before
using speaker state. A TCP endpoint, successful health check, historical
receipt or matching executable name is not enough. Policy and attestation
failures block new advertisement and unsafe cleanup; they do not justify
adopting ambiguous routes.

## Ownership and withdrawal

Each route is tracked with its exact GoBGP path identity and durable ownership
receipt. Withdrawal is limited to the route the runtime can verify it owns.
Uncertain policy, incomplete observations, mismatched installation/node
identity or an unavailable speaker must not be translated into permission to
delete or re-advertise. Preserve foreign state and unresolved evidence.

The standalone authorization model is separate from Kubernetes CNI continuity
and from legacy management RPCs. See [CNI endpoint continuity](cni-endpoint-continuity.md),
[security contract](../security/netlink-deployment-security-contract.md), and
[Type-2 ownership](type2-restart-ownership.md).
