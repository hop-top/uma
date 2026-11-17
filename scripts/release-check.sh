#!/usr/bin/env bash
# Release wiring gate: what release-please needs so consumers can resolve the
# tags it cuts.
#
#   scripts/release-check.sh     (run from the repo root; `make release-check`)
#
# Fails when
#   - a module on disk is neither a release-please package nor listed in
#     RELEASE_EXCLUDE (modules deliberately never published), or a package
#     has no module, or a package is missing from the linked-versions group
#     or lacks its own go.mod in extra-files
#   - an in-repo require (hop.top/uma...) in a published module lacks the
#     `// x-release-please-version` marker, differs from the linked version,
#     or points at an unpublished module
#   - a marked go.mod is not updated by any package's extra-files
# An unpublished module (RELEASE_EXCLUDE) may leave its in-repo requires
# unmarked (pinned by replace, never resolved by consumers); a marker there
# must still carry the linked version.
set -euo pipefail

cd "$(git rev-parse --show-toplevel)"
cfg=.github/release-please-config.json
man=.github/.release-please-manifest.json

RELEASE_EXCLUDE="examples examples/fakeprovider examples/imessagegateway examples/inbox examples/triage"

mods="$(find . -name go.mod -not -path './.git/*' -not -path '*/testdata/*' |
	sed -e 's|^\./||' -e 's|/\{0,1\}go\.mod$||' -e 's|^$|.|' | LC_ALL=C sort)"

# Linked version: the manifest's root entry once released, else the root
# package's initial-version.
linked="$(sed -n 's|.*"\.": *"\([^"]*\)".*|\1|p' "$man" | head -1)"
if [ -z "$linked" ]; then
	linked="$(awk '/^    "\.": \{/{f=1} f&&/"initial-version"/{gsub(/[",]/,"",$2); print $2; exit}' "$cfg")"
fi
[ -n "$linked" ] || { echo "release-check: no linked version in $man or $cfg" >&2; exit 1; }

# packages: one path per line. extras: "<package> <extra-file path>" lines.
packages="$(awk '/^  "packages": \{/{f=1;next} f&&/^  \}/{exit} f&&/^    "[^"]+": \{/{gsub(/[":{ ]/,"");print}' "$cfg" | LC_ALL=C sort)"
extras="$(awk '/^  "packages": \{/{f=1;next} f&&/^  \}/{exit}
	f&&/^    "[^"]+": \{/{p=$1;gsub(/[":]/,"",p)}
	f&&/"path":/{v=$2;gsub(/[",]/,"",v);print p" "v}' "$cfg")"
# linked-versions components: the root package's component is "uma".
comps="$(awk '/"components": \[/{f=1;next} f&&/\]/{exit} f{gsub(/[", ]/,"");print}' "$cfg")"

fail=0
bad() { echo "release-check: $*" >&2; fail=1; }
# here-string, not a pipe: grep -q exits early and pipefail would flag printf.
has() { grep -qxF -- "$2" <<<"$1"; }

for m in $mods; do
	if has "$packages" "$m"; then
		if has "$(printf '%s\n' $RELEASE_EXCLUDE)" "$m"; then bad "$m is both a package and in RELEASE_EXCLUDE"; fi
		c="$m"; [ "$m" = . ] && c=uma
		has "$comps" "$c" || bad "$m: package missing from the linked-versions components"
		has "$extras" "$m go.mod" || bad "$m: go.mod missing from the package extra-files"
	elif ! has "$(printf '%s\n' $RELEASE_EXCLUDE)" "$m"; then
		bad "$m: module is not a release-please package nor in RELEASE_EXCLUDE"
	fi
done
for p in $packages; do has "$mods" "$p" || bad "$p: package has no go.mod on disk"; done
for x in $RELEASE_EXCLUDE; do has "$mods" "$x" || bad "$x: RELEASE_EXCLUDE entry has no go.mod on disk"; done

for m in $mods; do
	f="$m/go.mod"; published=1
	has "$(printf '%s\n' $RELEASE_EXCLUDE)" "$m" && published=0
	# owner package: longest package path that prefixes the module dir.
	owner=""
	for p in $packages; do
		if [ "$p" = . ] || [ "$m" = "$p" ] || [ "${m#"$p"/}" != "$m" ]; then
			[ ${#p} -gt ${#owner} ] && owner="$p"
		fi
	done
	while read -r mod ver marker; do
		[ -n "$mod" ] || continue
		target="${mod#hop.top/uma}"; target="${target#/}"; [ -n "$target" ] || target=.
		if [ "$published" = 1 ] && has "$(printf '%s\n' $RELEASE_EXCLUDE)" "$target"; then
			bad "$f: published module requires unpublished $mod"
			continue
		fi
		if [ -z "$marker" ]; then
			[ "$published" = 0 ] || bad "$f: require $mod $ver lacks // x-release-please-version"
			continue
		fi
		[ "$ver" = "v$linked" ] || bad "$f: require $mod $ver, linked version is v$linked"
		rel="${f#"$owner"/}"; [ "$owner" = . ] && rel="$f"
		has "$extras" "$owner $rel" || bad "$f: not in extra-files of package $owner"
	done < <(awk '/=>/{next} /^[[:space:]]*(require[[:space:]]+)?hop\.top\/uma(\/[^[:space:]]*)?[[:space:]]+v/ {
		i=1; if ($1=="require") i=2
		print $i, $(i+1), ($0 ~ /\/\/ x-release-please-version/ ? "m" : "")}' "$f")
done

[ "$fail" = 0 ] || exit 1
echo "release-check: ok (linked version v$linked)"
