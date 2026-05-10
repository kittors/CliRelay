#!/usr/bin/env sh
set -eu

bad_paths=""
for path in assets manage.html management.html panel-meta.json dist panel-dist.zip; do
	if [ -e "$path" ]; then
		bad_paths="${bad_paths}
${path}"
	fi
done

if [ -n "$bad_paths" ]; then
	echo "Frontend panel build output must not be committed to the backend repository."
	echo "Build and deploy panel assets from kittors/codeProxy instead."
	echo "Unexpected path(s):${bad_paths}"
	exit 1
fi
