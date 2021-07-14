
all: install



.PHONY: install
install:
	go install kugg/enum_serialize

.PHONY: test
test:
	go test ./...

.PHONY: lint
lint:
	gometalinter ./...
