VERSION ?= $(shell date -I)

push: build
	docker push ghcr.io/hipofinance/poker:${VERSION}

build: tag
	docker build --tag ghcr.io/hipofinance/poker:${VERSION} .

tag:
	git tag -sf ${VERSION} -m ${VERSION}

test:
	go build ./... && go vet ./... && go test ./...

.PHONY: push build tag test
