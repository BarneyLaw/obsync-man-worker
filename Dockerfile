# syntax=docker/dockerfile:1
#
# The obsync worker image: obsync-worker, which the CronJob runs, plus the
# obsync CLI for inspecting or garbage-collecting the store from a one-off Job.

FROM golang:1.26-alpine AS build
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY cmd ./cmd
COPY internal ./internal

# Static binaries: the runtime image has no libc.
ENV CGO_ENABLED=0
RUN go build -trimpath -ldflags="-s -w" -o /out/obsync-worker ./cmd/obsync-worker \
 && go build -trimpath -ldflags="-s -w" -o /out/obsync ./cmd/obsync

# distroless static: CA certificates for Canvas over HTTPS, a nonroot user
# (65532, the CronJob's runAsUser) and no shell. /tmp exists; the CronJob mounts
# an emptyDir there for in-flight downloads.
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /out/obsync-worker /out/obsync /usr/local/bin/

USER 65532:65532
ENTRYPOINT ["/usr/local/bin/obsync-worker"]
CMD ["help"]
