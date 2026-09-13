.PHONY: test build lint courses run-dev pull-dev serve-dev plugin plugin-test plugin-lint contract fixtures ci
# Every recipe is one plain command, so on Windows without make you can paste
# it into a shell directly (add .exe to the binary names).
test:
	go test ./... -race
lint:
	gofmt -l cmd internal
	go vet ./...
build:
	go build -o bin/obsync-worker ./cmd/obsync-worker
	go build -o bin/obsync         ./cmd/obsync
# Which course codes and ids exist, and whether their files are reachable.
courses: build
	./bin/obsync-worker courses -probe
# Full pipeline into a local directory. No Garage, no cluster.
run-dev: build
	./bin/obsync-worker run -rules=deploy/rules.json -fs-store=./.obsync-store
	./bin/obsync -fs-store=./.obsync-store ls
# Manual pull of one course: make pull-dev COURSE=CS3103 [DRY=1]
pull-dev: build
	./bin/obsync-worker pull -rules=deploy/rules.json -fs-store=./.obsync-store -course=$(COURSE) $(if $(DRY),-dry-run)
# Read-only HTTP view of the dev store; point the plugin's base URL at it.
serve-dev: build
	./bin/obsync -fs-store=./.obsync-store serve
plugin:
	cd plugin && npm run build
plugin-test:
	cd plugin && npm test
plugin-lint:
	cd plugin && npm run lint
# Rewrite the generated contract fixtures in schema/ after changing what the
# worker writes, then run both halves against them.
fixtures:
	go test ./internal/manifest -run TestContractFixtures -count=1 -update
contract:
	go test -count=1 ./internal/manifest ./internal/policy
	cd plugin && npx vitest run src/contract.test.ts src/preview.test.ts src/policy.test.ts
# Everything both CI workflows run.
ci: lint test plugin-lint plugin-test plugin
