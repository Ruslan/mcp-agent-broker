.PHONY: build run test clean ui-build

# Variables
BINARY_NAME=broker
SOURCE_DIR=agent-broker
PORT=9197
DB_PATH=data/broker.db

ui-build:
	@echo "Building UI..."
	cd ui && npm ci && npm run build
	rm -rf $(SOURCE_DIR)/dist
	cp -r ui/dist $(SOURCE_DIR)/dist

build: ui-build
	@echo "Building $(BINARY_NAME)..."
	cd $(SOURCE_DIR) && go build -o ../$(BINARY_NAME) .

run: build
	@echo "Starting Agent Task Broker on port $(PORT)..."
	PORT=$(PORT) DB_PATH=$(DB_PATH) ./$(BINARY_NAME)

test: build
	@echo "Running Go tests..."
	cd $(SOURCE_DIR) && go test -v ./...
	@echo "Running integration tests..."
	bash .gemini/test_v0.0.3.sh

clean:
	@echo "Cleaning up..."
	rm -f $(BINARY_NAME)
	rm -rf task-data-test*
	rm -rf $(SOURCE_DIR)/dist
