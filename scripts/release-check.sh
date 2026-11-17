#!/usr/bin/env bash
# Release wiring gate for independent module versions: every module releases
# on its own, and consumers must be able to resolve every in-repo require.
#
#   scripts/release-check.sh     (run from the repo root; `make release-check`)
#
# Fails when
#   - a module on disk is neither a release-please package nor listed in
#     RELEASE_EXCLUDE (modules deliberately never published), or a package
#     has no go.mod on disk
#   - an in-repo require (hop.top/uma...) in a published module
#       * points at an unpublished module (RELEASE_EXCLUDE) or a module that
#         is not a package, or
#       * points at a package with no released version in the manifest, or
#       * names a version newer than that module's manifest version, or
#       * names a version that is not an existing tag of that module
#   - any in-repo require still carries a `// x-release-please-version`
#     marker (versions are independent; nothing rewrites requires on release)
#
# "Released version" of a module: an existing git tag (`v<ver>` for the root,
# `<module path>/v<ver>` for a submodule). Without git, or in a checkout with
# no tags (shallow CI clone), the tag test is skipped and only the manifest
# bound applies: the require must be at most the manifest version.
#
# An unpublished module (RELEASE_EXCLUDE) may require in-repo modules at any
# version: its replace directives pin them and consumers never resolve it.
set -euo pipefail

cd "$(git rev-parse --show-toplevel 2>/dev/null || echo "$(dirname "$0")/..")"
cfg=.github/release-please-config.json
man=.github/.release-please-manifest.json

RELEASE_EXCLUDE="examples examples/fakeprovider examples/imessagegateway examples/inbox examples/triage"

mods="$(find . -name go.mod -not -path './.git/*' -not -path '*/testdata/*' |
	sed -e 's|^\./||' -e 's|/\{0,1\}go\.mod$||' -e 's|^$|.|' | LC_ALL=C sort)"

# packages: one path per line.
packages="$(awk '/^  "packages": \{/{f=1;next} f&&/^  \}/{exit} f&&/^    "[^"]+": \{/{gsub(/[":{ ]/,"");print}' "$cfg" | LC_ALL=C sort)"

# manifest_version <package path>: version without the leading v, or empty.
manifest_version() {
	sed -n 's|.*"'"$1"'": *"\([^"]*\)".*|\1|p' "$man" | head -1
}

# Tags, when the checkout has any.
tags=""
if command -v git >/dev/null 2>&1 && git rev-parse --git-dir >/dev/null 2>&1; then
	tags="$(git tag 2>/dev/null || true)"
fi

fail=0
bad() { echo "release-check: $*" >&2; fail=1; }
# here-string, not a pipe: grep -q exits early and pipefail would flag printf.
has() { grep -qxF -- "$2" <<<"$1"; }
excluded() { has "$(printf '%s\n' $RELEASE_EXCLUDE)" "$1"; }

# semver_cmp A B: prints -1, 0 or 1 comparing semver strings (no leading v).
# Numeric core first; a release outranks any prerelease of the same core;
# prerelease identifiers compare dot-wise, numeric ones numerically.
semver_cmp() {
	awk -v a="$1" -v b="$2" '
		function split3(v, o,   core, i, n, p, parts) {
			sub(/\+.*/, "", v)
			p = index(v, "-"); o["pre"] = ""
			if (p) { o["pre"] = substr(v, p+1); core = substr(v, 1, p-1) } else core = v
			n = split(core, parts, ".")
			for (i = 1; i <= 3; i++) o[i] = (i <= n ? parts[i] + 0 : 0)
		}
		function idcmp(x, y) {
			if (x ~ /^[0-9]+$/ && y ~ /^[0-9]+$/) return (x+0 < y+0) ? -1 : (x+0 > y+0)
			if (x ~ /^[0-9]+$/) return -1
			if (y ~ /^[0-9]+$/) return 1
			return (x < y) ? -1 : (x > y)
		}
		BEGIN {
			split3(a, A); split3(b, B)
			for (i = 1; i <= 3; i++) if (A[i] != B[i]) { print (A[i] < B[i]) ? -1 : 1; exit }
			if (A["pre"] == B["pre"]) { print 0; exit }
			if (A["pre"] == "") { print 1; exit }
			if (B["pre"] == "") { print -1; exit }
			na = split(A["pre"], x, "."); nb = split(B["pre"], y, ".")
			for (i = 1; i <= na && i <= nb; i++) { c = idcmp(x[i], y[i]); if (c) { print c; exit } }
			print (na < nb) ? -1 : (na > nb)
		}'
}

# tag_for <package path> <version without v>
tag_for() {
	if [ "$1" = . ]; then echo "v$2"; else echo "$1/v$2"; fi
}

for m in $mods; do
	if has "$packages" "$m"; then
		if excluded "$m"; then bad "$m is both a package and in RELEASE_EXCLUDE"; fi
	elif ! excluded "$m"; then
		bad "$m: module is not a release-please package nor in RELEASE_EXCLUDE"
	fi
done
for p in $packages; do has "$mods" "$p" || bad "$p: package has no go.mod on disk"; done
for x in $RELEASE_EXCLUDE; do has "$mods" "$x" || bad "$x: RELEASE_EXCLUDE entry has no go.mod on disk"; done

for m in $mods; do
	f="$m/go.mod"; published=1
	excluded "$m" && published=0
	while read -r mod ver marker; do
		[ -n "$mod" ] || continue
		if [ -n "$marker" ]; then
			bad "$f: require $mod $ver carries // x-release-please-version (versions are independent; drop the marker)"
		fi
		[ "$published" = 1 ] || continue
		target="${mod#hop.top/uma}"; target="${target#/}"; [ -n "$target" ] || target=.
		if excluded "$target"; then
			bad "$f: published module requires unpublished $mod"
			continue
		fi
		if ! has "$packages" "$target"; then
			bad "$f: require $mod is not a release-please package"
			continue
		fi
		cur="$(manifest_version "$target")"
		if [ -z "$cur" ]; then
			bad "$f: require $mod $ver but $target has no released version in $man"
			continue
		fi
		v="${ver#v}"
		if [ "$(semver_cmp "$v" "$cur")" = 1 ]; then
			bad "$f: require $mod $ver is newer than its released version v$cur"
			continue
		fi
		if [ -n "$tags" ]; then
			has "$tags" "$(tag_for "$target" "$v")" ||
				bad "$f: require $mod $ver is not a released version (no tag $(tag_for "$target" "$v"))"
		fi
	done < <(awk '/=>/{next} /^[[:space:]]*(require[[:space:]]+)?hop\.top\/uma(\/[^[:space:]]*)?[[:space:]]+v/ {
		i=1; if ($1=="require") i=2
		print $i, $(i+1), ($0 ~ /\/\/ x-release-please-version/ ? "m" : "")}' "$f")
done

[ "$fail" = 0 ] || exit 1
src="manifest bound"; [ -z "$tags" ] || src="tags and manifest bound"
echo "release-check: ok (independent versions; $src)"
