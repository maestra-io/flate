# Packages, Go modules and base images only through nora (packages.maestra.io, 3-day hold,
# issues-maestra#1649), no public fallback: the PRIVATE_PACKAGES_TOKEN BuildKit secret is
# required (CI: the GitHub OIDC token; locally: your nra_ token). GOSUMDB stays default.
# Go follows go.mod (go 1.27); the digest pin replaces the old GO_VERSION build arg.
FROM packages.maestra.io/docker-hub/library/golang:1.27-alpine@sha256:eb61c3873c97416fff0116553fff65d67a3549f72e11d9758849a689cdd8b523 AS build
ARG VERSION=dev
ARG REVISION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=secret,id=PRIVATE_PACKAGES_TOKEN,env=NORA_TOKEN,required=true \
    GOPROXY="https://token:${NORA_TOKEN}@packages.maestra.io/go" go mod download
COPY . ./
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w -X main.version=${VERSION} -X main.commit=${REVISION}" -o /out/flate ./cmd/flate

FROM packages.maestra.io/gcr/distroless/static:nonroot@sha256:25ac8f0c3b2334a9eb05452266bd4697864a8c8ef0ef71e5e6480d7f92868329
COPY --from=build /out/flate /flate
ENTRYPOINT ["/flate"]
