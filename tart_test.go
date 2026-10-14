package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// VMs run headless and without the devices that make macOS ask the controller
// for a permission no job needs: audio input for the microphone, keyboard and
// pointer for Input Monitoring.
func TestStartBootsWithoutGraphicsAudioOrInput(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "tart")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nprintf '%s\\n' \"$@\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	var log bytes.Buffer
	exited, err := (&Tart{Bin: bin}).Start("eph-vm", []string{"--net-softnet"}, &log)
	if err != nil {
		t.Fatal(err)
	}
	if err := <-exited; err != nil {
		t.Fatal(err)
	}
	got := strings.Fields(log.String())
	want := []string{"run", "eph-vm", "--no-graphics", "--no-audio", "--no-keyboard", "--no-pointer", "--net-softnet"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("tart args = %q, want %q", got, want)
	}
}
