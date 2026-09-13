# obsync

Mirrors Canvas LMS course files into an Obsidian vault, read-only, with
per-device exclusion rules.

This is the **consumer** half of obsync. A Go worker (a Kubernetes CronJob)
pulls files out of Canvas into an object store and publishes a manifest; this
plugin mirrors that store into your vault. The plugin never talks to Canvas.

See `DESIGN.md` for the whole system.

## What it does

- Reads `manifests/<course>/latest`, then the manifest it points at.
- Applies **your** exclusion rules locally to decide what this device wants.
- Downloads blobs by content hash, verifying SHA-256 **before** anything is
  moved into place.
- Large files are pulled as Range requests and appended chunk by chunk, so peak
  memory is one chunk rather than one file. This is what makes mobile work.
- Files Canvas has removed go to a trash folder. Nothing is ever hard deleted.
- Files you have edited locally go to a conflict folder. A one-way mirror is not
  a licence to destroy your data.

## The panel

Everything happens in the sidebar panel. Click the cloud icon in the left
ribbon (or run **Open panel**) and it opens on the right:

- **Status** and the two global actions, Pull now and Refresh.
- **One section per course**: how much is pending, and a checklist of exactly
  what will be written. Untick anything you do not want and press Pull
  selected. Nothing is downloaded until you do.
- **Not included**, per course: everything Canvas has that was withheld, with
  the reason. A file you can see in Canvas that silently does not appear in
  your vault is the worst possible outcome, so nothing is hidden from you.
- **Conflicts**: files you edited that the mirror refused to overwrite. Open
  one to inspect it, or Discard your copy to accept the mirrored version.
- **Setup**: the whole configuration form, inline.

The settings tab renders that same form (via `renderSettings`), so the two
surfaces cannot drift apart. Use whichever you prefer.

## Setup

1. Set **Store URL** to the read-only endpoint for your obsync bucket. For
   local development, run `obsync serve` in the repo root and use
   `http://127.0.0.1:8765`; leave **Bucket** as `obsync` or empty, it accepts
   both.
2. Set **Course IDs** to a comma-separated list of numeric Canvas course IDs.
   The store is keyed by ID, not course code: `obsync ls` or
   `obsync-worker courses` lists them.
3. Pick a **Target folder**. Keep it to itself; see the warning below.

The panel starts fetching as soon as those are filled in.

### Credentials

There are none, by design. Plugin settings persist to
`.obsidian/plugins/obsync/data.json` **inside the vault**, which is synced by
whatever else syncs the vault — an S3 secret there travels everywhere the vault
does. Since phase 1 consumers are read-only, expose the bucket behind Tailscale
or an auth proxy and do plain GETs.

If you must sign requests directly, use `aws4fetch` with a **read-only** Garage
key, and verify SubtleCrypto is actually available in Obsidian's mobile origin
first. See the comments at the top of `src/store.ts`.

## Exclusion rules

Rules are applied to the manifest locally, so changing them **never needs a
refetch** — that is why they can be as aggressive as you like. The worker's own
rules are deliberately boring hard caps; everything opinionated belongs here.

```json
{
  "version": 1,
  "default": "include",
  "rules": [
    { "name": "no-huge",  "priority": 10, "action": "skip",
      "match": { "min_size": 52428800 } },
    { "name": "no-video", "priority": 20, "action": "skip",
      "match": { "ext": ["mp4", "mov"] } },
    { "name": "slides",   "priority": 40, "action": "include",
      "match": { "ext": ["pdf"], "glob": ["*/lectures/*"] } }
  ]
}
```

Highest priority wins; ties break by document order, so the file reads top to
bottom. Every clause inside one `match` must hold. Globs follow Go's
`path.Match`: `*` does not cross `/`, `[a-z]` classes work, `^` negates.

A rule with an empty `match` is rejected — that is what `default` is for.

The panel's **"not included"** section lists everything withheld and why, so a
rule can never quietly cost you a file without saying so. Files tagged
**pending** were left for later by a scoped manual pull on the worker
(`obsync-worker pull -path ...`) and arrive with its next full pull; there is
nothing to change on your side.

## A warning about search

Dropping hundreds of PDFs into a vault triggers an Obsidian reindex. Keep the
mirror in its own top-level folder and add that folder to **Settings → Files and
links → Excluded files** if search gets noisy.

## Sync triggers

On load (after a delay), on an interval, and manually from the panel. **Never on vault file
change** — a mirror that reacts to your own edits is a feedback loop.

## Development

```sh
npm ci
npm run dev     # watch build into main.js
npm test        # vitest, including the contract fixtures in ../schema
npm run build   # tsc --noEmit, then a production bundle
npm run lint
```

`policy`, `preview`, and `types` are pure and test with no Obsidian mock at all;
that is why they are kept apart from anything touching `Vault`. `sync` is tested
against an in-memory `DataAdapter` in `test/obsidian.ts`.

The plugin and the Go worker ship as a pair, and the contract between them lives
in the repository root's `schema/`, one copy read by both test suites:

- `policy-golden.json`: decisions both rule engines must reach, and policies both
  validators must reject. Hand-written.
- `manifest-golden.json` and `store-contract.json`: a manifest covering every
  state, and the store keys and constants. **Generated** by the Go types with
  `go test ./internal/manifest -run TestContractFixtures -update`; never edit
  them by hand.
- `manifest-invalid.json`: manifests both `parseManifest` and Go's `Decode` must
  refuse. Hand-written.

CI for this folder is `.github/workflows/plugin.yml` at the repository root. It
runs on plugin changes and on any contract change, alongside the worker's
workflow.

## Requirements

Obsidian 1.12.3 or later. `appendBinary` landed there, and without it the mobile
file-size ceiling is structural rather than incidental.
