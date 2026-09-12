package secrets

import (
	"context"
	"fmt"
)

// LintResolver accepts every well-formed reference for the given schemes without resolving it:
// references are checked for shape (scheme:locator; a vault: locator must name mount, path and key) and answered with a placeholder.
// It lets a repository be validated on a machine that has none of the secrets.
func LintResolver() *Resolver {
	return NewResolver(
		placeholder{scheme: "env"},
		placeholder{scheme: "file"},
		placeholder{scheme: "vault", check: func(locator string) error {
			_, err := ParseVaultRef(locator)
			return err
		}},
	)
}

type placeholder struct {
	scheme string
	check  func(locator string) error
}

func (p placeholder) Scheme() string { return p.scheme }

func (p placeholder) Resolve(_ context.Context, locator string) (string, error) {
	if p.check != nil {
		if err := p.check(locator); err != nil {
			return "", err
		}
	}
	return fmt.Sprintf("<%s:%s>", p.scheme, locator), nil
}
