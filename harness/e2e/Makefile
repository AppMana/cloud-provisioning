STATE ?= .state/platform
VM_WRAPPER_IMAGE ?= cloud-provisioning/vm:single-nic

.PHONY: vm-wrapper platform-image kubeadm-image coverage
vm-wrapper:
	@test -n "$(LABCONTAINERS_VM_IMAGE)" || { echo "LABCONTAINERS_VM_IMAGE must select a prepared SDK VM image" >&2; exit 1; }
	docker image inspect "$(LABCONTAINERS_VM_IMAGE)" >/dev/null
	docker build --build-arg LABCONTAINERS_VM_IMAGE="$(LABCONTAINERS_VM_IMAGE)" -t "$(VM_WRAPPER_IMAGE)" rig/vm/image

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
