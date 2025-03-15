.PHONY: install
install:
	@mkdir --parents $${HOME}/.local/bin \
	&& mkdir --parents $${HOME}/.config/systemd/user \
	&& cp activitywatch_exporter $${HOME}/.local/bin/ \
	&& chmod +x $${HOME}/.local/bin/activitywatch_exporter \
	&& cp --no-clobber activitywatch_exporter.json $${HOME}/.config/activitywatch_exporter.json \
	&& chmod 400 $${HOME}/.config/activitywatch_exporter.json \
	&& cp activitywatch-exporter.timer $${HOME}/.config/systemd/user/ \
	&& cp activitywatch-exporter.service $${HOME}/.config/systemd/user/ \
	&& systemctl --user enable --now activitywatch-exporter.timer

.PHONY: uninstall
uninstall:
	@rm -f $${HOME}/.local/bin/activitywatch_exporter \
	&& rm -f $${HOME}/.config/activitywatch_exporter.json \
	&& systemctl --user disable --now activitywatch-exporter.timer \
	&& rm -f $${HOME}/.config/.config/systemd/user/activitywatch-exporter.timer \
	&& rm -f $${HOME}/.config/systemd/user/activitywatch-exporter.service

.PHONY: build
build:
	@go build -ldflags="-s -w" -o activitywatch_exporter main.go