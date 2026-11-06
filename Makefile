.PHONY: build build-native test start start-runner clean

KEKWFARM_USER ?= $(shell id -u):$(shell id -g)
KEKWFARM_DOCKER_SOCKET ?= /var/run/docker.sock
KEKWFARM_DOCKER_GID ?= $(shell if [ -S "$(KEKWFARM_DOCKER_SOCKET)" ]; then \
	stat -L -c '%g' "$(KEKWFARM_DOCKER_SOCKET)" 2>/dev/null || stat -L -f '%g' "$(KEKWFARM_DOCKER_SOCKET)" 2>/dev/null || printf 0; \
	else printf 0; fi)

build:
	docker compose build

build-native:
	mkdir -p bin
	CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o bin/duckfarm ./cmd/duckfarm
	CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o bin/duckrunner ./cmd/duckrunner

test:
	go test ./...

start:
	umask 077; mkdir -p vol/duckfarm-go/data
	KEKWFARM_USER="$(KEKWFARM_USER)" docker compose up --build -d

start-runner:
	umask 077; mkdir -p vol/duckfarm-go/data vol/duckfarm-go/runner/uploads
	KEKWFARM_USER="$(KEKWFARM_USER)" KEKWFARM_DOCKER_SOCKET="$(KEKWFARM_DOCKER_SOCKET)" \
		KEKWFARM_DOCKER_GID="$(KEKWFARM_DOCKER_GID)" docker compose --profile runner up --build -d

clean:
	rm -f bin/duckfarm bin/duckrunner
