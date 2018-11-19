build:
	mkdir -p bin
	go build -o bin/karau-server ./cmd/karau-server
	go build -o bin/karau-agent ./cmd/karau-agent

.PHONY = build
