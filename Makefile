.PHONY: test build run-dev lint plugin
test:
	go test ./... -race
build:
	go build -o bin/obsync-worker ./cmd/obsync-worker
	go build -o bin/obsync         ./cmd/obsync
# Full pipeline into a local directory. No Garage, no cluster.
run-dev: build
	CANVAS_BASE_URL=$$CANVAS_BASE_URL CANVAS_TOKEN=$$CANVAS_TOKEN \
	  ./bin/obsync-worker -rules=deploy/rules.json -fs-store=./.obsync-store
	./bin/obsync -fs-store=./.obsync-store ls
lint:
	go vet ./...
plugin:
	cd plugin && npm run build
