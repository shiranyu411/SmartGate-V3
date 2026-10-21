package netgate

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestDNSMasqOverrideUsesLoadedDirectoryAndCleansOnlyOwnedFiles(t *testing.T) {
	root := t.TempDir()
	canonical := filepath.Join(root, "canonical", "smartgate.conf")
	runtimeDir := filepath.Join(root, "loaded")
	generated := filepath.Join(root, "configs")
	bin := filepath.Join(root, "bin")
	for _, dir := range []string{filepath.Dir(canonical), runtimeDir, generated, bin} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	owner := "owner=smartgate:dnsmasq:smartgate-upstream"
	content := "# " + owner + "\nno-resolv\nserver=127.0.0.1#1053\n"
	write := func(path, body string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(body), 0700); err != nil {
			t.Fatal(err)
		}
	}
	write(canonical, content)
	write(filepath.Join(generated, "dnsmasq.conf.instance"), "conf-dir="+runtimeDir+"\n")
	write(filepath.Join(bin, "dnsmasq"), "#!/bin/sh\nexit 0\n")
	reload := filepath.Join(bin, "reload")
	write(reload, "#!/bin/sh\nexit 0\n")
	run := func(script string) error {
		script = strings.ReplaceAll(script, "/etc/dnsmasq.d/smartgate.conf", canonical)
		script = strings.ReplaceAll(script, "/var/etc/dnsmasq.conf.*", filepath.Join(generated, "dnsmasq.conf.*"))
		script = strings.ReplaceAll(script, "/etc/init.d/dnsmasq", reload)
		cmd := exec.Command("sh", "-c", script, "sh", canonical, owner)
		cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"))
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Logf("shell: %s", out)
		}
		return err
	}
	if run(DNSMasqRuntimeVerifyScript) == nil {
		t.Fatal("canonical file alone was reported loaded")
	}
	if err := run("set -eu\n" + DNSMasqRuntimeInstallScript); err != nil {
		t.Fatal(err)
	}
	if err := run(DNSMasqRuntimeVerifyScript); err != nil {
		t.Fatal(err)
	}
	unrelated := filepath.Join(runtimeDir, "unrelated.conf")
	write(unrelated, "address=/router.lan/192.168.1.1\n")
	if err := run(DNSMasqRuntimeCleanupScript); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{canonical, filepath.Join(runtimeDir, "smartgate.conf")} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("owned file remains: %s", path)
		}
	}
	if _, err := os.Stat(unrelated); err != nil {
		t.Fatal("unrelated config was removed")
	}
	write(canonical, content)
	write(filepath.Join(runtimeDir, "smartgate.conf"), "# foreign owner\n")
	if run("set -eu\n"+DNSMasqRuntimeInstallScript) == nil {
		t.Fatal("overwrote unowned override")
	}
}
