# The vm isolation's guest kernel: `make vm-kernel` (docker, on the host
# or in CI) writes kernel-<arch> (the uncompressed Image, which
# Virtualization.framework boots) and kernel-<arch>.config (the resolved
# config, shipped beside it for the GPL's sake) from kernel.org's
# unpatched source, pinned by version and sha256 in the Makefile.
#
# It builds on the build machine's own architecture and cross-compiles
# when that is not the guest's (an amd64 runner), so no emulation is
# involved. The config is allnoconfig plus config-common and
# config-<arch>, and check-config fails the build when Kconfig did not
# take a line of them.
#
# Reproducible: the Debian image is pinned by digest and its packages by
# snapshot.debian.org's date, and the build's time, user, host and number
# are fixed (the time is the tarball's), so two builds on the same kind of
# machine give the same Image byte for byte. Only the same kind: an amd64
# machine's compiler is Debian's gcc-14-cross, an arm64 one's is gcc-14
# itself, and the two differ in a few functions' code (GCC's objsz pass,
# behind FORTIFY_SOURCE's __builtin_dynamic_object_size, numbers its SSA
# names differently on each), so the Image is reproducible per build
# machine's architecture, not across them.
ARG DEBIAN=debian:trixie-20260918-slim@sha256:a99cfc517144bc59b1978475ec53b46ecabec7e43635402ee5b77cc54cd1b20a

FROM --platform=$BUILDPLATFORM ${DEBIAN} AS build
ARG TARGETARCH
ARG DEBIAN_SNAPSHOT=20260918T000000Z
ARG LINUX_VERSION
ARG LINUX_SHA256
RUN [ -n "$LINUX_VERSION" ] && [ -n "$LINUX_SHA256" ] || \
	{ echo "LINUX_VERSION and LINUX_SHA256 are the Makefile's: run make vm-kernel" >&2; exit 1; }
# The kernel's name for the architecture, the toolchain's prefix and the
# image it boots. Debian packages the compiler under that prefix on every
# architecture (the native one included), so the build is the same
# command everywhere; gcc alone is the host's, for the build's tools.
RUN case "$TARGETARCH" in \
	arm64) echo "arm64 aarch64-linux-gnu- arch/arm64/boot/Image" ;; \
	*) echo "no guest kernel for $TARGETARCH yet: vm/kernel has no config-$TARGETARCH" >&2; exit 1 ;; \
	esac > /target
# Packages at the snapshot's date, not the mirror's today: the compiler is
# part of what the Image is. apt checks the archive's signatures, so http
# is enough, and needs no certificates yet. The guest's libc is there so
# the compiler can link for it on every build machine, as the native one
# always can: Kconfig records that (CC_CAN_LINK), and the .config would
# differ between them otherwise.
RUN set -e; read -r _ cross _ < /target; \
	printf '%s\n' "Types: deb" \
	  "URIs: http://snapshot.debian.org/archive/debian/$DEBIAN_SNAPSHOT/" \
	  "Suites: trixie trixie-updates" "Components: main" \
	  "Signed-By: /usr/share/keyrings/debian-archive-keyring.gpg" "" \
	  "Types: deb" \
	  "URIs: http://snapshot.debian.org/archive/debian-security/$DEBIAN_SNAPSHOT/" \
	  "Suites: trixie-security" "Components: main" \
	  "Signed-By: /usr/share/keyrings/debian-archive-keyring.gpg" \
	  > /etc/apt/sources.list.d/debian.sources; \
	apt-get -o Acquire::Check-Valid-Until=false -o Acquire::Retries=3 -o Acquire::http::Timeout=30 -o Acquire::https::Timeout=30 update -q; \
	apt-get -o Acquire::Retries=3 -o Acquire::http::Timeout=30 -o Acquire::https::Timeout=30 install -qy --no-install-recommends \
	  ca-certificates curl xz-utils make gcc libc6-dev flex bison bc \
	  "gcc-${cross%-}" "binutils-${cross%-}" "libc6-dev-$TARGETARCH-cross"; \
	rm -rf /var/lib/apt/lists/*

WORKDIR /build
RUN set -e; \
	url="https://cdn.kernel.org/pub/linux/kernel/v${LINUX_VERSION%%.*}.x/linux-$LINUX_VERSION.tar.xz"; \
	curl -fsSL --connect-timeout 15 --max-time 900 --retry 3 --retry-delay 2 --retry-connrefused \
	  -o linux.tar.xz "$url" \
	  || { echo "could not download $url: check the network from the build (a VPN route? a proxy?)" >&2; exit 1; }; \
	echo "$LINUX_SHA256  linux.tar.xz" | sha256sum -c -; \
	tar -xJf linux.tar.xz; rm linux.tar.xz

COPY config-* check-config /build/fragments/
# The build's time is the source's: every file in kernel.org's tarball
# carries its release commit's time.
RUN set -e; read -r arch cross image < /target; \
	cd "linux-$LINUX_VERSION"; \
	epoch=$(stat -c %Y Makefile); \
	export ARCH="$arch" CROSS_COMPILE="$cross" LC_ALL=C TZ=UTC \
	  SOURCE_DATE_EPOCH="$epoch" KBUILD_BUILD_TIMESTAMP="$(date -u -d "@$epoch")" \
	  KBUILD_BUILD_USER=caboose KBUILD_BUILD_HOST=caboose KBUILD_BUILD_VERSION=1; \
	scripts/kconfig/merge_config.sh -n /build/fragments/config-common "/build/fragments/config-$TARGETARCH"; \
	sh /build/fragments/check-config .config /build/fragments/config-common "/build/fragments/config-$TARGETARCH"; \
	make -s -j"$(nproc)" "$(basename "$image")"; \
	mkdir /out; cp "$image" /out/kernel; cp .config /out/kernel.config; \
	echo "built Linux $(make -s kernelrelease)"

FROM scratch
COPY --from=build /out/ /
