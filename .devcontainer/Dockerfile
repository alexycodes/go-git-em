FROM debian:13.7-slim

RUN apt-get update && apt-get install -y --no-install-recommends \
        ca-certificates \
        curl \
        file \
        git \
        openssh-client \
        vim && \
    rm -rf /var/lib/apt/lists/*

COPY --from=golang:1.27.3-alpine /usr/local/go/ /usr/local/go/
COPY --from=golangci/golangci-lint:v2.15-alpine /usr/bin/golangci-lint /usr/local/go/bin/golangci-lint

ENV PATH="$PATH:/usr/local/go/bin:/root/go/bin"

RUN go install golang.org/x/tools/gopls@latest && \
    go install github.com/go-delve/delve/cmd/dlv@latest && \
    go install golang.org/x/vuln/cmd/govulncheck@v1.9.0 && \
    go install github.com/rhysd/actionlint/cmd/actionlint@v1.7.12 && \
    go install github.com/goreleaser/goreleaser/v2@v2.19.0 && \
    go install github.com/cli/cli/v2/cmd/gh@v2.102.0
