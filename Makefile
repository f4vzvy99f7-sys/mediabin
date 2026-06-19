DIST_REPO := https://github.com/f4vzvy99f7-sys/mediabin-web.git
DIST_BRANCH := dist

.PHONY: all web-dist build clean
.DEFAULT_GOAL := build

www:
	mkdir -p www

web-dist: | www
	rm -rf /tmp/mediabin-web-dist
	git clone --depth 1 --branch $(DIST_BRANCH) $(DIST_REPO) /tmp/mediabin-web-dist
	cp -r /tmp/mediabin-web-dist/* www/
	rm -rf /tmp/mediabin-web-dist

build: web-dist
	go build -o build/mediabin .

clean:
	rm -rf www

all: build
