.PHONY: build run test clean

# Variables
BINARY_NAME=broker
SOURCE_DIR=agent-broker
PORT=9197
DATA_DIR=data

build:
	@echo "Building $(BINARY_NAME)..."
	cd $(SOURCE_DIR) && go build -o ../$(BINARY_NAME) .

run: build
	@echo "Starting Agent Task Broker on port $(PORT)..."
	PORT=$(PORT) DATA_DIR=$(DATA_DIR) ./$(BINARY_NAME)

test: build
	@echo "Running Go tests..."
	cd $(SOURCE_DIR) && go test -v ./...
	@echo "Running integration tests..."
	bash .gemini/test_v0.0.3.sh

clean:
	@echo "Cleaning up..."
	rm -f $(BINARY_NAME)
	rm -rf task-data-test*
