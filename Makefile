.PHONY: build build-arm64 run clean

BINARY=console-manager
VERSION=1.0.0

build:
	cd backend && go build -o ../$(BINARY) -ldflags="-s -w" .

build-arm64:
	cd backend && GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -o ../$(BINARY)-linux-arm64 -ldflags="-s -w" .

run: build
	./$(BINARY)

clean:
	rm -f $(BINARY) $(BINARY)-linux-arm64

install: build-arm64
	@echo "Copy $(BINARY)-linux-arm64 to your ARM64 server"
	@echo "Install systemd service: sudo cp deploy/console-manager.service /etc/systemd/system/"
	@echo "sudo systemctl enable --now console-manager"
