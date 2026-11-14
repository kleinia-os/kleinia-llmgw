# Unified build interface: just build / test / package.
# All build artifacts land in ./build-out/ (gitignored).

default: build

# Package version (tag-driven; short sha until the first tag exists)
version := `git describe --tags --always 2>/dev/null || echo 0.1.0`

# Build the daemon and CLI into ./build-out/
build:
	go build -o build-out/ ./cmd/...

# Run the Go unit tests (all packages) and the GUI test suite.
# GUI tests need the GTK4/libadwaita typelibs (python3-gi); UI smoke tests
# additionally need a display (xvfb-run works).
test:
	go test ./...
	cd gui && python3 -m unittest discover tests

# Package ./build-out/ plus dist/systemd/ and prices/ declarations into an RPM
# (version is rendered from git into a transient root-level config)
package: build
	find gui -name __pycache__ -prune -exec rm -rf {} +
	sed "s/{{"{{PKG_VERSION}}"}}/{{version}}/" dist/nfpm.yaml > .nfpm.rendered.yaml
	nfpm pkg -f .nfpm.rendered.yaml -p rpm -t build-out/
	rm -f .nfpm.rendered.yaml

# Build the OCI image (embeds the RPM via dist/Containerfile) into the local store
image:
	podman build -f dist/Containerfile -t localhost/kleinia/kleinia-llmgw:{{version}} .

# RPM-carrier artifact image (scratch + /rpms/*.rpm) consumed by the kleinia
# assembly repo — same contract as kleinia-installer
artifact:
	podman build -f dist/Containerfile --target artifacts -t localhost/kleinia/kleinia-llmgw:rpm .

# Publish the artifact image ($KLN_REGISTRY contract; see scripts/push.sh)
push:
	bash scripts/push.sh

clean:
	rm -rf build-out bin
