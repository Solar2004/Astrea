BIN := bin

.PHONY: all agent hub windows clean test

all: agent hub

agent:
	go build -o $(BIN)/astrea-agent ./cmd/astrea-agent

hub:
	go build -o $(BIN)/astrea-hub ./cmd/astrea-hub

# Cross-compile the agent for the controlled Windows PC.
windows:
	GOOS=windows GOARCH=amd64 go build -o $(BIN)/astrea-agent.exe ./cmd/astrea-agent

test:
	go test ./...

clean:
	rm -rf $(BIN)
