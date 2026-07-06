# claude-status — build & deploy.
#
# The binary is a static, multi-call Go program; the daemon (spawned by niri) and
# the recap timers both run the SAME installed copy at $(BIN). Because it is a
# compiled artifact, new features only take effect after a rebuild+reinstall —
# `make install` is that single reproducible step. Run it after any code change,
# then restart the daemon (`make restart-daemon`) to pick it up.

PREFIX  ?= $(HOME)/.local
BIN      = $(PREFIX)/bin/claude-status
JOB      = $(PREFIX)/bin/claude-recap-job
UNITDIR  = $(HOME)/.config/systemd/user

.PHONY: build test install install-units restart-daemon uninstall-units

build:
	CGO_ENABLED=0 go build -o claude-status .

test:
	go test ./...

# Full deploy: rebuild, install the binary + recap-job script, then install and
# (re)enable the systemd user timers. daemon-reload picks up unit edits.
install: build install-units
	install -Dm755 claude-status $(BIN)
	install -Dm755 dist/claude-recap-job $(JOB)
	rm -f $(PREFIX)/bin/claude-daily-recap   # superseded by claude-recap-job
	@echo "installed $(BIN) and $(JOB)"
	@echo "restart the daemon to run the new binary: make restart-daemon"

install-units:
	install -Dm644 dist/systemd/claude-daily-recap.service  $(UNITDIR)/claude-daily-recap.service
	install -Dm644 dist/systemd/claude-daily-recap.timer    $(UNITDIR)/claude-daily-recap.timer
	install -Dm644 dist/systemd/claude-weekly-recap.service $(UNITDIR)/claude-weekly-recap.service
	install -Dm644 dist/systemd/claude-weekly-recap.timer   $(UNITDIR)/claude-weekly-recap.timer
	systemctl --user daemon-reload
	systemctl --user enable claude-daily-recap.timer claude-weekly-recap.timer

# The running daemon keeps the old binary until restarted. niri spawns it at
# login via spawn-sh-at-startup; this kills the current one and relaunches it
# detached so a manual deploy takes effect without a re-login.
restart-daemon:
	-pkill -f '$(BIN) daemon'
	setsid $(BIN) daemon >/dev/null 2>&1 < /dev/null & disown || true
	@echo "daemon restarted"

uninstall-units:
	-systemctl --user disable --now claude-daily-recap.timer claude-weekly-recap.timer
	rm -f $(UNITDIR)/claude-daily-recap.service $(UNITDIR)/claude-daily-recap.timer
	rm -f $(UNITDIR)/claude-weekly-recap.service $(UNITDIR)/claude-weekly-recap.timer
	systemctl --user daemon-reload
