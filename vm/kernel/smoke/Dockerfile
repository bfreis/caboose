# QEMU for make vm-kernel-smoke, which runs this image with the kernel and
# the initramfs holding the smoke test's init mounted read-only at /smoke:
# `run` boots them under emulation (TCG, so any machine with docker can)
# and prints the guest's console.
# The same Debian as vm/kernel's build, pinned the same way.
ARG DEBIAN=debian:trixie-20260918-slim@sha256:a99cfc517144bc59b1978475ec53b46ecabec7e43635402ee5b77cc54cd1b20a

FROM ${DEBIAN}
ARG DEBIAN_SNAPSHOT=20260918T000000Z
RUN set -e; \
	printf '%s\n' "Types: deb" \
	  "URIs: http://snapshot.debian.org/archive/debian/$DEBIAN_SNAPSHOT/" \
	  "Suites: trixie trixie-updates" "Components: main" \
	  "Signed-By: /usr/share/keyrings/debian-archive-keyring.gpg" \
	  > /etc/apt/sources.list.d/debian.sources; \
	apt-get -o Acquire::Check-Valid-Until=false -o Acquire::Retries=3 -o Acquire::http::Timeout=30 -o Acquire::https::Timeout=30 update -q; \
	apt-get -o Acquire::Retries=3 -o Acquire::http::Timeout=30 -o Acquire::https::Timeout=30 install -qy --no-install-recommends qemu-system-arm; \
	rm -rf /var/lib/apt/lists/*
COPY run /usr/local/bin/kernel-smoke
ENTRYPOINT ["/usr/local/bin/kernel-smoke"]
