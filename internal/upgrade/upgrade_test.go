package upgrade

import "testing"

func TestChecksumFor(t *testing.T) {
	sums := "abc123  firerunner-linux-amd64\ndef456 *install.sh\n"
	if got, err := checksumFor(sums, "firerunner-linux-amd64"); err != nil || got != "abc123" {
		t.Fatalf("got %q %v", got, err)
	}
	if got, _ := checksumFor(sums, "install.sh"); got != "def456" {
		t.Fatalf("binary-mode entry: %q", got)
	}
	if _, err := checksumFor(sums, "missing"); err == nil {
		t.Fatal("expected error")
	}
}

func TestBaseURL(t *testing.T) {
	if BaseURL("latest") != "https://github.com/ismoilovdevml/firerunner/releases/latest/download" {
		t.Fatal(BaseURL("latest"))
	}
	if BaseURL("v1.2.0") != "https://github.com/ismoilovdevml/firerunner/releases/download/v1.2.0" {
		t.Fatal(BaseURL("v1.2.0"))
	}
}
