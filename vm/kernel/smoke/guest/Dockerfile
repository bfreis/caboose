# QEMU for make vm-guest-smoke, apart from make vm-kernel-smoke's image: the
# real guest init needs a vsock backend to be driven, and
# vhost-device-vsock, the one QEMU takes without /dev/vhost-vsock on the
# machine, is not in trixie. So this is the testing suite, forky,
# pinned by digest and snapshot date as vm/kernel's build is. `boot`, its
# entrypoint, makes the disks and boots what make mounts at /smoke.
ARG DEBIAN=debian:forky-20260918-slim@sha256:7bb96d9b4ccd7f46f54853c258a56df0c6b628a63e65c2317959da18fd36932c

FROM ${DEBIAN}
ARG DEBIAN_SNAPSHOT=20260918T000000Z
RUN set -e; \
	printf '%s\n' "Types: deb" \
	  "URIs: http://snapshot.debian.org/archive/debian/$DEBIAN_SNAPSHOT/" \
	  "Suites: forky" "Components: main" \
	  "Signed-By: /usr/share/keyrings/debian-archive-keyring.gpg" \
	  > /etc/apt/sources.list.d/debian.sources; \
	apt-get -o Acquire::Check-Valid-Until=false -o Acquire::Retries=3 -o Acquire::http::Timeout=30 -o Acquire::https::Timeout=30 update -q; \
	apt-get -o Acquire::Retries=3 -o Acquire::http::Timeout=30 -o Acquire::https::Timeout=30 install -qy --no-install-recommends \
	  qemu-system-arm vhost-device-vsock e2fsprogs bash-static; \
	rm -rf /var/lib/apt/lists/*
COPY boot /usr/local/bin/guest-boot
ENTRYPOINT ["/usr/local/bin/guest-boot"]
