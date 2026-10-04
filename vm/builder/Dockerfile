# The vm isolation's builder guest, as a raw ext4 disk: `make vm-builder`
# (docker, on the host or in CI) writes builder-<arch>.img. The guest boots
# it read-only with caboose's kernel and init, runs dockerd in it, and
# builds the sandbox's image there (vm/builder/caboose-builder,
# internal/vm/builder), so a Mac under vm needs no Docker engine of its own.
ARG ALPINE=alpine:3.24

FROM ${ALPINE} AS rootfs
# dockerd and buildx for docker build -o, iptables for its bridge (spike 3),
# e2fsprogs for the root disk, GNU tar to keep owners and xattrs.
RUN apk add --no-cache docker-engine docker-cli docker-cli-buildx \
	iptables ip6tables e2fsprogs tar ca-certificates
COPY caboose-builder /usr/local/bin/caboose-builder

# The disk: the root filesystem above, owners and all, written by
# mkfs.ext4 -d as root, with room to spare for nothing (it is read-only).
FROM ${ALPINE} AS disk
RUN apk add --no-cache e2fsprogs
COPY --from=rootfs / /rootfs
RUN size=$(du -sm /rootfs | cut -f1) && \
	truncate -s "$((size + size / 4 + 64))M" /builder.img && \
	mkfs.ext4 -q -F -L caboose-builder -d /rootfs /builder.img && \
	e2fsck -fn /builder.img

FROM scratch
COPY --from=disk /builder.img /builder.img
