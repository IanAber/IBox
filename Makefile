LDFLAGS = -s -w
export CGO_ENABLED = 0

all: arm64 armv7 armv6

arm64:   # Pi 3/4/5/Zero 2 W on 64-bit Raspberry Pi OS
	GOOS=linux GOARCH=arm64 go build -trimpath -ldflags="$(LDFLAGS)" -o dist/ibox-linux-arm64 .

armv7:   # Pi 2/3/4 on 32-bit Raspberry Pi OS
	GOOS=linux GOARCH=arm GOARM=7 go build -trimpath -ldflags="$(LDFLAGS)" -o dist/ibox-linux-armv7 .

armv6:   # Pi Zero / Zero W / Pi 1
	GOOS=linux GOARCH=arm GOARM=6 go build -trimpath -ldflags="$(LDFLAGS)" -o dist/ibox-linux-armv6 .

test:
	go test ./...

.PHONY: all arm64 armv7 armv6 test
