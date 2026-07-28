package app

import (
	"strings"
	"testing"
)

func envFrom(vars map[string]string) func(string) string {
	return func(k string) string { return vars[k] }
}

func TestShouldUseHeadlessPairing(t *testing.T) {
	tests := []struct {
		name string
		goos string
		env  map[string]string
		want bool
	}{
		{name: "linux server with no display", goos: "linux", want: true},
		{name: "linux desktop with X11", goos: "linux", env: map[string]string{"DISPLAY": ":0"}},
		{name: "linux desktop on wayland", goos: "linux", env: map[string]string{"WAYLAND_DISPLAY": "wayland-0"}},
		{name: "macOS laptop", goos: "darwin"},
		{name: "windows desktop", goos: "windows"},
		// The browser would open on the far end of the SSH session, where
		// nobody is looking — treat it as headless regardless of OS.
		{name: "macOS over ssh", goos: "darwin", env: map[string]string{"SSH_CONNECTION": "1.2.3.4 22 5.6.7.8 22"}, want: true},
		{name: "linux desktop over ssh", goos: "linux", env: map[string]string{"DISPLAY": ":0", "SSH_TTY": "/dev/pts/0"}, want: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := shouldUseHeadlessPairing(tt.goos, envFrom(tt.env)); got != tt.want {
				t.Fatalf("shouldUseHeadlessPairing(%q) = %v, want %v", tt.goos, got, tt.want)
			}
		})
	}
}

func TestValidateProvisioningToken(t *testing.T) {
	valid := provisioningTokenPrefix + "abcdefghijklmnopqrstuvwxyz"

	tests := []struct {
		name      string
		token     string
		wantErr   bool
		errSubstr string
	}{
		{name: "valid token", token: valid},
		{name: "valid token with padding", token: "  " + valid + "\n"},
		{name: "empty", token: "", wantErr: true},
		{name: "whitespace only", token: "   ", wantErr: true},
		// The agent token lives right next to this one in the docs, so the
		// error has to name the confusion rather than just say "invalid".
		{name: "agent token pasted by mistake", token: localAgentTokenPrefix + "abcdefghijklmnopqrst", wantErr: true, errSubstr: "agent token"},
		{name: "unprefixed", token: "abcdefghijklmnopqrstuvwxyz", wantErr: true, errSubstr: provisioningTokenPrefix},
		{name: "truncated", token: provisioningTokenPrefix + "abc", wantErr: true, errSubstr: "truncated"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateProvisioningToken(tt.token)
			if tt.wantErr && err == nil {
				t.Fatalf("validateProvisioningToken(%q) = nil, want an error", tt.token)
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("validateProvisioningToken(%q) = %v, want nil", tt.token, err)
			}
			if tt.errSubstr != "" && !strings.Contains(err.Error(), tt.errSubstr) {
				t.Fatalf("error %q does not mention %q", err, tt.errSubstr)
			}
		})
	}
}

func TestChoosePublishMode(t *testing.T) {
	tests := []struct {
		name          string
		hasUserToken  bool
		hasAgentCreds bool
		want          publishMode
	}{
		// The user token wins when both exist: only it can re-pair a machine
		// whose agent row was deleted server-side.
		{name: "both available", hasUserToken: true, hasAgentCreds: true, want: publishViaUser},
		{name: "signed-in laptop, unpaired", hasUserToken: true, want: publishViaUser},
		{name: "headless paired server", hasAgentCreds: true, want: publishViaAgent},
		{name: "neither", want: publishImpossible},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := choosePublishMode(tt.hasUserToken, tt.hasAgentCreds); got != tt.want {
				t.Fatalf("choosePublishMode(%v, %v) = %v, want %v", tt.hasUserToken, tt.hasAgentCreds, got, tt.want)
			}
		})
	}
}
