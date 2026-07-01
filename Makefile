BINARY := claude-dial

.PHONY: build run test vet fmt clean

build:
	go -C bridge build -o ../bin/$(BINARY) ./cmd/claude-dial

run: build
	./bin/$(BINARY) serve

test:
	go -C bridge test ./...

vet:
	go -C bridge vet ./...

fmt:
	go -C bridge fmt ./...

clean:
	rm -rf bin
