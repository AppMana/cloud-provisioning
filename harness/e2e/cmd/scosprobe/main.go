// Command scosprobe verifies serial management on the pinned SCOS platform.
// This standalone OS probe is not a cluster, CNI, or topology-isolation test.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/appmana/cloud-provisioning/harness/e2e/lab"
	"github.com/appmana/cloud-provisioning/harness/e2e/rig/vm/scos"
)

func main() {
	disk := flag.String("disk", ".state/okd-tools/scos.qcow2", "verified installer-stream SCOS disk")
	work := flag.String("work-dir", ".state/scos-platform-proof", "new directory for evidence")
	flag.Parse()
	if err := run(*disk, *work); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(disk, work string) error {
	disk, err := filepath.Abs(disk)
	if err != nil {
		return err
	}
	if err := scos.ValidateDisk(disk); err != nil {
		return err
	}
	work, err = filepath.Abs(work)
	if err != nil {
		return err
	}
	if err := os.Mkdir(work, 0700); err != nil {
		return err
	}
	seed, err := scos.Seed("cldt-scos-probe", "10.0.2.15/24", "10.0.2.2", nil)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(work, "management.ign"), seed, 0600); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	command := func(argv ...string) error {
		out, err := exec.CommandContext(ctx, argv[0], argv[1:]...).CombinedOutput()
		if err != nil {
			return fmt.Errorf("%s: %w: %s", argv[0], err, out)
		}
		return nil
	}
	overlay := filepath.Join(work, "probe.qcow2")
	if err := command("qemu-img", "create", "-f", "qcow2", "-F", "qcow2", "-b", disk, overlay); err != nil {
		return err
	}
	// The backing image is mounted separately, so its guest-wrapper path differs.
	if err := command("qemu-img", "rebase", "-u", "-f", "qcow2", "-F", "qcow2", "-b", "/scos-base.qcow2", overlay); err != nil {
		return err
	}
	name := fmt.Sprintf("cldt-scos-probe-%d", time.Now().UnixNano())
	qemu := `/cldt-guest serve & exec qemu-system-x86_64 -enable-kvm -nodefaults -display none
-machine pc -cpu host -m 4096 -smp 2
-drive if=none,id=os,file=/proof/probe.qcow2,format=qcow2
-device virtio-blk-pci,drive=os,addr=0x4
-netdev user,id=net0 -device virtio-net-pci,netdev=net0,addr=0x2
-chardev socket,path=/run/cldt-qga.sock,server=on,wait=off,id=qga0
-device virtio-serial-pci,id=serial1,addr=0x3
-device virtserialport,chardev=qga0,name=org.qemu.guest_agent.0
-serial file:/proof/console.log
-fw_cfg name=opt/com.coreos/config,file=/proof/management.ign`
	qemu = string(bytes.ReplaceAll([]byte(qemu), []byte("\n"), []byte(" ")))
	if err := command("docker", "run", "-d", "--name", name, "--network", "none", "--device", "/dev/kvm", "-v", disk+":/scos-base.qcow2:ro", "-v", work+":/proof", "--entrypoint", "sh", lab.VMImage, "-c", qemu); err != nil {
		return err
	}
	defer func() {
		cleanup, stop := context.WithTimeout(context.Background(), 15*time.Second)
		defer stop()
		_ = exec.CommandContext(cleanup, "docker", "rm", "-f", name).Run()
	}()
	guest := func(input []byte, args ...string) ([]byte, []byte, int, error) {
		mode := "0"
		if input != nil {
			mode = "1"
		}
		argv := []string{"exec", "-i", name, "/cldt-guest", "exec", "15s", mode, scos.ExecWrapper}
		cmd := exec.CommandContext(ctx, "docker", append(argv, args...)...)
		cmd.Stdin = bytes.NewReader(input)
		var out, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &stderr
		err := cmd.Run()
		code := -1
		if cmd.ProcessState != nil {
			code = cmd.ProcessState.ExitCode()
		}
		return out.Bytes(), stderr.Bytes(), code, err
	}
	fmt.Println("Booting the pinned SCOS disk with lab serial management")
	for {
		// Observed on SCOS: QGA starts before NetworkManager finishes applying
		// the Ignition keyfile. Management availability is not network readiness.
		if _, _, _, err := guest(nil, "sh", "-c", `test "$(hostname)" = cldt-scos-probe && ip -4 -o address show ens2 | grep -q '10.0.2.15/24'`); err == nil {
			break
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("SCOS management did not become available: %w; console: %s", ctx.Err(), filepath.Join(work, "console.log"))
		case <-time.After(2 * time.Second):
		}
	}
	// Only sysfs-backed Ethernet devices count as physical NICs.
	script := `set -eu
test "$(getenforce)" = Enforcing
count=0
for n in /sys/class/net/*; do
  if test -e "$n/device" && test "$(cat "$n/type")" = 1; then count=$((count+1)); fi
done
test "$count" = 1
test "$(hostname)" = cldt-scos-probe
ip -4 -o address show ens2 | grep -q '10.0.2.15/24'
cat /etc/os-release
id
ip link set ens2 down
test "$(cat /sys/class/net/ens2/operstate)" = down
ip -json link show ens2`
	out, stderr, _, err := guest(nil, "sh", "-c", script)
	if saveErr := os.WriteFile(filepath.Join(work, "platform.txt"), append(out, stderr...), 0600); saveErr != nil {
		return saveErr
	}
	if err != nil {
		return fmt.Errorf("SCOS platform assertions: %w: %s", err, stderr)
	}
	out, stderr, code, _ := guest([]byte("serial-input\n"), "sh", "-c", "cat; echo serial-error >&2; exit 7")
	proof := struct {
		Release        string
		DiskSHA256     string
		Exit           int
		Stdout, Stderr string
	}{scos.Release, scos.DiskSHA256, code, string(out), string(stderr)}
	raw, err := json.MarshalIndent(proof, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(work, "serial.json"), raw, 0600); err != nil {
		return err
	}
	if code != 7 || string(out) != "serial-input\n" || string(stderr) != "serial-error\n" {
		return fmt.Errorf("serial contract failed: %s", raw)
	}
	fmt.Println("PASS: one NIC, SELinux enforcing, NIC down, stdin/stdout/stderr/exit 7 preserved")
	return nil
}
