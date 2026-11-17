#!/usr/bin/env bash
# Scoped gate: run `make check` only for what a change can affect.
#
#   scripts/check-changed.sh [base]        base defaults to $CHECK_BASE or origin/next
#   CHECK_DRY=1 scripts/check-changed.sh   print the plan, run nothing
#
# A change set is the diff against the merge-base with <base>, plus untracked
# files. Each changed file maps to the nearest enclosing Go package. Source and
# embedded data propagate to every package that imports it, in any module
# (resolved through the module replaces, as the gate does); test files, docs
# and testdata only rerun the tests of that package. Then, per module:
#   build        modules holding an affected package, so dependents without
#                tests still compile
#   vet, test    affected packages only
#   lint, fmt    changed packages and files only
#   tidy         modules with changed Go files
#   gen          modules with sqlc output, when migrations, queries or
#                generated code changed
# Anything that changes how the gate itself runs (Makefile, go.mod, go.sum,
# go.work, toolchain or lint config, this script) falls back to `make check`.
set -euo pipefail

base="${1:-${CHECK_BASE:-origin/next}}"
root="$(git rev-parse --show-toplevel)"
cd "$root"
export GOWORK=off

mb="$(git merge-base "$base" HEAD)"
tmp="$(mktemp -d)"
trap '[ -n "${CHECK_KEEP:-}" ] || rm -rf "$tmp"' EXIT

{ git diff --name-only "$mb"; git ls-files -o --exclude-standard; } | LC_ALL=C sort -u >"$tmp/changed"
if [ ! -s "$tmp/changed" ]; then
	echo "check-changed: no changes against $base"
	exit 0
fi

full() {
	echo "check-changed: $1; running the full gate"
	[ -n "${CHECK_DRY:-}" ] && exit 0
	exec make check
}

inputs='(^|/)(go\.mod|go\.sum|go\.work|go\.work\.sum)$|^Makefile$|^mise\.toml$|^\.config/|^\.golangci|^\.github/|^scripts/check-changed\.sh$'
if grep -qE "$inputs" "$tmp/changed"; then
	grep -E "$inputs" "$tmp/changed" | head -3 | sed 's/^/  /'
	full "gate inputs changed"
fi

# Modules: every directory holding a go.mod (same rule as the Makefile),
# longest path first so the owner of a file is the first prefix match.
find . -name go.mod -not -path './.git/*' -not -path '*/testdata/*' | sed 's|/go.mod$||;s|^\./||' |
	awk '{ print length($0) "\t" $0 }' | LC_ALL=C sort -rn | cut -f2 >"$tmp/modules"

modpath() { (cd "$1" && go list -m); }

