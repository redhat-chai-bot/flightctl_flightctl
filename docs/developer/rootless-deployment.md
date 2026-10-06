# Rootless local deployment

Use Kind or Quadlets to develop with a local, user-owned Podman store.
The invoking UID selects the scope: regular users use rootless Podman and
user systemd; UID 0 uses the system manager and rootful Podman.

## Prepare your development host

1. Install Podman, Kind, Go, and the development dependencies from the
   [developer guide](README.md).
2. Configure subordinate UID and GID ranges in `/etc/subuid` and `/etc/subgid`.
3. Use cgroup v2 and an active user systemd session. Enable lingering if services
   must survive logout.
4. Unset `CONTAINER_HOST` and `CONTAINER_CONNECTION` to select the local store.
5. Run `make deploy` for Kind or `make deploy-quadlets` for Quadlets.
   Do not run both stacks concurrently because their published ports overlap.
6. For Quadlets, copy the login command printed after deployment.

KVM and crun are prerequisites for VM-backed image exports, not API deployment.
Grant read/write access to `/dev/kvm` using the KVM group or a device ACL.
Group changes require a refreshed login and user systemd session.
Rootless nested Kind workers may need a per-user device ACL because host groups
can be unmapped inside the node. ACLs do not bypass SELinux or device cgroups.

## Configure VM image exports

All disk-image exports use native `image-builder build --in-vm` in both scopes.
**Breaking change for existing rootful deployments:** ImageExport now requires
read/write access to `/dev/kvm` and a VM-compatible builder image. Helm enables
`imageBuilderWorker.kvmDevice.enabled` by default; schedule workers on nodes with
KVM and ensure device policy permits access. Before upgrading, publish a compatible
builder image and configure its registry reference as described below. ImageBuild
does not require KVM. Disabling the KVM mount prevents ImageExport in either scope.

Leaving `imageBuilderWorker.serviceImages.bootcImageBuilder.image` empty selects
`ghcr.io/osbuild/bootc-image-builder@sha256:e7aadce6b3f5639cd47d83354791931ea219891a0d113c2fe74a0f0d352b165c`.
The standard upstream builder image lacks QEMU, virtiofsd, and the TOML module
needed by this path. Build the compatible image explicitly:

```bash
BIB_IMAGE=localhost/flightctl-vm-image-builder:latest make build-vm-image-builder
```

The compatible image selects the image type's export pipelines for VM execution,
including the ISO pipeline, instead of assuming the QCOW2 `image` pipeline.
It selects the QEMU package for x86_64 or aarch64 and preserves the complete
TOML package inside the OSBuild VM's Python package tree.

For QCOW preparation, unset `BIB_IMAGE` to build this image automatically, or
set `BIB_IMAGE` to your compatible registry reference. This setting is separate
from worker configuration.

For service exports, push the compatible image to a registry reachable by the
worker and set `imageBuilderWorker.serviceImages.bootcImageBuilder.image` in
Quadlet service configuration. With Helm, set
`imageBuilderWorker.serviceImages.bootcImageBuilder.image` in chart values.
A checkout-local image is not visible in the nested worker's Podman store;
the worker pulls the configured registry reference into that store.
The worker checks CLI flags, QEMU, virtiofsd, and TOML before exporting.

## Scope and security boundaries

Rootless Podman does not make privileged containers unprivileged.
The worker and builder retain container privileges within their user namespace.
VM builds select the helper container's `unconfined_t` SELinux process type,
retaining volume relabeling rather than disabling SELinux labels entirely. They
do not require an unavailable host `osbuild_container_t` policy type. This relaxed
process domain is deliberate for the privileged VM helper; the VM is the build boundary.

Quadlet configuration uses `%E`, `%D`, and `%S` for manager-specific paths.
User data lives below the Flight Control XDG namespace; system data uses the
existing system paths. Rootless-only drop-ins preserve supplementary groups;
the privileged base unit already provides device access and relaxed labeling.
Services attach to the Flight Control core and observability targets in both scopes.

## Cleanup and ownership

`make clean` operates on the invoking user's local store. Generated unit files
carry provenance markers, so cleanup preserves unrelated user units and CLI
credentials. Ownership reclamation never dereferences symlinks outside the tree.
`make clean-all` additionally removes owned checkout artifacts.

Rootful RPM builds leave output owned by root. Before switching to unprivileged
builds, clean those artifacts as their owner or arrange writable user-owned output.

## Known limitations

Image exports require the compatible builder image; the default upstream image
is not sufficient. KVM access and device-cgroup policy remain host requirements.
ISO and other VM exports are experimental until validated on a supported host.
No live deployment or export validation is implied by unit-test results.
Behavioral tests cover script scope selection, generated-file cleanup, and
rendered mount configuration. They do not validate live container execution.
