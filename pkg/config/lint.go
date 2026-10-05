package config

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

func LoadPaths(paths ...string) (*Index, error) {
	ix := NewIndex()
	for _, p := range paths {
		info, err := os.Stat(p)
		if err != nil {
			return nil, err
		}
		if info.IsDir() {
			if err := ix.LoadTree("", p); err != nil {
				return nil, err
			}
			continue
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return nil, err
		}
		if err := ix.LoadBytes("", filepath.Clean(p), data); err != nil {
			return nil, err
		}
	}
	return ix, nil
}

// problems collects validation errors to report at once, each with an optional prefix.
type problems struct {
	prefix string
	errs   []error
}

func (p *problems) add(format string, args ...any) {
	p.errs = append(p.errs, fmt.Errorf(p.prefix+format, args...))
}

// err returns the problems as one sorted error, or nil.
func (p *problems) err() error {
	slices.SortFunc(p.errs, func(a, b error) int { return strings.Compare(a.Error(), b.Error()) })
	return errors.Join(p.errs...)
}

// Finding is one cross-document problem compiling a host, e.g. an undefined reference.
type Finding struct {
	Host string `json:"host"`
	Err  string `json:"error"`
}

// Lint compiles every Host (or those given) and returns findings; values as in ResolveOptions.
func Lint(ctx context.Context, ix *Index, values Values, hosts ...string) []Finding {
	if len(hosts) == 0 {
		hosts = ix.HostNames()
	}
	slices.Sort(hosts)
	var findings []Finding
	for _, h := range hosts {
		_, err := ix.Resolve(ctx, ResolveOptions{Host: h, Values: values})
		if err == nil {
			continue
		}
		// errors.Join produces one error per line; report them separately.
		if joined, ok := err.(interface{ Unwrap() []error }); ok {
			for _, e := range joined.Unwrap() {
				findings = append(findings, Finding{Host: h, Err: e.Error()})
			}
			continue
		}
		findings = append(findings, Finding{Host: h, Err: err.Error()})
	}
	return findings
}

// ErrNoHosts is returned by LintPaths when the documents define no Host: they loaded, but nothing could be compiled.
var ErrNoHosts = errors.New("no Host documents found; nothing to compile")

// LintPaths is LoadPaths then Lint. An error means a document is invalid on its own or nothing loaded.
func LintPaths(ctx context.Context, hosts []string, values Values, paths ...string) (*Index, []Finding, error) {
	ix, err := LoadPaths(paths...)
	if err != nil {
		return nil, nil, err
	}
	if len(ix.Hosts) == 0 && len(hosts) == 0 {
		return ix, nil, ErrNoHosts
	}
	for _, h := range hosts {
		if _, ok := ix.Hosts[h]; !ok {
			return ix, nil, fmt.Errorf("no Host named %q (known: %v)", h, ix.HostNames())
		}
	}
	return ix, Lint(ctx, ix, values, hosts...), nil
}
