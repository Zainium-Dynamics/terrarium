# terrarium: one static binary.
#   make                      → build/terrarium
#   make install DESTDIR=...  → $(DESTDIR)/overlayer/zexlib/union/bin/terrarium

PREFIX ?= /overlayer/zexlib/union

.PHONY: all install clean

all:
	CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o build/terrarium ./cmd/terrarium

install: all
	install -Dm755 build/terrarium $(DESTDIR)$(PREFIX)/bin/terrarium

clean:
	rm -rf build
