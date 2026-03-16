.PHONY: all build server clean

all: build server

build:
	@echo "Building WASM client..."
	mkdir -p server/client
	cd client && GOOS=js GOARCH=wasm go build -o ../server/client/chat.wasm
	@echo "Build complete!"

server: build
	@echo "Starting server..."
	cd server && go run main.go

clean:
	rm -f client/chat.wasm
	rm -rf server/data

help:
	@echo "Available targets:"
	@echo "  make build   - Build the WASM client"
	@echo "  make server  - Build and start the server"
	@echo "  make clean   - Remove built files and data"
