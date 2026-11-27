VERSION ?= dev
# -H=windowsgui: a windowed program, no console window behind it.
LDFLAGS := -s -w -H=windowsgui -X main.version=$(VERSION)
DIST := dist
BUILD := CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)"

.PHONY: all windows release icon demo test vet fmt clean

all: windows

windows:
	@mkdir -p $(DIST)
	GOOS=windows GOARCH=amd64 $(BUILD) -o $(DIST)/uefimgr.exe ./cmd/uefimgr
	GOOS=windows GOARCH=arm64 $(BUILD) -o $(DIST)/uefimgr-arm64.exe ./cmd/uefimgr

release: windows
	cd $(DIST) && shasum -a 256 uefimgr.exe uefimgr-arm64.exe > SHA256SUMS

# Re-render the icon and rebuild the resource objects in cmd/uefimgr. Only
# needed after editing the SVGs in cmd/uefimgr/winres; needs Google Chrome.
icon:
	cmd/uefimgr/winres/build.sh

# Run the interface on this machine against a simulated variable file.
demo:
	go run ./cmd/uefimgr --store file:demo.json

test:
	go test ./...

vet:
	go vet ./...
	GOOS=windows GOARCH=amd64 go vet ./...
	GOOS=windows GOARCH=arm64 go vet ./...

fmt:
	gofmt -w .

clean:
	rm -rf $(DIST)
