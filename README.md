# obsync

Phase 1 (`obsync-man`): mirror Canvas LMS course files into an object store, and
from there into an Obsidian vault. One-way. See [DESIGN.md](DESIGN.md).

## Status

| Component | State |
|---|---|
| `internal/portable`, `policy`, `manifest`, `plan` | done, tests green |
| `internal/canvas` | client + rate limiter written, untested against live Canvas |
| `internal/store` FS + Memory | done |
| `internal/store` S3 | stub, build step 5 |
| `internal/run` | sequenced, downloads still serial |
| `cmd/obsync` ls/preview/cat | done; log/diff/gc stubbed |
| `internal/obs` | logs only, Pushgateway not wired |
| plugin: types/policy/preview | done, cross-tested against Go |
| plugin: sync/store/UI | written, untested in a real vault |

## Dev loop, no cluster needed

```sh
make test                    # go test ./... -race
export CANVAS_BASE_URL=https://canvas.nus.edu.sg
export CANVAS_TOKEN=...
make run-dev                 # full pipeline into ./.obsync-store
./bin/obsync -fs-store=./.obsync-store preview <course-id>
```

## Plugin

```sh
cd plugin
npm install
npm test                     # cross-checks the rule engine against the Go golden fixture
npm run dev                  # esbuild watch
ln -s $PWD ~/ObsidianDev/.obsidian/plugins/obsync
```

Requires Obsidian >= 1.12.3 for `appendBinary`.

## Before you start

Confirm NUS has not disabled manual access token generation in Canvas user
settings. If it has, phase 1 has no data source.
