.PHONY: build run test clean ui-build systemd-install systemd-restart systemd-status systemd-logs systemd-stop

# Variables
BINARY_NAME=broker
SOURCE_DIR=agent-broker
PORT=9197
DB_PATH=data/broker.db
SERVICE=broker
SERVICE_TEMPLATE=deploy/broker.service.in
WORKDIR=$(CURDIR)
RUN_USER=$(shell id -un)
RUN_GROUP=$(shell id -gn)

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

# --- systemd -----------------------------------------------------------------
# Install the service (once): render unit from template using the current
# user and working directory, then enable autostart on boot.
systemd-install:
	@echo "Installing $(SERVICE).service (user=$(RUN_USER), dir=$(WORKDIR))..."
	@sed -e 's|__USER__|$(RUN_USER)|g' \
	     -e 's|__GROUP__|$(RUN_GROUP)|g' \
	     -e 's|__WORKDIR__|$(WORKDIR)|g' \
	     -e 's|__PORT__|$(PORT)|g' \
	     -e 's|__DB_PATH__|$(DB_PATH)|g' \
	     $(SERVICE_TEMPLATE) | sudo tee /etc/systemd/system/$(SERVICE).service > /dev/null
	sudo systemctl daemon-reload
	sudo systemctl enable --now $(SERVICE)
	@echo "Done. Status:"
	@systemctl --no-pager status $(SERVICE) || true

# Rebuild (UI + Go) and restart the service. Main deploy command.
systemd-restart: build
	@echo "Restarting $(SERVICE)..."
	sudo systemctl restart $(SERVICE)
	@systemctl --no-pager status $(SERVICE) || true

systemd-status:
	@systemctl --no-pager status $(SERVICE) || true

systemd-logs:
	journalctl -u $(SERVICE) -f -n 100

systemd-stop:
	sudo systemctl stop $(SERVICE)
