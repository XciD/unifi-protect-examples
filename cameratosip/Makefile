.PHONY: init
init:
	go install golang.org/x/tools/cmd/goimports@latest
	go install github.com/mgechev/revive@v1.0.5


.PHONY: lint
lint:
	revive -config revive.toml -formatter unix ./...

.PHONY: fmt
fmt:
	goimports -e -w -d $(shell find ./ -type f -name '*.go' -print)
	gofmt -e -w -d $(shell find ./ -type f -name '*.go' -print)

.PHONY: docker-build
docker-build:
	docker build -t cameratosip .
