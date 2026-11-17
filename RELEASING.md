# Releasing

uma is one repository with several Go modules. Each released module is a
release-please component with its own tags, changelog and release PR.

| Module | Tag | Released |
| --- | --- | --- |
| `hop.top/uma` (root) | `vX.Y.Z[-pre]` | yes |
| `hop.top/uma/<dir>` (`adapter/messaging/twilio`, `contrib/kit`, `store/postgres`, ...) | `<dir>/vX.Y.Z[-pre]` | once it has an exported API |
| `hop.top/uma/examples` | none | never |
| `hop.top/uma/examples/fakeprovider` | none | never |
| `hop.top/uma/examples/triage` | none | never |

Go resolves a submodule version from a tag prefixed with its directory path:
`adapter/messaging/twilio/v0.1.0` is
`hop.top/uma/adapter/messaging/twilio@v0.1.0`.

Release-please (`.github/release-please-config.json`) keeps a standing release
PR per component. Merging it tags the release. Channel changes (alpha, beta,
rc, stable) go through `make promote` and `Release-As:` footers; see
`scripts/promote-release.sh`.

## Module layout rules

- Modules are grouped by role: `adapter/<category>/<provider>` (categories `mail`,
  `messaging` for contact-centric phone/app channels, `chat` for workspace and
  community rooms, `ticket`, `web`, `agent` for agent protocols; one module per
  provider), `store/<backend>`,
  `contrib/<integration>`, `testing/<helper>`. Only integrations with their
  own dependency tree get a module; standard-library-only packages under the
  same role directories (`adapter/web/widget`, `adapter/agent/a2a`,
  `adapter/messaging/relay`, `store/memory`) stay in the root module.
- Each contract package sits at the top of its directory (`adapter`, `store`);
  implementations live below it, never in a parallel plural directory.
- The root package `hop.top/uma` holds public API only: `New`, `Config` and
  its option types, the engine, client, runtime and service interfaces,
  request and result types, constants. New engine code goes in
  `internal/<concern>`, never in the root package. The concern is the
  package, not a file name prefix: `internal/runtime/webhook.go`, not
  `runtime_webhook.go`.
- Every module directory is listed in `go.work`; `make work-check` fails
  otherwise.
- `go.work` is for development. `make` targets and CI run each module with
  `GOWORK=off`, as a consumer resolves it: in workspace mode an import of
  `hop.top/uma` builds even when the module's `go.mod` lacks the require.
- `GOFLAGS=-mod=mod` is rejected in workspace mode. Use `GOWORK=off` when a
  command needs it.
- A module declares only the requirements its code uses (`make tidy-check`).

## Depending on the root module

A submodule that imports `hop.top/uma` carries both lines:

```text
require hop.top/uma v0.0.0-20261011012310-7977d38c3252
replace hop.top/uma => ../../
```

The `replace` makes builds and `go mod tidy` in this repository use the
checkout. Consumers ignore it and resolve the `require`. Until the root has a
tag, the require is a pseudo-version of a commit on `main`:

```sh
cd store/postgres
GOWORK=off go get hop.top/uma@<main-commit>
GOWORK=off go mod tidy
```

## Adding a submodule to releases

Placeholder modules have no release-please entry, so nothing tags them. The
change that gives a module its first exported API adds the entry:

```json
"store/postgres": {
  "release-type": "go",
  "component": "store/postgres",
  "include-component-in-tag": true,
  "tag-separator": "/",
  "include-v-in-tag": true,
  "initial-version": "0.1.0-alpha.0",
  "prerelease": true,
  "prerelease-type": "alpha.0",
  "versioning": "prerelease",
  "bump-minor-pre-major": true
}
```

The component is the directory path, `/` included. With `"tag-separator":
"/"` release-please tags `store/postgres/v0.1.0-alpha.0`, the prefix Go
expects, and parses the same tag back as component `store/postgres`.

With it:

- Keep top-level `"separate-pull-requests": true`. Without it release-please
  folds every component into one `chore: release main` PR, and the bump order
  below cannot be followed.
- Keep the directory in the root package's `exclude-paths`, so commits that
  only touch the submodule stay out of the root changelog. Every module
  directory is listed there already; add new ones.
- Match the other packages' `prerelease-type`:
  `scripts/promote-release.sh` requires every package on one channel.
- The shared release-please preflight must accept this Go-only shape: more
  than one package with no `.github/workflows/publish.yml`, and components
  containing `/`. `hop-top/.github` v0.14.1 and later (`@v0`) allow both
  when every package is `release-type: go`.

- Modules never published go in `RELEASE_EXCLUDE` in `scripts/release-check.sh`.
  `make release-check` (CI) fails on a module that is neither a package nor
  excluded, and on in-repo requires that consumers cannot resolve (see
  Independent versions).

A new module directory also goes into `go.work` and the gomod `directories`
of `.github/dependabot.yml`.

## Independent versions

Every module has its own version. A package releases only when a releasable
commit touches its own directory; there is no linked-versions group, so a root
change does not release the submodules and a submodule change does not release
the root. (release-please cannot link the root: it must tag plain `vX.Y.Z`,
and the group skips packages without a component in the tag.)

In-repo requires name a version that has been released:

```text
require hop.top/uma v0.1.0-alpha.0
replace hop.top/uma => ../../
```

Nothing rewrites them on release; no `// x-release-please-version` markers.
The `replace` is development wiring only. A require stays at the version the
module was last built against and moves only when the module needs newer code
(google-cloud-go style).

`make release-check` enforces: every module is a package or in
`RELEASE_EXCLUDE`; every in-repo require in a published module names an
existing tag of that module (`vX` for the root, `<dir>/vX` otherwise; skipped
when the checkout has no tags) and no version newer than that module's
manifest entry; no marker remains. Unpublished examples are exempt.

## Bump order: root before submodule

A submodule that needs newer root code moves its require to a released root
tag. Release the root first, so no submodule require points at an untagged
root:

1. Land the root change on `main`.
2. Merge the root release PR (tag `vX.Y.Z`).
3. In each submodule that needs it, `GOWORK=off go get hop.top/uma@vX.Y.Z`,
   then `GOWORK=off go mod tidy`. Commit with a releasable type scoped to the
   module, e.g. `fix(postgres): require hop.top/uma vX.Y.Z`. The commit touches
   the module's `go.mod`, so that module releases.
4. Merge the submodule release PR (tag `store/postgres/vX.Y.Z`).

A submodule that does not need the new root code keeps its older require and
does not release. The same applies to a require on another submodule
(`hop.top/uma/adapter/mail`, `hop.top/uma/testing/xrrtest`).
