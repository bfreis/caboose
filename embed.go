// Package caboose exists only to embed the files the launcher carries with it.
//
// //go:embed cannot reach a parent directory, and these files live at the
// repo root where the Dockerfile, the integration suite and the docs expect
// them. So the embedding happens here, in a root-level package, and
// internal/assets is the one place that reads it.
package caboose

import "embed"

// Files holds the images' build inputs -- the Dockerfile a dockerfile image
// profile's dir is seeded with, and the layer built on every base
// (layer.Dockerfile and the files it COPYs) --
// the sandbox-wide CLAUDE.md, and the image probe that caboose check-image runs
// (imagecheck.sh, which is not part of any image: internal/assets'
// BaseContext and LayerContext decide what is).
//
// agent-bin holds caboose-agent for each architecture, built by `make
// agent` before the launcher (its README says so when they are missing).
//
// Only what is listed here can ever reach a `docker build` run by
// `caboose build`, which keeps the allowlist property .dockerignore gave
// the checkout-based build: nothing unlisted can drift into the context.
//
//go:embed Dockerfile layer.Dockerfile layer-user.sh entrypoint.sh tmux.conf shellrc.bash sandbox/CLAUDE.md imagecheck.sh agent-bin
var Files embed.FS
