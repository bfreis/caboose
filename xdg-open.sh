#!/bin/sh
# The sandbox's xdg-open: opens an http(s) URL in the host's browser.
exec /usr/local/bin/caboose-agent xdg-open "$@"
