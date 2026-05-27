package media

import "testing"

func TestParseYtDlpProgressLine(t *testing.T) {
	progress, message, ok := parseYtDlpProgressLine("[download]  83.1% of  116.35MiB at  22.93MiB/s ETA 00:04")
	if !ok {
		t.Fatal("expected progress line to be parsed")
	}
	if progress != 83 {
		t.Fatalf("expected progress 83, got %d", progress)
	}
	if message != "Downloading media" {
		t.Fatalf("expected progress message, got %q", message)
	}
}

func TestParseYtDlpStageLine(t *testing.T) {
	message, ok := parseYtDlpStageLine("[Merger] Merging formats into \"/tmp/test.mp4\"")
	if !ok {
		t.Fatal("expected stage line to be parsed")
	}
	if message != "Merging formats" {
		t.Fatalf("expected merger message, got %q", message)
	}
}