# Coverage below this fails `make cover` (and CI).
COVER_MIN ?= 90

.PHONY: build test cover e2e

build:
	CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o sftp-s3 .

test:
	go vet ./...
	go test -race -count=1 -timeout 5m ./...

cover:
	go test -race -count=1 -timeout 5m -coverprofile=coverage.out ./...
	go tool cover -html=coverage.out -o coverage.html
	@go tool cover -func=coverage.out | awk -v min=$(COVER_MIN) \
		'/^total:/ { pct=$$3+0; printf "total coverage %.1f%% (minimum %d%%)\n", pct, min; exit (pct < min) }'

# Needs MinIO (or real S3), sshpass and OpenSSH sftp; see scripts/e2e.sh.
e2e: build
	scripts/e2e.sh ./sftp-s3 $(or $(BIG),$(error set BIG=<path to a large test file>))
