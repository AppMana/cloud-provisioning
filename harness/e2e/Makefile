STATE ?= .state/platform

.PHONY: vm-wrapper platform-image kubeadm-image coverage
vm-wrapper:
	mkdir -p $(STATE)/wrapper
	CGO_ENABLED=0 go build -trimpath -o $(STATE)/wrapper/guestagent ./cmd/guestagent
	cp rig/vm/image/Dockerfile rig/vm/image/launcher.py $(STATE)/wrapper/
	docker build -t cloud-provisioning/vm:single-nic $(STATE)/wrapper

platform-image: vm-wrapper
	go run ./cmd/nodeimage -platform-only -work-dir $(STATE)

kubeadm-image: vm-wrapper
	go run ./cmd/nodeimage -work-dir $(STATE)

coverage:
	mkdir -p $(STATE)/coverage
	go test -mod=readonly -short -count=1 -covermode=atomic -coverprofile=$(STATE)/coverage/harness.cover ./...
	cd ../../controller && CLDT_NETNS=0 go test -mod=readonly -short -count=1 -covermode=atomic -coverprofile=$(abspath $(STATE))/coverage/controller.cover ./...
	go tool cover -func $(STATE)/coverage/harness.cover
	cd ../../controller && go tool cover -func $(abspath $(STATE))/coverage/controller.cover

.PHONY: lab-covered
lab-covered:
	mkdir -p $(STATE)/coverage/runtime
	go build -cover -coverpkg=./... -o $(STATE)/lab ./cmd/lab
	@echo 'Run with GOCOVERDIR=$(abspath $(STATE))/coverage/runtime $(STATE)/lab ...'
