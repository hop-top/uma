#!/usr/bin/env bash
# Build, vet and test the modules whose declared Go floor the given toolchain
# satisfies, on that toolchain and no other.
#
#   scripts/min-toolchain.sh [version]     version defaults to 1.26.0
#   TESTFLAGS="-race" scripts/min-toolchain.sh
#
# A module's floor is the `go` directive in its go.mod. The repository
# floor differs per module: the library and the modules that depend on little
# else declare go 1.26.0, the ones that depend on hop.top/xrr declare go
# 1.26.1, contrib/kit declares go 1.27.3 (the floor of hop.top/kit). A module above <version> is listed and skipped; the
# pinned toolchain in mise.toml covers it. Run from a
# toolchain at <version>: GOTOOLCHAIN=local stops go from fetching a newer one
# behind the job's back, which would test nothing about the floor.
set -euo pipefail

want="${1:-1.26.0}"
cd "$(git rev-parse --show-toplevel)"

export GOWORK=off GOTOOLCHAIN=local

have="$(go env GOVERSION)"
[ "$have" = "go$want" ] || {
	echo "min-toolchain: running $have, want go$want" >&2
	exit 1
}

# norm <v>: 1.26 -> 1.26.0, so versions compare as three fields.
norm() { awk -F. '{ printf "%d.%d.%d\n", $1, $2, ($3 == "" ? 0 : $3) }' <<<"$1"; }

# within <floor> <want>: true when floor <= want.
within() { [ "$(printf '%s\n%s\n' "$(norm "$1")" "$(norm "$2")" | sort -V | head -n1)" = "$(norm "$1")" ]; }

ran=0
while IFS= read -r gomod; do
	dir="$(dirname "$gomod")"
	floor="$(cd "$dir" && go mod edit -json | sed -n 's|.*"Go": "\(.*\)".*|\1|p')"
	if ! within "$floor" "$want"; then
		echo "== $dir: declares go $floor, above $want; skipped"
		continue
	fi
	echo "== $dir (go $floor)"
	# shellcheck disable=SC2086 # TESTFLAGS is a word list
	(cd "$dir" && go build -buildvcs=false ./... && go vet ./... && go test -buildvcs=false ${TESTFLAGS:-} ./...)
	ran=$((ran + 1))
done < <(find . -name go.mod -not -path './.git/*' -not -path '*/testdata/*' | LC_ALL=C sort)

[ "$ran" -gt 0 ] || {
	echo "min-toolchain: no module declares go $want or below" >&2
	exit 1
}
echo "min-toolchain: $ran module(s) pass on go$want"
