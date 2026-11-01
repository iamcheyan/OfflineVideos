BINARY ?= videod
LDFLAGS ?= -s -w

.PHONY: build run tidy clean

build:
	go build -trimpath -ldflags "$(LDFLAGS)" -o $(BINARY) ./cmd/videod

run:
	go run ./cmd/videod

tidy:
	go mod tidy

clean:
	rm -f $(BINARY)
