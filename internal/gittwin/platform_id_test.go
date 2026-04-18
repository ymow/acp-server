package gittwin

import "testing"

func TestValidatePlatformID(t *testing.T) {
	cases := []struct {
		in      string
		wantErr bool
	}{
		{"github:ymow", false},
		{"gitlab:alice", false},
		{"email:abc123def", false},
		{"gitea:git.example.com:alice", false},
		{"", true},
		{":", true},
		{"github:", true},
		{":ymow", true},
		{"twitter:ymow", true},
		{"gitea:alice", true}, // missing host:username
	}
	for _, c := range cases {
		err := ValidatePlatformID(c.in)
		if c.wantErr && err == nil {
			t.Errorf("%q: expected error, got nil", c.in)
		}
		if !c.wantErr && err != nil {
			t.Errorf("%q: unexpected error %v", c.in, err)
		}
	}
}

func TestPlatformIDFromGitHubLogin(t *testing.T) {
	if got := PlatformIDFromGitHubLogin("ymow"); got != "github:ymow" {
		t.Fatalf("got %q", got)
	}
}
