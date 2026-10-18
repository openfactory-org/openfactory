#!/bin/sh
# Print the client settings of the local stack, for: eval "$(make -s local-env)"
# It prints only export lines. The file holds the API key: do not print it to a log.
set -eu
LOCAL_DIR=${LOCAL_DIR:-$HOME/.openfactory-local}
if [ ! -f "$LOCAL_DIR/env" ]; then
	echo "local-env: $LOCAL_DIR/env does not exist. Run: make local-up" >&2
	exit 1
fi
grep '^export ' "$LOCAL_DIR/env"
