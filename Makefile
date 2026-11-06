PREFIX ?= $(HOME)/.local

.PHONY: build linux install rex

build:
	go build -o heypad .

# A Linux build for running heypad on another machine through Rex.
linux:
	GOOS=linux GOARCH=amd64 go build -o heypad-linux-amd64 .
	GOOS=linux GOARCH=arm64 go build -o heypad-linux-arm64 .

install:
	go build -o $(PREFIX)/bin/heypad .

# Writes ~/.config/rex/heypad.lua (palette actions); prints the init.lua line.
rex: install
	$(PREFIX)/bin/heypad rex-install
