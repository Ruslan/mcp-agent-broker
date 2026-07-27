# `make kamal <words>` (see the Kamal section at the bottom) hands <words> to kamal.
# Make parses those words as additional GOALS, and a goal that names a real target
# here would FIRE it: `make kamal deploy` would deploy twice, `make kamal build push`
# would rebuild the Svelte UI on the way. A catch-all rule can't prevent that —
# explicit rules beat pattern rules. So during that one invocation this Makefile
# defines nothing but the kamal target, and the stray words get a no-op rule there.
ifeq (kamal,$(firstword $(MAKECMDGOALS)))
kamal-passthru := 1
endif

ifndef kamal-passthru

.PHONY: build run test clean ui-build sync-skillfiles systemd-install systemd-restart systemd-status systemd-logs systemd-stop

# Regenerate the install copy of the broker-async-poll skill from the canonical
# embedded sources. Run this after editing any file under agent-broker/skillfiles/;
# the parity test (TestSkillfilesMatchCanonical) fails if the two copies drift.
SKILL_CANON=$(SOURCE_DIR)/skillfiles
SKILL_INSTALL=.claude/skills/broker-async-poll
sync-skillfiles:
	@echo "Syncing $(SKILL_INSTALL) from $(SKILL_CANON)..."
	@mkdir -p $(SKILL_INSTALL)
	@cp $(SKILL_CANON)/broker-poll.sh $(SKILL_CANON)/await-poll.sh $(SKILL_CANON)/broker-monitor.sh $(SKILL_CANON)/SKILL.md $(SKILL_INSTALL)/
	@chmod +x $(SKILL_INSTALL)/broker-poll.sh $(SKILL_INSTALL)/await-poll.sh $(SKILL_INSTALL)/broker-monitor.sh
	@echo "Done."

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

endif # kamal-passthru

# --- Kamal (Docker) deploy ---------------------------------------------------
.PHONY: kamal
# The alternative to the systemd recipe above: build an image and run the broker
# as a Docker container behind kamal-proxy, with SQLite on a persistent volume.
# Full walkthrough: deploy/README-kamal.md.
#
# Everything goes through ONE target, `make kamal <words>`, which loads .env and
# hands <words> to kamal. Kamal does NOT read .env itself, and without it
# config/deploy.yml's ERB lookups and .kamal/secrets resolve empty — kamal then
# fails with a confusing "servers/web/0". Nothing deployment-specific is
# committed: put your server, domain and keys in .env.
KAMAL ?= kamal
ENVFILE ?= .env
# Run a kamal subcommand with .env exported. `set -a` marks every assignment for
# export; the subshell keeps it out of the other targets.
kamal-run = @[ -f $(ENVFILE) ] || { echo "!! $(ENVFILE) missing — cp .env.example .env and fill it in"; exit 1; }; \
	set -a; . ./$(ENVFILE); set +a; \
	[ -n "$$KAMAL_SERVER_IP" ] || { echo "!! KAMAL_SERVER_IP unset in $(ENVFILE)"; exit 1; }; \
	[ -n "$$BROKER_HOST" ]     || { echo "!! BROKER_HOST unset in $(ENVFILE)"; exit 1; }; \
	[ -n "$$KAMAL_SSH_USER" ]  || { echo "!! KAMAL_SSH_USER unset in $(ENVFILE)"; exit 1; }; \
	[ -n "$$KAMAL_REGISTRY_SERVER" ] || { echo "!! KAMAL_REGISTRY_SERVER unset in $(ENVFILE)"; exit 1; }; \
	command -v $(KAMAL) >/dev/null || { echo "!! kamal not on PATH — install it or: make kamal deploy KAMAL=/path/to/kamal"; exit 1; }; \
	$(KAMAL)

# Any kamal command, with .env loaded — the words after `kamal` are passed through:
#
#   make kamal deploy          make kamal config        make kamal rollback
#   make kamal app details     make kamal proxy reboot  make kamal accessory boot db
#
# Options can NOT be typed bare: make parses the command line before the recipe
# runs and swallows a leading `-` itself (`-f` even demands a makefile after it).
# Put them in ARGS, which replaces the words entirely:
#
#   make kamal ARGS="app logs -n 200 --grep=poll"
#
# The daily commands that only exist to carry options are Kamal ALIASES, declared
# under `aliases:` in config/deploy.yml rather than wrapped here — that way they
# work from a raw `kamal` invocation too, and this target stays a plain conduit:
#
#   make kamal logs     make kamal shell     echo "SELECT …;" | make kamal sql
kamal:
	$(kamal-run) $(if $(ARGS),$(ARGS),$(wordlist 2,$(words $(MAKECMDGOALS)),$(MAKECMDGOALS)))

# `make kamal app details` asks make to build three goals: kamal, app, details.
# Only the first is real — give each of the others an empty recipe. Two details
# earn their keep here:
#   .PHONY, because some of these words name real files — `make kamal config`
#   finds the config/ directory and would report it up to date instead;
#   an explicit rule rather than a `%:` catch-all, because make skips implicit
#   rules for phony targets and would answer "Nothing to be done" for every word.
ifdef kamal-passthru
kamal-strays := $(wordlist 2,$(words $(MAKECMDGOALS)),$(MAKECMDGOALS))
ifneq ($(kamal-strays),)
.PHONY: $(kamal-strays)
$(kamal-strays):
	@:
endif
endif
