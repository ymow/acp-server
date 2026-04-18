// Package gittwin implements ACR-400 Git Covenant Twin primitives:
// platform_id validation, unit mapping, and GitHub webhook parsing.
//
// The package itself has no DB dependencies so cmd/acp-git-bridge can reuse it.
package gittwin

import (
	"fmt"
	"strings"
)

// ValidProviderPrefixes lists the platform_id prefixes accepted by ACR-400 Part 3.
// email: is used for privacy-preserving mapping (sha256 of git author email).
var ValidProviderPrefixes = []string{"github", "gitlab", "gitea", "email"}

// ValidatePlatformID enforces the "<provider>:<identifier>" shape from ACR-400 Part 3.
// It does not hit the network; format only.
func ValidatePlatformID(pid string) error {
	if pid == "" {
		return fmt.Errorf("platform_id: empty")
	}
	idx := strings.IndexByte(pid, ':')
	if idx <= 0 || idx == len(pid)-1 {
		return fmt.Errorf("platform_id %q: missing \"<provider>:<identifier>\" separator", pid)
	}
	provider := pid[:idx]
	identifier := pid[idx+1:]

	if provider == "gitea" {
		// gitea:<host>:<username> — require a second colon.
		rest := identifier
		j := strings.IndexByte(rest, ':')
		if j <= 0 || j == len(rest)-1 {
			return fmt.Errorf("platform_id %q: gitea form is \"gitea:<host>:<username>\"", pid)
		}
		return nil
	}

	for _, p := range ValidProviderPrefixes {
		if provider == p {
			return nil
		}
	}
	return fmt.Errorf("platform_id %q: unknown provider %q (allowed: %s)",
		pid, provider, strings.Join(ValidProviderPrefixes, ", "))
}

// PlatformIDFromGitHubLogin returns the canonical platform_id for a GitHub user.
func PlatformIDFromGitHubLogin(login string) string {
	return "github:" + login
}
