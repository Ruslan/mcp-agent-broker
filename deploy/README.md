# Running Agent Task Broker under systemd

Instead of `make run` in a pinned tmux session, run the broker as a system
service: autostart on boot, auto-restart on failure, logs in journald.

## The process

A single process — the compiled Go binary `broker`. It listens on `PORT`
(default `9197`) and writes to the SQLite database at `DB_PATH`
(`data/broker.db`). The UI is built into `dist/` and served by the same
process. There are no separate workers or daemons — one unit is all you need.

Unit template: [`broker.service.in`](./broker.service.in). `make systemd-install`
renders it into `/etc/systemd/system/broker.service`, filling in the user,
group, and working directory from the current environment (`id -un` / `id -gn`
/ `pwd`) — nothing is hardcoded. `WorkingDirectory` becomes the repo root, so
`data/...` paths resolve the same way they do under `make run`.

## Initial install (once)

```bash
make build            # build UI + binary (a compiled ./broker is required)
make systemd-install  # copy unit, daemon-reload, enable --now
```

`systemd-install` copies the unit to `/etc/systemd/system/broker.service`,
runs `daemon-reload`, and `enable --now` (start now + autostart on boot). The
command uses sudo.

If you edit `deploy/broker.service.in`, re-run `make systemd-install` to
re-render, reinstall, and reload the daemon.

## Everyday commands

| Command                | What it does                                      |
|------------------------|---------------------------------------------------|
| `make systemd-restart` | Rebuild (UI + Go) and restart the service         |
| `make systemd-status`  | Show current status                               |
| `make systemd-logs`    | Follow logs (`journalctl -u broker -f`)           |
| `make systemd-stop`    | Stop the service                                  |

Normal development loop: edit code, then `make systemd-restart`.

## Manual equivalents (without make)

```bash
sudo systemctl restart broker        # restart
sudo systemctl stop broker           # stop
sudo systemctl disable --now broker  # disable autostart and stop
systemctl status broker              # status
journalctl -u broker -f              # logs
```

## Port / database path

Override `PORT` / `DB_PATH` on the make command line, e.g.
`make systemd-install PORT=8080`. They default to the Makefile values
(`PORT=9197`, `DB_PATH=data/broker.db`) and are baked into the rendered unit.

## Unit hardening notes

The unit enables light sandboxing: `ProtectSystem=full`,
`ProtectHome=read-only`, `NoNewPrivileges`, `PrivateTmp`. Writes are allowed
only under the repo's `data/` directory (`ReadWritePaths`). If the broker needs
to write elsewhere, add that path to `ReadWritePaths` in the template.