# owner <file>: module dir holding the file.
owner() {
	local m
	while IFS= read -r m; do
		if [ "$m" = . ]; then echo .; return; fi
		case "$1" in "$m"/*) echo "$m"; return ;; esac
	done <"$tmp/modules"
}

# pkgdir <file> <module>: nearest directory at or above the file that holds a
# Go file, stopping at the module root.
pkgdir() {
	local d
	d="$(dirname "$1")"
	while :; do
		if ls "$d"/*.go >/dev/null 2>&1 || [ "$d" = "$2" ]; then echo "$d"; return; fi
		d="$(dirname "$d")"
	done
}

: >"$tmp/changed_dirs" # module <TAB> dir <TAB> prop|own
: >"$tmp/changed_go"   # module <TAB> file
while IFS= read -r f; do
	m="$(owner "$f")"
	[ -n "$m" ] || continue
	d="$(pkgdir "$f" "$m")"
	case "$f" in
	*_test.go | *.md | docs/* | */docs/* | testdata/* | */testdata/*) class=own ;;
	*) class=prop ;;
	esac
	printf '%s\t%s\t%s\n' "$m" "$d" "$class" >>"$tmp/changed_dirs"
	case "$f" in *.go) [ -e "$f" ] && printf '%s\t%s\n' "$m" "$f" >>"$tmp/changed_go" ;; esac
done <"$tmp/changed"
sort -u -o "$tmp/changed_dirs" "$tmp/changed_dirs"

# Changed import paths, tagged prop or own.
while IFS="$(printf '\t')" read -r m d class; do
	mp="$(modpath "$m")"
	if [ "$d" = "$m" ]; then p="$mp"; elif [ "$m" = . ]; then p="$mp/$d"; else p="$mp/${d#"$m"/}"; fi
	printf '%s\t%s\n' "$class" "$p"
done <"$tmp/changed_dirs" | sort -u >"$tmp/changed_pkgs"

# Affected packages, per module, from the import graph of that module.
: >"$tmp/plan"
while IFS= read -r m; do
	if ! (cd "$m" && go list -test -f '{{.ImportPath}}|{{.ForTest}}|{{join .Deps " "}}' ./... >"$tmp/graph" 2>"$tmp/graph.err"); then
		head -5 "$tmp/graph.err" | sed 's/^/  /'
		full "go list failed in $m"
	fi
	awk -F'|' -v mod="$m" -v changed="$tmp/changed_pkgs" '
		BEGIN {
			while ((getline l < changed) > 0) {
				split(l, a, "\t")
				if (a[1] == "prop") prop[a[2]] = 1; else own[a[2]] = 1
			}
		}
		$1 ~ / \[/ { next }                   # test variants; the .test line carries the deps
		{
			istest = ($1 ~ /\.test$/)
			pkg = istest ? substr($1, 1, length($1) - 5) : $1
			hit = (pkg in prop) || (istest && (pkg in own))
			n = split($3, dep, " ")
			for (i = 1; !hit && i <= n; i++) if (dep[i] in prop) hit = 1
			if (hit) print mod "\t" (istest ? "test" : "pkg") "\t" pkg
		}' "$tmp/graph" >>"$tmp/plan"
done <"$tmp/modules"
sort -u -o "$tmp/plan" "$tmp/plan"

mods_with_work="$(cut -f1 "$tmp/plan" | sort -u)"
nwork="$(printf '%s\n' "$mods_with_work" | grep -c . || true)"
ntotal="$(wc -l <"$tmp/modules" | tr -d ' ')"
echo "check-changed: $(wc -l <"$tmp/changed" | tr -d ' ') files against $base ($(git rev-parse --short "$mb"))"
echo "  changed packages: $(wc -l <"$tmp/changed_pkgs" | tr -d ' ') ($(grep -c '^own' "$tmp/changed_pkgs" || true) test-only)"
echo "  affected: $(awk -F'\t' '$2=="test"' "$tmp/plan" | wc -l | tr -d ' ') test packages in $nwork of $ntotal modules"
if [ -n "${CHECK_DRY:-}" ]; then
	awk -F'\t' '$2=="test" { print "  " $1 "\t" $3 }' "$tmp/plan"
	exit 0
fi

for m in $mods_with_work; do
	pkgs="$(awk -F'\t' -v m="$m" '$1==m { print $3 }' "$tmp/plan" | sort -u)"
	tests="$(awk -F'\t' -v m="$m" '$1==m && $2=="test" { print $3 }' "$tmp/plan" | sort -u)"
	echo "== $m"
	(
		cd "$m"
		# shellcheck disable=SC2086
		go build -buildvcs=false ./... &&
			go vet $pkgs &&
			{ [ -z "$tests" ] || go test -buildvcs=false ${TESTFLAGS:-} $tests; }
	) || exit 1
done

# Lint, format and tidy: changed packages and files only.
for m in $(cut -f1 "$tmp/changed_go" | sort -u); do
	echo "== lint $m"
	dirs="$(awk -F'\t' -v m="$m" '$1==m { print $2 }' "$tmp/changed_dirs" | sort -u | while IFS= read -r d; do
		ls "$d"/*.go >/dev/null 2>&1 || continue
		if [ "$d" = "$m" ]; then echo .; else echo "./${d#"$m"/}"; fi
	done)"
	files="$(awk -F'\t' -v m="$m" '$1==m { print $2 }' "$tmp/changed_go" | while IFS= read -r f; do
		if [ "$m" = . ]; then echo "$f"; else echo "${f#"$m"/}"; fi
	done)"
	(
		cd "$m"
		if [ -n "$dirs" ]; then
			# shellcheck disable=SC2086
			golangci-lint run $dirs || exit 1
			golangci-lint fmt --diff $files || exit 1
		fi
		go mod tidy -diff || { echo "go mod tidy needed in $m"; exit 1; }
	) || exit 1
done

# Generated query code, when its inputs or output changed.
for m in $(find . -name sqlc.yaml -not -path './.git/*' -not -path '*/testdata/*' | sed 's|/sqlc.yaml$||;s|^\./||' | sort); do
	prefix="$m/"
	[ "$m" = . ] && prefix=""
	if grep -E "^$prefix" "$tmp/changed" | grep -qE '(^|/)(migrations|queries|db)/|sqlc\.yaml$'; then
		echo "== gen $m"
		(cd "$m" && sqlc diff) || { echo "generated code stale in $m; run make gen"; exit 1; }
	fi
done

echo "check-changed: ok"
