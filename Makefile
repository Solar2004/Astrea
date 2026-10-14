BIN := bin

.PHONY: all agent hub panel windows clean test

all: agent hub panel

agent:
	go build -o $(BIN)/astrea-agent ./cmd/astrea-agent

hub:
	go build -o $(BIN)/astrea-hub ./cmd/astrea-hub

# Local dashboard (CPU/RAM/GPU) of the controlled machines.
panel:
	go build -o $(BIN)/astrea-panel ./cmd/astrea-panel

# Cross-compile the agent for the controlled Windows PC.
# -H=windowsgui: sin ventana de consola (corre oculto, sin flash al logon).
windows:
	GOOS=windows GOARCH=amd64 go build -ldflags "-H=windowsgui" -o $(BIN)/astrea-agent.exe ./cmd/astrea-agent

test:
	go test ./...

clean:
	rm -rf $(BIN)
