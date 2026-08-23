package cmd

import (
	"net/http"
	"testing"
	"time"
)

func TestDownloadOptionsMapFlags(t *testing.T) {
	selectionFlag := rootCmd.Flags().Lookup("enable-node-selection")
	if selectionFlag == nil {
		t.Fatal("--enable-node-selection is not registered")
	}
	if selectionFlag.DefValue != "false" {
		t.Fatalf("--enable-node-selection default = %q, want false", selectionFlag.DefValue)
	}

	savedFlags := flags
	savedChanged := selectionFlag.Changed
	t.Cleanup(func() {
		flags = savedFlags
		selectionFlag.Changed = savedChanged
	})

	flags.parts = 6
	flags.timeout = 17 * time.Second
	flags.retries = 3
	flags.sha256 = "0123456789abcdef"
	flags.resumeID = "stable-object"
	flags.force = true
	if err := selectionFlag.Value.Set("true"); err != nil {
		t.Fatalf("set --enable-node-selection: %v", err)
	}
	selectionFlag.Changed = true

	headers := http.Header{"X-Test": {"value"}}
	opt := downloadOptions(headers)

	if opt.Parts != flags.parts {
		t.Errorf("Parts = %d, want %d", opt.Parts, flags.parts)
	}
	if opt.Timeout != flags.timeout {
		t.Errorf("Timeout = %v, want %v", opt.Timeout, flags.timeout)
	}
	if opt.MaxRetries != flags.retries {
		t.Errorf("MaxRetries = %d, want %d", opt.MaxRetries, flags.retries)
	}
	if opt.Headers.Get("X-Test") != "value" {
		t.Errorf("Headers[X-Test] = %q, want value", opt.Headers.Get("X-Test"))
	}
	if opt.ExpectedSHA256 != flags.sha256 {
		t.Errorf("ExpectedSHA256 = %q, want %q", opt.ExpectedSHA256, flags.sha256)
	}
	if opt.ResumeID != flags.resumeID {
		t.Errorf("ResumeID = %q, want %q", opt.ResumeID, flags.resumeID)
	}
	if opt.Overwrite != flags.force {
		t.Errorf("Overwrite = %t, want %t", opt.Overwrite, flags.force)
	}
	if !opt.EnableNodeSelection {
		t.Error("EnableNodeSelection = false after --enable-node-selection=true")
	}
	if opt.Logger == nil {
		t.Error("Logger = nil")
	}
}
