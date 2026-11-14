.PHONY: build package vet test icon clean

build:
	python3 scripts/build.py

package:
	python3 scripts/package.py

vet:
	go vet ./...
	go vet -tags live ./...

test:
	go test -race ./...
	go test -C third_party/goroslib -race -run '^TestScout' .

icon:
	@tmp=$$(mktemp -d) && \
	swift scripts/make_icon.swift assets/icon-1024.png && \
	mkdir -p "$$tmp/Scout.iconset" && \
	for s in 16 32 128 256 512; do \
		sips -z $$s $$s assets/icon-1024.png --out "$$tmp/Scout.iconset/icon_$${s}x$${s}.png" >/dev/null && \
		s2=$$((s * 2)) && \
		sips -z $$s2 $$s2 assets/icon-1024.png --out "$$tmp/Scout.iconset/icon_$${s}x$${s}@2x.png" >/dev/null; \
	done && \
	iconutil -c icns "$$tmp/Scout.iconset" -o assets/Scout.icns && \
	rm -rf "$$tmp"

clean:
	rm -rf dist
