caboose-agent, cross-compiled for linux/amd64 and linux/arm64 by `make agent`
(and goreleaser's before hook), lands here before the launcher is built: the
launcher embeds both (embed.go), and the layer COPYs the one for the image's
architecture. The binaries are build output, gitignored; this file keeps the
directory, and the embed pattern, from ever being empty.
